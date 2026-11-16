package icloud

import (
	"strings"
	"testing"
	"time"

	ical "github.com/emersion/go-ical"
)

var chicago = func() *time.Location {
	loc, err := time.LoadLocation("America/Chicago")
	if err != nil {
		panic(err)
	}
	return loc
}()

func parseEvents(t *testing.T, raw string) []ical.Event {
	t.Helper()
	cal, err := ical.NewDecoder(strings.NewReader(raw)).Decode()
	if err != nil {
		t.Fatalf("decoding fixture ICS: %v", err)
	}
	return cal.Events()
}

func mustParse(t *testing.T, value string) time.Time {
	t.Helper()
	ts, err := time.ParseInLocation("2006-01-02T15:04:05", value, chicago)
	if err != nil {
		t.Fatalf("parsing %q: %v", value, err)
	}
	return ts
}

// A weekly standup, Mondays 09:00-09:30 America/Chicago, with:
//   - 2026-11-09 excluded via EXDATE (e.g. a holiday)
//   - 2026-11-16 overridden via RECURRENCE-ID to move to 10:00 and rename
const weeklyWithExceptionICS = `BEGIN:VCALENDAR
VERSION:2.0
PRODID:-//test//test//EN
BEGIN:VEVENT
UID:weekly-standup@example.com
DTSTAMP:20261001T000000Z
DTSTART;TZID=America/Chicago:20261102T090000
DTEND;TZID=America/Chicago:20261102T093000
RRULE:FREQ=WEEKLY;BYDAY=MO
EXDATE;TZID=America/Chicago:20261109T090000
SUMMARY:Standup
END:VEVENT
BEGIN:VEVENT
UID:weekly-standup@example.com
DTSTAMP:20261001T000000Z
RECURRENCE-ID;TZID=America/Chicago:20261116T090000
DTSTART;TZID=America/Chicago:20261116T100000
DTEND;TZID=America/Chicago:20261116T103000
SUMMARY:Standup (moved for client visit)
END:VEVENT
END:VCALENDAR
`

func TestExpandEvents_WeeklySeriesWithExceptions(t *testing.T) {
	events := parseEvents(t, weeklyWithExceptionICS)

	windowStart := mustParse(t, "2026-11-01T00:00:00")
	windowEnd := mustParse(t, "2026-12-01T00:00:00")

	instances, err := ExpandEvents("/cal/", "/cal/weekly-standup.ics", events, windowStart, windowEnd, chicago)
	if err != nil {
		t.Fatalf("ExpandEvents() error = %v", err)
	}

	// November 2026 Mondays: 2, 9, 16, 23, 30 — minus the EXDATE'd 9th = 4 instances.
	if len(instances) != 4 {
		starts := make([]string, len(instances))
		for i, inst := range instances {
			starts[i] = inst.Start.Format(time.RFC3339)
		}
		t.Fatalf("got %d instances (%v), want 4", len(instances), starts)
	}

	wantStarts := []time.Time{
		mustParse(t, "2026-11-02T09:00:00"),
		mustParse(t, "2026-11-16T10:00:00"), // overridden time
		mustParse(t, "2026-11-23T09:00:00"),
		mustParse(t, "2026-11-30T09:00:00"),
	}
	for i, want := range wantStarts {
		if !instances[i].Start.Equal(want) {
			t.Errorf("instance %d start = %v, want %v", i, instances[i].Start, want)
		}
	}

	// The EXDATE'd Nov 9 occurrence must not appear at all.
	for _, inst := range instances {
		if inst.Start.Month() == time.November && inst.Start.Day() == 9 {
			t.Errorf("Nov 9 occurrence should have been excluded by EXDATE, got %v", inst.Start)
		}
	}

	// The overridden occurrence carries its own title and a RecurrenceID
	// pinned to the *original* (pre-move) slot, not the new time.
	overridden := instances[1]
	if overridden.Title != "Standup (moved for client visit)" {
		t.Errorf("overridden title = %q, want %q", overridden.Title, "Standup (moved for client visit)")
	}
	if overridden.RecurrenceID == nil || !overridden.RecurrenceID.Equal(mustParse(t, "2026-11-16T09:00:00")) {
		t.Errorf("overridden RecurrenceID = %v, want 2026-11-16T09:00:00 (original slot)", overridden.RecurrenceID)
	}

	// Un-overridden instances keep the master's title and are marked Recurring.
	if instances[0].Title != "Standup" || !instances[0].Recurring {
		t.Errorf("instance 0 = %+v, want master title %q and Recurring=true", instances[0], "Standup")
	}
}

