// Package worker is the delivery engine: it moves notifications from the queue in
// PostgreSQL to the provider, at most RatePerSecond per channel (ADR-0003, ADR-0013).
package worker

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/time/rate"

	"github.com/kaanismen/insider-assessment/internal/config"
	"github.com/kaanismen/insider-assessment/internal/logging"
	"github.com/kaanismen/insider-assessment/internal/notification"
	"github.com/kaanismen/insider-assessment/internal/provider"
	"github.com/kaanismen/insider-assessment/internal/store"
)

// Store is the part of the store the engine needs; tests pass an in-memory fake.
type Store interface {
	Claim(ctx context.Context, p store.ClaimParams) ([]notification.Notification, error)
	MarkSent(ctx context.Context, id uuid.UUID, attempt int, providerMessageID string) (bool, error)
	MarkFailed(ctx context.Context, id uuid.UUID, attempt int, reason string) (bool, error)
}

// Sender delivers one message to the provider.
type Sender interface {
	Send(ctx context.Context, m provider.Message) (provider.Result, error)
}

// resultWriteTimeout bounds the database update after a send. It is separate from the
// shutdown signal, because a result we fail to record turns into a duplicate later.
const resultWriteTimeout = 5 * time.Second

// Engine runs one independent pipeline per channel.
type Engine struct {
	store  Store
	sender Sender
	logger *slog.Logger
	cfg    config.Worker
}

// New returns an engine; call Run to start it.
func New(st Store, sender Sender, logger *slog.Logger, cfg config.Worker) *Engine {
	return &Engine{store: st, sender: sender, logger: logger.With("component", "worker"), cfg: cfg}
}

// Run delivers notifications until ctx is cancelled, then waits for in-flight sends to
// finish and returns.
func (e *Engine) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, ch := range []notification.Channel{notification.ChannelSMS, notification.ChannelEmail, notification.ChannelPush} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.runChannel(ctx, ch)
		}()
	}
	wg.Wait()
}

// runChannel is one channel's pipeline: a dispatcher claims rows and hands them one by
// one to a pool of senders over an unbuffered Go channel. Unbuffered means the dispatcher
// can only hand over a row when a sender is free, so it never claims far ahead of what
// it can send and claimed rows wait well under their lease.
func (e *Engine) runChannel(ctx context.Context, ch notification.Channel) {
	logger := e.logger.With("channel", string(ch))
	// Burst 1: no tokens pile up while the channel is idle, so no one-second window can
	// carry more than RatePerSecond sends (ADR-0003).
	limiter := rate.NewLimiter(rate.Limit(e.cfg.RatePerSecond), 1)
	// The token bucket paces; the window cap makes the per-second ceiling exact (see ratelimit.go).
	limit := limits{pace: limiter, ceiling: newWindowCap(e.cfg.RatePerSecond, time.Second+windowMargin)}
	jobs := make(chan notification.Notification)

	var senders sync.WaitGroup
	for range e.cfg.SendersPerChannel {
		senders.Add(1)
		go func() {
			defer senders.Done()
			for n := range jobs {
				e.deliver(ctx, logger, limit, n)
			}
		}()
	}

	logger.Info("channel started", "rate_per_second", e.cfg.RatePerSecond, "senders", e.cfg.SendersPerChannel)
	e.dispatch(ctx, logger, ch, jobs)
	close(jobs) // lets the senders' range loops end
	senders.Wait()
	logger.Info("channel stopped")
}

func (e *Engine) dispatch(ctx context.Context, logger *slog.Logger, ch notification.Channel, jobs chan<- notification.Notification) {
	for ctx.Err() == nil {
		batch, err := e.store.Claim(ctx, store.ClaimParams{
			Channel: ch,
			Limit:   e.cfg.ClaimBatchSize,
			Weights: e.cfg.PriorityWeights,
			Lease:   e.cfg.Lease,
		})
		if err != nil {
			if ctx.Err() == nil {
				logger.Error("claim failed", "error", err)
				sleep(ctx, e.cfg.PollInterval)
			}
			continue
		}

		for i, n := range batch {
			select {
			case jobs <- n:
			case <-ctx.Done():
				// These rows stay processing until their lease expires, then any worker
				// reclaims them. Phase 4 releases them straight away instead.
				logger.Info("stopping with claimed rows not yet sent", "count", len(batch)-i)
				return
			}
		}

		// A full batch suggests more work is waiting: claim again at once. Otherwise
		// poll, so an idle channel costs a few cheap queries per second (ADR-0013).
		if len(batch) < e.cfg.ClaimBatchSize {
			sleep(ctx, e.cfg.PollInterval)
		}
	}
}

func (e *Engine) deliver(ctx context.Context, logger *slog.Logger, limit limits, n notification.Notification) {
	// Worker log lines carry the request ID of the API call that created the
	// notification, so one search finds its whole history.
	ctx = logging.WithRequestID(ctx, n.CorrelationID)
	logger = logger.With("notification_id", n.ID.String(), "attempt", n.Attempts)

	if err := limit.wait(ctx); err != nil {
		return // shutting down before this row got a slot; it has not been sent
	}

	// From here on, finish even if shutdown starts: cancelling a request the provider
	// may already have accepted, or skipping the result update, causes a duplicate send.
	// The provider timeout and resultWriteTimeout bound how long this takes.
	keep := context.WithoutCancel(ctx)

	start := time.Now()
	res, sendErr := e.sender.Send(keep, provider.Message{
		ID: n.ID, Channel: n.Channel, Recipient: n.Recipient, Content: n.Content, CorrelationID: n.CorrelationID,
	})
	elapsed := time.Since(start)

	writeCtx, cancel := context.WithTimeout(keep, resultWriteTimeout)
	defer cancel()

	if sendErr != nil {
		// Phase 3: every failure is final. Phase 4 retries transient ones with backoff.
		owned, err := e.store.MarkFailed(writeCtx, n.ID, n.Attempts, sendErr.Error())
		switch {
		case err != nil:
			logger.ErrorContext(ctx, "could not record failed delivery", "error", err, "send_error", sendErr)
		case !owned:
			logger.WarnContext(ctx, "failed delivery not recorded: row no longer owned by this worker", "send_error", sendErr)
		default:
			logger.WarnContext(ctx, "delivery failed", "error", sendErr, "duration_ms", elapsed.Milliseconds())
		}
		return
	}

	if res.MessageID == "" {
		logger.WarnContext(ctx, "provider accepted the message without a messageId")
	}
	owned, err := e.store.MarkSent(writeCtx, n.ID, n.Attempts, res.MessageID)
	switch {
	case err != nil:
		logger.ErrorContext(ctx, "could not record successful delivery", "error", err)
	case !owned:
		logger.WarnContext(ctx, "delivery not recorded: row no longer owned by this worker")
	default:
		logger.InfoContext(ctx, "notification sent",
			"provider_message_id", res.MessageID, "duration_ms", elapsed.Milliseconds())
	}
}

// sleep waits for d or until ctx is cancelled, whichever comes first.
func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// limits combines the two rate controls of one channel.
type limits struct {
	pace    *rate.Limiter // spreads sends evenly, 1/rate apart
	ceiling *windowCap    // guarantees at most rate sends in any one-second window
}

func (l limits) wait(ctx context.Context) error {
	if err := l.pace.Wait(ctx); err != nil {
		return err
	}
	return l.ceiling.Wait(ctx)
}
