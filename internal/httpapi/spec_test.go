package httpapi

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	openapi "github.com/kaanismen/event-driven-notification-system/api"
	"github.com/kaanismen/event-driven-notification-system/internal/notification"
)

func TestSpecIsServed(t *testing.T) {
	h, _ := newTestHandler(t)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/openapi.yaml", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/yaml" {
		t.Errorf("Content-Type = %q, want application/yaml", ct)
	}
	if !bytes.Equal(rec.Body.Bytes(), openapi.Spec) {
		t.Error("served body differs from the embedded spec")
	}
	if !bytes.HasPrefix(openapi.Spec, []byte("openapi: 3.1.")) {
		t.Error("spec does not start with an OpenAPI 3.1 version line")
	}
}

// TestSpecCoversEveryRoute fails when the server and the spec disagree about which
// endpoints exist, in either direction: an undocumented route or a documented one that
// is not served.
func TestSpecCoversEveryRoute(t *testing.T) {
	var served []string
	for _, rt := range (&api{}).routes(nil) {
		served = append(served, rt.pattern)
	}
	documented := specOperations(t, openapi.Spec)

	for _, p := range served {
		if !slices.Contains(documented, p) {
			t.Errorf("route %q is served but missing from api/openapi.yaml", p)
		}
	}
	for _, op := range documented {
		if !slices.Contains(served, op) {
			t.Errorf("api/openapi.yaml documents %q, which the server does not serve", op)
		}
	}
}

// TestSpecEnumsMatchDomain checks that the spec lists exactly the channels, priorities
// and statuses the domain package defines, in the same order.
func TestSpecEnumsMatchDomain(t *testing.T) {
	enums := map[string][]string{
		"Channel":  {string(notification.ChannelSMS), string(notification.ChannelEmail), string(notification.ChannelPush)},
		"Priority": {string(notification.PriorityHigh), string(notification.PriorityNormal), string(notification.PriorityLow)},
	}
	var statuses []string
	for _, s := range notification.Statuses() {
		statuses = append(statuses, string(s))
	}
	enums["Status"] = statuses

	spec := string(openapi.Spec)
	for name, values := range enums {
		want := "enum: [" + strings.Join(values, ", ") + "]"
		if !strings.Contains(spec, want) {
			t.Errorf("schema %s: spec has no line %q", name, want)
		}
	}
}

// specOperations returns "METHOD /path" for every operation in the spec, in the same
// form as a mux pattern. It reads the YAML by indentation rather than with a YAML
// library: the spec is hand-written with a fixed layout (paths at two spaces, methods at
// four), and CI validates the YAML itself with Redocly, so a parser dependency would add
// nothing (ADR-0018).
func specOperations(t *testing.T, spec []byte) []string {
	t.Helper()
	methods := map[string]string{
		"get:": "GET", "post:": "POST", "put:": "PUT", "patch:": "PATCH", "delete:": "DELETE",
	}
	var (
		ops     []string
		inPaths bool
		path    string
	)
	for line := range strings.Lines(string(spec)) {
		line = strings.TrimRight(line, "\r\n")
		indent := len(line) - len(strings.TrimLeft(line, " "))
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "" || strings.HasPrefix(trimmed, "#"):
			continue
		case indent == 0:
			inPaths = trimmed == "paths:"
			path = ""
		case !inPaths:
			continue
		case indent == 2 && strings.HasPrefix(trimmed, "/"):
			path = strings.TrimSuffix(trimmed, ":")
		case indent == 4 && path != "" && methods[trimmed] != "":
			ops = append(ops, methods[trimmed]+" "+path)
		}
	}
	if len(ops) == 0 {
		t.Fatal("found no operations under paths: in the spec")
	}
	return ops
}
