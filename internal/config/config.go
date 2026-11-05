// Package config turns environment variables into a typed, validated Config.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Config holds every setting the process needs. It is built once at startup and passed
// down explicitly; nothing reads environment variables after Load returns.
type Config struct {
	HTTPAddr        string
	DatabaseURL     string
	DBMaxConns      int
	LogLevel        slog.Level
	ShutdownTimeout time.Duration
	// MaxAttempts is the delivery budget copied into each new notification (ADR-0002).
	MaxAttempts int

	// ProviderURL is where notifications are POSTed (webhook.site or the mock provider).
	// It is never logged in full: a webhook.site URL works like a password.
	ProviderURL     string
	ProviderTimeout time.Duration

	Worker Worker
}

// Worker holds the delivery engine settings (ADR-0003, ADR-0005, ADR-0008, ADR-0013).
type Worker struct {
	RatePerSecond     int           // per channel
	SendersPerChannel int           // concurrent provider calls per channel
	ClaimBatchSize    int           // rows claimed per database round trip
	PollInterval      time.Duration // wait between claims when the queue is empty
	Lease             time.Duration // how long a claimed row belongs to one worker
	// PriorityWeights are the shares for high, normal and low when a channel has a
	// backlog. Every weight must be at least 1, so no level can starve.
	PriorityWeights [3]int
	// RetryBaseDelay and RetryMaxDelay shape the backoff between attempts (ADR-0002):
	// the wait after attempt n is random in [0, min(max, base*2^(n-1))].
	RetryBaseDelay time.Duration
	RetryMaxDelay  time.Duration
}

// maxRetryDelay bounds RETRY_MAX_DELAY. Notifications are time-sensitive, so waits of
// hours are a misconfiguration; the bound also keeps the backoff arithmetic far from
// overflowing a Duration.
const maxRetryDelay = time.Hour

// Load reads the configuration through getenv (os.Getenv in production, a map lookup in
// tests). It reports every invalid setting at once, so a misconfigured deployment fails
// with one complete message instead of one error per restart.
func Load(getenv func(string) string) (Config, error) {
	p := parser{getenv: getenv}

	cfg := Config{
		HTTPAddr:        p.str("HTTP_ADDR", ":8080"),
		DatabaseURL:     p.str("DATABASE_URL", ""),
		DBMaxConns:      p.integer("DB_MAX_CONNS", 20, 2, 500),
		ShutdownTimeout: p.duration("SHUTDOWN_TIMEOUT", 15*time.Second),
		// The upper bound keeps a typo such as 1000 from turning a dead provider into
		// hours of retries per message.
		MaxAttempts:     p.integer("MAX_ATTEMPTS", 8, 1, 20),
		ProviderURL:     p.str("PROVIDER_URL", ""),
		ProviderTimeout: p.duration("PROVIDER_TIMEOUT", 10*time.Second),
		Worker: Worker{
			RatePerSecond:     p.integer("RATE_LIMIT_PER_SECOND", 100, 1, 10_000),
			SendersPerChannel: p.integer("SENDERS_PER_CHANNEL", 50, 1, 1000),
			// At least 3, so each priority level gets one slot per claim (ADR-0008).
			ClaimBatchSize:  p.integer("CLAIM_BATCH_SIZE", 20, 3, 1000),
			PollInterval:    p.duration("POLL_INTERVAL", 250*time.Millisecond),
			Lease:           p.duration("LEASE_DURATION", 30*time.Second),
			PriorityWeights: p.weights("PRIORITY_WEIGHTS", [3]int{70, 20, 10}),
			RetryBaseDelay:  p.duration("RETRY_BASE_DELAY", 2*time.Second),
			RetryMaxDelay:   p.duration("RETRY_MAX_DELAY", 5*time.Minute),
		},
	}

	if err := cfg.LogLevel.UnmarshalText([]byte(p.str("LOG_LEVEL", "info"))); err != nil {
		p.fail("LOG_LEVEL: %w", err)
	}
	if cfg.DatabaseURL == "" {
		p.fail("DATABASE_URL is required")
	}
	if cfg.ProviderURL == "" {
		p.fail("PROVIDER_URL is required")
	} else if u, err := url.Parse(cfg.ProviderURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		// The value is not repeated in the error: it may contain a secret token.
		p.fail("PROVIDER_URL must be an absolute http or https URL")
	}

	if r := cfg.Worker; r.RetryMaxDelay > maxRetryDelay {
		p.fail("RETRY_MAX_DELAY must be at most %s, got %s", maxRetryDelay, r.RetryMaxDelay)
	} else if r.RetryBaseDelay > r.RetryMaxDelay {
		p.fail("RETRY_BASE_DELAY (%s) must not exceed RETRY_MAX_DELAY (%s)", r.RetryBaseDelay, r.RetryMaxDelay)
	}

	// A claimed row must not outlive its lease while it waits: in the worst case it waits
	// for every row ahead of it (claim batch plus busy senders) to get a rate-limit token,
	// then for one provider call. Otherwise another worker could claim it and send twice.
	w := cfg.Worker
	if w.RatePerSecond > 0 {
		queueWait := time.Duration(w.ClaimBatchSize+w.SendersPerChannel) * time.Second / time.Duration(w.RatePerSecond)
		if minimum := cfg.ProviderTimeout + queueWait; w.Lease <= minimum {
			p.fail("LEASE_DURATION (%s) must be longer than PROVIDER_TIMEOUT plus the worst rate-limit wait (%s)", w.Lease, minimum)
		}
	}

	if err := errors.Join(p.errs...); err != nil {
		return Config{}, fmt.Errorf("invalid configuration: %w", err)
	}
	return cfg, nil
}

// parser collects errors instead of stopping at the first, so Load can report them all.
type parser struct {
	getenv func(string) string
	errs   []error
}

func (p *parser) fail(format string, args ...any) {
	p.errs = append(p.errs, fmt.Errorf(format, args...))
}

func (p *parser) str(name, fallback string) string {
	if v := strings.TrimSpace(p.getenv(name)); v != "" {
		return v
	}
	return fallback
}

func (p *parser) duration(name string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(p.getenv(name))
	if raw == "" {
		return fallback
	}
	d, err := time.ParseDuration(raw)
	switch {
	case err != nil:
		p.fail("%s: %w", name, err)
	case d <= 0:
		p.fail("%s: must be positive, got %s", name, d)
	default:
		return d
	}
	return fallback
}

func (p *parser) integer(name string, fallback, lo, hi int) int {
	raw := strings.TrimSpace(p.getenv(name))
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	switch {
	case err != nil:
		p.fail("%s: %w", name, err)
	case n < lo || n > hi:
		p.fail("%s: must be between %d and %d, got %d", name, lo, hi, n)
	default:
		return n
	}
	return fallback
}

// weights parses "high,normal,low", for example "70,20,10".
func (p *parser) weights(name string, fallback [3]int) [3]int {
	raw := strings.TrimSpace(p.getenv(name))
	if raw == "" {
		return fallback
	}
	parts := strings.Split(raw, ",")
	if len(parts) != 3 {
		p.fail("%s: want three comma-separated weights for high,normal,low, got %q", name, raw)
		return fallback
	}
	var w [3]int
	for i, part := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || n < 1 || n > 1000 {
			p.fail("%s: each weight must be an integer from 1 to 1000, got %q", name, part)
			return fallback
		}
		w[i] = n
	}
	return w
}