func TestExpandEvents_CancelledOccurrenceIsOmitted(t *testing.T) {
	const raw = `BEGIN:VCALENDAR
VERSION:2.0
PRODID:-//test//test//EN
BEGIN:VEVENT
UID:weekly-cancel@example.com
DTSTAMP:20261001T000000Z
DTSTART;TZID=America/Chicago:20261102T090000
DTEND;TZID=America/Chicago:20261102T093000
RRULE:FREQ=WEEKLY;BYDAY=MO
SUMMARY:Standup
END:VEVENT
BEGIN:VEVENT
UID:weekly-cancel@example.com
DTSTAMP:20261001T000000Z
RECURRENCE-ID;TZID=America/Chicago:20261109T090000
DTSTART;TZID=America/Chicago:20261109T090000
DTEND;TZID=America/Chicago:20261109T093000
SUMMARY:Standup
STATUS:CANCELLED
END:VEVENT
END:VCALENDAR
`
	events := parseEvents(t, raw)
	windowStart := mustParse(t, "2026-11-01T00:00:00")
	windowEnd := mustParse(t, "2026-11-10T00:00:00")

	instances, err := ExpandEvents("/cal/", "/cal/weekly-cancel.ics", events, windowStart, windowEnd, chicago)
	if err != nil {
		t.Fatalf("ExpandEvents() error = %v", err)
	}
	if len(instances) != 1 {
		t.Fatalf("got %d instances, want 1 (Nov 9 cancelled via STATUS override)", len(instances))
	}
	if instances[0].Start.Day() != 2 {
		t.Errorf("remaining instance = %v, want Nov 2", instances[0].Start)
	}
}

func TestExpandEvents_AllDayEvent(t *testing.T) {
	const raw = `BEGIN:VCALENDAR
VERSION:2.0
PRODID:-//test//test//EN
BEGIN:VEVENT
UID:all-day@example.com
DTSTAMP:20261001T000000Z
DTSTART;VALUE=DATE:20261126
DTEND;VALUE=DATE:20261127
SUMMARY:Thanksgiving
END:VEVENT
END:VCALENDAR
`
	events := parseEvents(t, raw)
	windowStart := mustParse(t, "2026-11-01T00:00:00")
	windowEnd := mustParse(t, "2026-12-01T00:00:00")

	instances, err := ExpandEvents("/cal/", "/cal/all-day.ics", events, windowStart, windowEnd, chicago)
	if err != nil {
		t.Fatalf("ExpandEvents() error = %v", err)
	}
	if len(instances) != 1 {
		t.Fatalf("got %d instances, want 1", len(instances))
	}
	inst := instances[0]
	if !inst.AllDay {
		t.Error("expected AllDay = true")
	}
	wantStart := time.Date(2026, 11, 26, 0, 0, 0, 0, chicago)
	wantEnd := time.Date(2026, 11, 27, 0, 0, 0, 0, chicago)
	if !inst.Start.Equal(wantStart) || !inst.End.Equal(wantEnd) {
		t.Errorf("all-day instance = [%v, %v), want [%v, %v)", inst.Start, inst.End, wantStart, wantEnd)
	}
}

func TestExpandEvents_RecurringAllDayEvent(t *testing.T) {
	// An annual all-day event (a birthday), expanded over a 2-year window.
	const raw = `BEGIN:VCALENDAR
VERSION:2.0
PRODID:-//test//test//EN
BEGIN:VEVENT
UID:birthday@example.com
DTSTAMP:20261001T000000Z
DTSTART;VALUE=DATE:20261224
DTEND;VALUE=DATE:20261225
RRULE:FREQ=YEARLY
SUMMARY:Birthday
END:VEVENT
END:VCALENDAR
`
	events := parseEvents(t, raw)
	windowStart := mustParse(t, "2026-11-01T00:00:00")
	windowEnd := mustParse(t, "2027-12-31T00:00:00")

	instances, err := ExpandEvents("/cal/", "/cal/birthday.ics", events, windowStart, windowEnd, chicago)
	if err != nil {
		t.Fatalf("ExpandEvents() error = %v", err)
	}
	if len(instances) != 2 {
		t.Fatalf("got %d instances, want 2 (one per year)", len(instances))
	}
	for _, inst := range instances {
		if !inst.AllDay {
			t.Errorf("instance %v should be AllDay", inst.Start)
		}
	}
}

