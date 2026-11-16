package icloud

import (
	"fmt"
	"sort"
	"strings"
	"time"

	ical "github.com/emersion/go-ical"
	"github.com/teambition/rrule-go"
)

// Instance is one concrete occurrence of a calendar event: either a
// non-recurring VEVENT, or one expanded instance of a recurring series.
type Instance struct {
	UID          string
	ObjectPath   string
	CalendarPath string
	Title        string
	Start        time.Time
	End          time.Time
	AllDay       bool
	Location     string
	Notes        string
	Recurring    bool
	// RecurrenceID identifies which instance of a recurring series this is
	// (the original, as-scheduled start time of the slot — RFC 5545's
	// RECURRENCE-ID). Nil for a non-recurring event.
	RecurrenceID *time.Time
}

// expansionPad widens the window used to generate candidate occurrences
// from a recurring series' RRULE. It exists so that an occurrence whose
// RECURRENCE-ID override moved it close to, but outside, the caller's
// requested window is still found and correctly excluded (or included) by
// the overlap check — rather than silently missing because the rule
// generator was never asked about that slot at all.
const expansionPad = 48 * time.Hour

// ExpandEvents turns the raw VEVENT components belonging to a single CalDAV
// calendar object — a recurring master plus zero or more RECURRENCE-ID
// overrides sharing its UID, or a single non-recurring VEVENT — into
// concrete Instances overlapping the half-open window [windowStart,
// windowEnd).
//
// Recurrence (RRULE/EXDATE, and RECURRENCE-ID overrides) is expanded here,
// in server code, deliberately: iCloud's server-side expansion
// (CalendarExpandRequest) handles exceptions inconsistently, so this
// queries the raw, unexpanded VEVENTs and expands them with
// go-ical/rrule-go instead.
//
// An error means this one calendar object couldn't be interpreted (e.g. an
// unparseable date); callers listing many objects should skip it and carry
// on rather than fail the whole listing. Known real-world quirks are
// tolerated rather than treated as errors: multi-value EXDATE/RDATE
// properties, and TZIDs that aren't IANA names (see resolveTZID).
func ExpandEvents(calendarPath, objectPath string, events []ical.Event, windowStart, windowEnd time.Time, loc *time.Location) ([]Instance, error) {
	if len(events) == 0 {
		return nil, nil
	}

	type overrideEntry struct {
		recurrenceID time.Time
		event        *ical.Event
	}

	var master *ical.Event
	overridesByKey := map[int64]overrideEntry{}
	for i := range events {
		e := &events[i]
		if prop := e.Props.Get(ical.PropRecurrenceID); prop != nil {
			rid, err := propDateTime(prop, loc)
			if err != nil {
				return nil, fmt.Errorf("icloud: parsing RECURRENCE-ID: %w", err)
			}
			overridesByKey[rid.UTC().Unix()] = overrideEntry{recurrenceID: rid, event: e}
		} else if master == nil {
			master = e
		}
	}

	if master == nil {
		// Every VEVENT here is an override with no master in sight (e.g. the
		// master was deleted but an exception survived server-side). Treat
		// each override as its own standalone instance.
		var out []Instance
		for _, entry := range overridesByKey {
			if isCancelled(entry.event) {
				continue
			}
			rid := entry.recurrenceID
			inst, err := instanceFromEvent(calendarPath, objectPath, entry.event, &rid, loc)
			if err != nil {
				return nil, err
			}
			if overlaps(inst.Start, inst.End, windowStart, windowEnd) {
				out = append(out, inst)
			}
		}
		sortInstances(out)
		return out, nil
	}

	masterStart, err := eventStart(master, loc)
	if err != nil {
		return nil, fmt.Errorf("icloud: parsing master DTSTART: %w", err)
	}

	ruleSet, err := recurrenceSet(master, masterStart, loc)
	if err != nil {
		return nil, fmt.Errorf("icloud: parsing recurrence rule: %w", err)
	}

	if ruleSet == nil {
		// Not a recurring series: a single instance, unless cancelled.
		if isCancelled(master) {
			return nil, nil
		}
		inst, err := instanceFromEvent(calendarPath, objectPath, master, nil, loc)
		if err != nil {
			return nil, err
		}
		if overlaps(inst.Start, inst.End, windowStart, windowEnd) {
			return []Instance{inst}, nil
		}
		return nil, nil
	}

	// The organizer cancelled the entire series.
	if isCancelled(master) {
		return nil, nil
	}

	masterEnd, err := eventEnd(master, masterStart, loc)
	if err != nil {
		return nil, fmt.Errorf("icloud: parsing master DTEND: %w", err)
	}
	duration := masterEnd.Sub(masterStart)
	allDay := isAllDay(master)
	// An all-day series' occurrences last a whole number of calendar days,
	// not a fixed duration: across a DST change, start+24h lands at 23:00 or
	// 01:00 local rather than the next midnight.
	allDayDays := calendarDaysBetween(masterStart, masterEnd)
	occurrenceEnd := func(occStart time.Time) time.Time {
		if allDay {
			return occStart.AddDate(0, 0, allDayDays)
		}
		return occStart.Add(duration)
	}
	masterUID := textProp(master, ical.PropUID)
	masterTitle := textProp(master, ical.PropSummary)
	masterLocation := textProp(master, ical.PropLocation)
	masterNotes := textProp(master, ical.PropDescription)

	// Between matches on occurrence *start*, so reach back by the event's
	// own length too: an occurrence that started before the window but is
	// still running inside it must be generated to be considered at all.
	occurrences := ruleSet.Between(windowStart.Add(-(expansionPad + duration)), windowEnd.Add(expansionPad), true)

	consumed := map[int64]bool{}
	var out []Instance
	for _, occStart := range occurrences {
		key := occStart.UTC().Unix()
		rid := occStart

		if entry, ok := overridesByKey[key]; ok {
			consumed[key] = true
			if isCancelled(entry.event) {
				continue // this occurrence was deleted from the series
			}
			inst, err := instanceFromEvent(calendarPath, objectPath, entry.event, &rid, loc)
			if err != nil {
				return nil, err
			}
			if overlaps(inst.Start, inst.End, windowStart, windowEnd) {
				out = append(out, inst)
			}
			continue
		}

		start := occStart
		end := occurrenceEnd(occStart)
		if overlaps(start, end, windowStart, windowEnd) {
			out = append(out, Instance{
				UID:          masterUID,
				ObjectPath:   objectPath,
				CalendarPath: calendarPath,
				Title:        masterTitle,
				Start:        start,
				End:          end,
				AllDay:       allDay,
				Location:     masterLocation,
				Notes:        masterNotes,
				Recurring:    true,
				RecurrenceID: &rid,
			})
		}
	}

	// An override whose RECURRENCE-ID wasn't produced by the rule within the
	// padded window (e.g. it was rescheduled far enough that its original
	// slot falls well outside [windowStart, windowEnd)) still needs
	// consideration using its own, possibly-moved start time.
	for key, entry := range overridesByKey {
		if consumed[key] || isCancelled(entry.event) {
			continue
		}
		rid := entry.recurrenceID
		inst, err := instanceFromEvent(calendarPath, objectPath, entry.event, &rid, loc)
		if err != nil {
			return nil, err
		}
		if overlaps(inst.Start, inst.End, windowStart, windowEnd) {
			out = append(out, inst)
		}
	}

	sortInstances(out)
	return out, nil
}

