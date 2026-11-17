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
	// TZIDFallback is set to the raw TZID when one of the date-times this
	// instance's Start/End/RecurrenceID come from (DTSTART, DTEND,
	// RECURRENCE-ID, or the RDATE that added it) carried a TZID that
	// couldn't be resolved to a real time zone and was interpreted in the
	// default location instead (see zones.location), so Start/End may be
	// wrong. Empty normally; never set because of an EXDATE (see
	// Expansion.ExclusionFallbackTZIDs).
	TZIDFallback string
}

// expansionPad widens the window used to generate candidate occurrences
// from a recurring series' RRULE. It exists so that an occurrence whose
// RECURRENCE-ID override moved it close to, but outside, the caller's
// requested window is still found and correctly excluded (or included) by
// the overlap check — rather than silently missing because the rule
// generator was never asked about that slot at all.
const expansionPad = 48 * time.Hour

// Expansion is the result of expanding one CalDAV calendar object.
type Expansion struct {
	Instances []Instance
	// IgnoredMasters counts VEVENTs in the object that, like the series
	// master, have no RECURRENCE-ID but came after it, and so were ignored:
	// one calendar object should hold exactly one such VEVENT (RFC 4791
	// section 4.1), so only the first is used.
	IgnoredMasters int
	// FallbackTZIDs lists, once each, TZIDs on properties that determine
	// instances' times (DTSTART, DTEND, RECURRENCE-ID, RDATE) that are
	// neither IANA names, nor defined by a VTIMEZONE in the object that
	// could be interpreted, nor known Windows zone IDs, and were therefore
	// interpreted in the default location (see zones.location). Times in
	// affected instances may be wrong; each such Instance has TZIDFallback
	// set.
	FallbackTZIDs []string
	// ExclusionFallbackTZIDs is FallbackTZIDs for EXDATE properties. An
	// EXDATE only removes occurrences, never sets a displayed time, so no
	// Instance is flagged for it; but a guessed EXDATE time may fail to
	// match the occurrence it was meant to remove, leaving a deleted
	// occurrence listed. Callers should log it.
	ExclusionFallbackTZIDs []string
}

// window selects which instances an expansion returns: those overlapping
// the half-open time range [start, end); or, if unbounded, every instance
// regardless of time (see SingleEvent); or, if slot is set, only the
// occurrence of a recurring series whose RECURRENCE-ID is *slot, wherever
// in time it now falls (see Occurrence).
type window struct {
	start, end time.Time
	unbounded  bool
	slot       *time.Time
}

func (w window) includes(start, end time.Time) bool {
	return w.unbounded || w.slot != nil || overlaps(start, end, w.start, w.end)
}

// inSlot reports whether the occurrence keyed by key (its RECURRENCE-ID's
// Unix time) is one this window can return.
func (w window) inSlot(key int64) bool {
	return w.slot == nil || w.slot.UTC().Unix() == key
}

// ExpandEvents is Expand, returning only the instances.
func ExpandEvents(calendarPath, objectPath string, events []ical.Event, timezones []*ical.Component, windowStart, windowEnd time.Time, loc *time.Location) ([]Instance, error) {
	x, err := Expand(calendarPath, objectPath, events, timezones, windowStart, windowEnd, loc)
	return x.Instances, err
}

// Expand turns the raw VEVENT components belonging to a single CalDAV
// calendar object — a recurring master plus zero or more RECURRENCE-ID
// overrides sharing its UID, or a single non-recurring VEVENT — into
// concrete Instances overlapping the half-open window [windowStart,
// windowEnd), along with diagnostics about anything it had to guess at or
// ignore.
//
// timezones are the object's VTIMEZONE components (see Timezones), used to
// resolve TZIDs that aren't IANA names; loc is the default zone for
// floating times and for TZIDs nothing can resolve.
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
// properties, and TZIDs that aren't IANA names (see zones.location).
func Expand(calendarPath, objectPath string, events []ical.Event, timezones []*ical.Component, windowStart, windowEnd time.Time, loc *time.Location) (Expansion, error) {
	return expand(calendarPath, objectPath, events, timezones, window{start: windowStart, end: windowEnd}, loc)
}

