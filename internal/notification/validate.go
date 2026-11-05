package notification

import (
	"fmt"
	"net/mail"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// Request is what a client asks us to send. The rules follow ASSUMPTIONS A-06, A-07 and
// A-09.
type Request struct {
	Channel   Channel
	Recipient string
	Content   string
	Priority  Priority
	// ScheduledAt is the earliest time to send (ADR-0017, A-38); nil means now.
	// omitempty keeps the idempotency hash of unscheduled requests identical to the
	// hash stored before scheduling existed, so their replays still match.
	ScheduledAt *time.Time `json:",omitempty"`
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

	// scheduleTolerance is how far in the past scheduledAt may be and still count as
	// "now": it absorbs small differences between the client's clock and ours. Further in
	// the past is almost always a client bug, such as a wrong time zone (ADR-0017).
	scheduleTolerance = time.Minute
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
	if r.ScheduledAt != nil {
		// UTC and microseconds, which is what PostgreSQL stores, so the time a client
		// reads back and the idempotency hash both match what was sent.
		t := r.ScheduledAt.UTC().Truncate(time.Microsecond)
		r.ScheduledAt = &t
	}
	return r
}

// Validate returns every problem with a normalized request, or nil if it is valid. now
// and maxScheduleAhead bound scheduledAt; they are parameters, not time.Now() and a
// constant, so the rules stay pure and testable.
func (r Request) Validate(now time.Time, maxScheduleAhead time.Duration) []FieldError {
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

	if r.ScheduledAt != nil {
		switch at := *r.ScheduledAt; {
		case at.Before(now.Add(-scheduleTolerance)):
			add("scheduledAt", "must not be in the past (got %s, now is %s)", at.Format(time.RFC3339), now.UTC().Format(time.RFC3339))
		case at.After(now.Add(maxScheduleAhead)):
			add("scheduledAt", "must be at most %s ahead", aheadText(maxScheduleAhead))
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

// aheadText writes a whole number of days as "30 days" instead of Go's "720h0m0s".
func aheadText(d time.Duration) string {
	if day := 24 * time.Hour; d >= day && d%day == 0 {
		return fmt.Sprintf("%d days", d/day)
	}
	return d.String()
}