func instanceFromEvent(calendarPath, objectPath string, e *ical.Event, recurrenceID *time.Time, loc *time.Location) (Instance, error) {
	start, err := eventStart(e, loc)
	if err != nil {
		return Instance{}, fmt.Errorf("icloud: parsing DTSTART: %w", err)
	}
	end, err := eventEnd(e, start, loc)
	if err != nil {
		return Instance{}, fmt.Errorf("icloud: parsing DTEND: %w", err)
	}
	return Instance{
		UID:          textProp(e, ical.PropUID),
		ObjectPath:   objectPath,
		CalendarPath: calendarPath,
		Title:        textProp(e, ical.PropSummary),
		Start:        start,
		End:          end,
		AllDay:       isAllDay(e),
		Location:     textProp(e, ical.PropLocation),
		Notes:        textProp(e, ical.PropDescription),
		Recurring:    recurrenceID != nil,
		RecurrenceID: recurrenceID,
	}, nil
}

// recurrenceSet builds the rrule.Set for a recurring master, or returns nil
// if it has no RRULE.
//
// This replaces go-ical's Component.RecurrenceSet, which (a) parses the
// RRULE via rrule.StrToROption, reading a DATE or floating UNTIL as UTC
// rather than in DTSTART's zone, so the last occurrence of e.g. an all-day
// series ending UNTIL=<that date> is dropped; and (b) parses each EXDATE /
// RDATE property as a single timestamp, failing on RFC 5545's
// comma-separated multi-value form. Otherwise it assembles the set the same
// way (RRULE + DTSTART, then every EXDATE and RDATE).
func recurrenceSet(e *ical.Event, dtstart time.Time, loc *time.Location) (*rrule.Set, error) {
	prop := e.Props.Get(ical.PropRecurrenceRule)
	if prop == nil {
		return nil, nil
	}

	opt, err := rrule.StrToROptionInLocation(prop.Value, dtstart.Location())
	if err != nil {
		return nil, err
	}
	if until, ok := rruleUntilDate(prop.Value, dtstart.Location()); ok {
		// RFC 5545: a DATE-valued UNTIL includes that whole day.
		opt.Until = until.AddDate(0, 0, 1).Add(-time.Second)
	}
	opt.Dtstart = dtstart

	rule, err := rrule.NewRRule(*opt)
	if err != nil {
		return nil, err
	}

	set := &rrule.Set{}
	set.RRule(rule)
	set.DTStart(dtstart)

	exdates := e.Props.Values(ical.PropExceptionDates)
	for i := range exdates {
		times, err := propDateTimes(&exdates[i], loc)
		if err != nil {
			return nil, fmt.Errorf("parsing EXDATE: %w", err)
		}
		for _, t := range times {
			set.ExDate(t)
		}
	}
	rdates := e.Props.Values(ical.PropRecurrenceDates)
	for i := range rdates {
		times, err := propDateTimes(&rdates[i], loc)
		if err != nil {
			return nil, fmt.Errorf("parsing RDATE: %w", err)
		}
		for _, t := range times {
			set.RDate(t)
		}
	}
	return set, nil
}

