// Package worker is the delivery engine: it moves notifications from the queue in
// PostgreSQL to the provider, at most RatePerSecond per channel (ADR-0003, ADR-0013).
package worker

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/time/rate"

	"github.com/kaanismen/event-driven-notification-system/internal/config"
	"github.com/kaanismen/event-driven-notification-system/internal/logging"
	"github.com/kaanismen/event-driven-notification-system/internal/metrics"
	"github.com/kaanismen/event-driven-notification-system/internal/notification"
	"github.com/kaanismen/event-driven-notification-system/internal/provider"
	"github.com/kaanismen/event-driven-notification-system/internal/store"
	"github.com/kaanismen/event-driven-notification-system/internal/tracing"
)

// Store is the part of the store the engine needs; tests pass an in-memory fake.
type Store interface {
	Claim(ctx context.Context, p store.ClaimParams) ([]notification.Notification, error)
	MarkSent(ctx context.Context, id uuid.UUID, attempt int, providerMessageID string) (bool, time.Duration, error)
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
	metrics *metrics.Metrics
	cfg     config.Worker
	backoff backoff

	// sendStarted, when set, is told the exact time the rate limiter admitted each send.
	// Only tests set it: they check the 100-per-second ceiling on these times, which is
	// what the engine controls, instead of only on arrival times at a mock provider,
	// which also include goroutine scheduling and network jitter (ADR-0003).
	sendStarted func(ch notification.Channel, at time.Time)

	tracer trace.Tracer
}

// New returns an engine; call Run to start it. tracer creates one trace per delivery
// attempt (ADR-0021); pass a no-op tracer to turn tracing off.
func New(st Store, sender Sender, logger *slog.Logger, m *metrics.Metrics, cfg config.Worker, tracer trace.Tracer) *Engine {
	return &Engine{
		store: st, sender: sender, logger: logger.With("component", "worker"), metrics: m, cfg: cfg,
		backoff: newBackoff(cfg.RetryBaseDelay, cfg.RetryMaxDelay),
		tracer:  tracer,
	}
}

// Outcomes of one delivery attempt, recorded on its span.
const (
	spanSent     = "sent"
	spanRetry    = "retry"
	spanFailed   = "failed"
	spanReleased = "released"
	spanUnknown  = "not recorded" // the result could not be written (lease lost, database error)
)

// startDelivery starts the root span of one delivery attempt. It is a new trace, linked
// to the API request that created the notification instead of being its child: an
// attempt can run minutes or hours after the request (retries, scheduling), and a batch
// request has up to 1000 of them, which would make that one trace huge and never-ending.
// This is OpenTelemetry's recommended shape for messages taken from a queue.
func (e *Engine) startDelivery(ctx context.Context, n notification.Notification) (context.Context, trace.Span) {
	opts := []trace.SpanStartOption{
		trace.WithNewRoot(),
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(
			attribute.String("notification.id", n.ID.String()),
			attribute.String("notification.channel", string(n.Channel)),
			attribute.String("notification.priority", string(n.Priority)),
			attribute.Int("notification.attempt", n.Attempts),
			// How long the row was due before a worker took it: the time spent queueing.
			attribute.Int64("notification.queue_delay_ms", time.Since(n.NextAttemptAt).Milliseconds()),
		),
	}
	if n.BatchID != nil {
		opts = append(opts, trace.WithAttributes(attribute.String("notification.batch_id", n.BatchID.String())))
	}
	if sc := tracing.SpanContextOf(n.TraceParent); sc.IsValid() {
		opts = append(opts, trace.WithLinks(trace.Link{
			SpanContext: sc,
			Attributes:  []attribute.KeyValue{attribute.String("link.reason", "created by this request")},
		}))
	}
	return e.tracer.Start(ctx, "deliver "+string(n.Channel), opts...)
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
	brk := newBreaker(e.cfg.BreakerThreshold, e.cfg.BreakerCooldown, logger)
	brk.onChange = func(s breakerState) { e.metrics.BreakerState(ch, int(s)) }
	e.metrics.BreakerState(ch, int(breakerClosed))
	jobs := make(chan job)

	var senders sync.WaitGroup
	for range e.cfg.SendersPerChannel {
		senders.Add(1)
		go func() {
			defer senders.Done()
			for j := range jobs {
				e.deliver(ctx, logger, limit, brk, j)
			}
		}()
	}

	logger.Info("channel started", "rate_per_second", e.cfg.RatePerSecond, "senders", e.cfg.SendersPerChannel)
	e.dispatch(ctx, logger, ch, brk, jobs)
	close(jobs) // lets the senders' range loops end
	senders.Wait()
	logger.Info("channel stopped")
}

