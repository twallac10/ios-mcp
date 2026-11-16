package icloud

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"
	"time"

	ical "github.com/emersion/go-ical"
)

// Timezones returns the VTIMEZONE components of a calendar object, which
// Calendar.Events drops. Pass them to Expand / SingleEvent / Occurrence so
// a TZID that isn't an IANA name can be resolved against the object's own
// definition of it.
func Timezones(cal *ical.Calendar) []*ical.Component {
	if cal == nil || cal.Component == nil {
		return nil
	}
	var out []*ical.Component
	for _, child := range cal.Children {
		if child.Name == ical.CompTimezone {
			out = append(out, child)
		}
	}
	return out
}

// zones resolves the TZIDs found in one calendar object's date-times.
type zones struct {
	// fallback is the server's configured default zone: where floating
	// times are read by default, and where a TZID nothing else can resolve
	// is (guessingly) read.
	fallback *time.Location
	// custom maps a TZID to a location built from the object's own
	// VTIMEZONE of that TZID (see vtimezoneLocation).
	custom map[string]*time.Location
}

func newZones(fallback *time.Location, timezones []*ical.Component) *zones {
	if fallback == nil {
		fallback = time.UTC
	}
	z := &zones{fallback: fallback, custom: map[string]*time.Location{}}
	for _, tz := range timezones {
		if tz == nil || tz.Name != ical.CompTimezone {
			continue
		}
		prop := tz.Props.Get(ical.PropTimezoneID)
		if prop == nil || prop.Value == "" {
			continue
		}
		if loc, ok := vtimezoneLocation(prop.Value, tz); ok {
			z.custom[normalizeTZID(prop.Value)] = loc
		}
	}
	return z
}

func normalizeTZID(tzid string) string {
	return strings.Trim(strings.TrimSpace(tzid), `"`)
}

// location returns the location a date-time with this (non-IANA) TZID is
// read in, in order of preference: the object's own VTIMEZONE for it, a
// known Windows zone ID's IANA equivalent, else the fallback zone. guessed
// reports the last case, where the result is only the most likely intent
// (a structurally valid local time in the user's own zone) and times may
// be wrong; it's reported via Expansion.FallbackTZIDs and
// Instance.TZIDFallback.
func (z *zones) location(tzid string) (loc *time.Location, guessed bool) {
	if loc, ok := z.custom[normalizeTZID(tzid)]; ok {
		return loc, false
	}
	if loc, ok := windowsZone(tzid); ok {
		return loc, false
	}
	return z.fallback, true
}

// guesses reports whether a date-time with this TZID would be read in the
// fallback location because nothing could resolve the TZID.
func (z *zones) guesses(tzid string) bool {
	if tzid == "" {
		return false
	}
	if _, err := time.LoadLocation(tzid); err == nil {
		return false
	}
	_, guessed := z.location(tzid)
	return guessed
}

// vtimezoneLocation builds a *time.Location from a VTIMEZONE, for the two
// shapes custom (non-IANA) VTIMEZONEs from sync/export tools such as
// Outlook/Exchange nearly always take:
//
//   - a single observance (one STANDARD or DAYLIGHT, or two with the same
//     TZOFFSETTO): a fixed offset, TZOFFSETTO;
//   - exactly one STANDARD and one DAYLIGHT, each with an annual rule of the
//     form FREQ=YEARLY;BYMONTH=<m>;BYDAY=<n><weekday> (n = 1..4, or -1 for
//     the last): a zone with that DST rule, applied to every year.
//
// Anything else — several observances of a kind (a zone whose rules changed
// over the years), rules with UNTIL/COUNT/BYMONTHDAY/BYSETPOS, RDATE-only
// observances — reports false, so the TZID falls through to the Windows
// zone table / server default and is flagged as a guess, rather than being
// resolved confidently but possibly wrongly. Also a known approximation:
// observance DTSTARTs are ignored, so the rule (or fixed offset) is applied
// to all of time, including years before the VTIMEZONE says it began.
func vtimezoneLocation(tzid string, tz *ical.Component) (*time.Location, bool) {
	var std, dst []*ical.Component
	for _, child := range tz.Children {
		switch child.Name {
		case ical.CompTimezoneStandard:
			std = append(std, child)
		case ical.CompTimezoneDaylight:
			dst = append(dst, child)
		}
	}

	if len(std)+len(dst) == 1 {
		obs := dst
		if len(std) == 1 {
			obs = std
		}
		off, ok := utcOffsetProp(obs[0], ical.PropTimezoneOffsetTo)
		if !ok {
			return nil, false
		}
		return time.FixedZone(tzid, off), true
	}
	if len(std) != 1 || len(dst) != 1 {
		return nil, false
	}

	stdOff, ok1 := utcOffsetProp(std[0], ical.PropTimezoneOffsetTo)
	dstOff, ok2 := utcOffsetProp(dst[0], ical.PropTimezoneOffsetTo)
	if !ok1 || !ok2 {
		return nil, false
	}
	if stdOff == dstOff {
		return time.FixedZone(tzid, stdOff), true
	}
	toDST, ok1 := posixTransition(dst[0])
	toSTD, ok2 := posixTransition(std[0])
	if !ok1 || !ok2 {
		return nil, false
	}

	// Go can only build a rule-based Location from TZif data, so encode the
	// rule as a POSIX TZ string in the footer of an otherwise empty TZif v2
	// file: with no transitions, Go applies the footer rule to all times.
	posix := fmt.Sprintf("STD%sDST%s,%s,%s", posixOffset(stdOff), posixOffset(dstOff), toDST, toSTD)
	loc, err := time.LoadLocationFromTZData(tzid, tzifWithRule(posix, stdOff))
	if err != nil || !observesOffsets(loc, stdOff, dstOff) {
		return nil, false
	}
	return loc, true
}