// rruleUntilDate returns the UNTIL part of an RRULE value parsed as a
// local date in loc, if it is a DATE (YYYYMMDD) rather than a DATE-TIME.
func rruleUntilDate(rruleValue string, loc *time.Location) (time.Time, bool) {
	for _, part := range strings.Split(rruleValue, ";") {
		key, value, ok := strings.Cut(part, "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(key), "UNTIL") {
			continue
		}
		value = strings.TrimSpace(value)
		if len(value) != len("20060102") {
			return time.Time{}, false
		}
		t, err := time.ParseInLocation("20060102", value, loc)
		return t, err == nil
	}
	return time.Time{}, false
}

// eventStart returns the event's DTSTART (see propDateTime).
func eventStart(e *ical.Event, loc *time.Location) (time.Time, error) {
	prop := e.Props.Get(ical.PropDateTimeStart)
	if prop == nil {
		return time.Time{}, fmt.Errorf("missing DTSTART")
	}
	return propDateTime(prop, loc)
}

// eventEnd returns the event's non-inclusive end, mirroring go-ical's
// Event.DateTimeEnd (DTEND, else DTSTART+DURATION, else DTSTART plus one
// day for a DATE DTSTART or zero for a DATE-TIME one, per RFC 5545 section
// 3.6.1), but with propDateTime's tolerant parsing, and with the implied
// all-day length as one calendar day rather than 24h.
func eventEnd(e *ical.Event, start time.Time, loc *time.Location) (time.Time, error) {
	if prop := e.Props.Get(ical.PropDateTimeEnd); prop != nil {
		return propDateTime(prop, loc)
	}
	if prop := e.Props.Get(ical.PropDuration); prop != nil {
		dur, err := prop.Duration()
		if err != nil {
			return time.Time{}, err
		}
		return start.Add(dur), nil
	}
	if isAllDay(e) {
		return start.AddDate(0, 0, 1), nil
	}
	return start, nil
}

// propDateTime parses a single DATE or DATE-TIME property value like
// go-ical's Prop.DateTime, except that a TZID which isn't an IANA name is
// resolved via resolveTZID instead of failing.
func propDateTime(prop *ical.Prop, loc *time.Location) (time.Time, error) {
	tzid := prop.Params.Get(ical.PropTimezoneID)
	if tzid == "" {
		return prop.DateTime(loc)
	}
	if _, err := time.LoadLocation(tzid); err == nil {
		return prop.DateTime(loc)
	}
	// Re-parse without the unrecognised TZID, in its best-guess location.
	// Copy Params so the caller's property isn't modified.
	stripped := *prop
	stripped.Params = make(ical.Params, len(prop.Params))
	for k, v := range prop.Params {
		stripped.Params[k] = v
	}
	stripped.Params.Del(ical.PropTimezoneID)
	return stripped.DateTime(resolveTZID(tzid, loc))
}

