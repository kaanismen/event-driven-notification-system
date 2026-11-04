package notification

import (
	"strings"
	"testing"
)

func TestNormalized(t *testing.T) {
	got := Request{Channel: ChannelSMS, Recipient: "  +905551234567\n", Content: "  hi  "}.Normalized()
	if got.Recipient != "+905551234567" {
		t.Errorf("Recipient = %q, want trimmed", got.Recipient)
	}
	if got.Content != "  hi  " {
		t.Errorf("Content = %q, want unchanged", got.Content)
	}
	if got.Priority != PriorityNormal {
		t.Errorf("Priority = %q, want normal by default", got.Priority)
	}

	if p := (Request{Priority: PriorityHigh}).Normalized().Priority; p != PriorityHigh {
		t.Errorf("explicit priority overwritten: %q", p)
	}
}

func TestValidate(t *testing.T) {
	valid := map[Channel]Request{
		ChannelSMS:   {Channel: ChannelSMS, Recipient: "+905551234567", Content: "Your code is 1234"},
		ChannelEmail: {Channel: ChannelEmail, Recipient: "user@example.com", Content: "Hello"},
		ChannelPush:  {Channel: ChannelPush, Recipient: "device-token-abc", Content: "Sale starts now"},
	}
	with := func(c Channel, edit func(*Request)) Request {
		r := valid[c]
		edit(&r)
		return r.Normalized()
	}

	tests := []struct {
		name       string
		req        Request
		wantFields []string // nil means valid
	}{
		{"valid sms", valid[ChannelSMS].Normalized(), nil},
		{"valid email", valid[ChannelEmail].Normalized(), nil},
		{"valid push", valid[ChannelPush].Normalized(), nil},
		{"sms at limit", with(ChannelSMS, func(r *Request) { r.Content = strings.Repeat("a", 160) }), nil},
		{"sms limit counts characters not bytes", with(ChannelSMS, func(r *Request) { r.Content = strings.Repeat("ü", 160) }), nil},
		{"sms over limit", with(ChannelSMS, func(r *Request) { r.Content = strings.Repeat("a", 161) }), []string{"content"}},
		{"push over limit", with(ChannelPush, func(r *Request) { r.Content = strings.Repeat("a", 257) }), []string{"content"}},
		{"email at limit", with(ChannelEmail, func(r *Request) { r.Content = strings.Repeat("a", 10_000) }), nil},
		{"email over limit", with(ChannelEmail, func(r *Request) { r.Content = strings.Repeat("a", 10_001) }), []string{"content"}},
		{"blank content", with(ChannelSMS, func(r *Request) { r.Content = " \n\t" }), []string{"content"}},
		{"sms without plus", with(ChannelSMS, func(r *Request) { r.Recipient = "905551234567" }), []string{"recipient"}},
		{"sms too short", with(ChannelSMS, func(r *Request) { r.Recipient = "+1234567" }), []string{"recipient"}},
		{"sms too long", with(ChannelSMS, func(r *Request) { r.Recipient = "+1234567890123456" }), []string{"recipient"}},
		{"sms leading zero", with(ChannelSMS, func(r *Request) { r.Recipient = "+0555123456" }), []string{"recipient"}},
		{"email invalid", with(ChannelEmail, func(r *Request) { r.Recipient = "not-an-email" }), []string{"recipient"}},
		{"email with display name", with(ChannelEmail, func(r *Request) { r.Recipient = "Ann <ann@example.com>" }), []string{"recipient"}},
		{"email list", with(ChannelEmail, func(r *Request) { r.Recipient = "a@example.com, b@example.com" }), []string{"recipient"}},
		{"push token too long", with(ChannelPush, func(r *Request) { r.Recipient = strings.Repeat("t", 513) }), []string{"recipient"}},
		{"missing recipient", with(ChannelSMS, func(r *Request) { r.Recipient = "   " }), []string{"recipient"}},
		{"missing channel", with(ChannelSMS, func(r *Request) { r.Channel = "" }), []string{"channel"}},
		{"unknown channel", with(ChannelSMS, func(r *Request) { r.Channel = "fax" }), []string{"channel"}},
		{"channel is case sensitive", with(ChannelSMS, func(r *Request) { r.Channel = "SMS" }), []string{"channel"}},
		{"unknown priority", with(ChannelSMS, func(r *Request) { r.Priority = "urgent" }), []string{"priority"}},
		{"everything wrong", Request{Priority: "x"}.Normalized(), []string{"channel", "priority", "recipient", "content"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := tt.req.Validate()
			var got []string
			for _, e := range errs {
				got = append(got, e.Field)
				if e.Message == "" {
					t.Errorf("field %q has an empty message", e.Field)
				}
			}
			if strings.Join(got, ",") != strings.Join(tt.wantFields, ",") {
				t.Errorf("invalid fields = %v, want %v (errors: %+v)", got, tt.wantFields, errs)
			}
		})
	}
}

func TestPriorityRankRoundTrip(t *testing.T) {
	for _, p := range []Priority{PriorityHigh, PriorityNormal, PriorityLow} {
		got, err := PriorityFromRank(p.Rank())
		if err != nil || got != p {
			t.Errorf("PriorityFromRank(%d) = %q, %v; want %q", p.Rank(), got, err, p)
		}
	}
	if PriorityHigh.Rank() >= PriorityNormal.Rank() || PriorityNormal.Rank() >= PriorityLow.Rank() {
		t.Error("ranks must sort high < normal < low")
	}
	if _, err := PriorityFromRank(9); err == nil {
		t.Error("PriorityFromRank(9) succeeded, want error")
	}
}

func TestStatusValid(t *testing.T) {
	for _, s := range Statuses() {
		if !s.Valid() {
			t.Errorf("%q not valid", s)
		}
	}
	if Status("lost").Valid() {
		t.Error(`"lost" reported as valid`)
	}
}