func TestExpandEvents_DSTSpringForwardCrossover(t *testing.T) {
	// US DST begins 2026-03-08. A weekly 09:00 America/Chicago series
	// spanning that date must keep 09:00 local wall-clock time on both
	// sides of the transition (the UTC offset changes, the local hour
	// doesn't).
	const raw = `BEGIN:VCALENDAR
VERSION:2.0
PRODID:-//test//test//EN
BEGIN:VEVENT
UID:dst-weekly@example.com
DTSTAMP:20260101T000000Z
DTSTART;TZID=America/Chicago:20260302T090000
DTEND;TZID=America/Chicago:20260302T093000
RRULE:FREQ=WEEKLY;BYDAY=MO
SUMMARY:Standup
END:VEVENT
END:VCALENDAR
`
	events := parseEvents(t, raw)
	windowStart := mustParse(t, "2026-03-01T00:00:00")
	windowEnd := mustParse(t, "2026-03-31T00:00:00")

	instances, err := ExpandEvents("/cal/", "/cal/dst-weekly.ics", events, windowStart, windowEnd, chicago)
	if err != nil {
		t.Fatalf("ExpandEvents() error = %v", err)
	}
	if len(instances) != 5 {
		t.Fatalf("got %d instances, want 5 Mondays in March 2026", len(instances))
	}

	for _, inst := range instances {
		if h, m := inst.Start.Hour(), inst.Start.Minute(); h != 9 || m != 0 {
			t.Errorf("instance %v: local wall-clock time = %02d:%02d, want 09:00", inst.Start, h, m)
		}
	}

	// Before the transition (Mar 2): CST, UTC-6. After it (Mar 9 onward): CDT, UTC-5.
	before := instances[0] // Mar 2
	after := instances[1]  // Mar 9
	if _, offset := before.Start.Zone(); offset != -6*3600 {
		t.Errorf("Mar 2 offset = %d, want -21600 (CST)", offset)
	}
	if _, offset := after.Start.Zone(); offset != -5*3600 {
		t.Errorf("Mar 9 offset = %d, want -18000 (CDT)", offset)
	}
}

func TestExpandEvents_SingleNonRecurringEvent(t *testing.T) {
	const raw = `BEGIN:VCALENDAR
VERSION:2.0
PRODID:-//test//test//EN
BEGIN:VEVENT
UID:single@example.com
DTSTAMP:20261001T000000Z
DTSTART;TZID=America/Chicago:20261115T140000
DTEND;TZID=America/Chicago:20261115T150000
SUMMARY:Dentist
LOCATION:123 Main St
END:VEVENT
END:VCALENDAR
`
	events := parseEvents(t, raw)
	windowStart := mustParse(t, "2026-11-01T00:00:00")
	windowEnd := mustParse(t, "2026-12-01T00:00:00")

	instances, err := ExpandEvents("/cal/", "/cal/single.ics", events, windowStart, windowEnd, chicago)
	if err != nil {
		t.Fatalf("ExpandEvents() error = %v", err)
	}
	if len(instances) != 1 {
		t.Fatalf("got %d instances, want 1", len(instances))
	}
	inst := instances[0]
	if inst.Recurring || inst.RecurrenceID != nil {
		t.Errorf("non-recurring event got Recurring=%v RecurrenceID=%v", inst.Recurring, inst.RecurrenceID)
	}
	if inst.Location != "123 Main St" {
		t.Errorf("Location = %q, want %q", inst.Location, "123 Main St")
	}
}

func TestExpandEvents_OutsideWindowIsExcluded(t *testing.T) {
	events := parseEvents(t, weeklyWithExceptionICS)

	// A window that only covers the first week of November.
	windowStart := mustParse(t, "2026-11-01T00:00:00")
	windowEnd := mustParse(t, "2026-11-03T00:00:00")

	instances, err := ExpandEvents("/cal/", "/cal/weekly-standup.ics", events, windowStart, windowEnd, chicago)
	if err != nil {
		t.Fatalf("ExpandEvents() error = %v", err)
	}
	if len(instances) != 1 {
		t.Fatalf("got %d instances, want 1 (only Nov 2 falls in this narrow window)", len(instances))
	}
}
