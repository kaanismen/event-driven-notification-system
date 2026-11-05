package httpapi

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/kaanismen/event-driven-notification-system/internal/notification"
	"github.com/kaanismen/event-driven-notification-system/internal/store"
)

// errorBody is the JSON shape of every error response.
type errorBody struct {
	Error   string `json:"error"`
	Message string `json:"message"`
	// Details lists invalid fields (422 only).
	Details []fieldErrorBody `json:"details,omitempty"`
	// CurrentStatus tells the client why a cancel was refused (409 not_cancellable only).
	CurrentStatus notification.Status `json:"currentStatus,omitempty"`
}

type fieldErrorBody struct {
	// Index is the position in a batch; omitted for single requests. A pointer, so
	// index 0 is still printed.
	Index   *int   `json:"index,omitempty"`
	Field   string `json:"field"`
	Message string `json:"message"`
}

// apiError is an error that already knows its HTTP response. Handlers return it from
// helpers (decoding, query parsing) and fail writes it.
type apiError struct {
	status int
	body   errorBody
}

func (e *apiError) Error() string {
	return fmt.Sprintf("%d %s: %s", e.status, e.body.Error, e.body.Message)
}

func badRequest(code, format string, args ...any) *apiError {
	return &apiError{status: http.StatusBadRequest, body: errorBody{Error: code, Message: fmt.Sprintf(format, args...)}}
}

func validationFailed(details []fieldErrorBody) *apiError {
	return &apiError{status: http.StatusUnprocessableEntity, body: errorBody{
		Error: "validation_failed", Message: "the request has invalid fields", Details: details,
	}}
}

// fieldErrors converts domain validation errors; index is nil for single requests.
func fieldErrors(index *int, errs []notification.FieldError) []fieldErrorBody {
	out := make([]fieldErrorBody, len(errs))
	for i, e := range errs {
		out[i] = fieldErrorBody{Index: index, Field: e.Field, Message: e.Message}
	}
	return out
}

// fail maps any error to a response. Known errors get a specific status; everything else
// is logged and becomes a generic 500, so internal details never reach the client.
func (a *api) fail(w http.ResponseWriter, r *http.Request, err error) {
	var (
		ae *apiError
		nc *store.NotCancellableError
	)
	switch {
	case errors.As(err, &ae):
		writeJSON(w, ae.status, ae.body)
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "no resource with this ID")
	case errors.Is(err, store.ErrIdempotencyConflict):
		writeError(w, http.StatusConflict, "idempotency_key_conflict",
			"this Idempotency-Key was already used with a different request")
	case errors.As(err, &nc):
		writeJSON(w, http.StatusConflict, errorBody{
			Error: "not_cancellable", Message: nc.Error(), CurrentStatus: nc.Status,
		})
	default:
		a.logger.ErrorContext(r.Context(), "request failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
	}
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorBody{Error: code, Message: message})
}
