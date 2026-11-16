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

func instanceStarts(instances []Instance) []string {
	out := make([]string, len(instances))
	for i, inst := range instances {
		out[i] = inst.Start.Format(time.RFC3339)
	}
	return out
}

func TestExpandEvents_MultiValueEXDATE(t *testing.T) {
	const raw = `BEGIN:VCALENDAR
VERSION:2.0
PRODID:-//test//test//EN
BEGIN:VEVENT
UID:multi-exdate@example.com
DTSTAMP:20261001T000000Z
DTSTART;TZID=America/Chicago:20261102T090000
DTEND;TZID=America/Chicago:20261102T093000
RRULE:FREQ=WEEKLY;BYDAY=MO
EXDATE;TZID=America/Chicago:20261109T090000,20261116T090000
SUMMARY:Standup
END:VEVENT
END:VCALENDAR
`
	instances, err := ExpandEvents("/cal/", "/cal/x.ics", parseEvents(t, raw),
		mustParse(t, "2026-11-01T00:00:00"), mustParse(t, "2026-12-01T00:00:00"), chicago)
	if err != nil {
		t.Fatalf("ExpandEvents() error = %v", err)
	}
	// Mondays 2, 9, 16, 23, 30 minus the two EXDATEs.
	want := []string{"2026-11-02T09:00:00-06:00", "2026-11-23T09:00:00-06:00", "2026-11-30T09:00:00-06:00"}
	if got := instanceStarts(instances); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("starts = %v, want %v", got, want)
	}
}

func TestExpandEvents_NonIANATZIDs(t *testing.T) {
	// A Windows zone ID (as Exchange sends) resolves to its IANA zone; an
	// unrecognisable label falls back to the default location rather than
	// failing the whole object.
	const raw = `BEGIN:VCALENDAR
VERSION:2.0
PRODID:-//test//test//EN
BEGIN:VEVENT
UID:windows-tz@example.com
DTSTAMP:20261001T000000Z
DTSTART;TZID=Eastern Standard Time:20261117T100000
DTEND;TZID=Eastern Standard Time:20261117T110000
SUMMARY:From Exchange
END:VEVENT
END:VCALENDAR
`
	instances, err := ExpandEvents("/cal/", "/cal/a.ics", parseEvents(t, raw),
		mustParse(t, "2026-11-17T00:00:00"), mustParse(t, "2026-11-18T00:00:00"), chicago)
	if err != nil {
		t.Fatalf("ExpandEvents(windows tz) error = %v", err)
	}
	if len(instances) != 1 || !instances[0].Start.Equal(mustParse(t, "2026-11-17T09:00:00")) {
		t.Errorf("windows-tz instances = %v, want one at 10:00 Eastern (09:00 Chicago)", instanceStarts(instances))
	}

	const rawUnknown = `BEGIN:VCALENDAR
VERSION:2.0
PRODID:-//test//test//EN
BEGIN:VEVENT
UID:unknown-tz@example.com
DTSTAMP:20261001T000000Z
DTSTART;TZID=Customized Time Zone 1:20261102T090000
DTEND;TZID=Customized Time Zone 1:20261102T093000
RRULE:FREQ=WEEKLY;COUNT=3
EXDATE;TZID=Customized Time Zone 1:20261109T090000
SUMMARY:Odd zone
END:VEVENT
END:VCALENDAR
`
	instances, err = ExpandEvents("/cal/", "/cal/b.ics", parseEvents(t, rawUnknown),
		mustParse(t, "2026-11-01T00:00:00"), mustParse(t, "2026-12-01T00:00:00"), chicago)
	if err != nil {
		t.Fatalf("ExpandEvents(unknown tz) error = %v", err)
	}
	want := []string{"2026-11-02T09:00:00-06:00", "2026-11-16T09:00:00-06:00"}
	if got := instanceStarts(instances); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("unknown-tz starts = %v, want %v", got, want)
	}
}

