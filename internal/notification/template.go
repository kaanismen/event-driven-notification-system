package notification

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// Template is reusable message text with {{name}} placeholders (ADR-0019). Templates are
// immutable: a template ID always means the same text, so a notification's content can
// always be traced back to it. To change the wording, create a new template.
type Template struct {
	ID        uuid.UUID
	Name      string
	Channel   Channel
	Body      string
	CreatedAt time.Time
}

const (
	maxTemplateName = 100
	// maxTemplateVariables bounds the work per render and keeps error messages short.
	maxTemplateVariables = 50
)

var (
	// placeholder is {{name}}, optionally with spaces inside the braces: {{ name }}.
	// Names are what a JSON client can type without quoting problems.
	placeholder = regexp.MustCompile(`\{\{\s*([A-Za-z_][A-Za-z0-9_]*)\s*\}\}`)
	// templateName is a slug such as "order-shipped" or "otp.v2".
	templateName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
)

// Variables returns the placeholder names in the order they first appear.
func (t Template) Variables() []string {
	return placeholderNames(t.Body)
}

func placeholderNames(body string) []string {
	var names []string
	for _, m := range placeholder.FindAllStringSubmatch(body, -1) {
		if !slices.Contains(names, m[1]) {
			names = append(names, m[1])
		}
	}
	return names
}

// ValidateTemplate returns every problem with a new template, or nil if it is valid.
func ValidateTemplate(name string, channel Channel, body string) []FieldError {
	var errs []FieldError
	add := func(field, format string, args ...any) {
		errs = append(errs, FieldError{Field: field, Message: fmt.Sprintf(format, args...)})
	}

	switch {
	case name == "":
		add("name", "is required")
	case utf8.RuneCountInString(name) > maxTemplateName || !templateName.MatchString(name):
		add("name", "must be 1 to %d letters, digits, '.', '_' or '-', starting with a letter or digit", maxTemplateName)
	}

	switch {
	case channel == "":
		add("channel", "is required")
	case !channel.Valid():
		add("channel", "must be one of sms, email, push")
	}

	// The fixed text is what remains with every variable empty. If even that does not
	// fit the channel, no notification can ever be rendered from the template.
	fixed := placeholder.ReplaceAllString(body, "")
	switch {
	case strings.TrimSpace(body) == "":
		add("body", "is required and must not be blank")
	case strings.Contains(fixed, "{{"):
		// Strict on purpose: "{{first name}}" or an unclosed "{{name" is almost certainly
		// a mistake, and sending it literally to customers would be worse than a 422.
		add("body", "has a malformed placeholder; use {{name}} with letters, digits and '_' only")
	case len(placeholderNames(body)) > maxTemplateVariables:
		add("body", "must use at most %d different variables", maxTemplateVariables)
	default:
		if limit := maxContentLength(channel); limit > 0 {
			if n := utf8.RuneCountInString(fixed); n > limit {
				add("body", "the text outside the placeholders is %d characters; %s allows at most %d", n, channel, limit)
			}
		}
	}
	return errs
}

// Render fills in the placeholders. Every placeholder needs a value and every value needs
// a placeholder: an extra variable is usually a typo in its name, which would otherwise
// leave the intended placeholder unfilled. Values are inserted as they are and are not
// rendered again, so a value containing "{{x}}" stays literal text.
func (t Template) Render(vars map[string]string) (string, []FieldError) {
	names := t.Variables()

	var missing, extra []string
	for _, n := range names {
		if _, ok := vars[n]; !ok {
			missing = append(missing, n)
		}
	}
	for n := range vars {
		if !slices.Contains(names, n) {
			extra = append(extra, n)
		}
	}
	sort.Strings(extra) // map order is random; error messages should not be

	var errs []FieldError
	if len(missing) > 0 {
		errs = append(errs, FieldError{Field: "variables", Message: "missing values for " + strings.Join(missing, ", ")})
	}
	if len(extra) > 0 {
		errs = append(errs, FieldError{Field: "variables", Message: "the template has no placeholder for " + strings.Join(extra, ", ")})
	}
	if len(errs) > 0 {
		return "", errs
	}

	return placeholder.ReplaceAllStringFunc(t.Body, func(m string) string {
		return vars[placeholder.FindStringSubmatch(m)[1]]
	}), nil
}
