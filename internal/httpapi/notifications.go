package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/kaanismen/insider-assessment/internal/logging"
	"github.com/kaanismen/insider-assessment/internal/notification"
	"github.com/kaanismen/insider-assessment/internal/store"
)

const maxBatchItems = 1000

// createRequest is the wire format of one notification. It is kept apart from the
// domain type so the JSON contract can change without touching business rules.
type createRequest struct {
	Channel   string `json:"channel"`
	Recipient string `json:"recipient"`
	Content   string `json:"content"`
	Priority  string `json:"priority"`
}

func (c createRequest) toDomain() notification.Request {
	return notification.Request{
		Channel:   notification.Channel(c.Channel),
		Recipient: c.Recipient,
		Content:   c.Content,
		Priority:  notification.Priority(c.Priority),
	}.Normalized()
}

type batchRequest struct {
	Notifications []createRequest `json:"notifications"`
}

type notificationResponse struct {
	ID                string     `json:"id"`
	BatchID           string     `json:"batchId,omitempty"`
	Channel           string     `json:"channel"`
	Recipient         string     `json:"recipient"`
	Content           string     `json:"content"`
	Priority          string     `json:"priority"`
	Status            string     `json:"status"`
	Attempts          int        `json:"attempts"`
	MaxAttempts       int        `json:"maxAttempts"`
	NextAttemptAt     *time.Time `json:"nextAttemptAt,omitempty"`
	LastError         string     `json:"lastError,omitempty"`
	ProviderMessageID string     `json:"providerMessageId,omitempty"`
	CorrelationID     string     `json:"correlationId,omitempty"`
	CreatedAt         time.Time  `json:"createdAt"`
	UpdatedAt         time.Time  `json:"updatedAt"`
	SentAt            *time.Time `json:"sentAt,omitempty"`
}

func toResponse(n notification.Notification) notificationResponse {
	resp := notificationResponse{
		ID:                n.ID.String(),
		Channel:           string(n.Channel),
		Recipient:         n.Recipient,
		Content:           n.Content,
		Priority:          string(n.Priority),
		Status:            string(n.Status),
		Attempts:          n.Attempts,
		MaxAttempts:       n.MaxAttempts,
		LastError:         n.LastError,
		ProviderMessageID: n.ProviderMessageID,
		CorrelationID:     n.CorrelationID,
		CreatedAt:         n.CreatedAt,
		UpdatedAt:         n.UpdatedAt,
		SentAt:            n.SentAt,
	}
	if n.BatchID != nil {
		resp.BatchID = n.BatchID.String()
	}
	// The next attempt time only means something while the notification waits to be sent.
	if n.Status == notification.StatusPending {
		t := n.NextAttemptAt
		resp.NextAttemptAt = &t
	}
	return resp
}

type batchItemSummary struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

type batchCreateResponse struct {
	BatchID       string             `json:"batchId"`
	Count         int                `json:"count"`
	Notifications []batchItemSummary `json:"notifications"`
}

// createNotification handles POST /v1/notifications.
func (a *api) createNotification(w http.ResponseWriter, r *http.Request) {
	key, err := idempotencyKey(r)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	var body createRequest
	if err := decodeJSON(w, r, &body, maxSingleBody); err != nil {
		a.fail(w, r, err)
		return
	}

	req := body.toDomain()
	if errs := req.Validate(); len(errs) > 0 {
		a.fail(w, r, validationFailed(fieldErrors(nil, errs)))
		return
	}

	res, err := a.store.Create(r.Context(), a.createParams(r.Context(), []notification.Request{req}, false, key))
	if err != nil {
		a.fail(w, r, err)
		return
	}

	n := res.Notifications[0]
	w.Header().Set("Location", "/v1/notifications/"+n.ID.String())
	writeJSON(w, createdStatus(w, res), toResponse(n))
}

