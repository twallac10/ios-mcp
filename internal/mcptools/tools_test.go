package mcptools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	ical "github.com/emersion/go-ical"
	"github.com/emersion/go-webdav/caldav"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/twallac10/ios-mcp/internal/config"
	"github.com/twallac10/ios-mcp/internal/credentials"
	"github.com/twallac10/ios-mcp/internal/icloud"
)

var chicago = func() *time.Location {
	loc, err := time.LoadLocation("America/Chicago")
	if err != nil {
		panic(err)
	}
	return loc
}()

// fakeClient is an in-memory calendarClient.
type fakeClient struct {
	calendars []icloud.Calendar
	// objects maps a calendar path to the objects FetchObjects returns, and
	// is also searched by GetObject.
	objects map[string][]caldav.CalendarObject
	// fetchErrs makes FetchObjects fail for the given calendar paths.
	fetchErrs map[string]error
	// fetchErrsOnce is fetchErrs for only the first FetchObjects call per
	// calendar path.
	fetchErrsOnce map[string]error
	// fetchDelay makes each FetchObjects call take this long.
	fetchDelay time.Duration

	// mu guards the fields below, which FetchObjects (called
	// concurrently) updates.
	mu          sync.Mutex
	fetchCounts map[string]int
	inFlight    int
	maxInFlight int
	listCalls   int

	getCalls    []string
	createCalls []icloud.EventDraft
	// createErrs is consumed one per CreateEvent call; nil or exhausted
	// means success.
	createErrs []error
}

func (f *fakeClient) ListCalendars(context.Context) ([]icloud.Calendar, error) {
	f.mu.Lock()
	f.listCalls++
	f.mu.Unlock()
	return f.calendars, nil
}

func (f *fakeClient) FetchObjects(_ context.Context, calendarPath string, _, _ time.Time) ([]caldav.CalendarObject, error) {
	f.mu.Lock()
	if f.fetchCounts == nil {
		f.fetchCounts = map[string]int{}
	}
	f.fetchCounts[calendarPath]++
	first := f.fetchCounts[calendarPath] == 1
	f.inFlight++
	f.maxInFlight = max(f.maxInFlight, f.inFlight)
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.inFlight--
		f.mu.Unlock()
	}()

	time.Sleep(f.fetchDelay)
	if err := f.fetchErrsOnce[calendarPath]; err != nil && first {
		return nil, err
	}
	if err := f.fetchErrs[calendarPath]; err != nil {
		return nil, err
	}
	return f.objects[calendarPath], nil
}

func (f *fakeClient) GetObject(_ context.Context, objectPath string) (*caldav.CalendarObject, error) {
	f.getCalls = append(f.getCalls, objectPath)
	for _, objs := range f.objects {
		for i := range objs {
			if objs[i].Path == objectPath {
				return &objs[i], nil
			}
		}
	}
	return nil, fmt.Errorf("404 Not Found")
}

func (f *fakeClient) CreateEvent(_ context.Context, calendarPath string, draft icloud.EventDraft) (icloud.Instance, error) {
	f.createCalls = append(f.createCalls, draft)
	if len(f.createErrs) > 0 {
		err := f.createErrs[0]
		f.createErrs = f.createErrs[1:]
		if err != nil {
			return icloud.Instance{}, err
		}
	}
	return icloud.Instance{
		UID:          draft.UID,
		CalendarPath: calendarPath,
		ObjectPath:   strings.TrimSuffix(calendarPath, "/") + "/" + draft.UID + ".ics",
		Title:        draft.Title,
		Start:        draft.Start,
		End:          draft.End,
	}, nil
}

type connectCall struct{ refresh bool }

func newTestService(fake *fakeClient, allow ...string) (*Service, *[]connectCall) {
	var calls []connectCall
	svc := &Service{
		cfg: &config.Config{CalendarAllowlist: allow, DefaultLocation: chicago},
		log: slog.New(slog.DiscardHandler),
		connect: func(_ context.Context, refresh bool) (calendarClient, error) {
			calls = append(calls, connectCall{refresh: refresh})
			return fake, nil
		},
	}
	return svc, &calls
}

func object(t *testing.T, path, ics string) caldav.CalendarObject {
	t.Helper()
	cal, err := ical.NewDecoder(strings.NewReader(ics)).Decode()
	if err != nil {
		t.Fatalf("decoding fixture for %s: %v", path, err)
	}
	return caldav.CalendarObject{Path: path, Data: cal}
}

func singleEventICS(uid, dtstart, dtend string) string {
	return "BEGIN:VCALENDAR\nVERSION:2.0\nPRODID:-//test//test//EN\nBEGIN:VEVENT\nUID:" + uid +
		"\nDTSTAMP:20261001T000000Z\n" + dtstart + "\n" + dtend + "\nSUMMARY:" + uid + "\nEND:VEVENT\nEND:VCALENDAR\n"
}

