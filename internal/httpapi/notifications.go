package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/kaanismen/event-driven-notification-system/internal/logging"
	"github.com/kaanismen/event-driven-notification-system/internal/notification"
	"github.com/kaanismen/event-driven-notification-system/internal/store"
	"github.com/kaanismen/event-driven-notification-system/internal/tracing"
)

const maxBatchItems = 1000

// createRequest is the wire format of one notification. It is kept apart from the
// domain type so the JSON contract can change without touching business rules.
type createRequest struct {
	Channel   string `json:"channel"`
	Recipient string `json:"recipient"`
	Content   string `json:"content"`
	Priority  string `json:"priority"`
	// ScheduledAt is a string, not a time.Time, so a malformed value becomes a field
	// error with a clear message (422) instead of a generic JSON decoding error (400).
	ScheduledAt string `json:"scheduledAt"`
	// TemplateID and Variables replace Content (ADR-0019). TemplateID is a string for the
	// same reason as ScheduledAt: a malformed ID should be a field error, not a 400.
	TemplateID string            `json:"templateId"`
	Variables  map[string]string `json:"variables"`
}

// toDomain converts and normalizes the request. A scheduledAt that is not a valid
// RFC 3339 time with a time zone is returned as a field error; the rest of the request
// is still converted so every problem can be reported at once.
func (c createRequest) toDomain() (notification.Request, []notification.FieldError) {
	r := notification.Request{
		Channel:   notification.Channel(c.Channel),
		Recipient: c.Recipient,
		Content:   c.Content,
		Priority:  notification.Priority(c.Priority),
	}
	var errs []notification.FieldError
	if c.ScheduledAt != "" {
		t, err := time.Parse(time.RFC3339Nano, c.ScheduledAt)
		if err != nil {
			errs = append(errs, notification.FieldError{
				Field:   "scheduledAt",
				Message: "must be an RFC 3339 time with a time zone, for example 2026-11-06T09:00:00Z or 2026-11-06T12:00:00+03:00",
			})
		} else {
			r.ScheduledAt = &t
		}
	}
	return r.Normalized(), errs
}

// applyTemplate renders the content of a templated request into req (ADR-0019). The
// rendered text is then validated like any other content, so the channel's length limit
// applies to what will actually be sent. templates holds every template the request
// names; one that is missing does not exist.
func (c createRequest) applyTemplate(req *notification.Request, templates map[uuid.UUID]notification.Template) []notification.FieldError {
	if c.TemplateID == "" {
		if c.Variables != nil {
			return []notification.FieldError{{Field: "variables", Message: "requires templateId"}}
		}
		return nil
	}
	id, err := uuid.Parse(c.TemplateID)
	if err != nil {
		return []notification.FieldError{{Field: "templateId", Message: "must be a UUID"}}
	}

	var errs []notification.FieldError
	if c.Content != "" {
		errs = append(errs, notification.FieldError{Field: "content", Message: "must be omitted when templateId is given; the template provides it"})
	}
	t, ok := templates[id]
	if !ok {
		return append(errs, notification.FieldError{Field: "templateId", Message: "no template with this ID"})
	}
	switch req.Channel {
	case "":
		req.Channel = t.Channel // the template already says where it can be sent
	case t.Channel:
	default:
		errs = append(errs, notification.FieldError{
			Field: "channel", Message: fmt.Sprintf("must be %s, the channel of the template, or omitted", t.Channel),
		})
	}
	content, renderErrs := t.Render(c.Variables)
	errs = append(errs, renderErrs...)
	req.Content = content
	req.TemplateID = &t.ID
	return errs
}

// validate converts one wire request, renders its template if it names one, and returns
// it with every problem found.
func (a *api) validate(c createRequest, templates map[uuid.UUID]notification.Template) (notification.Request, []notification.FieldError) {
	req, errs := c.toDomain()
	templateErrs := c.applyTemplate(&req, templates)
	errs = append(errs, templateErrs...)
	for _, e := range req.Validate(time.Now(), a.maxScheduleAhead) {
		// When the template step failed there is no rendered text to judge, and the
		// template errors already say what is wrong.
		if e.Field == "content" && len(templateErrs) > 0 {
			continue
		}
		errs = append(errs, e)
	}
	return req, errs
}

// templatesFor loads every template the requests name, with one query for a whole
// batch. Malformed IDs are skipped here; validation reports them.
func (a *api) templatesFor(ctx context.Context, items []createRequest) (map[uuid.UUID]notification.Template, error) {
	var ids []uuid.UUID
	seen := map[uuid.UUID]bool{}
	for _, it := range items {
		if id, err := uuid.Parse(it.TemplateID); err == nil && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil, nil // reading a nil map is fine: every lookup misses
	}
	templates, err := a.store.GetTemplates(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("load templates: %w", err)
	}
	return templates, nil
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
	ScheduledAt       *time.Time `json:"scheduledAt,omitempty"`
	TemplateID        string     `json:"templateId,omitempty"`
	Version           int64      `json:"version"`
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
		ScheduledAt:       n.ScheduledAt,
		Version:           n.Version,
	}
	if n.BatchID != nil {
		resp.BatchID = n.BatchID.String()
	}
	if n.TemplateID != nil {
		resp.TemplateID = n.TemplateID.String()
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

	templates, err := a.templatesFor(r.Context(), []createRequest{body})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	req, errs := a.validate(body, templates)
	if len(errs) > 0 {
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

	templates, err := a.templatesFor(r.Context(), body.Notifications)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	reqs := make([]notification.Request, len(body.Notifications))
	var details []fieldErrorBody
	for i, item := range body.Notifications {
		var errs []notification.FieldError
		reqs[i], errs = a.validate(item, templates)
		if len(errs) > 0 {
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
		// The delivery attempts link their traces to this request's span (ADR-0021).
		TraceParent: tracing.TraceParent(ctx),
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
	writeJSON(w, http.StatusOK, toBatchStatus(id, items))
}

// toBatchStatus builds the batch summary, also used as the snapshot of a batch's event
// stream.
func toBatchStatus(id uuid.UUID, items []notification.Notification) batchStatusResponse {
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
	return resp
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