// job is one claimed row handed to a sender. probe marks the single message a half-open
// circuit breaker lets through to test the provider.
type job struct {
	n     notification.Notification
	probe bool
}

func (e *Engine) dispatch(ctx context.Context, logger *slog.Logger, ch notification.Channel, brk *breaker, jobs chan<- job) {
	for ctx.Err() == nil {
		if n, err := e.store.FailExhaustedLeases(ctx, ch); err != nil {
			if ctx.Err() == nil {
				logger.Error("failing exhausted leases", "error", err)
			}
		} else if n > 0 {
			logger.Warn("dead-lettered rows whose worker stopped during their final attempt", "count", n)
			for range n {
				e.metrics.Delivery(ch, metrics.OutcomeFailed)
			}
		}

		// While the breaker is open nothing is claimed, so waiting messages keep their
		// attempts instead of using them up against a provider that is down (ADR-0015).
		probe, wait, ok := brk.gate()
		if !ok {
			if wait <= 0 {
				wait = e.cfg.PollInterval // a probe is out; check again soon
			}
			sleep(ctx, wait)
			continue
		}
		limit := e.cfg.ClaimBatchSize
		if probe {
			limit = 1
		}

		batch, err := e.store.Claim(ctx, store.ClaimParams{
			Channel: ch,
			Limit:   limit,
			Weights: e.cfg.PriorityWeights,
			Lease:   e.cfg.Lease,
		})
		if probe && (err != nil || len(batch) == 0) {
			brk.abortProbe() // nothing to probe with; try again on the next round
		}
		if err != nil {
			if ctx.Err() == nil {
				logger.Error("claim failed", "error", err)
				sleep(ctx, e.cfg.PollInterval)
			}
			continue
		}

		for i, n := range batch {
			select {
			case jobs <- job{n: n, probe: probe}:
			case <-ctx.Done():
				// Shutting down: hand the rows nobody started back to the queue now,
				// instead of leaving them until their lease expires (ADR-0014).
				logger.Info("releasing claimed rows not yet sent", "count", len(batch)-i)
				for _, rest := range batch[i:] {
					e.release(ctx, logger, rest, metrics.ReleaseShutdown)
				}
				return
			}
		}

		// A full batch suggests more work is waiting: claim again at once. Otherwise
		// poll, so an idle channel costs a few cheap queries per second (ADR-0013).
		if len(batch) < limit {
			sleep(ctx, e.cfg.PollInterval)
		}
	}
}

