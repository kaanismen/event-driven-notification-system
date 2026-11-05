package httpapi

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/kaanismen/event-driven-notification-system/internal/notification"
	"github.com/kaanismen/event-driven-notification-system/internal/store"
)

type templateRequest struct {
	Name    string `json:"name"`
	Channel string `json:"channel"`
	Body    string `json:"body"`
}

type templateResponse struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Channel string `json:"channel"`
	Body    string `json:"body"`
	// Variables are the placeholder names a notification must provide values for.
	Variables []string  `json:"variables"`
	CreatedAt time.Time `json:"createdAt"`
}

func toTemplateResponse(t notification.Template) templateResponse {
	vars := t.Variables()
	if vars == nil {
		vars = []string{} // "variables": [] rather than null
	}
	return templateResponse{
		ID: t.ID.String(), Name: t.Name, Channel: string(t.Channel), Body: t.Body,
		Variables: vars, CreatedAt: t.CreatedAt,
	}
}

// createTemplate handles POST /v1/templates. A template is created synchronously, so
// the answer is 201, not the 202 of a notification. Sending the same template again is
// answered with the stored one (200, Idempotent-Replayed), so a retry is safe without an
// Idempotency-Key (ADR-0019).
func (a *api) createTemplate(w http.ResponseWriter, r *http.Request) {
	var body templateRequest
	if err := decodeJSON(w, r, &body, maxSingleBody); err != nil {
		a.fail(w, r, err)
		return
	}
	name, channel := strings.TrimSpace(body.Name), notification.Channel(body.Channel)
	if errs := notification.ValidateTemplate(name, channel, body.Body); len(errs) > 0 {
		a.fail(w, r, validationFailed(fieldErrors(nil, errs)))
		return
	}

	t, created, err := a.store.CreateTemplate(r.Context(), name, channel, body.Body)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	w.Header().Set("Location", "/v1/templates/"+t.ID.String())
	status := http.StatusCreated
	if !created {
		w.Header().Set(replayedHeader, "true")
		status = http.StatusOK
	}
	writeJSON(w, status, toTemplateResponse(t))
}

// getTemplate handles GET /v1/templates/{id}.
func (a *api) getTemplate(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	t, err := a.store.GetTemplate(r.Context(), id)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toTemplateResponse(t))
}

type templateListResponse struct {
	Items      []templateResponse `json:"items"`
	NextCursor *string            `json:"nextCursor"`
}

// listTemplates handles GET /v1/templates, newest first, with the same cursor pagination
// as the notification list.
func (a *api) listTemplates(w http.ResponseWriter, r *http.Request) {
	filter, err := parseTemplateQuery(r.URL.Query())
	if err != nil {
		a.fail(w, r, err)
		return
	}
	page, err := a.store.ListTemplates(r.Context(), filter)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	resp := templateListResponse{Items: make([]templateResponse, len(page.Items))}
	for i, t := range page.Items {
		resp.Items[i] = toTemplateResponse(t)
	}
	if page.Next != nil {
		c := encodeCursor(*page.Next)
		resp.NextCursor = &c
	}
	writeJSON(w, http.StatusOK, resp)
}

func parseTemplateQuery(q url.Values) (store.TemplateFilter, error) {
	f := store.TemplateFilter{Limit: defaultPageSize}
	for name, values := range q {
		switch name {
		case "channel", "limit", "cursor":
		default:
			return f, badRequest("invalid_query", "unknown query parameter %q", name)
		}
		if len(values) > 1 {
			return f, badRequest("invalid_query", "query parameter %q must be given once", name)
		}
	}
	if v := q.Get("channel"); v != "" {
		f.Channel = notification.Channel(v)
		if !f.Channel.Valid() {
			return f, badRequest("invalid_query", "channel must be one of sms, email, push")
		}
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxPageSize {
			return f, badRequest("invalid_query", "limit must be an integer from 1 to %d", maxPageSize)
		}
		f.Limit = n
	}
	if v := q.Get("cursor"); v != "" {
		c, err := decodeCursor(v)
		if err != nil {
			return f, err
		}
		f.After = &c
	}
	return f, nil
}
