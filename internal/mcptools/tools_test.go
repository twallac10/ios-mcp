package mcptools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	ical "github.com/emersion/go-ical"
	"github.com/emersion/go-webdav/caldav"

	"github.com/twallac10/ios-mcp/internal/config"
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

	getCalls    []string
	createCalls []icloud.EventDraft
	// createErrs is consumed one per CreateEvent call; nil or exhausted
	// means success.
	createErrs []error
}

func (f *fakeClient) ListCalendars(context.Context) ([]icloud.Calendar, error) {
	return f.calendars, nil
}

func (f *fakeClient) FetchObjects(_ context.Context, calendarPath string, _, _ time.Time) ([]caldav.CalendarObject, error) {
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