func (e *Engine) deliver(ctx context.Context, logger *slog.Logger, limit limits, brk *breaker, j job) {
	n := j.n
	// Worker log lines carry the request ID of the API call that created the
	// notification, so one search finds its whole history.
	ctx = logging.WithRequestID(ctx, n.CorrelationID)
	logger = logger.With("notification_id", n.ID.String(), "attempt", n.Attempts)

	ctx, span := e.startDelivery(ctx, n)
	outcome := spanUnknown
	defer func() {
		span.SetAttributes(attribute.String("notification.outcome", outcome))
		span.End()
	}()

	waitCtx, waitSpan := e.tracer.Start(ctx, "rate limit wait")
	admitted, err := limit.wait(waitCtx)
	waitSpan.End()
	if err != nil {
		// Shutting down before this row got a rate slot: it was never sent.
		if j.probe {
			brk.abortProbe()
		}
		outcome = spanReleased
		span.SetAttributes(attribute.String("notification.release_reason", metrics.ReleaseShutdown))
		e.release(ctx, logger, n, metrics.ReleaseShutdown)
		return
	}
	if !j.probe && !brk.closed() {
		// The breaker opened while this row waited for its rate slot. Hand it back with
		// its attempt rather than send it into the outage.
		logger.DebugContext(ctx, "circuit breaker open: row handed back unsent")
		outcome = spanReleased
		span.SetAttributes(attribute.String("notification.release_reason", metrics.ReleaseBreaker))
		e.release(ctx, logger, n, metrics.ReleaseBreaker)
		return
	}
	if e.sendStarted != nil {
		e.sendStarted(n.Channel, admitted)
	}

	// From here on, finish even if shutdown starts: cancelling a request the provider
	// may already have accepted, or skipping the result update, causes a duplicate send.
	// The provider timeout and resultWriteTimeout bound how long this takes.
	keep := context.WithoutCancel(ctx)

	sendCtx, sendSpan := e.tracer.Start(keep, "provider call", trace.WithSpanKind(trace.SpanKindClient))
	start := time.Now()
	res, sendErr := e.sender.Send(sendCtx, provider.Message{
		ID: n.ID, Channel: n.Channel, Recipient: n.Recipient, Content: n.Content, CorrelationID: n.CorrelationID,
	})
	elapsed := time.Since(start)
	transient := sendErr != nil && provider.Retryable(sendErr)
	brk.record(j.probe, transient)
	result := callResult(sendErr, transient)
	e.metrics.ProviderCall(n.Channel, result, elapsed)
	sendSpan.SetAttributes(attribute.String("provider.result", result))
	if sendErr != nil {
		sendSpan.SetStatus(codes.Error, sendErr.Error())
	}
	sendSpan.End()

	writeCtx, cancel := context.WithTimeout(keep, resultWriteTimeout)
	defer cancel()
	writeCtx, writeSpan := e.tracer.Start(writeCtx, "record result")
	defer writeSpan.End()

	if sendErr != nil {
		outcome = e.recordFailure(writeCtx, logger, n, sendErr, elapsed)
		if outcome == spanFailed {
			span.SetStatus(codes.Error, sendErr.Error())
		}
		return
	}

	if res.MessageID == "" {
		logger.WarnContext(ctx, "provider accepted the message without a messageId")
	}
	owned, latency, err := e.store.MarkSent(writeCtx, n.ID, n.Attempts, res.MessageID)
	switch {
	case err != nil:
		logger.ErrorContext(ctx, "could not record successful delivery", "error", err)
	case !owned:
		logger.WarnContext(ctx, "delivery not recorded: row no longer owned by this worker")
	default:
		outcome = spanSent
		e.metrics.Delivery(n.Channel, metrics.OutcomeSent)
		e.metrics.Delivered(n.Channel, latency)
		logger.InfoContext(ctx, "notification sent",
			"provider_message_id", res.MessageID, "duration_ms", elapsed.Milliseconds())
	}
}

// recordFailure retries a transient failure while attempts remain and dead-letters
// everything else (ADR-0002). A retry is a row back in the queue with a later due time,
// not a sleep in this goroutine: the sender is free at once, the retry survives a
// restart, and it passes the channel's rate limiter like any first attempt (DR-06).
// ctx bounds the database update and carries the request ID for the log lines. It
// returns the outcome for the delivery span.
func (e *Engine) recordFailure(ctx context.Context, logger *slog.Logger, n notification.Notification, sendErr error, elapsed time.Duration) string {
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
			e.metrics.Delivery(n.Channel, metrics.OutcomeRetry)
			logger.WarnContext(ctx, "delivery failed, will retry",
				append(attrs, "retry_in_ms", delay.Milliseconds(), "max_attempts", n.MaxAttempts)...)
			return spanRetry
		}
		return spanUnknown
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
		e.metrics.Delivery(n.Channel, metrics.OutcomeFailed)
		logger.ErrorContext(ctx, "delivery failed for good", append(attrs, "reason", why)...)
		return spanFailed
	}
	return spanUnknown
}

// release hands an unsent row back to the queue, at shutdown or when the breaker is
// open. At shutdown ctx is already cancelled, so the update gets its own short deadline.
func (e *Engine) release(ctx context.Context, logger *slog.Logger, n notification.Notification, reason string) {
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), resultWriteTimeout)
	defer cancel()
	owned, err := e.store.Release(releaseCtx, n.ID, n.Attempts)
	switch {
	case err != nil:
		// Not fatal: the row stays processing and is reclaimed when its lease expires.
		logger.ErrorContext(ctx, "could not release unsent row", "notification_id", n.ID.String(), "error", err)
	case owned:
		e.metrics.Released(n.Channel, reason)
	}
}

// callResult names the result of a provider call for the duration histogram.
func callResult(sendErr error, transient bool) string {
	switch {
	case sendErr == nil:
		return metrics.ResultSuccess
	case transient:
		return metrics.ResultTransient
	default:
		return metrics.ResultPermanent
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

// wait blocks until both controls allow one more send and returns the moment the
// window cap recorded for it.
func (l limits) wait(ctx context.Context) (time.Time, error) {
	if err := l.pace.Wait(ctx); err != nil {
		return time.Time{}, err
	}
	return l.ceiling.wait(ctx)
}
