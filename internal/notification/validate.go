package notification

import (
	"fmt"
	"net/mail"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Request is what a client asks us to send. The rules follow ASSUMPTIONS A-06, A-07 and
// A-09.
type Request struct {
	Channel   Channel
	Recipient string
	Content   string
	Priority  Priority
}

// FieldError describes one invalid field, in terms a client can act on.
type FieldError struct {
	Field   string
	Message string
}

const (
	maxSMSLength   = 160 // one segment; multipart SMS is out of scope (A-07)
	maxPushLength  = 256
	maxEmailLength = 10_000
	maxPushToken   = 512
)

// e164 is "+", a non-zero digit, then 7 to 14 more digits: 8 to 15 digits in total.
var e164 = regexp.MustCompile(`^\+[1-9][0-9]{7,14}$`)

// Normalized returns a copy with surrounding whitespace removed from the recipient and
// the default priority applied. Content is left exactly as sent: spaces and line breaks
// in a message body are the sender's choice.
func (r Request) Normalized() Request {
	r.Recipient = strings.TrimSpace(r.Recipient)
	if r.Priority == "" {
		r.Priority = PriorityNormal
	}
	return r
}

// Validate returns every problem with a normalized request, or nil if it is valid.
func (r Request) Validate() []FieldError {
	var errs []FieldError
	add := func(field, format string, args ...any) {
		errs = append(errs, FieldError{Field: field, Message: fmt.Sprintf(format, args...)})
	}

	switch {
	case r.Channel == "":
		add("channel", "is required")
	case !r.Channel.Valid():
		add("channel", "must be one of sms, email, push")
	}

	if !r.Priority.Valid() {
		add("priority", "must be one of high, normal, low")
	}

	if r.Recipient == "" {
		add("recipient", "is required")
	} else if msg := recipientProblem(r.Channel, r.Recipient); msg != "" {
		add("recipient", "%s", msg)
	}

	if strings.TrimSpace(r.Content) == "" {
		add("content", "is required and must not be blank")
	} else if limit := maxContentLength(r.Channel); limit > 0 {
		// Characters (Unicode code points), not bytes: "ü" counts as one.
		if n := utf8.RuneCountInString(r.Content); n > limit {
			add("content", "must be at most %d characters for %s, got %d", limit, r.Channel, n)
		}
	}

	return errs
}

func recipientProblem(c Channel, recipient string) string {
	switch c {
	case ChannelSMS:
		if !e164.MatchString(recipient) {
			return "must be an E.164 phone number, for example +905551234567"
		}
	case ChannelEmail:
		addr, err := mail.ParseAddress(recipient)
		// Reject display-name forms such as "Ann <ann@example.com>": the provider body has
		// a single "to" field that must hold the bare address.
		if err != nil || addr.Name != "" || addr.Address != recipient {
			return "must be a single email address, for example user@example.com"
		}
	case ChannelPush:
		if utf8.RuneCountInString(recipient) > maxPushToken {
			return fmt.Sprintf("must be a device token of at most %d characters", maxPushToken)
		}
	}
	// An unknown channel is already reported on its own field.
	return ""
}

func maxContentLength(c Channel) int {
	switch c {
	case ChannelSMS:
		return maxSMSLength
	case ChannelPush:
		return maxPushLength
	case ChannelEmail:
		return maxEmailLength
	}
	return 0
}