// propDateTimes parses an EXDATE/RDATE property, whose value may be a
// comma-separated list of DATE or DATE-TIME values (RFC 5545 sections
// 3.8.5.1 and 3.8.5.2). An RDATE PERIOD value contributes its start.
func propDateTimes(prop *ical.Prop, loc *time.Location) ([]time.Time, error) {
	var out []time.Time
	for _, value := range strings.Split(prop.Value, ",") {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if start, _, isPeriod := strings.Cut(value, "/"); isPeriod {
			value = start
		}
		single := *prop
		single.Value = value
		if single.ValueType() == ical.ValuePeriod {
			single.Params = make(ical.Params, len(prop.Params))
			for k, v := range prop.Params {
				single.Params[k] = v
			}
			single.Params.Del(ical.ParamValue)
		}
		t, err := propDateTime(&single, loc)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

// windowsZones maps the Windows time zone IDs most often seen as TZIDs in
// invites from Exchange/Outlook to IANA names. Go's time.LoadLocation only
// knows IANA names, and go-ical never consults a calendar's VTIMEZONE.
var windowsZones = map[string]string{
	"Eastern Standard Time":          "America/New_York",
	"Central Standard Time":          "America/Chicago",
	"Mountain Standard Time":         "America/Denver",
	"US Mountain Standard Time":      "America/Phoenix",
	"Pacific Standard Time":          "America/Los_Angeles",
	"Alaskan Standard Time":          "America/Anchorage",
	"Hawaiian Standard Time":         "Pacific/Honolulu",
	"Atlantic Standard Time":         "America/Halifax",
	"Canada Central Standard Time":   "America/Regina",
	"Central America Standard Time":  "America/Guatemala",
	"Mexico Standard Time":           "America/Mexico_City",
	"Central Standard Time (Mexico)": "America/Mexico_City",
	"GMT Standard Time":              "Europe/London",
	"Greenwich Standard Time":        "Atlantic/Reykjavik",
	"W. Europe Standard Time":        "Europe/Berlin",
	"Romance Standard Time":          "Europe/Paris",
	"Central Europe Standard Time":   "Europe/Budapest",
	"Central European Standard Time": "Europe/Warsaw",
	"E. Europe Standard Time":        "Europe/Chisinau",
	"FLE Standard Time":              "Europe/Kiev",
	"India Standard Time":            "Asia/Kolkata",
	"China Standard Time":            "Asia/Shanghai",
	"Tokyo Standard Time":            "Asia/Tokyo",
	"AUS Eastern Standard Time":      "Australia/Sydney",
	"UTC":                            "UTC",
}

// resolveTZID maps a TZID that time.LoadLocation doesn't recognise to a
// location: a known Windows zone ID's IANA equivalent, else fallback (the
// server's configured default zone). The fallback keeps one oddly-labelled
// event from breaking a listing; a structurally valid local time in the
// user's own zone is the most likely intent for an unknown label.
func resolveTZID(tzid string, fallback *time.Location) *time.Location {
	name := strings.Trim(strings.TrimSpace(tzid), `"`)
	if iana, ok := windowsZones[name]; ok {
		if loc, err := time.LoadLocation(iana); err == nil {
			return loc
		}
	}
	if fallback == nil {
		return time.UTC
	}
	return fallback
}

// calendarDaysBetween counts the calendar days from start's date to end's
// date (each taken in its own location), ignoring time of day.
func calendarDaysBetween(start, end time.Time) int {
	s := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, time.UTC)
	e := time.Date(end.Year(), end.Month(), end.Day(), 0, 0, 0, 0, time.UTC)
	return int(e.Sub(s) / (24 * time.Hour))
}

// isAllDay reports whether an event's DTSTART is a DATE value (no
// time-of-day component), per RFC 5545 section 3.3.4.
func isAllDay(e *ical.Event) bool {
	prop := e.Props.Get(ical.PropDateTimeStart)
	if prop == nil {
		return false
	}
	switch prop.ValueType() {
	case ical.ValueDate:
		return true
	case ical.ValueDefault:
		return len(prop.Value) == len("20060102")
	default:
		return false
	}
}

func isCancelled(e *ical.Event) bool {
	status, err := e.Status()
	return err == nil && status == ical.EventCancelled
}

func textProp(e *ical.Event, name string) string {
	prop := e.Props.Get(name)
	if prop == nil {
		return ""
	}
	v, err := prop.Text()
	if err != nil {
		return prop.Value
	}
	return v
}

// overlaps reports whether the half-open interval [aStart, aEnd) intersects
// [bStart, bEnd).
func overlaps(aStart, aEnd, bStart, bEnd time.Time) bool {
	return aStart.Before(bEnd) && aEnd.After(bStart)
}

func sortInstances(instances []Instance) {
	sort.Slice(instances, func(i, j int) bool {
		return instances[i].Start.Before(instances[j].Start)
	})
}
