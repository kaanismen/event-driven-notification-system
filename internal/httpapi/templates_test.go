package httpapi

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/kaanismen/event-driven-notification-system/internal/notification"
)

func TestCreateTemplate(t *testing.T) {
	s := newTestServer(t)
	body := `{"name":"otp","channel":"sms","body":"Hi {{name}}, your code is {{code}}"}`

	rec := s.do(http.MethodPost, "/v1/templates", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body: %s", rec.Code, rec.Body.String())
	}
	got := decodeBody(t, rec)
	if loc := rec.Header().Get("Location"); loc != "/v1/templates/"+got["id"].(string) {
		t.Errorf("Location = %q", loc)
	}
	if vars := fmt.Sprint(got["variables"]); vars != "[name code]" {
		t.Errorf("variables = %s, want [name code]", vars)
	}

	// The same template again is a safe retry: the stored one comes back.
	again := s.do(http.MethodPost, "/v1/templates", body)
	if again.Code != http.StatusOK || again.Header().Get(replayedHeader) != "true" {
		t.Errorf("repeat: status %d, %s=%q; want 200 and true", again.Code, replayedHeader, again.Header().Get(replayedHeader))
	}
	if decodeBody(t, again)["id"] != got["id"] {
		t.Error("repeat returned a different template")
	}

	expectError(t, s.do(http.MethodPost, "/v1/templates", `{"name":"otp","channel":"sms","body":"Changed {{code}}"}`),
		http.StatusConflict, "template_name_taken")
}

func TestCreateTemplateRejectsInvalidInput(t *testing.T) {
	s := newTestServer(t)
	rec := s.do(http.MethodPost, "/v1/templates", `{"name":"bad name","channel":"fax","body":"Hi {{first name}}"}`)
	body := expectError(t, rec, http.StatusUnprocessableEntity, "validation_failed")
	if n := len(body["details"].([]any)); n != 3 {
		t.Errorf("got %d field errors, want 3 (name, channel, body): %s", n, rec.Body.String())
	}
	expectError(t, s.do(http.MethodPost, "/v1/templates", `{"name":"x","channel":"sms","body":"x","vars":[]}`),
		http.StatusBadRequest, "unknown_field")
	// A template with no placeholders lists no variables as [], not null.
	rec = s.do(http.MethodPost, "/v1/templates", `{"name":"static","channel":"push","body":"Sale!"}`)
	if !strings.Contains(rec.Body.String(), `"variables":[]`) {
		t.Errorf("body = %s, want \"variables\":[]", rec.Body.String())
	}
}

func TestGetAndListTemplates(t *testing.T) {
	s := newTestServer(t)
	tpl := s.addTemplate("welcome", notification.ChannelEmail, "Welcome {{name}}")

	rec := s.do(http.MethodGet, "/v1/templates/"+tpl.ID.String(), "")
	if rec.Code != http.StatusOK || decodeBody(t, rec)["name"] != "welcome" {
		t.Errorf("get: status %d body %s", rec.Code, rec.Body.String())
	}
	expectError(t, s.do(http.MethodGet, "/v1/templates/019a0000-0000-7000-8000-000000000000", ""), http.StatusNotFound, "not_found")
	expectError(t, s.do(http.MethodGet, "/v1/templates/nope", ""), http.StatusBadRequest, "invalid_id")

	rec = s.do(http.MethodGet, "/v1/templates?channel=email&limit=10", "")
	if rec.Code != http.StatusOK || len(decodeBody(t, rec)["items"].([]any)) != 1 {
		t.Errorf("list: status %d body %s", rec.Code, rec.Body.String())
	}
	if f := s.store.listTemplatesCalls[0]; f.Channel != notification.ChannelEmail || f.Limit != 10 {
		t.Errorf("filter = %+v", f)
	}
	expectError(t, s.do(http.MethodGet, "/v1/templates?status=sent", ""), http.StatusBadRequest, "invalid_query")
	expectError(t, s.do(http.MethodGet, "/v1/templates?channel=fax", ""), http.StatusBadRequest, "invalid_query")
}

func TestCreateNotificationFromTemplate(t *testing.T) {
	s := newTestServer(t)
	tpl := s.addTemplate("otp", notification.ChannelSMS, "Hi {{name}}, your code is {{code}}")

	// channel is optional: the template knows it.
	rec := s.do(http.MethodPost, "/v1/notifications", fmt.Sprintf(
		`{"templateId":%q,"recipient":"+905551234567","variables":{"name":"Ayşe","code":"4829"}}`, tpl.ID))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
	}
	got := decodeBody(t, rec)
	if got["content"] != "Hi Ayşe, your code is 4829" || got["channel"] != "sms" || got["templateId"] != tpl.ID.String() {
		t.Errorf("response = %s", rec.Body.String())
	}
	stored := s.store.createCalls[0].Requests[0]
	if stored.TemplateID == nil || *stored.TemplateID != tpl.ID || stored.Content != "Hi Ayşe, your code is 4829" {
		t.Errorf("stored request = %+v", stored)
	}
}

