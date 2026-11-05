// Package notification holds the domain model: channels, priorities, statuses, the
// Notification record and request validation. It does no I/O, so every rule here is
// testable with plain unit tests.
package notification

import (
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Channel is the delivery channel of a notification.
type Channel string

const (
	ChannelSMS   Channel = "sms"
	ChannelEmail Channel = "email"
	ChannelPush  Channel = "push"
)

// Valid reports whether c is one of the supported channels.
func (c Channel) Valid() bool {
	switch c {
	case ChannelSMS, ChannelEmail, ChannelPush:
		return true
	}
	return false
}

// Priority decides who waits when a channel has a backlog (ADR-0008).
type Priority string

const (
	PriorityHigh   Priority = "high"
	PriorityNormal Priority = "normal"
	PriorityLow    Priority = "low"
)

// Valid reports whether p is one of the supported priorities.
func (p Priority) Valid() bool {
	switch p {
	case PriorityHigh, PriorityNormal, PriorityLow:
		return true
	}
	return false
}

// Rank is the value stored in the database: lower is more urgent, so an ascending sort
// puts high priority first.
func (p Priority) Rank() int16 {
	switch p {
	case PriorityHigh:
		return 1
	case PriorityLow:
		return 3
	default:
		return 2
	}
}

// PriorityFromRank is the inverse of Rank.
func PriorityFromRank(rank int16) (Priority, error) {
	switch rank {
	case 1:
		return PriorityHigh, nil
	case 2:
		return PriorityNormal, nil
	case 3:
		return PriorityLow, nil
	}
	return "", fmt.Errorf("unknown priority rank %d", rank)
}

// Status is the lifecycle state of a notification (see docs/architecture.md).
type Status string

const (
	StatusPending    Status = "pending"
	StatusProcessing Status = "processing"
	StatusSent       Status = "sent"
	StatusFailed     Status = "failed"
	StatusCancelled  Status = "cancelled"
)

// Statuses returns every status in lifecycle order. It is a function, not a package
// variable, so no caller can modify the shared list.
func Statuses() []Status {
	return []Status{StatusPending, StatusProcessing, StatusSent, StatusFailed, StatusCancelled}
}

// Valid reports whether s is a known status.
func (s Status) Valid() bool {
	for _, known := range Statuses() {
		if s == known {
			return true
		}
	}
	return false
}

// Notification is one message to one recipient on one channel.
type Notification struct {
	ID            uuid.UUID
	BatchID       *uuid.UUID // nil for notifications created one at a time
	Channel       Channel
	Recipient     string
	Content       string
	Priority      Priority
	Status        Status
	Attempts      int
	MaxAttempts   int
	NextAttemptAt time.Time
	ScheduledAt   *time.Time // nil unless the client asked for a send time (ADR-0017)
	TemplateID    *uuid.UUID // the template Content was rendered from (ADR-0019), if any
	// Version counts the updates of the row; a higher version is a later state (ADR-0020).
	Version           int64
	LastError         string // empty when no attempt has failed
	ProviderMessageID string // empty until the provider accepts the message
	CorrelationID     string // request ID of the API call that created it
	TraceParent       string // W3C traceparent of that API call, if traced (ADR-0021)
	CreatedAt         time.Time
	UpdatedAt         time.Time
	SentAt            *time.Time
}