// SingleEvent is Expand with no time window, for looking up a known
// non-recurring event wherever in time it falls: the object's non-recurring
// VEVENT (unless cancelled) is returned regardless of its date. A recurring
// series has unboundedly many occurrences, so for one only standalone
// overrides with no master are returned; look up one occurrence of a series
// with Occurrence.
func SingleEvent(calendarPath, objectPath string, events []ical.Event, timezones []*ical.Component, loc *time.Location) (Expansion, error) {
	return expand(calendarPath, objectPath, events, timezones, window{unbounded: true}, loc)
}

// Occurrence is Expand for looking up one known occurrence of a recurring
// series by its RECURRENCE-ID (its original, as-scheduled start): it
// returns that occurrence if the series has it — the RECURRENCE-ID override
// for that slot wherever in time the override moved it, else the rule's
// own occurrence at that slot unless it's excluded — and nothing otherwise.
func Occurrence(calendarPath, objectPath string, events []ical.Event, timezones []*ical.Component, recurrenceID time.Time, loc *time.Location) (Expansion, error) {
	return expand(calendarPath, objectPath, events, timezones, window{slot: &recurrenceID}, loc)
}

// Date-time properties, grouped by what a TZID guessed at on them affects
// (see Expansion.FallbackTZIDs and ExclusionFallbackTZIDs). RDATE is in
// neither per-event list: only the occurrences it adds are affected, and
// those are flagged individually (see recurrenceSet).
var (
	instanceTimeProps  = []string{ical.PropDateTimeStart, ical.PropDateTimeEnd, ical.PropRecurrenceID}
	instanceTZIDProps  = []string{ical.PropDateTimeStart, ical.PropDateTimeEnd, ical.PropRecurrenceID, ical.PropRecurrenceDates}
	exclusionTZIDProps = []string{ical.PropExceptionDates}
)

