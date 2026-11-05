package notification

import (
	"slices"
	"strings"
	"testing"
)

func TestTemplateVariables(t *testing.T) {
	tpl := Template{Body: "Hi {{name}}, your code is {{ code }}. Bye {{name}}!"}
	if got, want := tpl.Variables(), []string{"name", "code"}; !slices.Equal(got, want) {
		t.Errorf("Variables() = %v, want %v (first appearance, no duplicates)", got, want)
	}
	if got := (Template{Body: "No placeholders"}).Variables(); len(got) != 0 {
		t.Errorf("Variables() = %v, want none", got)
	}
}

func TestTemplateRender(t *testing.T) {
	tpl := Template{Channel: ChannelSMS, Body: "Hi {{name}}, your code is {{ code }}. Bye {{name}}!"}

	tests := []struct {
		name     string
		vars     map[string]string
		want     string
		wantErrs []string
	}{
		{
			name: "every placeholder filled, repeated ones too",
			vars: map[string]string{"name": "Ayşe", "code": "4829"},
			want: "Hi Ayşe, your code is 4829. Bye Ayşe!",
		},
		{
			name: "empty values are allowed",
			vars: map[string]string{"name": "", "code": "1"},
			want: "Hi , your code is 1. Bye !",
		},
		{
			name: "values are not rendered again",
			vars: map[string]string{"name": "{{code}}", "code": "1"},
			want: "Hi {{code}}, your code is 1. Bye {{code}}!",
		},
		{
			name:     "missing value",
			vars:     map[string]string{"name": "Kaan"},
			wantErrs: []string{"missing values for code"},
		},
		{
			name:     "extra values, reported in a stable order",
			vars:     map[string]string{"name": "Kaan", "code": "1", "zeta": "z", "alpha": "a"},
			wantErrs: []string{"the template has no placeholder for alpha, zeta"},
		},
		{
			name:     "both problems at once",
			vars:     map[string]string{"nme": "Kaan", "code": "1"},
			wantErrs: []string{"missing values for name", "the template has no placeholder for nme"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, errs := tpl.Render(tt.vars)
			var msgs []string
			for _, e := range errs {
				if e.Field != "variables" {
					t.Errorf("error field = %q, want variables", e.Field)
				}
				msgs = append(msgs, e.Message)
			}
			if !slices.Equal(msgs, tt.wantErrs) {
				t.Fatalf("errors = %q, want %q", msgs, tt.wantErrs)
			}
			if got != tt.want {
				t.Errorf("Render() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestValidateTemplate(t *testing.T) {
	tests := []struct {
		name    string
		tName   string
		channel Channel
		body    string
		want    []string // fields with errors
	}{
		{"valid", "order-shipped.v2", ChannelSMS, "Hi {{name}}", nil},
		{"valid without placeholders", "static", ChannelPush, "Sale starts now", nil},
		{"missing everything", "", "", "  ", []string{"name", "channel", "body"}},
		{"name with a space", "order shipped", ChannelSMS, "x", []string{"name"}},
		{"name starting with a dot", ".hidden", ChannelSMS, "x", []string{"name"}},
		{"name too long", strings.Repeat("a", 101), ChannelSMS, "x", []string{"name"}},
		{"unknown channel", "t", "fax", "x", []string{"channel"}},
		{"space inside a name", "t", ChannelSMS, "Hi {{first name}}", []string{"body"}},
		{"unclosed placeholder", "t", ChannelSMS, "Hi {{name", []string{"body"}},
		{"placeholder starting with a digit", "t", ChannelSMS, "Hi {{1st}}", []string{"body"}},
		{"fixed text too long for sms", "t", ChannelSMS, strings.Repeat("x", 161) + "{{name}}", []string{"body"}},
		{"placeholders do not count towards the limit", "t", ChannelSMS, strings.Repeat("x", 160) + "{{name}}", nil},
		{"too many variables", "t", ChannelEmail, manyPlaceholders(51), []string{"body"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var fields []string
			for _, e := range ValidateTemplate(tt.tName, tt.channel, tt.body) {
				fields = append(fields, e.Field)
			}
			if !slices.Equal(fields, tt.want) {
				t.Errorf("error fields = %v, want %v", fields, tt.want)
			}
		})
	}
}

func manyPlaceholders(n int) string {
	var b strings.Builder
	for i := range n {
		b.WriteString("{{v")
		b.WriteString(strings.Repeat("x", i))
		b.WriteString("}} ")
	}
	return b.String()
}
