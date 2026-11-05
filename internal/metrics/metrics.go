// Package metrics defines the Prometheus metrics of the service (ADR-0016) and serves
// them at /metrics. Everything lives in one Registry owned by main, not in the
// client library's global default registry, so tests get fresh counters and nothing is
// shared between instances.
package metrics

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/kaanismen/event-driven-notification-system/internal/notification"
	"github.com/kaanismen/event-driven-notification-system/internal/store"
)

// Outcomes of a delivery attempt, as recorded in the database.
const (
	OutcomeSent   = "sent"   // the provider accepted the message
	OutcomeRetry  = "retry"  // transient failure, back in the queue
	OutcomeFailed = "failed" // permanent failure or no attempts left (dead letter)
)

// Results of a single provider call.
const (
	ResultSuccess   = "success"
	ResultTransient = "transient_error"
	ResultPermanent = "permanent_error"
)

// Reasons a claimed row was handed back unsent.
const (
	ReleaseShutdown = "shutdown"
	ReleaseBreaker  = "breaker_open"
)

// Reasons a real-time subscriber was disconnected by the server (ADR-0020).
const (
	SubscriberSlow   = "slow"   // its buffer filled up: it was not reading fast enough
	SubscriberResync = "resync" // the database listener reconnected and events may be missing
)

// Metrics holds every metric the service records. It is safe for concurrent use.
type Metrics struct {
	registry *prometheus.Registry

	deliveries       *prometheus.CounterVec
	providerDuration *prometheus.HistogramVec
	deliveryLatency  *prometheus.HistogramVec
	breakerState     *prometheus.GaugeVec
	releases         *prometheus.CounterVec
	subscribers      prometheus.Gauge
	subscribersEnded *prometheus.CounterVec
}

// New creates the metrics and registers them, together with the Go runtime and process
// metrics (goroutines, memory, open files) that come with the client library.
func New() *Metrics {
	m := &Metrics{
		registry: prometheus.NewRegistry(),
		deliveries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "notifier_deliveries_total",
			Help: "Delivery attempts by recorded outcome: sent, retry (transient failure, requeued) or failed (dead letter).",
		}, []string{"channel", "outcome"}),
		providerDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "notifier_provider_request_duration_seconds",
			Help: "Duration of provider calls, by result.",
			// From a fast mock (about 1 ms) to the 10 s default provider timeout.
			Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
		}, []string{"channel", "result"}),
		deliveryLatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "notifier_delivery_latency_seconds",
			Help: "Time from creation to acceptance by the provider, including queueing and retries.",
			// From near-instant delivery to a long backlog (A-20) or a retry window of minutes.
			Buckets: []float64{.1, .25, .5, 1, 2.5, 5, 10, 30, 60, 120, 300, 900, 3600},
		}, []string{"channel"}),
		breakerState: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "notifier_circuit_breaker_state",
			Help: "Circuit breaker state per channel: 0 closed, 1 open, 2 half-open (ADR-0015).",
		}, []string{"channel"}),
		releases: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "notifier_released_total",
			Help: "Claimed rows handed back to the queue unsent, with their attempt given back.",
		}, []string{"channel", "reason"}),
		subscribers: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "notifier_event_subscribers",
			Help: "Open real-time status connections on this instance (ADR-0020).",
		}),
		subscribersEnded: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "notifier_event_subscribers_dropped_total",
			Help: "Real-time connections closed by the server: slow (could not keep up) or resync (events may have been missed).",
		}, []string{"reason"}),
	}
	m.registry.MustRegister(
		m.deliveries, m.providerDuration, m.deliveryLatency, m.breakerState, m.releases,
		m.subscribers, m.subscribersEnded,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return m
}

// Delivery counts one attempt whose outcome was recorded in the database.
func (m *Metrics) Delivery(ch notification.Channel, outcome string) {
	m.deliveries.WithLabelValues(string(ch), outcome).Inc()
}

