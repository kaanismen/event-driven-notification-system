package config

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(key string) string { return m[key] }
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(env(map[string]string{"DATABASE_URL": "postgres://localhost/db"}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HTTPAddr != ":8080" {
		t.Errorf("HTTPAddr = %q, want :8080", cfg.HTTPAddr)
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel = %v, want INFO", cfg.LogLevel)
	}
	if cfg.ShutdownTimeout != 15*time.Second {
		t.Errorf("ShutdownTimeout = %v, want 15s", cfg.ShutdownTimeout)
	}
	if cfg.MaxAttempts != 8 {
		t.Errorf("MaxAttempts = %d, want 8", cfg.MaxAttempts)
	}
}

func TestLoadOverrides(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"DATABASE_URL":     "postgres://localhost/db",
		"HTTP_ADDR":        ":9090",
		"LOG_LEVEL":        "debug",
		"SHUTDOWN_TIMEOUT": "3s",
		"MAX_ATTEMPTS":     "3",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HTTPAddr != ":9090" || cfg.LogLevel != slog.LevelDebug || cfg.ShutdownTimeout != 3*time.Second ||
		cfg.MaxAttempts != 3 {
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
			name:    "missing database url",
			env:     map[string]string{},
			wantErr: []string{"DATABASE_URL is required"},
		},
		{
			name: "every setting invalid",
			env: map[string]string{
				"LOG_LEVEL":        "loud",
				"SHUTDOWN_TIMEOUT": "soon",
				"MAX_ATTEMPTS":     "many",
			},
			wantErr: []string{"DATABASE_URL", "LOG_LEVEL", "SHUTDOWN_TIMEOUT", "MAX_ATTEMPTS"},
		},
		{
			name: "max attempts out of range",
			env: map[string]string{
				"DATABASE_URL": "postgres://localhost/db",
				"MAX_ATTEMPTS": "0",
			},
			wantErr: []string{"between 1 and 20"},
		},
		{
			name: "non-positive timeout",
			env: map[string]string{
				"DATABASE_URL":     "postgres://localhost/db",
				"SHUTDOWN_TIMEOUT": "0s",
			},
			wantErr: []string{"must be positive"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(env(tt.env))
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
