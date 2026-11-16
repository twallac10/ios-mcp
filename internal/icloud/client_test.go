package icloud

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	ical "github.com/emersion/go-ical"
)

// encodeEvent renders a single VEVENT inside a VCALENDAR, as CreateEvent
// would send it.
func encodeEvent(t *testing.T, e *ical.Event) string {
	t.Helper()
	cal := ical.NewCalendar()
	cal.Props.SetText(ical.PropProductID, "-//test//test//EN")
	cal.Props.SetText(ical.PropVersion, "2.0")
	cal.Children = append(cal.Children, e.Component)
	var buf bytes.Buffer
	if err := ical.NewEncoder(&buf).Encode(cal); err != nil {
		t.Fatalf("encoding: %v", err)
	}
	return buf.String()
}

func TestBuildEvent_OffsetTimesEncodeAsUTC(t *testing.T) {
	// The exact form the create_event schema suggests: a numeric offset,
	// which parses to an unnamed fixed zone.
	start, _ := time.Parse(time.RFC3339, "2026-11-17T09:00:00-06:00")
	end, _ := time.Parse(time.RFC3339, "2026-11-17T09:30:00-06:00")

	out := encodeEvent(t, buildEvent(EventDraft{UID: "u@x", Title: "T", Start: start, End: end}, start))

	for _, want := range []string{"DTSTART:20261117T150000Z\r\n", "DTEND:20261117T153000Z\r\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("encoded event missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "TZID") {
		t.Errorf("encoded event should carry no TZID:\n%s", out)
	}

	// And it round-trips to the same instants.
	cal, err := ical.NewDecoder(strings.NewReader(out)).Decode()
	if err != nil {
		t.Fatalf("decoding: %v", err)
	}
	ev := cal.Events()[0]
	gotStart, err := ev.DateTimeStart(chicago)
	if err != nil || !gotStart.Equal(start) {
		t.Errorf("round-tripped DTSTART = %v (err %v), want %v", gotStart, err, start)
	}
}

// fakeCalDAV is a minimal CalDAV server for exercising Client against
// go-webdav's real request encoding and response decoding.
type fakeCalDAV struct {
	mu         sync.Mutex
	reportBody string
	putBodies  map[string]string
}

const fakeObjectICS = `BEGIN:VCALENDAR
VERSION:2.0
PRODID:-//test//test//EN
BEGIN:VEVENT
UID:fetched@example.com
DTSTAMP:20261001T000000Z
DTSTART:20261117T150000Z
DTEND:20261117T160000Z
SUMMARY:Fetched
END:VEVENT
END:VCALENDAR
`

func (f *fakeCalDAV) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()

	switch {
	case strings.Contains(r.URL.Path, "/unauthorized/"):
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, "Unauthorized")
	case r.Method == "REPORT" && r.URL.Path == "/cals/home/":
		f.reportBody = string(body)
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		w.WriteHeader(http.StatusMultiStatus)
		io.WriteString(w, `<?xml version="1.0" encoding="utf-8"?>
<d:multistatus xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav">
  <d:response>
    <d:href>/cals/home/fetched.ics</d:href>
    <d:propstat>
      <d:prop>
        <d:getetag>"1"</d:getetag>
        <c:calendar-data>`+fakeObjectICS+`</c:calendar-data>
      </d:prop>
      <d:status>HTTP/1.1 200 OK</d:status>
    </d:propstat>
  </d:response>
</d:multistatus>`)
	case r.Method == http.MethodPut:
		if f.putBodies == nil {
			f.putBodies = map[string]string{}
		}
		f.putBodies[r.URL.Path] = string(body)
		w.WriteHeader(http.StatusCreated)
	default:
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, "not found")
	}
}

func newTestClient(t *testing.T) (*Client, *fakeCalDAV) {
	t.Helper()
	fake := &fakeCalDAV{}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	c, err := newClient(srv.Client(), srv.URL, "user@example.com", "pw")
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	return c, fake
}

func TestFetchObjects_RequestsFullCalendarData(t *testing.T) {
	c, fake := newTestClient(t)

	start := time.Date(2026, 11, 17, 0, 0, 0, 0, chicago)
	objs, err := c.FetchObjects(context.Background(), "/cals/home/", start, start.AddDate(0, 0, 1))
	if err != nil {
		t.Fatalf("FetchObjects: %v", err)
	}

	// The calendar-data request must name VCALENDAR and ask for all of its
	// properties and subcomponents, not an anonymous, empty <comp>.
	for _, want := range []string{`name="VCALENDAR"`, "allprop", "allcomp", `name="VEVENT"`, "time-range"} {
		if !strings.Contains(fake.reportBody, want) {
			t.Errorf("REPORT body missing %q:\n%s", want, fake.reportBody)
		}
	}
	if strings.Contains(fake.reportBody, `name=""`) {
		t.Errorf("REPORT body contains an unnamed comp:\n%s", fake.reportBody)
	}

	if len(objs) != 1 || objs[0].Path != "/cals/home/fetched.ics" || objs[0].Data == nil {
		t.Fatalf("FetchObjects returned %+v, want the one fetched object", objs)
	}
	if got := textProp(&objs[0].Data.Events()[0], ical.PropSummary); got != "Fetched" {
		t.Errorf("SUMMARY = %q, want Fetched", got)
	}
}

func TestCreateEvent_PutsUTCTimesAtUIDPath(t *testing.T) {
	c, fake := newTestClient(t)

	start, _ := time.Parse(time.RFC3339, "2026-11-17T09:00:00-06:00")
	end := start.Add(30 * time.Minute)
	inst, err := c.CreateEvent(context.Background(), "/cals/home/", EventDraft{UID: "fixed-uid@x", Title: "T", Start: start, End: end})
	if err != nil {
		t.Fatalf("CreateEvent: %v", err)
	}
	if inst.ObjectPath != "/cals/home/fixed-uid@x.ics" || inst.UID != "fixed-uid@x" {
		t.Errorf("instance = %+v, want object path from the draft's UID", inst)
	}
	body, ok := fake.putBodies["/cals/home/fixed-uid@x.ics"]
	if !ok {
		t.Fatalf("no PUT to the UID path; got %v", fake.putBodies)
	}
	if !strings.Contains(body, "DTSTART:20261117T150000Z") || strings.Contains(body, "TZID") {
		t.Errorf("PUT body has wrong DTSTART form:\n%s", body)
	}
}

func TestClient_Unauthorized(t *testing.T) {
	c, _ := newTestClient(t)
	ctx := context.Background()

	_, err := c.GetObject(ctx, "/unauthorized/x.ics")
	if !errors.Is(err, ErrUnauthorized) {
		t.Errorf("GetObject on a 401 = %v, want ErrUnauthorized", err)
	}

	// A non-401 failure whose text happens to contain "401" isn't one.
	_, err = c.GetObject(ctx, "/cals/home/401.ics")
	if err == nil || errors.Is(err, ErrUnauthorized) {
		t.Errorf("GetObject on a 404 = %v, want a non-ErrUnauthorized error", err)
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("test premise: error text %q should contain 401", err)
	}
}