// ProviderCall records how long one provider call took and how it ended.
func (m *Metrics) ProviderCall(ch notification.Channel, result string, d time.Duration) {
	m.providerDuration.WithLabelValues(string(ch), result).Observe(d.Seconds())
}

// Delivered records the end-to-end latency of a sent message.
func (m *Metrics) Delivered(ch notification.Channel, latency time.Duration) {
	m.deliveryLatency.WithLabelValues(string(ch)).Observe(latency.Seconds())
}

// BreakerState records a channel's circuit breaker state (0, 1 or 2).
func (m *Metrics) BreakerState(ch notification.Channel, state int) {
	m.breakerState.WithLabelValues(string(ch)).Set(float64(state))
}

// Released counts a row handed back unsent.
func (m *Metrics) Released(ch notification.Channel, reason string) {
	m.releases.WithLabelValues(string(ch), reason).Inc()
}

// Subscribers adds delta to the number of open real-time connections.
func (m *Metrics) Subscribers(delta int) {
	m.subscribers.Add(float64(delta))
}

// SubscriberDropped counts a real-time connection the server closed, by reason.
func (m *Metrics) SubscriberDropped(reason string) {
	m.subscribersEnded.WithLabelValues(reason).Inc()
}

// QueueDepthReader reads the current queue size from the database.
type QueueDepthReader interface {
	QueueDepth(ctx context.Context) ([]store.QueueDepth, error)
}

// WatchQueue adds the queue depth gauge, read from the database on every scrape (A-22).
func (m *Metrics) WatchQueue(r QueueDepthReader, logger *slog.Logger) {
	m.registry.MustRegister(&queueCollector{reader: r, logger: logger})
}

// Handler serves the metrics in the Prometheus text format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{
		// A failing queue depth query must not hide every other metric.
		ErrorHandling: promhttp.ContinueOnError,
	})
}

// queueDepthTimeout bounds the database query behind one scrape.
const queueDepthTimeout = 2 * time.Second

var queueDepthDesc = prometheus.NewDesc(
	"notifier_queue_depth",
	"Open notifications per channel, priority and state: pending (due now or retrying later, A-37), scheduled (waiting for a future send time) or processing.",
	[]string{"channel", "priority", "status"}, nil,
)

// queueCollector is a custom collector: instead of a gauge updated by the code, it asks
// the database at scrape time, so the number is always current and survives restarts.
type queueCollector struct {
	reader QueueDepthReader
	logger *slog.Logger
}

func (c *queueCollector) Describe(ch chan<- *prometheus.Desc) { ch <- queueDepthDesc }

func (c *queueCollector) Collect(ch chan<- prometheus.Metric) {
	// The collector interface has no context; the timeout keeps a slow database from
	// holding the scrape open.
	ctx, cancel := context.WithTimeout(context.Background(), queueDepthTimeout)
	defer cancel()

	rows, err := c.reader.QueueDepth(ctx)
	if err != nil {
		c.logger.Warn("queue depth query failed", "error", err)
		ch <- prometheus.NewInvalidMetric(queueDepthDesc, err)
		return
	}

	// Every combination is reported, zeros included, so a series does not vanish when
	// its queue empties (a missing series reads as "unknown", not as "zero").
	counts := map[[3]string]int64{}
	for _, r := range rows {
		counts[[3]string{string(r.Channel), string(r.Priority), r.State}] = r.Count
	}
	for _, chn := range []notification.Channel{notification.ChannelSMS, notification.ChannelEmail, notification.ChannelPush} {
		for _, p := range []notification.Priority{notification.PriorityHigh, notification.PriorityNormal, notification.PriorityLow} {
			for _, s := range store.QueueStates() {
				key := [3]string{string(chn), string(p), s}
				ch <- prometheus.MustNewConstMetric(queueDepthDesc, prometheus.GaugeValue, float64(counts[key]), key[:]...)
			}
		}
	}
}
