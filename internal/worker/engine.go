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
	MarkRetry(ctx context.Context, id uuid.UUID, attempt int, delay time.Duration, reason string) (bool, error)
	Release(ctx context.Context, id uuid.UUID, attempt int) (bool, error)
	FailExhaustedLeases(ctx context.Context, ch notification.Channel) (int64, error)
}

// Sender delivers one message to the provider.
type Sender interface {
	Send(ctx context.Context, m provider.Message) (provider.Result, error)
}

// resultWriteTimeout bounds the database update after a send, and the release of unsent
// rows at shutdown. It is separate from the shutdown signal, because a result we fail to
// record turns into a duplicate later, and an unreleased row waits for its lease.
const resultWriteTimeout = 5 * time.Second

// Engine runs one independent pipeline per channel.
type Engine struct {
	store   Store
	sender  Sender
	logger  *slog.Logger
	cfg     config.Worker
	backoff backoff
}

// New returns an engine; call Run to start it.
func New(st Store, sender Sender, logger *slog.Logger, cfg config.Worker) *Engine {
	return &Engine{
		store: st, sender: sender, logger: logger.With("component", "worker"), cfg: cfg,
		backoff: newBackoff(cfg.RetryBaseDelay, cfg.RetryMaxDelay),
	}
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
		if n, err := e.store.FailExhaustedLeases(ctx, ch); err != nil {
			if ctx.Err() == nil {
				logger.Error("failing exhausted leases", "error", err)
			}
		} else if n > 0 {
			logger.Warn("dead-lettered rows whose worker stopped during their final attempt", "count", n)
		}

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
				// Shutting down: hand the rows nobody started back to the queue now,
				// instead of leaving them until their lease expires (ADR-0014).
				logger.Info("releasing claimed rows not yet sent", "count", len(batch)-i)
				for _, rest := range batch[i:] {
					e.release(ctx, logger, rest)
				}
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
		// Shutting down before this row got a rate slot: it was never sent.
		e.release(ctx, logger, n)
		return
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
		e.recordFailure(writeCtx, logger, n, sendErr, elapsed)
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

// recordFailure retries a transient failure while attempts remain and dead-letters
// everything else (ADR-0002). A retry is a row back in the queue with a later due time,
// not a sleep in this goroutine: the sender is free at once, the retry survives a
// restart, and it passes the channel's rate limiter like any first attempt (DR-06).
// ctx bounds the database update and carries the request ID for the log lines.
func (e *Engine) recordFailure(ctx context.Context, logger *slog.Logger, n notification.Notification, sendErr error, elapsed time.Duration) {
	reason := sendErr.Error()
	attrs := []any{"error", reason, "duration_ms", elapsed.Milliseconds()}

	if provider.Retryable(sendErr) && n.Attempts < n.MaxAttempts {
		delay := e.backoff.delay(n.Attempts, provider.RetryAfter(sendErr))
		owned, err := e.store.MarkRetry(ctx, n.ID, n.Attempts, delay, reason)
		switch {
		case err != nil:
			logger.ErrorContext(ctx, "could not schedule retry", append(attrs, "db_error", err)...)
		case !owned:
			logger.WarnContext(ctx, "retry not recorded: row no longer owned by this worker", attrs...)
		default:
			logger.WarnContext(ctx, "delivery failed, will retry",
				append(attrs, "retry_in_ms", delay.Milliseconds(), "max_attempts", n.MaxAttempts)...)
		}
		return
	}

	why := "permanent error"
	if provider.Retryable(sendErr) {
		why = "no attempts left"
	}
	owned, err := e.store.MarkFailed(ctx, n.ID, n.Attempts, reason)
	switch {
	case err != nil:
		logger.ErrorContext(ctx, "could not record failed delivery", append(attrs, "db_error", err)...)
	case !owned:
		logger.WarnContext(ctx, "failed delivery not recorded: row no longer owned by this worker", attrs...)
	default:
		logger.ErrorContext(ctx, "delivery failed for good", append(attrs, "reason", why)...)
	}
}

// release hands an unsent row back to the queue during shutdown. ctx is already
// cancelled at this point, so the update gets its own short deadline.
func (e *Engine) release(ctx context.Context, logger *slog.Logger, n notification.Notification) {
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), resultWriteTimeout)
	defer cancel()
	if _, err := e.store.Release(releaseCtx, n.ID, n.Attempts); err != nil {
		// Not fatal: the row stays processing and is reclaimed when its lease expires.
		logger.ErrorContext(ctx, "could not release unsent row", "notification_id", n.ID.String(), "error", err)
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