func TestExpandEvents_UnparseableObjectIsAnError(t *testing.T) {
	const raw = `BEGIN:VCALENDAR
VERSION:2.0
PRODID:-//test//test//EN
BEGIN:VEVENT
UID:broken@example.com
DTSTAMP:20261001T000000Z
DTSTART:not-a-date
SUMMARY:Broken
END:VEVENT
END:VCALENDAR
`
	_, err := ExpandEvents("/cal/", "/cal/broken.ics", parseEvents(t, raw),
		mustParse(t, "2026-11-01T00:00:00"), mustParse(t, "2026-12-01T00:00:00"), chicago)
	if err == nil {
		t.Fatal("ExpandEvents() on an unparseable DTSTART returned no error")
	}
}

func TestExpandEvents_RecurringAllDayAcrossDST(t *testing.T) {
	// Weekly all-day Sunday event; US DST starts Sunday 2026-03-08, a 23h
	// local day, and ends Sunday 2026-11-01, a 25h one.
	const raw = `BEGIN:VCALENDAR
VERSION:2.0
PRODID:-//test//test//EN
BEGIN:VEVENT
UID:sunday@example.com
DTSTAMP:20261001T000000Z
DTSTART;VALUE=DATE:20260222
DTEND;VALUE=DATE:20260223
RRULE:FREQ=WEEKLY
SUMMARY:Sunday thing
END:VEVENT
END:VCALENDAR
`
	events := parseEvents(t, raw)

	// The day after each transition must not include the Sunday event.
	for _, day := range []string{"2026-03-09", "2026-11-02"} {
		ws := mustParse(t, day+"T00:00:00")
		instances, err := ExpandEvents("/cal/", "/cal/sunday.ics", events, ws, ws.AddDate(0, 0, 1), chicago)
		if err != nil {
			t.Fatalf("ExpandEvents() error = %v", err)
		}
		if len(instances) != 0 {
			t.Errorf("Monday %s: got %v, want no instances (Sunday all-day event bled over)", day, instanceStarts(instances))
		}
	}

	// And every occurrence spans exactly local midnight to local midnight.
	instances, err := ExpandEvents("/cal/", "/cal/sunday.ics", events,
		mustParse(t, "2026-02-01T00:00:00"), mustParse(t, "2026-12-01T00:00:00"), chicago)
	if err != nil {
		t.Fatalf("ExpandEvents() error = %v", err)
	}
	if len(instances) == 0 {
		t.Fatal("no instances")
	}
	for _, inst := range instances {
		s, e := inst.Start, inst.End
		if s.Hour() != 0 || e.Hour() != 0 || e.Minute() != 0 || calendarDaysBetween(s, e) != 1 || s.Weekday() != time.Sunday {
			t.Errorf("occurrence [%v, %v) is not one local Sunday midnight-to-midnight", s, e)
		}
	}
}

func TestExpandEvents_LongOccurrenceQueriedMidSpan(t *testing.T) {
	// A 5-day annual all-day event (Nov 2-6). Querying Nov 5 alone must find
	// it even though it started more than expansionPad earlier.
	const raw = `BEGIN:VCALENDAR
VERSION:2.0
PRODID:-//test//test//EN
BEGIN:VEVENT
UID:conference@example.com
DTSTAMP:20251001T000000Z
DTSTART;VALUE=DATE:20251102
DTEND;VALUE=DATE:20251107
RRULE:FREQ=YEARLY
SUMMARY:Conference
END:VEVENT
END:VCALENDAR
`
	ws := mustParse(t, "2026-11-05T00:00:00")
	instances, err := ExpandEvents("/cal/", "/cal/conf.ics", parseEvents(t, raw), ws, ws.AddDate(0, 0, 1), chicago)
	if err != nil {
		t.Fatalf("ExpandEvents() error = %v", err)
	}
	if len(instances) != 1 || !instances[0].Start.Equal(mustParse(t, "2026-11-02T00:00:00")) {
		t.Fatalf("got %v, want the 2026 occurrence starting Nov 2", instanceStarts(instances))
	}
	if want := mustParse(t, "2026-11-07T00:00:00"); !instances[0].End.Equal(want) {
		t.Errorf("end = %v, want %v", instances[0].End, want)
	}
}

