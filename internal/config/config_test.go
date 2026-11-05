package config

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

// required holds the settings that have no default.
func required(extra map[string]string) func(string) string {
	m := map[string]string{
		"DATABASE_URL": "postgres://localhost/db",
		"PROVIDER_URL": "https://webhook.example/secret-token",
	}
	for k, v := range extra {
		m[k] = v
	}
	return func(key string) string { return m[key] }
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(required(nil))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := Config{
		HTTPAddr:        ":8080",
		DatabaseURL:     "postgres://localhost/db",
		DBMaxConns:      20,
		LogLevel:        slog.LevelInfo,
		ShutdownTimeout: 15 * time.Second,
		MaxAttempts:     8,
		ProviderURL:     "https://webhook.example/secret-token",
		ProviderTimeout: 10 * time.Second,
		Worker: Worker{
			RatePerSecond:     100,
			SendersPerChannel: 50,
			ClaimBatchSize:    20,
			PollInterval:      250 * time.Millisecond,
			Lease:             30 * time.Second,
			PriorityWeights:   [3]int{70, 20, 10},
			RetryBaseDelay:    2 * time.Second,
			RetryMaxDelay:     5 * time.Minute,
		},
	}
	if cfg != want {
		t.Errorf("defaults:\n got %+v\nwant %+v", cfg, want)
	}
}

func TestLoadOverrides(t *testing.T) {
	cfg, err := Load(required(map[string]string{
		"HTTP_ADDR":             ":9090",
		"LOG_LEVEL":             "debug",
		"SHUTDOWN_TIMEOUT":      "3s",
		"MAX_ATTEMPTS":          "3",
		"DB_MAX_CONNS":          "40",
		"PROVIDER_TIMEOUT":      "2s",
		"RATE_LIMIT_PER_SECOND": "10",
		"SENDERS_PER_CHANNEL":   "5",
		"CLAIM_BATCH_SIZE":      "5",
		"POLL_INTERVAL":         "1s",
		"LEASE_DURATION":        "1m",
		"PRIORITY_WEIGHTS":      " 5, 3 ,2 ",
		"RETRY_BASE_DELAY":      "100ms",
		"RETRY_MAX_DELAY":       "30s",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	w := cfg.Worker
	if cfg.HTTPAddr != ":9090" || cfg.LogLevel != slog.LevelDebug || cfg.ShutdownTimeout != 3*time.Second ||
		cfg.MaxAttempts != 3 || cfg.DBMaxConns != 40 || cfg.ProviderTimeout != 2*time.Second ||
		w.RatePerSecond != 10 || w.SendersPerChannel != 5 || w.ClaimBatchSize != 5 ||
		w.PollInterval != time.Second || w.Lease != time.Minute || w.PriorityWeights != [3]int{5, 3, 2} ||
		w.RetryBaseDelay != 100*time.Millisecond || w.RetryMaxDelay != 30*time.Second {
		t.Errorf("unexpected config: %+v", cfg)
	}
}

func TestLoadReportsAllErrors(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantErr []string
	}{
		{
			name:    "missing required settings",
			env:     map[string]string{"DATABASE_URL": "", "PROVIDER_URL": ""},
			wantErr: []string{"DATABASE_URL is required", "PROVIDER_URL is required"},
		},
		{
			name: "every setting invalid",
			env: map[string]string{
				"LOG_LEVEL":             "loud",
				"SHUTDOWN_TIMEOUT":      "soon",
				"MAX_ATTEMPTS":          "many",
				"RATE_LIMIT_PER_SECOND": "fast",
				"PRIORITY_WEIGHTS":      "70,20",
			},
			wantErr: []string{"LOG_LEVEL", "SHUTDOWN_TIMEOUT", "MAX_ATTEMPTS", "RATE_LIMIT_PER_SECOND", "PRIORITY_WEIGHTS"},
		},
		{"non-positive duration", map[string]string{"SHUTDOWN_TIMEOUT": "0s"}, []string{"must be positive"}},
		{"max attempts out of range", map[string]string{"MAX_ATTEMPTS": "0"}, []string{"between 1 and 20"}},
		{"claim batch too small", map[string]string{"CLAIM_BATCH_SIZE": "2"}, []string{"CLAIM_BATCH_SIZE"}},
		{"zero weight could starve a level", map[string]string{"PRIORITY_WEIGHTS": "70,30,0"}, []string{"PRIORITY_WEIGHTS"}},
		{"provider url not http", map[string]string{"PROVIDER_URL": "ftp://example.com/x"}, []string{"absolute http or https"}},
		{"provider url relative", map[string]string{"PROVIDER_URL": "/hook"}, []string{"absolute http or https"}},
		{"retry cap too long", map[string]string{"RETRY_MAX_DELAY": "2h"}, []string{"RETRY_MAX_DELAY must be at most"}},
		{"retry base above cap", map[string]string{"RETRY_BASE_DELAY": "10m"}, []string{"must not exceed RETRY_MAX_DELAY"}},
		{"lease shorter than the worst wait", map[string]string{"LEASE_DURATION": "10s"}, []string{"LEASE_DURATION"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(required(tt.env))
			if err == nil {
				t.Fatal("Load succeeded, want error")
			}
			for _, want := range tt.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
		})
	}
}

func TestProviderURLIsNotEchoed(t *testing.T) {
	_, err := Load(required(map[string]string{"PROVIDER_URL": "ftp://example.com/secret-token"}))
	if err == nil || strings.Contains(err.Error(), "secret-token") {
		t.Errorf("error must reject the URL without repeating it: %v", err)
	}
}
