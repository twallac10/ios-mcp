package icloud

import (
	"fmt"
	"sort"
	"time"

	ical "github.com/emersion/go-ical"
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
			rid, err := prop.DateTime(loc)
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

	ruleSet, err := master.RecurrenceSet(loc)
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

	masterStart, err := master.DateTimeStart(loc)
	if err != nil {
		return nil, fmt.Errorf("icloud: parsing master DTSTART: %w", err)
	}
	masterEnd, err := master.DateTimeEnd(loc)
	if err != nil {
		return nil, fmt.Errorf("icloud: parsing master DTEND: %w", err)
	}
	duration := masterEnd.Sub(masterStart)
	allDay := isAllDay(master)
	masterUID := textProp(master, ical.PropUID)
	masterTitle := textProp(master, ical.PropSummary)
	masterLocation := textProp(master, ical.PropLocation)
	masterNotes := textProp(master, ical.PropDescription)

	occurrences := ruleSet.Between(windowStart.Add(-expansionPad), windowEnd.Add(expansionPad), true)

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
		end := occStart.Add(duration)
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
	start, err := e.DateTimeStart(loc)
	if err != nil {
		return Instance{}, fmt.Errorf("icloud: parsing DTSTART: %w", err)
	}
	end, err := e.DateTimeEnd(loc)
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
