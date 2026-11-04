// Package httpapi exposes the HTTP API: routing, middleware, handlers and JSON helpers.
package httpapi

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// NewHandler returns the root handler with every route and middleware attached.
func NewHandler(logger *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", handleHealth)

	// The first middleware listed runs first: the request ID must exist before anything
	// logs, and recovery sits inside the access log so a panic is logged as a 500.
	return chain(mux,
		requestID,
		accessLog(logger),
		recoverPanic(logger),
	)
}

// handleHealth is a liveness check: it only proves the process can serve HTTP. It does
// not touch the database, so a database outage does not get the app restarted in a loop.
func handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func chain(h http.Handler, middleware ...func(http.Handler) http.Handler) http.Handler {
	for i := len(middleware) - 1; i >= 0; i-- {
		h = middleware[i](h)
	}
	return h
}

type errorBody struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorBody{Error: code, Message: message})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// The status line is already sent, so an encoding error cannot change the response;
	// it can only mean the client went away.
	_ = json.NewEncoder(w).Encode(v)
}