// utcOffsetProp parses a TZOFFSETFROM/TZOFFSETTO value ("+HHMM",
// "-HHMM" or "+HHMMSS") into seconds east of UTC.
func utcOffsetProp(c *ical.Component, name string) (int, bool) {
	prop := c.Props.Get(name)
	if prop == nil {
		return 0, false
	}
	v := strings.TrimSpace(prop.Value)
	if len(v) != 5 && len(v) != 7 {
		return 0, false
	}
	sign := 1
	switch v[0] {
	case '+':
	case '-':
		sign = -1
	default:
		return 0, false
	}
	parts := []int{}
	for i := 1; i < len(v); i += 2 {
		n, err := strconv.Atoi(v[i : i+2])
		if err != nil {
			return 0, false
		}
		parts = append(parts, n)
	}
	secs := parts[0]*3600 + parts[1]*60
	if len(parts) == 3 {
		secs += parts[2]
	}
	if parts[0] > 23 || parts[1] > 59 {
		return 0, false
	}
	return sign * secs, true
}

var posixWeekdays = map[string]int{"SU": 0, "MO": 1, "TU": 2, "WE": 3, "TH": 4, "FR": 5, "SA": 6}

// posixTransition renders an observance's annual onset as a POSIX TZ rule
// ("Mm.w.d/hh:mm:ss"), if its RRULE is the simple
// FREQ=YEARLY;BYMONTH=m;BYDAY=<n><weekday> form. The time of day is
// DTSTART's, which RFC 5545 gives in local time before the transition —
// the same convention POSIX uses.
func posixTransition(obs *ical.Component) (string, bool) {
	rule := obs.Props.Get(ical.PropRecurrenceRule)
	start := obs.Props.Get(ical.PropDateTimeStart)
	if rule == nil || start == nil {
		return "", false
	}
	// DTSTART is a local DATE-TIME, e.g. 19700308T020000.
	_, clock, ok := strings.Cut(start.Value, "T")
	if !ok || len(clock) < 6 {
		return "", false
	}
	hh, err1 := strconv.Atoi(clock[0:2])
	mm, err2 := strconv.Atoi(clock[2:4])
	ss, err3 := strconv.Atoi(clock[4:6])
	if err1 != nil || err2 != nil || err3 != nil {
		return "", false
	}

	var month, week, weekday = 0, 0, -1
	freqYearly := false
	for _, part := range strings.Split(rule.Value, ";") {
		key, value, _ := strings.Cut(part, "=")
		key, value = strings.ToUpper(strings.TrimSpace(key)), strings.ToUpper(strings.TrimSpace(value))
		switch key {
		case "FREQ":
			freqYearly = value == "YEARLY"
		case "INTERVAL":
			if value != "1" {
				return "", false
			}
		case "WKST":
		case "BYMONTH":
			m, err := strconv.Atoi(value)
			if err != nil || m < 1 || m > 12 {
				return "", false
			}
			month = m
		case "BYDAY":
			if len(value) < 3 {
				return "", false
			}
			d, ok := posixWeekdays[value[len(value)-2:]]
			if !ok {
				return "", false
			}
			n, err := strconv.Atoi(strings.TrimPrefix(value[:len(value)-2], "+"))
			if err != nil {
				return "", false
			}
			switch {
			case n >= 1 && n <= 4:
				week = n
			case n == -1:
				week = 5 // POSIX: the last such weekday of the month
			default:
				return "", false
			}
			weekday = d
		default:
			// UNTIL, COUNT, BYMONTHDAY, BYSETPOS, ...: not this simple form.
			return "", false
		}
	}
	if !freqYearly || month == 0 || week == 0 || weekday < 0 {
		return "", false
	}
	return fmt.Sprintf("M%d.%d.%d/%02d:%02d:%02d", month, week, weekday, hh, mm, ss), true
}

// posixOffset renders seconds east of UTC as a POSIX TZ offset, which counts
// hours west of UTC.
func posixOffset(eastSecs int) string {
	west := -eastSecs
	sign := ""
	if west < 0 {
		sign, west = "-", -west
	}
	return fmt.Sprintf("%s%d:%02d:%02d", sign, west/3600, west/60%60, west%60)
}

// tzifWithRule returns a TZif version 2 file with no transitions, one local
// time type (standard time) and posix as its footer rule.
func tzifWithRule(posix string, stdOff int) []byte {
	var b bytes.Buffer
	for range 2 { // the v1 block, then the v2 (64-bit) block; identical here
		b.WriteString("TZif2")
		b.Write(make([]byte, 15))
		// isutcnt, isstdcnt, leapcnt, timecnt, typecnt, charcnt
		for _, n := range []uint32{0, 0, 0, 0, 1, 4} {
			b.Write(binary.BigEndian.AppendUint32(nil, n))
		}
		b.Write(binary.BigEndian.AppendUint32(nil, uint32(int32(stdOff))))
		b.WriteByte(0) // isdst
		b.WriteByte(0) // abbreviation index
		b.WriteString("STD\x00")
	}
	b.WriteString("\n" + posix + "\n")
	return b.Bytes()
}

// observesOffsets sanity-checks a rule-based location: over a sample year
// both the standard and the daylight offset must actually occur. (Go
// silently ignores a footer rule it can't parse.)
func observesOffsets(loc *time.Location, offsets ...int) bool {
	seen := map[int]bool{}
	for m := time.January; m <= time.December; m++ {
		_, off := time.Date(2026, m, 15, 12, 0, 0, 0, time.UTC).In(loc).Zone()
		seen[off] = true
	}
	for _, off := range offsets {
		if !seen[off] {
			return false
		}
	}
	return true
}
