package httpapi

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/kaanismen/insider-assessment/internal/notification"
)

const (
	// maxSingleBody fits the largest valid single request (10,000 email characters of up
	// to 4 bytes each, plus fields) with room to spare.
	maxSingleBody = 64 << 10 // 64 KiB
	// maxBatchBody bounds memory per batch request. 1000 maximum-length emails in plain
	// ASCII fit; batches of long multi-byte content must be split (ASSUMPTIONS A-26).
	maxBatchBody = 12 << 20 // 12 MiB

	idempotencyHeader  = "Idempotency-Key"
	maxIdempotencyKey  = 255
	replayedHeader     = "Idempotent-Replayed"
	unknownFieldPrefix = "json: unknown field "
)

// decodeJSON reads exactly one JSON object into dst. Unknown fields are rejected
// (ADR-0012), so a typo such as "priorty" fails loudly instead of silently defaulting.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any, maxBytes int64) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		return decodeError(err, maxBytes)
	}
	// A second value after the object ("{...}{...}" or "{...} x") is a client bug.
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err != nil {
			if de := decodeError(err, maxBytes); de.status == http.StatusRequestEntityTooLarge {
				return de
			}
		}
		return badRequest("invalid_json", "request body must contain a single JSON object")
	}
	return nil
}

func decodeError(err error, maxBytes int64) *apiError {
	var (
		tooLarge *http.MaxBytesError
		syntax   *json.SyntaxError
		typeErr  *json.UnmarshalTypeError
	)
	switch {
	case errors.As(err, &tooLarge):
		return &apiError{status: http.StatusRequestEntityTooLarge, body: errorBody{
			Error: "request_too_large", Message: "request body exceeds " + humanBytes(maxBytes),
		}}
	case errors.Is(err, io.EOF):
		return badRequest("invalid_json", "request body is empty")
	case errors.As(err, &syntax), errors.Is(err, io.ErrUnexpectedEOF):
		return badRequest("invalid_json", "request body is not valid JSON")
	case errors.As(err, &typeErr):
		return badRequest("invalid_json", "field %q has the wrong type, expected %s", typeErr.Field, typeErr.Type)
	case strings.HasPrefix(err.Error(), unknownFieldPrefix):
		// encoding/json has no typed error for unknown fields, only this message format.
		return badRequest("unknown_field", "unknown field %s", strings.TrimPrefix(err.Error(), unknownFieldPrefix))
	default:
		return badRequest("invalid_json", "request body could not be decoded")
	}
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20 && n%(1<<20) == 0:
		return strconv.FormatInt(n>>20, 10) + " MiB"
	case n >= 1<<10 && n%(1<<10) == 0:
		return strconv.FormatInt(n>>10, 10) + " KiB"
	}
	return strconv.FormatInt(n, 10) + " bytes"
}

// idempotencyKey returns the optional Idempotency-Key header (ADR-0004).
func idempotencyKey(r *http.Request) (string, error) {
	key := r.Header.Get(idempotencyHeader)
	if key == "" {
		return "", nil
	}
	if strings.TrimSpace(key) == "" || len(key) > maxIdempotencyKey {
		return "", badRequest("invalid_idempotency_key", "Idempotency-Key must be 1 to %d characters", maxIdempotencyKey)
	}
	for _, c := range []byte(key) {
		if c < 0x20 || c > 0x7e {
			return "", badRequest("invalid_idempotency_key", "Idempotency-Key must contain printable ASCII only")
		}
	}
	return key, nil
}

// requestHash fingerprints the normalized request, not the raw bytes, so a retry with
// different whitespace or key order is recognised as the same request. kind keeps a
// single create and a one-item batch apart.
func requestHash(kind string, reqs []notification.Request) []byte {
	h := sha256.New()
	_, _ = io.WriteString(h, kind+"\n")
	// Encoding a slice of structs is deterministic: fields always come out in declaration
	// order. Writes to a hash never fail.
	_ = json.NewEncoder(h).Encode(reqs)
	return h.Sum(nil)
}
