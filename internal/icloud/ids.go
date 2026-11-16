package icloud

import (
	"encoding/base64"
	"fmt"
	"strings"
	"time"
)

// idSep separates the fields packed into an opaque event ID. It's a
// control character, so it can't collide with a calendar href or a UID.
const idSep = "\x1f"

// EventID identifies one concrete event instance well enough for get_event
// to re-fetch and, if needed, re-expand it: which CalDAV object (.ics
// resource) it lives in, which VEVENT within that object (by UID, since one
// object can hold a recurring master plus its overrides), and — for a
// recurring series — which instance.
type EventID struct {
	CalendarPath string
	ObjectPath   string
	UID          string
	// RecurrenceID is nil for a non-recurring event, and set to the
	// instance's original (as-scheduled) start time for a recurring one.
	RecurrenceID *time.Time
}

// Encode packs the EventID into an opaque string suitable for returning to
// an MCP client as a tool result's "id" field.
func (id EventID) Encode() string {
	recurrencePart := ""
	if id.RecurrenceID != nil {
		recurrencePart = id.RecurrenceID.UTC().Format(time.RFC3339)
	}
	raw := strings.Join([]string{id.CalendarPath, id.ObjectPath, id.UID, recurrencePart}, idSep)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// DecodeEventID reverses EventID.Encode.
func DecodeEventID(id string) (EventID, error) {
	raw, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil {
		return EventID{}, fmt.Errorf("icloud: malformed event id: %w", err)
	}
	parts := strings.Split(string(raw), idSep)
	if len(parts) != 4 {
		return EventID{}, fmt.Errorf("icloud: malformed event id")
	}

	decoded := EventID{CalendarPath: parts[0], ObjectPath: parts[1], UID: parts[2]}
	if parts[3] != "" {
		t, err := time.Parse(time.RFC3339, parts[3])
		if err != nil {
			return EventID{}, fmt.Errorf("icloud: malformed event id recurrence timestamp: %w", err)
		}
		decoded.RecurrenceID = &t
	}
	return decoded, nil
}

// ID returns this instance's opaque, encoded event ID.
func (i Instance) ID() string {
	return EventID{
		CalendarPath: i.CalendarPath,
		ObjectPath:   i.ObjectPath,
		UID:          i.UID,
		RecurrenceID: i.RecurrenceID,
	}.Encode()
}