func TestIsAuthError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"sentinel", icloud.ErrUnauthorized, true},
		{"wrapped sentinel", fmt.Errorf("icloud: querying calendar %q: %w", "/cal/", icloud.ErrUnauthorized), true},
		{"401 in a path", errors.New(`icloud: fetching calendar object "/1234014019/calendars/home/401.ics": 404 Not Found`), false},
		{"401 Unauthorized text only", errors.New("401 Unauthorized"), false},
	}
	for _, tc := range cases {
		if got := isAuthError(tc.err); got != tc.want {
			t.Errorf("%s: isAuthError(%v) = %v, want %v", tc.name, tc.err, got, tc.want)
		}
	}
}

func TestMatchesEventID(t *testing.T) {
	rid := time.Date(2026, 11, 16, 9, 0, 0, 0, chicago)
	sameInstantUTC := rid.UTC()
	other := rid.AddDate(0, 0, 7)

	single := icloud.Instance{UID: "a"}
	occurrence := icloud.Instance{UID: "a", RecurrenceID: &rid}

	cases := []struct {
		name string
		inst icloud.Instance
		id   icloud.EventID
		want bool
	}{
		{"non-recurring match", single, icloud.EventID{UID: "a"}, true},
		{"UID mismatch", single, icloud.EventID{UID: "b"}, false},
		{"id wants single, got occurrence", occurrence, icloud.EventID{UID: "a"}, false},
		{"id wants occurrence, got single", single, icloud.EventID{UID: "a", RecurrenceID: &rid}, false},
		{"occurrence match across zones", occurrence, icloud.EventID{UID: "a", RecurrenceID: &sameInstantUTC}, true},
		{"different occurrence", occurrence, icloud.EventID{UID: "a", RecurrenceID: &other}, false},
	}
	for _, tc := range cases {
		if got := matchesEventID(tc.inst, tc.id); got != tc.want {
			t.Errorf("%s: matchesEventID = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestObjectInCalendar(t *testing.T) {
	cases := []struct {
		object, calendar string
		want             bool
	}{
		{"/1/calendars/home/a.ics", "/1/calendars/home/", true},
		{"/1/calendars/home/a.ics", "/1/calendars/home", true},
		{"/1/calendars/secret/a.ics", "/1/calendars/home/", false},
		{"/1/calendars/home/../secret/a.ics", "/1/calendars/home/", false},
		{"/1/calendars/home/sub/a.ics", "/1/calendars/home/", false},
		{"/1/calendars/homeX/a.ics", "/1/calendars/home/", false},
		{"https://evil.example/1/calendars/home/a.ics", "/1/calendars/home/", false},
		{"", "/1/calendars/home/", false},
	}
	for _, tc := range cases {
		if got := objectInCalendar(tc.object, tc.calendar); got != tc.want {
			t.Errorf("objectInCalendar(%q, %q) = %v, want %v", tc.object, tc.calendar, got, tc.want)
		}
	}
}

func getEventFixture(t *testing.T) *fakeClient {
	ics := func(uid string) string {
		return singleEventICS(uid, "DTSTART:20261117T150000Z", "DTEND:20261117T160000Z")
	}
	return &fakeClient{
		calendars: []icloud.Calendar{
			{Path: "/1/calendars/home/", Name: "Home"},
			{Path: "/1/calendars/secret/", Name: "Secret"},
		},
		objects: map[string][]caldav.CalendarObject{
			"/1/calendars/home/":   {object(t, "/1/calendars/home/h.ics", ics("home-event"))},
			"/1/calendars/secret/": {object(t, "/1/calendars/secret/s.ics", ics("secret-event"))},
		},
	}
}

func TestGetEvent_AllowedEvent(t *testing.T) {
	fake := getEventFixture(t)
	svc, _ := newTestService(fake, "Home")

	id := icloud.EventID{CalendarPath: "/1/calendars/home/", ObjectPath: "/1/calendars/home/h.ics", UID: "home-event"}.Encode()
	_, out, err := svc.getEvent(context.Background(), nil, GetEventArgs{ID: id})
	if err != nil {
		t.Fatalf("getEvent: %v", err)
	}
	if out.Calendar != "Home" || out.Title != "home-event" {
		t.Errorf("getEvent = %+v, want the Home event", out)
	}
}

func TestGetEvent_RejectsObjectFromAnotherCalendar(t *testing.T) {
	ids := map[string]icloud.EventID{
		"allowed calendar, other calendar's object": {
			CalendarPath: "/1/calendars/home/", ObjectPath: "/1/calendars/secret/s.ics", UID: "secret-event",
		},
		"dot-dot traversal": {
			CalendarPath: "/1/calendars/home/", ObjectPath: "/1/calendars/home/../secret/s.ics", UID: "secret-event",
		},
		"non-allowlisted calendar": {
			CalendarPath: "/1/calendars/secret/", ObjectPath: "/1/calendars/secret/s.ics", UID: "secret-event",
		},
		"calendar not in account": {
			CalendarPath: "/1/calendars/made-up/", ObjectPath: "/1/calendars/made-up/s.ics", UID: "secret-event",
		},
	}
	for name, id := range ids {
		t.Run(name, func(t *testing.T) {
			fake := getEventFixture(t)
			svc, _ := newTestService(fake, "Home")

			_, out, err := svc.getEvent(context.Background(), nil, GetEventArgs{ID: id.Encode()})
			if !errors.Is(err, errEventNotAllowed) {
				t.Errorf("getEvent err = %v (out %+v), want errEventNotAllowed", err, out)
			}
			if len(fake.getCalls) != 0 {
				t.Errorf("object was fetched (%v) before the allowlist check rejected it", fake.getCalls)
			}
		})
	}
}

func TestCreateEvent_RetryAfter401ReusesUID(t *testing.T) {
	fake := &fakeClient{
		calendars:  []icloud.Calendar{{Path: "/1/calendars/home/", Name: "Home"}},
		createErrs: []error{fmt.Errorf("icloud: creating event: %w", icloud.ErrUnauthorized)},
	}
	svc, connects := newTestService(fake, "Home")

	_, out, err := svc.createEvent(context.Background(), nil, CreateEventArgs{
		Calendar: "Home",
		Title:    "Dentist",
		Start:    "2026-11-17T09:00:00-06:00",
		End:      "2026-11-17T10:00:00-06:00",
	})
	if err != nil {
		t.Fatalf("createEvent: %v", err)
	}
	if len(*connects) != 2 || (*connects)[0].refresh || !(*connects)[1].refresh {
		t.Errorf("connect calls = %+v, want a normal connect then one refreshed reconnect", *connects)
	}
	if len(fake.createCalls) != 2 {
		t.Fatalf("CreateEvent called %d times, want 2 (initial + one retry)", len(fake.createCalls))
	}
	if a, b := fake.createCalls[0].UID, fake.createCalls[1].UID; a == "" || a != b {
		t.Errorf("retry used UID %q, first attempt %q; want the same non-empty UID", b, a)
	}
	if out.Title != "Dentist" || out.Calendar != "Home" {
		t.Errorf("createEvent result = %+v", out)
	}
}

func TestWithClient_NonAuthErrorIsNotRetried(t *testing.T) {
	fake := &fakeClient{}
	svc, connects := newTestService(fake)
	calls := 0
	_, err := withClient(context.Background(), svc, func(calendarClient) (int, error) {
		calls++
		return 0, errors.New(`fetching "/401/x.ics": 404 Not Found`)
	})
	if err == nil || calls != 1 || len(*connects) != 1 {
		t.Errorf("err=%v calls=%d connects=%d, want error, 1 call, 1 connect", err, calls, len(*connects))
	}
}

func TestListEvents_SkipsUnparseableObjectAndSortsByInstant(t *testing.T) {
	fake := &fakeClient{
		calendars: []icloud.Calendar{
			{Path: "/1/calendars/home/", Name: "Home"},
			{Path: "/1/calendars/work/", Name: "Work"},
		},
		objects: map[string][]caldav.CalendarObject{
			// 09:30 Chicago, rendered "...T09:30:00-06:00".
			"/1/calendars/home/": {
				object(t, "/1/calendars/home/b.ics", singleEventICS("later",
					"DTSTART;TZID=America/Chicago:20261117T093000", "DTEND;TZID=America/Chicago:20261117T100000")),
				object(t, "/1/calendars/home/broken.ics", singleEventICS("broken",
					"DTSTART:garbage", "DTEND:garbage")),
			},
			// 15:00Z = 09:00 Chicago, rendered "...T15:00:00Z": earlier as an
			// instant, but sorts after "...T09:30:00-06:00" as a string.
			"/1/calendars/work/": {
				object(t, "/1/calendars/work/a.ics", singleEventICS("earlier",
					"DTSTART:20261117T150000Z", "DTEND:20261117T153000Z")),
			},
		},
	}
	svc, _ := newTestService(fake, "Home", "Work")

	_, out, err := svc.listEvents(context.Background(), nil, ListEventsArgs{
		Start: "2026-11-17T00:00:00-06:00",
		End:   "2026-11-18T00:00:00-06:00",
	})
	if err != nil {
		t.Fatalf("listEvents: %v (one bad object must not fail the call)", err)
	}

	var titles []string
	for _, e := range out.Events {
		titles = append(titles, e.Title)
	}
	if strings.Join(titles, ",") != "earlier,later" {
		t.Errorf("event order = %v, want [earlier later] (chronological)", titles)
	}
	if len(out.Warnings) != 1 || !strings.Contains(out.Warnings[0], `"Home"`) {
		t.Errorf("warnings = %v, want one about the skipped Home entry", out.Warnings)
	}
	for _, w := range out.Warnings {
		if strings.Contains(w, "broken") {
			t.Errorf("warning %q leaks event content", w)
		}
	}
}

func TestGetEvent_NonRecurringEventFarFromNow(t *testing.T) {
	// More than a year from now in either direction: outside any window
	// anchored on the current time.
	for _, start := range []time.Time{time.Now().AddDate(1, 2, 0), time.Now().AddDate(-1, -2, 0)} {
		dtstart := start.UTC().Format("20060102T150405Z")
		dtend := start.Add(time.Hour).UTC().Format("20060102T150405Z")
		fake := &fakeClient{
			calendars: []icloud.Calendar{{Path: "/1/calendars/home/", Name: "Home"}},
			objects: map[string][]caldav.CalendarObject{
				"/1/calendars/home/": {object(t, "/1/calendars/home/far.ics", singleEventICS("far", "DTSTART:"+dtstart, "DTEND:"+dtend))},
			},
		}
		svc, _ := newTestService(fake, "Home")

		id := icloud.EventID{CalendarPath: "/1/calendars/home/", ObjectPath: "/1/calendars/home/far.ics", UID: "far"}.Encode()
		_, out, err := svc.getEvent(context.Background(), nil, GetEventArgs{ID: id})
		if err != nil {
			t.Fatalf("getEvent(event at %s): %v", dtstart, err)
		}
		if out.Title != "far" || out.Start != start.Truncate(time.Second).UTC().Format(time.RFC3339) {
			t.Errorf("getEvent(event at %s) = %+v", dtstart, out)
		}
	}
}

func TestGetEvent_UnrecognizedTZIDIsFlagged(t *testing.T) {
	fake := &fakeClient{
		calendars: []icloud.Calendar{{Path: "/1/calendars/home/", Name: "Home"}},
		objects: map[string][]caldav.CalendarObject{
			"/1/calendars/home/": {object(t, "/1/calendars/home/tz.ics", singleEventICS("tz",
				"DTSTART;TZID=Customized Time Zone 1:20261117T090000", "DTEND;TZID=Customized Time Zone 1:20261117T100000"))},
		},
	}
	var logs strings.Builder
	svc, _ := newTestService(fake, "Home")
	svc.log = slog.New(slog.NewTextHandler(&logs, nil))

	id := icloud.EventID{CalendarPath: "/1/calendars/home/", ObjectPath: "/1/calendars/home/tz.ics", UID: "tz"}.Encode()
	_, out, err := svc.getEvent(context.Background(), nil, GetEventArgs{ID: id})
	if err != nil {
		t.Fatalf("getEvent: %v", err)
	}
	if !strings.Contains(out.TimezoneWarning, "Customized Time Zone 1") || !strings.Contains(out.TimezoneWarning, "America/Chicago") {
		t.Errorf("TimezoneWarning = %q, want it to name the TZID and the fallback zone", out.TimezoneWarning)
	}
	if !strings.Contains(logs.String(), `tzid="Customized Time Zone 1"`) || !strings.Contains(logs.String(), "fallback_zone=America/Chicago") {
		t.Errorf("no TZID fallback warning logged:\n%s", logs.String())
	}
}

func TestCreateEvent_AmbiguousCalendarName(t *testing.T) {
	fake := &fakeClient{
		calendars: []icloud.Calendar{
			{Path: "/1/calendars/family-mine/", Name: "Family"},
			{Path: "/1/calendars/family-shared/", Name: "Family"},
		},
	}
	svc, _ := newTestService(fake, "Family")

	_, _, err := svc.createEvent(context.Background(), nil, CreateEventArgs{
		Calendar: "Family",
		Title:    "Dinner",
		Start:    "2026-11-17T18:00:00-06:00",
		End:      "2026-11-17T19:00:00-06:00",
	})
	if !errors.Is(err, errAmbiguousCalendar) || !strings.Contains(err.Error(), `"Family"`) {
		t.Errorf("createEvent err = %v, want an ambiguity error naming the calendar", err)
	}
	if len(fake.createCalls) != 0 {
		t.Errorf("CreateEvent was called %d times; must not write to either calendar", len(fake.createCalls))
	}

	_, cals, err := svc.listCalendars(context.Background(), nil, ListCalendarsArgs{})
	if err != nil {
		t.Fatalf("listCalendars: %v", err)
	}
	if len(cals.Calendars) != 2 || len(cals.Warnings) != 1 || cals.Calendars[0].ID == cals.Calendars[1].ID {
		t.Errorf("listCalendars = %+v, want both Family calendars with distinct ids, plus an ambiguity warning", cals)
	}
}

func TestCreateEvent_ByCalendarIDDisambiguatesSharedName(t *testing.T) {
	fake := &fakeClient{
		calendars: []icloud.Calendar{
			{Path: "/1/calendars/family-mine/", Name: "Family"},
			{Path: "/1/calendars/family-shared/", Name: "Family"},
			{Path: "/1/calendars/secret/", Name: "Secret"},
		},
	}
	svc, _ := newTestService(fake, "Family")

	_, cals, err := svc.listCalendars(context.Background(), nil, ListCalendarsArgs{})
	if err != nil {
		t.Fatalf("listCalendars: %v", err)
	}
	sharedID := ""
	for _, c := range cals.Calendars {
		if path, _ := icloud.DecodeCalendarToken(c.ID); path == "/1/calendars/family-shared/" {
			sharedID = c.ID
		}
	}
	if sharedID == "" {
		t.Fatalf("listCalendars = %+v, no id for the shared Family calendar", cals)
	}

	args := CreateEventArgs{Calendar: sharedID, Title: "Dinner", Start: "2026-11-17T18:00:00-06:00", End: "2026-11-17T19:00:00-06:00"}
	_, out, err := svc.createEvent(context.Background(), nil, args)
	if err != nil {
		t.Fatalf("createEvent by id: %v", err)
	}
	if out.Calendar != "Family" {
		t.Errorf("created event's calendar = %q, want Family", out.Calendar)
	}
	id, err := icloud.DecodeEventID(out.ID)
	if err != nil || id.CalendarPath != "/1/calendars/family-shared/" {
		t.Errorf("event created on %q (err %v), want /1/calendars/family-shared/", id.CalendarPath, err)
	}

	// The plain shared name is still refused.
	args.Calendar = "Family"
	if _, _, err := svc.createEvent(context.Background(), nil, args); !errors.Is(err, errAmbiguousCalendar) {
		t.Errorf("createEvent by shared name err = %v, want errAmbiguousCalendar", err)
	}

	// An id can't reach a calendar that isn't allow-listed, or one that
	// isn't in the account.
	for _, calID := range []string{icloud.CalendarToken("/1/calendars/secret/"), icloud.CalendarToken("/1/calendars/gone/")} {
		args.Calendar = calID
		if _, _, err := svc.createEvent(context.Background(), nil, args); err == nil {
			t.Errorf("createEvent with id %q succeeded; want an error", calID)
		}
	}
	if len(fake.createCalls) != 1 {
		t.Errorf("CreateEvent called %d times, want 1 (only the by-id create)", len(fake.createCalls))
	}
}

func TestListEvents_InstantEventAtWindowStart(t *testing.T) {
	ics := "BEGIN:VCALENDAR\nVERSION:2.0\nPRODID:-//test//test//EN\nBEGIN:VEVENT\nUID:instant\n" +
		"DTSTAMP:20261001T000000Z\nDTSTART:20261117T150000Z\nSUMMARY:instant\nEND:VEVENT\nEND:VCALENDAR\n"
	fake := &fakeClient{
		calendars: []icloud.Calendar{{Path: "/1/calendars/home/", Name: "Home"}},
		objects: map[string][]caldav.CalendarObject{
			"/1/calendars/home/": {object(t, "/1/calendars/home/i.ics", ics)},
		},
	}
	svc, _ := newTestService(fake, "Home")

	_, out, err := svc.listEvents(context.Background(), nil, ListEventsArgs{
		Start: "2026-11-17T09:00:00-06:00", // == 15:00Z, the event's instant
		End:   "2026-11-17T10:00:00-06:00",
	})
	if err != nil {
		t.Fatalf("listEvents: %v", err)
	}
	if len(out.Events) != 1 || out.Events[0].Title != "instant" {
		t.Errorf("events = %+v, want the instant event at the inclusive window start", out.Events)
	}
}

func TestListEvents_ManyCalendars(t *testing.T) {
	fake := &fakeClient{objects: map[string][]caldav.CalendarObject{}, fetchErrs: map[string]error{}}
	var allow []string
	var want []string
	for i := 0; i < 8; i++ {
		name := fmt.Sprintf("Cal%d", i)
		calPath := fmt.Sprintf("/1/calendars/c%d/", i)
		fake.calendars = append(fake.calendars, icloud.Calendar{Path: calPath, Name: name})
		allow = append(allow, name)
		if i == 5 {
			fake.fetchErrs[calPath] = errors.New("503 Service Unavailable")
			continue
		}
		// Reverse order of calendars, so sorting by start must reorder.
		uid := fmt.Sprintf("e%d", i)
		start := fmt.Sprintf("DTSTART:20261117T%02d0000Z", 20-i)
		end := fmt.Sprintf("DTEND:20261117T%02d3000Z", 20-i)
		fake.objects[calPath] = []caldav.CalendarObject{object(t, calPath+uid+".ics", singleEventICS(uid, start, end))}
		want = append([]string{uid}, want...)
	}
	// Two masters in one object: reported, not silently dropped.
	fake.objects["/1/calendars/c0/"] = append(fake.objects["/1/calendars/c0/"], object(t, "/1/calendars/c0/dup.ics",
		"BEGIN:VCALENDAR\nVERSION:2.0\nPRODID:-//test//test//EN\n"+
			"BEGIN:VEVENT\nUID:dup\nDTSTAMP:20261001T000000Z\nDTSTART:20261117T010000Z\nSUMMARY:dup\nEND:VEVENT\n"+
			"BEGIN:VEVENT\nUID:dup\nDTSTAMP:20261001T000000Z\nDTSTART:20261117T020000Z\nSUMMARY:dup2\nEND:VEVENT\n"+
			"END:VCALENDAR\n"))
	want = append([]string{"dup"}, want...)

	svc, _ := newTestService(fake, allow...)
	_, out, err := svc.listEvents(context.Background(), nil, ListEventsArgs{
		Start: "2026-11-17T00:00:00Z",
		End:   "2026-11-18T00:00:00Z",
	})
	if err != nil {
		t.Fatalf("listEvents: %v (one failing calendar must not fail the call)", err)
	}
	var got []string
	for _, e := range out.Events {
		got = append(got, e.Title)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("events = %v, want %v", got, want)
	}
	joined := strings.Join(out.Warnings, "\n")
	if len(out.Warnings) != 2 || !strings.Contains(joined, `calendar "Cal5" could not be queried`) || !strings.Contains(joined, "more than one main event") {
		t.Errorf("warnings = %q, want one about Cal5 and one about the duplicate master", out.Warnings)
	}
	if !out.Incomplete {
		t.Error("incomplete = false with a calendar omitted; want true")
	}
}

func TestListEvents_AllCalendarsFailingIsAnError(t *testing.T) {
	fake := &fakeClient{
		calendars: []icloud.Calendar{{Path: "/1/calendars/home/", Name: "Home"}},
		fetchErrs: map[string]error{"/1/calendars/home/": errors.New("503 Service Unavailable")},
	}
	svc, _ := newTestService(fake, "Home")
	_, _, err := svc.listEvents(context.Background(), nil, ListEventsArgs{Start: "2026-11-17T00:00:00Z", End: "2026-11-18T00:00:00Z"})
	if err == nil {
		t.Errorf("listEvents succeeded with every calendar failing; want an error")
	}
}

func TestListEvents_AuthErrorOnOneCalendarIsRetried(t *testing.T) {
	fake := &fakeClient{
		calendars: []icloud.Calendar{{Path: "/1/calendars/a/", Name: "A"}, {Path: "/1/calendars/b/", Name: "B"}},
		fetchErrs: map[string]error{"/1/calendars/b/": fmt.Errorf("icloud: querying: %w", icloud.ErrUnauthorized)},
	}
	svc, connects := newTestService(fake, "A", "B")
	_, _, err := svc.listEvents(context.Background(), nil, ListEventsArgs{Start: "2026-11-17T00:00:00Z", End: "2026-11-18T00:00:00Z"})
	if !errors.Is(err, icloud.ErrUnauthorized) || len(*connects) != 2 {
		t.Errorf("err=%v connects=%d, want ErrUnauthorized after one refreshed retry", err, len(*connects))
	}
}

func TestListEvents_AuthRetryRefetchesOnlyFailedCalendars(t *testing.T) {
	fake := &fakeClient{
		calendars: []icloud.Calendar{
			{Path: "/1/calendars/a/", Name: "A"},
			{Path: "/1/calendars/b/", Name: "B"},
			{Path: "/1/calendars/c/", Name: "C"},
		},
		objects: map[string][]caldav.CalendarObject{},
		// B's first query is rejected (say, the password was just rotated);
		// it succeeds after the credentials are refreshed.
		fetchErrsOnce: map[string]error{"/1/calendars/b/": fmt.Errorf("icloud: querying: %w", icloud.ErrUnauthorized)},
	}
	for _, name := range []string{"a", "b", "c"} {
		p := "/1/calendars/" + name + "/"
		fake.objects[p] = []caldav.CalendarObject{object(t, p+name+".ics", singleEventICS(name, "DTSTART:20261117T150000Z", "DTEND:20261117T160000Z"))}
	}
	svc, connects := newTestService(fake, "A", "B", "C")

	_, out, err := svc.listEvents(context.Background(), nil, ListEventsArgs{Start: "2026-11-17T00:00:00Z", End: "2026-11-18T00:00:00Z"})
	if err != nil {
		t.Fatalf("listEvents: %v", err)
	}
	if len(out.Events) != 3 || out.Incomplete {
		t.Errorf("events = %+v (incomplete %v), want all three", out.Events, out.Incomplete)
	}
	if len(*connects) != 2 || !(*connects)[1].refresh {
		t.Errorf("connect calls = %+v, want a normal connect then one refresh", *connects)
	}
	want := map[string]int{"/1/calendars/a/": 1, "/1/calendars/b/": 2, "/1/calendars/c/": 1}
	for p, n := range want {
		if fake.fetchCounts[p] != n {
			t.Errorf("calendar %s fetched %d times, want %d (only the rejected calendar is re-fetched)", p, fake.fetchCounts[p], n)
		}
	}
	if fake.listCalls != 1 {
		t.Errorf("ListCalendars called %d times, want 1", fake.listCalls)
	}
}

func TestListEvents_BoundsConcurrentFetches(t *testing.T) {
	fake := &fakeClient{fetchDelay: 20 * time.Millisecond}
	var allow []string
	for i := 0; i < 3*maxConcurrentCalendarFetches; i++ {
		name := fmt.Sprintf("Cal%d", i)
		fake.calendars = append(fake.calendars, icloud.Calendar{Path: fmt.Sprintf("/1/calendars/c%d/", i), Name: name})
		allow = append(allow, name)
	}
	svc, _ := newTestService(fake, allow...)

	if _, _, err := svc.listEvents(context.Background(), nil, ListEventsArgs{Start: "2026-11-17T00:00:00Z", End: "2026-11-18T00:00:00Z"}); err != nil {
		t.Fatalf("listEvents: %v", err)
	}
	if fake.maxInFlight > maxConcurrentCalendarFetches || fake.maxInFlight < 2 {
		t.Errorf("max concurrent FetchObjects calls = %d, want between 2 and %d", fake.maxInFlight, maxConcurrentCalendarFetches)
	}
	if len(fake.fetchCounts) != len(fake.calendars) {
		t.Errorf("fetched %d calendars, want all %d", len(fake.fetchCounts), len(fake.calendars))
	}
}

func TestListEvents_AllowListedCalendarMissingFromAccount(t *testing.T) {
	fake := &fakeClient{calendars: []icloud.Calendar{{Path: "/1/calendars/home/", Name: "Home"}}}
	svc, _ := newTestService(fake, "Home", "Deleted")

	_, _, err := svc.listEvents(context.Background(), nil, ListEventsArgs{Calendar: "Deleted", Start: "2026-11-17T00:00:00Z", End: "2026-11-18T00:00:00Z"})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("listEvents err = %v, want a calendar-not-found error", err)
	}
	// create_event says the same for the same calendar.
	_, _, cerr := svc.createEvent(context.Background(), nil, CreateEventArgs{Calendar: "Deleted", Title: "x", Start: "2026-11-17T09:00:00Z", End: "2026-11-17T10:00:00Z"})
	if cerr == nil || !strings.Contains(cerr.Error(), "not found") {
		t.Errorf("createEvent err = %v, want a calendar-not-found error", cerr)
	}

	// Without a calendar argument, a missing allow-listed calendar is fine.
	if _, _, err := svc.listEvents(context.Background(), nil, ListEventsArgs{Start: "2026-11-17T00:00:00Z", End: "2026-11-18T00:00:00Z"}); err != nil {
		t.Errorf("listEvents over all calendars: %v", err)
	}
}

func TestGetEvent_RecurringOverrideMovedMoreThanAYear(t *testing.T) {
	// The 2026-11-16 occurrence of a weekly series was rescheduled to
	// 2028-03-01: more than a year from its original slot.
	const ics = "BEGIN:VCALENDAR\nVERSION:2.0\nPRODID:-//test//test//EN\n" +
		"BEGIN:VEVENT\nUID:weekly\nDTSTAMP:20261001T000000Z\n" +
		"DTSTART;TZID=America/Chicago:20261102T090000\nDTEND;TZID=America/Chicago:20261102T093000\n" +
		"RRULE:FREQ=WEEKLY;BYDAY=MO\nSUMMARY:Standup\nEND:VEVENT\n" +
		"BEGIN:VEVENT\nUID:weekly\nDTSTAMP:20261001T000000Z\n" +
		"RECURRENCE-ID;TZID=America/Chicago:20261116T090000\n" +
		"DTSTART;TZID=America/Chicago:20280301T100000\nDTEND;TZID=America/Chicago:20280301T103000\n" +
		"SUMMARY:Standup (moved)\nEND:VEVENT\nEND:VCALENDAR\n"
	fake := &fakeClient{
		calendars: []icloud.Calendar{{Path: "/1/calendars/home/", Name: "Home"}},
		objects: map[string][]caldav.CalendarObject{
			"/1/calendars/home/": {object(t, "/1/calendars/home/weekly.ics", ics)},
		},
	}
	svc, _ := newTestService(fake, "Home")

	_, list, err := svc.listEvents(context.Background(), nil, ListEventsArgs{Start: "2028-03-01T00:00:00-06:00", End: "2028-03-02T00:00:00-06:00"})
	if err != nil {
		t.Fatalf("listEvents: %v", err)
	}
	var moved *EventOut
	for i := range list.Events {
		if list.Events[i].Title == "Standup (moved)" {
			moved = &list.Events[i]
		}
	}
	if moved == nil {
		t.Fatalf("list_events = %+v, want the moved occurrence", list.Events)
	}

	_, got, err := svc.getEvent(context.Background(), nil, GetEventArgs{ID: moved.ID})
	if err != nil {
		t.Fatalf("getEvent(id from list_events): %v", err)
	}
	if got.Title != "Standup (moved)" || got.Start != "2028-03-01T10:00:00-06:00" || got.RecurrenceID != "2026-11-16T09:00:00-06:00" {
		t.Errorf("getEvent = %+v, want the moved occurrence", got)
	}
}

func TestListEvents_ResolvesTZIDFromObjectVTIMEZONE(t *testing.T) {
	// A TZID that's neither an IANA name nor a known Windows zone, defined
	// by the object's own VTIMEZONE as a fixed UTC+05:30.
	const ics = "BEGIN:VCALENDAR\nVERSION:2.0\nPRODID:-//test//test//EN\n" +
		"BEGIN:VTIMEZONE\nTZID:Customized Time Zone 7\nBEGIN:STANDARD\nDTSTART:19700101T000000\n" +
		"TZOFFSETFROM:+0530\nTZOFFSETTO:+0530\nEND:STANDARD\nEND:VTIMEZONE\n" +
		"BEGIN:VEVENT\nUID:vtz\nDTSTAMP:20261001T000000Z\n" +
		"DTSTART;TZID=Customized Time Zone 7:20261117T090000\nDTEND;TZID=Customized Time Zone 7:20261117T100000\n" +
		"SUMMARY:vtz\nEND:VEVENT\nEND:VCALENDAR\n"
	fake := &fakeClient{
		calendars: []icloud.Calendar{{Path: "/1/calendars/home/", Name: "Home"}},
		objects: map[string][]caldav.CalendarObject{
			"/1/calendars/home/": {object(t, "/1/calendars/home/vtz.ics", ics)},
		},
	}
	svc, _ := newTestService(fake, "Home")

	_, list, err := svc.listEvents(context.Background(), nil, ListEventsArgs{Start: "2026-11-16T00:00:00Z", End: "2026-11-18T00:00:00Z"})
	if err != nil || len(list.Events) != 1 {
		t.Fatalf("listEvents = %+v, %v; want the one event", list, err)
	}
	ev := list.Events[0]
	start, _ := time.Parse(time.RFC3339, ev.Start)
	if !start.Equal(time.Date(2026, 11, 17, 3, 30, 0, 0, time.UTC)) || ev.TimezoneWarning != "" {
		t.Errorf("event start = %s (warning %q), want 03:30Z per its VTIMEZONE, unflagged", ev.Start, ev.TimezoneWarning)
	}

	_, got, err := svc.getEvent(context.Background(), nil, GetEventArgs{ID: ev.ID})
	if err != nil || got.Start != ev.Start {
		t.Errorf("getEvent = %+v, %v; want the same start as list_events", got, err)
	}
}

// fakeCredStore is a credentialSource returning the same pointer until
// invalidated.
type fakeCredStore struct {
	mu          sync.Mutex
	cached      *credentials.Credentials
	loads       int
	invalidates int
}

func (f *fakeCredStore) Load(context.Context) (*credentials.Credentials, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cached == nil {
		f.loads++
		f.cached = &credentials.Credentials{AppleID: "a@example.com", AppSpecificPassword: "pw"}
	}
	return f.cached, nil
}

func (f *fakeCredStore) Invalidate() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.invalidates++
	f.cached = nil
}

func TestStoreConnector_ReusesClientUntilRefresh(t *testing.T) {
	store := &fakeCredStore{}
	built := 0
	connect := storeConnector(store, func(string, string) (*fakeClient, error) {
		built++
		return &fakeClient{}, nil
	})
	ctx := context.Background()

	a, _ := connect(ctx, false)
	b, _ := connect(ctx, false)
	if a != b || built != 1 {
		t.Errorf("two plain connects built %d clients (same=%v), want one reused client", built, a == b)
	}
	c, _ := connect(ctx, true)
	if c == a || built != 2 || store.invalidates != 1 {
		t.Errorf("refresh: built=%d invalidates=%d same=%v, want a new client after invalidation", built, store.invalidates, c == a)
	}
	d, _ := connect(ctx, false)
	if d != c || built != 2 {
		t.Errorf("connect after refresh built %d clients, want the refreshed client reused", built)
	}
}

func TestRegister_ListEventsSchemaFlagsIncompleteResults(t *testing.T) {
	svc, _ := newTestService(&fakeClient{}, "Home")
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	svc.Register(server)

	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, st, nil)
	if err != nil {
		t.Fatalf("server.Connect: %v", err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "client", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("client.Connect: %v", err)
	}
	defer cs.Close()

	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	for _, tool := range tools.Tools {
		if tool.Name != "list_events" {
			continue
		}
		if !strings.Contains(tool.Description, "`incomplete`") || !strings.Contains(tool.Description, "`warnings`") {
			t.Errorf("list_events description doesn't tell callers to check incomplete/warnings: %q", tool.Description)
		}
		schema, _ := json.Marshal(tool.OutputSchema)
		if !strings.Contains(string(schema), `"incomplete"`) || !strings.Contains(string(schema), "may be missing") {
			t.Errorf("list_events output schema doesn't document incomplete: %s", schema)
		}
		return
	}
	t.Fatal("list_events not registered")
}

func TestListEvents_RejectsOverlongWindow(t *testing.T) {
	fake := &fakeClient{calendars: []icloud.Calendar{{Path: "/1/calendars/home/", Name: "Home"}}}
	svc, connects := newTestService(fake, "Home")
	_, _, err := svc.listEvents(context.Background(), nil, ListEventsArgs{Start: "2026-01-01T00:00:00Z", End: "2036-01-01T00:00:00Z"})
	if err == nil || !strings.Contains(err.Error(), "too long") {
		t.Fatalf("listEvents over ten years = %v, want a window-too-long error", err)
	}
	if len(*connects) != 0 {
		t.Errorf("listEvents connected to iCloud before rejecting the window")
	}
	if _, _, err := svc.listEvents(context.Background(), nil, ListEventsArgs{Start: "2026-01-01T00:00:00Z", End: "2027-01-01T00:00:00Z"}); err != nil {
		t.Errorf("listEvents over one year = %v, want success", err)
	}
}