func TestTemplatedNotificationErrors(t *testing.T) {
	s := newTestServer(t)
	sms := s.addTemplate("otp", notification.ChannelSMS, "Your code is {{code}}")
	long := s.addTemplate("long", notification.ChannelSMS, strings.Repeat("x", 150)+"{{tail}}")

	tests := []struct {
		name  string
		body  string
		field string // the one field error expected
	}{
		{"content and template together",
			fmt.Sprintf(`{"templateId":%q,"recipient":"+905551234567","content":"hi","variables":{"code":"1"}}`, sms.ID), "content"},
		{"variables without a template",
			`{"channel":"sms","recipient":"+905551234567","content":"hi","variables":{"code":"1"}}`, "variables"},
		{"malformed template ID",
			`{"templateId":"not-a-uuid","channel":"sms","recipient":"+905551234567"}`, "templateId"},
		{"unknown template",
			`{"templateId":"019a0000-0000-7000-8000-000000000000","channel":"sms","recipient":"+905551234567"}`, "templateId"},
		{"channel differs from the template",
			fmt.Sprintf(`{"templateId":%q,"channel":"push","recipient":"device-1","variables":{"code":"1"}}`, sms.ID), "channel"},
		{"missing variable",
			fmt.Sprintf(`{"templateId":%q,"recipient":"+905551234567","variables":{}}`, sms.ID), "variables"},
		{"rendered text over the sms limit",
			fmt.Sprintf(`{"templateId":%q,"recipient":"+905551234567","variables":{"tail":%q}}`, long.ID, strings.Repeat("y", 11)), "content"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := s.do(http.MethodPost, "/v1/notifications", tt.body)
			body := expectError(t, rec, http.StatusUnprocessableEntity, "validation_failed")
			details := body["details"].([]any)
			if len(details) != 1 || details[0].(map[string]any)["field"] != tt.field {
				t.Errorf("details = %v, want one error on %q", details, tt.field)
			}
		})
	}
}

func TestBatchWithTemplates(t *testing.T) {
	s := newTestServer(t)
	otp := s.addTemplate("otp", notification.ChannelSMS, "Code {{code}}")
	welcome := s.addTemplate("welcome", notification.ChannelEmail, "Welcome {{name}}")

	items := []string{
		fmt.Sprintf(`{"templateId":%q,"recipient":"+905551234567","variables":{"code":"1"}}`, otp.ID),
		fmt.Sprintf(`{"templateId":%q,"recipient":"+905551234568","variables":{"code":"2"}}`, otp.ID),
		fmt.Sprintf(`{"templateId":%q,"recipient":"a@example.com","variables":{"name":"Ann"}}`, welcome.ID),
		`{"channel":"push","recipient":"device-1","content":"plain"}`,
	}
	rec := s.do(http.MethodPost, "/v1/batches", `{"notifications":[`+strings.Join(items, ",")+`]}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
	}
	if calls := s.store.getTemplatesCalls; len(calls) != 1 || len(calls[0]) != 2 {
		t.Errorf("template lookups = %v, want one call with the 2 distinct IDs", calls)
	}
	reqs := s.store.createCalls[0].Requests
	if reqs[1].Content != "Code 2" || reqs[2].Channel != notification.ChannelEmail || reqs[3].TemplateID != nil {
		t.Errorf("stored requests = %+v", reqs)
	}

	// One bad item rejects the batch and is reported by index.
	items[2] = fmt.Sprintf(`{"templateId":%q,"recipient":"a@example.com","variables":{"nme":"Ann"}}`, welcome.ID)
	rec = s.do(http.MethodPost, "/v1/batches", `{"notifications":[`+strings.Join(items, ",")+`]}`)
	body := expectError(t, rec, http.StatusUnprocessableEntity, "validation_failed")
	for _, d := range body["details"].([]any) {
		if d.(map[string]any)["index"] != float64(2) {
			t.Errorf("error on item %v, want 2", d.(map[string]any)["index"])
		}
	}
}

// The idempotency fingerprint covers the template and its variables: the same key with
// different variables is a different request.
func TestTemplateVariablesAreInTheRequestHash(t *testing.T) {
	s := newTestServer(t)
	tpl := s.addTemplate("otp", notification.ChannelSMS, "Code {{code}}")
	for _, code := range []string{"1", "2", "1"} {
		s.do(http.MethodPost, "/v1/notifications", fmt.Sprintf(
			`{"templateId":%q,"recipient":"+905551234567","variables":{"code":%q}}`, tpl.ID, code),
			idempotencyHeader, "k")
	}
	h := func(i int) []byte { return s.store.createCalls[i].RequestHash }
	if bytes.Equal(h(0), h(1)) || !bytes.Equal(h(0), h(2)) {
		t.Error("hash must differ for different variables and match for the same ones")
	}
}