func expand(calendarPath, objectPath string, events []ical.Event, timezones []*ical.Component, win window, loc *time.Location) (Expansion, error) {
	var x Expansion
	if len(events) == 0 {
		return x, nil
	}

	z := newZones(loc, timezones)
	x.FallbackTZIDs = z.guessedTZIDs(events, instanceTZIDProps)
	x.ExclusionFallbackTZIDs = z.guessedTZIDs(events, exclusionTZIDProps)

	var master *ical.Event
	var overrides []*ical.Event
	for i := range events {
		e := &events[i]
		if e.Props.Get(ical.PropRecurrenceID) != nil {
			overrides = append(overrides, e)
		} else if master == nil {
			master = e
		} else {
			x.IgnoredMasters++
		}
	}

	// The organizer cancelled the event, or the entire series. Checked
	// before anything else about the master is parsed, so a cancelled
	// series is dropped even if its RRULE (say) wouldn't parse.
	if master != nil && isCancelled(master) {
		return x, nil
	}

	var masterStart time.Time
	// A RECURRENCE-ID without a TZID of its own names a slot of the
	// master's series, so it's read in the master's DTSTART zone, like
	// EXDATE/RDATE (see recurrenceSet).
	ridFloating := z.fallback
	if master != nil {
		var err error
		masterStart, err = z.eventStart(master)
		if err != nil {
			return Expansion{}, fmt.Errorf("icloud: parsing master DTSTART: %w", err)
		}
		ridFloating = masterStart.Location()
	}

	overridesByKey := map[int64]overrideEntry{}
	for _, e := range overrides {
		rid, err := z.dateTime(e.Props.Get(ical.PropRecurrenceID), ridFloating)
		if err != nil {
			return Expansion{}, fmt.Errorf("icloud: parsing RECURRENCE-ID: %w", err)
		}
		overridesByKey[rid.UTC().Unix()] = overrideEntry{recurrenceID: rid, event: e}
	}

	if master == nil {
		// Every VEVENT here is an override with no master in sight (e.g. the
		// master was deleted but an exception survived server-side). Treat
		// each override as its own standalone instance.
		for key, entry := range overridesByKey {
			if !win.inSlot(key) {
				continue
			}
			inst, ok, err := overrideInstance(calendarPath, objectPath, entry.event, entry.recurrenceID, win, z)
			if err != nil {
				return Expansion{}, err
			}
			if ok {
				x.Instances = append(x.Instances, inst)
			}
		}
		sortInstances(x.Instances)
		return x, nil
	}

	ruleSet, guessedRDates, err := recurrenceSet(master, masterStart, z)
	if err != nil {
		return Expansion{}, fmt.Errorf("icloud: parsing recurrence rule: %w", err)
	}

	if ruleSet == nil {
		// Not a recurring series: a single instance, which has no slots.
		if win.slot != nil {
			return x, nil
		}
		inst, err := instanceFromEvent(calendarPath, objectPath, master, nil, z)
		if err != nil {
			return Expansion{}, err
		}
		if win.includes(inst.Start, inst.End) {
			x.Instances = []Instance{inst}
		}
		return x, nil
	}

	if win.unbounded {
		// An unbounded window can't enumerate a recurring series.
		return x, nil
	}

	masterEnd, err := z.eventEnd(master, masterStart)
	if err != nil {
		return Expansion{}, fmt.Errorf("icloud: parsing master DTEND: %w", err)
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
	// Only the properties that set an occurrence's displayed time: a guessed
	// EXDATE TZID doesn't make every occurrence's Start/End suspect.
	masterTZIDFallback := z.eventGuessedTZID(master, instanceTimeProps)

	var occurrences []time.Time
	if win.slot != nil {
		occurrences = ruleSet.Between(*win.slot, *win.slot, true)
	} else {
		// Between matches on occurrence *start*, so reach back by the
		// event's own length too: an occurrence that started before the
		// window but is still running inside it must be generated to be
		// considered at all.
		occurrences = ruleSet.Between(win.start.Add(-(expansionPad + duration)), win.end.Add(expansionPad), true)
	}

	consumed := map[int64]bool{}
	for _, occStart := range occurrences {
		key := occStart.UTC().Unix()
		if !win.inSlot(key) {
			continue
		}
		rid := occStart

		if entry, ok := overridesByKey[key]; ok {
			consumed[key] = true
			inst, ok, err := overrideInstance(calendarPath, objectPath, entry.event, rid, win, z)
			if err != nil {
				return Expansion{}, err
			}
			if ok {
				x.Instances = append(x.Instances, inst)
			}
			continue
		}

		start := occStart
		end := occurrenceEnd(occStart)
		if win.includes(start, end) {
			tzidFallback := masterTZIDFallback
			if tzidFallback == "" {
				tzidFallback = guessedRDates[key]
			}
			x.Instances = append(x.Instances, Instance{
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
				TZIDFallback: tzidFallback,
			})
		}
	}

	// An override whose RECURRENCE-ID wasn't produced by the rule within the
	// padded window (e.g. it was rescheduled far enough that its original
	// slot falls well outside [windowStart, windowEnd)) still needs
	// consideration using its own, possibly-moved start time.
	for key, entry := range overridesByKey {
		if consumed[key] || !win.inSlot(key) {
			continue
		}
		inst, ok, err := overrideInstance(calendarPath, objectPath, entry.event, entry.recurrenceID, win, z)
		if err != nil {
			return Expansion{}, err
		}
		if ok {
			x.Instances = append(x.Instances, inst)
		}
	}

	sortInstances(x.Instances)
	return x, nil
}

// overrideEntry is a RECURRENCE-ID override VEVENT and its parsed
// RECURRENCE-ID.
type overrideEntry struct {
	recurrenceID time.Time
	event        *ical.Event
}

// overrideInstance turns a RECURRENCE-ID override VEVENT into the Instance
// for slot rid, reporting false if it was cancelled or falls outside win.
// (A slot window includes it wherever it moved: it was matched by its
// RECURRENCE-ID, which is what identifies it.)
func overrideInstance(calendarPath, objectPath string, e *ical.Event, rid time.Time, win window, z *zones) (Instance, bool, error) {
	if isCancelled(e) {
		return Instance{}, false, nil // this occurrence was deleted from the series
	}
	inst, err := instanceFromEvent(calendarPath, objectPath, e, &rid, z)
	if err != nil {
		return Instance{}, false, err
	}
	return inst, win.includes(inst.Start, inst.End), nil
}

func instanceFromEvent(calendarPath, objectPath string, e *ical.Event, recurrenceID *time.Time, z *zones) (Instance, error) {
	start, err := z.eventStart(e)
	if err != nil {
		return Instance{}, fmt.Errorf("icloud: parsing DTSTART: %w", err)
	}
	end, err := z.eventEnd(e, start)
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
		TZIDFallback: z.eventGuessedTZID(e, instanceTimeProps),
	}, nil
}