// createBatch handles POST /v1/batches. All-or-nothing (ADR-0006): if any item is
// invalid, nothing is stored and every invalid item is reported with its index.
func (a *api) createBatch(w http.ResponseWriter, r *http.Request) {
	key, err := idempotencyKey(r)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	var body batchRequest
	if err := decodeJSON(w, r, &body, maxBatchBody); err != nil {
		a.fail(w, r, err)
		return
	}

	if n := len(body.Notifications); n < 1 || n > maxBatchItems {
		a.fail(w, r, validationFailed([]fieldErrorBody{{
			Field:   "notifications",
			Message: fmt.Sprintf("must contain between 1 and %d items, got %d", maxBatchItems, n),
		}}))
		return
	}

	reqs := make([]notification.Request, len(body.Notifications))
	var details []fieldErrorBody
	for i, item := range body.Notifications {
		reqs[i] = item.toDomain()
		if errs := reqs[i].Validate(); len(errs) > 0 {
			index := i
			details = append(details, fieldErrors(&index, errs)...)
		}
	}
	if len(details) > 0 {
		a.fail(w, r, validationFailed(details))
		return
	}

	res, err := a.store.Create(r.Context(), a.createParams(r.Context(), reqs, true, key))
	if err != nil {
		a.fail(w, r, err)
		return
	}

	resp := batchCreateResponse{
		BatchID:       res.BatchID.String(),
		Count:         len(res.Notifications),
		Notifications: make([]batchItemSummary, len(res.Notifications)),
	}
	for i, n := range res.Notifications {
		resp.Notifications[i] = batchItemSummary{ID: n.ID.String(), Status: string(n.Status)}
	}
	w.Header().Set("Location", "/v1/batches/"+resp.BatchID)
	writeJSON(w, createdStatus(w, res), resp)
}

func (a *api) createParams(ctx context.Context, reqs []notification.Request, batch bool, key string) store.CreateParams {
	p := store.CreateParams{
		Requests:    reqs,
		Batch:       batch,
		MaxAttempts: a.maxAttempts,
		// The request ID travels with the row, so worker logs for this notification can
		// be found under the same ID as the API call that created it.
		CorrelationID:  logging.RequestID(ctx),
		IdempotencyKey: key,
	}
	if key != "" {
		kind := "notification"
		if batch {
			kind = "batch"
		}
		p.RequestHash = requestHash(kind, reqs)
	}
	return p
}

// createdStatus is 202 for a new resource (accepted, delivery happens later) and 200 for
// an idempotent replay, which also gets a header so clients can tell the two apart.
func createdStatus(w http.ResponseWriter, res store.CreateResult) int {
	if res.Replayed {
		w.Header().Set(replayedHeader, "true")
		return http.StatusOK
	}
	return http.StatusAccepted
}

// getNotification handles GET /v1/notifications/{id}.
func (a *api) getNotification(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	n, err := a.store.Get(r.Context(), id)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toResponse(n))
}

type batchStatusResponse struct {
	BatchID string `json:"batchId"`
	Total   int    `json:"total"`
	// Counts has every status, zeros included, so clients need no special cases.
	Counts        map[notification.Status]int `json:"counts"`
	Notifications []notificationResponse      `json:"notifications"`
}

// getBatch handles GET /v1/batches/{id}: a summary per status plus every item (A-11).
func (a *api) getBatch(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	items, err := a.store.GetBatch(r.Context(), id)
	if err != nil {
		a.fail(w, r, err)
		return
	}

	resp := batchStatusResponse{
		BatchID:       id.String(),
		Total:         len(items),
		Counts:        make(map[notification.Status]int),
		Notifications: make([]notificationResponse, len(items)),
	}
	for _, s := range notification.Statuses() {
		resp.Counts[s] = 0
	}
	// A batch has at most 1000 items, so counting here is cheaper than a second query.
	for i, n := range items {
		resp.Counts[n.Status]++
		resp.Notifications[i] = toResponse(n)
	}
	writeJSON(w, http.StatusOK, resp)
}

// cancelNotification handles POST /v1/notifications/{id}/cancel (ADR-0007).
func (a *api) cancelNotification(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	n, err := a.store.Cancel(r.Context(), id)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toResponse(n))
}

type listResponse struct {
	Items []notificationResponse `json:"items"`
	// NextCursor is null on the last page. There is no total count (ADR-0012).
	NextCursor *string `json:"nextCursor"`
}

// listNotifications handles GET /v1/notifications.
func (a *api) listNotifications(w http.ResponseWriter, r *http.Request) {
	filter, err := parseListQuery(r.URL.Query())
	if err != nil {
		a.fail(w, r, err)
		return
	}
	page, err := a.store.List(r.Context(), filter)
	if err != nil {
		a.fail(w, r, err)
		return
	}

	// make, not a nil slice, so an empty page is "items": [] rather than null.
	resp := listResponse{Items: make([]notificationResponse, len(page.Items))}
	for i, n := range page.Items {
		resp.Items[i] = toResponse(n)
	}
	if page.Next != nil {
		c := encodeCursor(*page.Next)
		resp.NextCursor = &c
	}
	writeJSON(w, http.StatusOK, resp)
}
