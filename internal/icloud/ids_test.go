package icloud

import (
	"testing"
	"time"
)

func TestEventIDRoundTrip_NonRecurring(t *testing.T) {
	id := EventID{
		CalendarPath: "/123/calendars/home/",
		ObjectPath:   "/123/calendars/home/abc.ics",
		UID:          "abc",
	}

	decoded, err := DecodeEventID(id.Encode())
	if err != nil {
		t.Fatalf("DecodeEventID() error = %v", err)
	}
	if decoded != id {
		t.Fatalf("round trip = %+v, want %+v", decoded, id)
	}
}

func TestEventIDRoundTrip_Recurring(t *testing.T) {
	rid := time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)
	id := EventID{
		CalendarPath: "/123/calendars/home/",
		ObjectPath:   "/123/calendars/home/weekly.ics",
		UID:          "weekly",
		RecurrenceID: &rid,
	}

	decoded, err := DecodeEventID(id.Encode())
	if err != nil {
		t.Fatalf("DecodeEventID() error = %v", err)
	}
	if decoded.CalendarPath != id.CalendarPath || decoded.ObjectPath != id.ObjectPath || decoded.UID != id.UID {
		t.Fatalf("round trip fields = %+v, want %+v", decoded, id)
	}
	if decoded.RecurrenceID == nil || !decoded.RecurrenceID.Equal(*id.RecurrenceID) {
		t.Fatalf("round trip RecurrenceID = %v, want %v", decoded.RecurrenceID, id.RecurrenceID)
	}
}

func TestDecodeEventID_RejectsGarbage(t *testing.T) {
	if _, err := DecodeEventID("not-valid-base64!!!"); err == nil {
		t.Fatal("expected an error for malformed input")
	}
	if _, err := DecodeEventID("aGVsbG8"); err == nil {
		t.Fatal("expected an error for valid base64 with the wrong field count")
	}
}