func TestExpandEvents_UntilIncludesLastOccurrence(t *testing.T) {
	cases := []struct {
		name, dtstart, dtend, rrule string
		want                        []string
	}{
		{
			name:    "all-day with DATE UNTIL",
			dtstart: "DTSTART;VALUE=DATE:20261102",
			dtend:   "DTEND;VALUE=DATE:20261103",
			rrule:   "RRULE:FREQ=DAILY;UNTIL=20261105",
			want:    []string{"2026-11-02T00:00:00-06:00", "2026-11-03T00:00:00-06:00", "2026-11-04T00:00:00-06:00", "2026-11-05T00:00:00-06:00"},
		},
		{
			name:    "TZID with floating UNTIL",
			dtstart: "DTSTART;TZID=America/Chicago:20261102T090000",
			dtend:   "DTEND;TZID=America/Chicago:20261102T100000",
			rrule:   "RRULE:FREQ=DAILY;UNTIL=20261104T090000",
			want:    []string{"2026-11-02T09:00:00-06:00", "2026-11-03T09:00:00-06:00", "2026-11-04T09:00:00-06:00"},
		},
		{
			name:    "TZID with DATE UNTIL",
			dtstart: "DTSTART;TZID=America/Chicago:20261102T090000",
			dtend:   "DTEND;TZID=America/Chicago:20261102T100000",
			rrule:   "RRULE:FREQ=DAILY;UNTIL=20261104",
			want:    []string{"2026-11-02T09:00:00-06:00", "2026-11-03T09:00:00-06:00", "2026-11-04T09:00:00-06:00"},
		},
		{
			name:    "UTC UNTIL unchanged",
			dtstart: "DTSTART;TZID=America/Chicago:20261102T090000",
			dtend:   "DTEND;TZID=America/Chicago:20261102T100000",
			rrule:   "RRULE:FREQ=DAILY;UNTIL=20261104T145959Z",
			want:    []string{"2026-11-02T09:00:00-06:00", "2026-11-03T09:00:00-06:00"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := "BEGIN:VCALENDAR\nVERSION:2.0\nPRODID:-//test//test//EN\nBEGIN:VEVENT\nUID:until@example.com\nDTSTAMP:20261001T000000Z\n" +
				tc.dtstart + "\n" + tc.dtend + "\n" + tc.rrule + "\nSUMMARY:Until\nEND:VEVENT\nEND:VCALENDAR\n"
			instances, err := ExpandEvents("/cal/", "/cal/until.ics", parseEvents(t, raw),
				mustParse(t, "2026-11-01T00:00:00"), mustParse(t, "2026-12-01T00:00:00"), chicago)
			if err != nil {
				t.Fatalf("ExpandEvents() error = %v", err)
			}
			if got := instanceStarts(instances); strings.Join(got, " ") != strings.Join(tc.want, " ") {
				t.Errorf("starts = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestExpandEvents_CancelledRecurringMasterIsOmitted(t *testing.T) {
	const raw = `BEGIN:VCALENDAR
VERSION:2.0
PRODID:-//test//test//EN
BEGIN:VEVENT
UID:cancelled-series@example.com
DTSTAMP:20261001T000000Z
DTSTART;TZID=America/Chicago:20261102T090000
DTEND;TZID=America/Chicago:20261102T093000
RRULE:FREQ=WEEKLY;BYDAY=MO
STATUS:CANCELLED
SUMMARY:Cancelled series
END:VEVENT
END:VCALENDAR
`
	instances, err := ExpandEvents("/cal/", "/cal/c.ics", parseEvents(t, raw),
		mustParse(t, "2026-11-01T00:00:00"), mustParse(t, "2026-12-01T00:00:00"), chicago)
	if err != nil {
		t.Fatalf("ExpandEvents() error = %v", err)
	}
	if len(instances) != 0 {
		t.Errorf("got %v, want no instances for a cancelled series", instanceStarts(instances))
	}
}