// recurrenceSet builds the rrule.Set for a recurring master, or returns nil
// if it has no RRULE. guessedRDates maps (by Unix time) each RDATE whose
// TZID could only be guessed at to that TZID, so the occurrences it adds
// can be flagged.
//
// This replaces go-ical's Component.RecurrenceSet, which (a) parses the
// RRULE via rrule.StrToROption, reading a DATE or floating UNTIL as UTC
// rather than in DTSTART's zone, so the last occurrence of e.g. an all-day
// series ending UNTIL=<that date> is dropped; and (b) parses each EXDATE /
// RDATE property as a single timestamp, failing on RFC 5545's
// comma-separated multi-value form. Otherwise it assembles the set the same
// way (RRULE + DTSTART, then every EXDATE and RDATE).
//
// EXDATE/RDATE values without a TZID of their own are read in DTSTART's
// zone, as the RRULE is: they name slots of this series (RFC 5545 wants
// them to match DTSTART's form), and exclusion is an exact-instant match,
// so reading a bare EXDATE in the server's default zone instead would
// silently fail to exclude anything whenever the series' zone differs.
func recurrenceSet(e *ical.Event, dtstart time.Time, z *zones) (set *rrule.Set, guessedRDates map[int64]string, err error) {
	prop := e.Props.Get(ical.PropRecurrenceRule)
	if prop == nil {
		return nil, nil, nil
	}
	seriesLoc := dtstart.Location()

	opt, err := rrule.StrToROptionInLocation(prop.Value, seriesLoc)
	if err != nil {
		return nil, nil, err
	}
	if until, ok := rruleUntilDate(prop.Value, seriesLoc); ok {
		// RFC 5545: a DATE-valued UNTIL includes that whole day.
		opt.Until = until.AddDate(0, 0, 1).Add(-time.Second)
	}
	opt.Dtstart = dtstart

	rule, err := rrule.NewRRule(*opt)
	if err != nil {
		return nil, nil, err
	}

	set = &rrule.Set{}
	set.RRule(rule)
	set.DTStart(dtstart)

	exdates := e.Props.Values(ical.PropExceptionDates)
	for i := range exdates {
		times, err := z.dateTimes(&exdates[i], seriesLoc)
		if err != nil {
			return nil, nil, fmt.Errorf("parsing EXDATE: %w", err)
		}
		for _, t := range times {
			set.ExDate(t)
		}
	}
	rdates := e.Props.Values(ical.PropRecurrenceDates)
	for i := range rdates {
		times, err := z.dateTimes(&rdates[i], seriesLoc)
		if err != nil {
			return nil, nil, fmt.Errorf("parsing RDATE: %w", err)
		}
		tzid := rdates[i].Params.Get(ical.PropTimezoneID)
		guessed := z.guesses(tzid)
		for _, t := range times {
			set.RDate(t)
			if guessed {
				if guessedRDates == nil {
					guessedRDates = map[int64]string{}
				}
				guessedRDates[t.UTC().Unix()] = tzid
			}
		}
	}
	return set, guessedRDates, nil
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

// eventStart returns the event's DTSTART (see zones.dateTime).
func (z *zones) eventStart(e *ical.Event) (time.Time, error) {
	prop := e.Props.Get(ical.PropDateTimeStart)
	if prop == nil {
		return time.Time{}, fmt.Errorf("missing DTSTART")
	}
	return z.dateTime(prop, z.fallback)
}

// eventEnd returns the event's non-inclusive end, mirroring go-ical's
// Event.DateTimeEnd (DTEND, else DTSTART+DURATION, else DTSTART plus one
// day for a DATE DTSTART or zero for a DATE-TIME one, per RFC 5545 section
// 3.6.1), but with zones.dateTime's tolerant parsing, and with the implied
// all-day length as one calendar day rather than 24h.
func (z *zones) eventEnd(e *ical.Event, start time.Time) (time.Time, error) {
	if prop := e.Props.Get(ical.PropDateTimeEnd); prop != nil {
		return z.dateTime(prop, z.fallback)
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

// dateTime parses a single DATE or DATE-TIME property value like go-ical's
// Prop.DateTime, reading a value with no TZID (a floating time or a DATE)
// in floating, except that a TZID which isn't an IANA name is resolved via
// zones.location instead of failing.
func (z *zones) dateTime(prop *ical.Prop, floating *time.Location) (time.Time, error) {
	tzid := prop.Params.Get(ical.PropTimezoneID)
	if tzid == "" {
		return prop.DateTime(floating)
	}
	if _, err := time.LoadLocation(tzid); err == nil {
		return prop.DateTime(floating)
	}
	// Re-parse without the unrecognised TZID, in its resolved location.
	// Copy Params so the caller's property isn't modified.
	stripped := *prop
	stripped.Params = make(ical.Params, len(prop.Params))
	for k, v := range prop.Params {
		stripped.Params[k] = v
	}
	stripped.Params.Del(ical.PropTimezoneID)
	loc, _ := z.location(tzid)
	return stripped.DateTime(loc)
}

// dateTimes parses an EXDATE/RDATE property, whose value may be a
// comma-separated list of DATE or DATE-TIME values (RFC 5545 sections
// 3.8.5.1 and 3.8.5.2). An RDATE PERIOD value contributes its start.
func (z *zones) dateTimes(prop *ical.Prop, floating *time.Location) ([]time.Time, error) {
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
		t, err := z.dateTime(&single, floating)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

// windowsZones maps the Windows time zone IDs most often seen as TZIDs in
// invites from Exchange/Outlook to IANA names. Go's time.LoadLocation only
// knows IANA names, and such invites' VTIMEZONEs are often too intricate
// for vtimezoneLocation.
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

func windowsZone(tzid string) (*time.Location, bool) {
	name := strings.Trim(strings.TrimSpace(tzid), `"`)
	if iana, ok := windowsZones[name]; ok {
		if loc, err := time.LoadLocation(iana); err == nil {
			return loc, true
		}
	}
	return nil, false
}

// eventGuessedTZID returns the first TZID on e's props (named in props)
// that zones can only guess at, or "".
func (z *zones) eventGuessedTZID(e *ical.Event, props []string) string {
	for _, name := range props {
		for _, prop := range e.Props.Values(name) {
			if tzid := prop.Params.Get(ical.PropTimezoneID); z.guesses(tzid) {
				return tzid
			}
		}
	}
	return ""
}

// guessedTZIDs returns, once each, every TZID on events' props (named in
// props) that zones can only guess at.
func (z *zones) guessedTZIDs(events []ical.Event, props []string) []string {
	var out []string
	seen := map[string]bool{}
	for i := range events {
		for _, name := range props {
			for _, prop := range events[i].Props.Values(name) {
				tzid := prop.Params.Get(ical.PropTimezoneID)
				if z.guesses(tzid) && !seen[tzid] {
					seen[tzid] = true
					out = append(out, tzid)
				}
			}
		}
	}
	return out
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

// overlaps reports whether the event [aStart, aEnd) intersects the
// half-open window [bStart, bEnd). A zero-duration event (aStart == aEnd,
// e.g. a timed VEVENT with neither DTEND nor DURATION) is an instant, and
// is included when bStart <= aStart < bEnd — so one exactly at the
// window's inclusive start counts, matching RFC 4791 section 9.9's
// time-range test for such events.
func overlaps(aStart, aEnd, bStart, bEnd time.Time) bool {
	return aStart.Before(bEnd) && (aEnd.After(bStart) || !aStart.Before(bStart))
}

func sortInstances(instances []Instance) {
	sort.Slice(instances, func(i, j int) bool {
		return instances[i].Start.Before(instances[j].Start)
	})
}
