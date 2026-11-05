package httpapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/kaanismen/insider-assessment/internal/notification"
)

// scheduledSMS is a valid SMS with the given scheduledAt value.
func scheduledSMS(at string) string {
	return fmt.Sprintf(`{"channel":"sms","recipient":"+905551234567","content":"Your code is 1234","scheduledAt":%q}`, at)
}

func TestCreateScheduledNotification(t *testing.T) {
	s := newTestServer(t)
	// A client in UTC+3 asks for a time one hour from now, written in its own zone.
	want := time.Now().Add(time.Hour).Truncate(time.Second).UTC()
	local := want.In(time.FixedZone("UTC+3", 3*60*60)).Format(time.RFC3339)

	rec := s.do(http.MethodPost, "/v1/notifications", scheduledSMS(local))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
	}
	got := s.store.createCalls[0].Requests[0].ScheduledAt
	if got == nil || !got.Equal(want) || got.Location() != time.UTC {
		t.Errorf("stored scheduledAt = %v, want %v in UTC", got, want)
	}
	// Scheduled notifications are plain pending ones with a send time (ADR-0017, 1A).
	body := decodeBody(t, rec)
	if body["status"] != "pending" || body["scheduledAt"] != want.Format(time.RFC3339) {
		t.Errorf("response status %v, scheduledAt %v; want pending and %s", body["status"], body["scheduledAt"], want.Format(time.RFC3339))
	}

	rec = s.do(http.MethodPost, "/v1/notifications", validSMS)
	if _, ok := decodeBody(t, rec)["scheduledAt"]; ok {
		t.Error("an unscheduled notification reports a scheduledAt")
	}
}

func TestScheduledAtRules(t *testing.T) {
	now := time.Now().UTC()
	tests := []struct {
		name string
		at   string
		want int
	}{
		{"30 s in the past counts as now", now.Add(-30 * time.Second).Format(time.RFC3339), http.StatusAccepted},
		{"two minutes in the past", now.Add(-2 * time.Minute).Format(time.RFC3339), http.StatusUnprocessableEntity},
		{"29 days ahead", now.Add(29 * 24 * time.Hour).Format(time.RFC3339), http.StatusAccepted},
		{"31 days ahead", now.Add(31 * 24 * time.Hour).Format(time.RFC3339), http.StatusUnprocessableEntity},
		{"no time zone", now.Add(time.Hour).Format("2006-01-02T15:04:05"), http.StatusUnprocessableEntity},
		{"a date only", now.Add(48 * time.Hour).Format("2006-01-02"), http.StatusUnprocessableEntity},
		{"not a time", "tomorrow morning", http.StatusUnprocessableEntity},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestServer(t)
			rec := s.do(http.MethodPost, "/v1/notifications", scheduledSMS(tt.at))
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d; body: %s", rec.Code, tt.want, rec.Body.String())
			}
			if tt.want != http.StatusUnprocessableEntity {
				return
			}
			details := decodeBody(t, rec)["details"].([]any)
			if len(details) != 1 || details[0].(map[string]any)["field"] != "scheduledAt" {
				t.Errorf("details = %v, want one scheduledAt error", details)
			}
		})
	}
}

// A batch may mix immediate and scheduled items; an invalid time fails the whole batch
// like any other invalid field (ADR-0006, A-40).
func TestBatchWithScheduledItems(t *testing.T) {
	s := newTestServer(t)
	later := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)

	rec := s.do(http.MethodPost, "/v1/batches", batchBody(scheduledSMS(later), validSMS))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
	}
	reqs := s.store.createCalls[0].Requests
	if reqs[0].ScheduledAt == nil || reqs[1].ScheduledAt != nil {
		t.Errorf("scheduledAt per item = %v, %v; want only the first set", reqs[0].ScheduledAt, reqs[1].ScheduledAt)
	}

	body := expectError(t, s.do(http.MethodPost, "/v1/batches", batchBody(validSMS, scheduledSMS("soon"))),
		http.StatusUnprocessableEntity, "validation_failed")
	d := body["details"].([]any)[0].(map[string]any)
	if d["index"] != 1.0 || d["field"] != "scheduledAt" {
		t.Errorf("detail = %v, want index 1, field scheduledAt", d)
	}
}

func TestScheduleIsPartOfTheIdempotencyHash(t *testing.T) {
	s := newTestServer(t)
	at := time.Now().Add(time.Hour).Truncate(time.Second).UTC()
	s.do(http.MethodPost, "/v1/notifications", scheduledSMS(at.Format(time.RFC3339)), idempotencyHeader, "k")
	// The same instant written in another zone is the same request.
	s.do(http.MethodPost, "/v1/notifications",
		scheduledSMS(at.In(time.FixedZone("UTC-5", -5*60*60)).Format(time.RFC3339)), idempotencyHeader, "k")
	// A different time is a different request, so the store answers 409 for the key.
	s.do(http.MethodPost, "/v1/notifications", scheduledSMS(at.Add(time.Minute).Format(time.RFC3339)), idempotencyHeader, "k")

	c := s.store.createCalls
	if !bytes.Equal(c[0].RequestHash, c[1].RequestHash) {
		t.Error("the same instant in two time zones hashed differently")
	}
	if bytes.Equal(c[0].RequestHash, c[2].RequestHash) {
		t.Error("different scheduled times hashed the same")
	}
}

// Keys stored before scheduling existed were hashed without the field. An unscheduled
// request must still produce exactly that hash, or its replay would turn into a 409.
func TestUnscheduledHashIsUnchanged(t *testing.T) {
	req := notification.Request{
		Channel: notification.ChannelSMS, Recipient: "+905551234567", Content: "hi", Priority: notification.PriorityHigh,
	}
	type before struct { // notification.Request as it was in Phase 2
		Channel   notification.Channel
		Recipient string
		Content   string
		Priority  notification.Priority
	}
	h := sha256.New()
	_, _ = io.WriteString(h, "notification\n")
	_ = json.NewEncoder(h).Encode([]before{{req.Channel, req.Recipient, req.Content, req.Priority}})

	if got := requestHash("notification", []notification.Request{req}); !bytes.Equal(got, h.Sum(nil)) {
		t.Error("adding ScheduledAt changed the hash of unscheduled requests")
	}
}

func TestListScheduledFilter(t *testing.T) {
	s := newTestServer(t)
	for query, want := range map[string]any{"true": true, "false": false} {
		rec := s.do(http.MethodGet, "/v1/notifications?scheduled="+query, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("scheduled=%s: status %d", query, rec.Code)
		}
		f := s.store.listCalls[len(s.store.listCalls)-1]
		if f.Scheduled == nil || *f.Scheduled != want {
			t.Errorf("scheduled=%s: filter %v, want %v", query, f.Scheduled, want)
		}
	}
	for _, bad := range []string{"yes", "1", "TRUE"} {
		expectError(t, s.do(http.MethodGet, "/v1/notifications?scheduled="+bad, ""), http.StatusBadRequest, "invalid_query")
	}
}
