package httpapi

import (
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/google/uuid"

	"github.com/kaanismen/insider-assessment/internal/logging"
)

const (
	requestIDHeader   = "X-Request-ID"
	maxRequestIDBytes = 128
)

// requestID adopts the caller's X-Request-ID when it is safe to log, or generates one,
// stores it in the request context and echoes it back so clients can quote it.
func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(requestIDHeader)
		if !validRequestID(id) {
			id = uuid.NewString()
		}
		w.Header().Set(requestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(logging.WithRequestID(r.Context(), id)))
	})
}

// validRequestID accepts a short, conservative character set. A client-supplied ID ends
// up in logs and later in provider calls, so anything that could forge log fields or
// headers is replaced rather than escaped.
func validRequestID(id string) bool {
	if id == "" || len(id) > maxRequestIDBytes {
		return false
	}
	for _, c := range []byte(id) {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.':
		default:
			return false
		}
	}
	return true
}

// accessLog writes one line per request after it completes.
func accessLog(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w}
			next.ServeHTTP(rec, r)
			logger.InfoContext(r.Context(), "http request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", rec.status(),
				"bytes", rec.bytes,
				"duration_ms", float64(time.Since(start).Microseconds())/1000,
			)
		})
	}
}

// recoverPanic turns a panic in a handler into a logged 500 instead of a dropped
// connection with no trace of the request that caused it.
func recoverPanic(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				v := recover()
				if v == nil {
					return
				}
				// net/http uses this sentinel to abort a response on purpose; let it through.
				if err, ok := v.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					panic(v)
				}
				logger.ErrorContext(r.Context(), "panic in handler",
					"panic", v, "stack", string(debug.Stack()))
				if rec, ok := w.(*statusRecorder); !ok || rec.code == 0 {
					writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// statusRecorder remembers the status code and body size for the access log.
type statusRecorder struct {
	http.ResponseWriter
	code  int
	bytes int
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.code == 0 {
		r.code = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.code == 0 {
		r.code = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

func (r *statusRecorder) status() int {
	if r.code == 0 {
		return http.StatusOK
	}
	return r.code
}

// Unwrap lets http.ResponseController reach the real writer (for flushing, deadlines).
func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}
