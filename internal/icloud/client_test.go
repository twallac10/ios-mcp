package icloud

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	case r.Method == "PROPFIND" && (r.URL.Path == "/" || r.URL.Path == "/principal/"):
		// Calendar discovery, which every Client method runs first.
		prop := `<d:current-user-principal><d:href>/principal/</d:href></d:current-user-principal>`
		if r.URL.Path == "/principal/" {
			prop = `<c:calendar-home-set><d:href>/cals/</d:href></c:calendar-home-set>`
		}
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		w.WriteHeader(http.StatusMultiStatus)
		io.WriteString(w, `<?xml version="1.0" encoding="utf-8"?>
<d:multistatus xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:response><d:href>`+r.URL.Path+`</d:href>
<d:propstat><d:prop>`+prop+`</d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`)
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

	// The calendar-data request must be bare: iCloud ignores <C:allprop/>
	// inside a <C:comp>, returning events with every property stripped.
	for _, want := range []string{"<c:calendar-data/>", `name="VEVENT"`, `start="20261117T060000Z"`, `end="20261118T060000Z"`} {
		if !strings.Contains(fake.reportBody, want) {
			t.Errorf("REPORT body missing %q:\n%s", want, fake.reportBody)
		}
	}
	for _, unwanted := range []string{"<c:comp ", "allprop", "allcomp"} {
		if strings.Contains(fake.reportBody, unwanted) {
			t.Errorf("REPORT body contains %q, which iCloud mishandles:\n%s", unwanted, fake.reportBody)
		}
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

// discoveryCalDAV answers the three calendar-discovery PROPFINDs, counting
// each kind.
type discoveryCalDAV struct {
	mu                                      sync.Mutex
	principalCalls, homeSetCalls, listCalls int
}

func (f *discoveryCalDAV) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Method != "PROPFIND" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var body string
	switch r.URL.Path {
	case "", "/":
		f.principalCalls++
		body = `<d:response><d:href>/</d:href><d:propstat><d:prop>
<d:current-user-principal><d:href>/principal/</d:href></d:current-user-principal>
</d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`
	case "/principal/":
		f.homeSetCalls++
		body = `<d:response><d:href>/principal/</d:href><d:propstat><d:prop>
<c:calendar-home-set><d:href>/cals/</d:href></c:calendar-home-set>
</d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`
	case "/cals/":
		f.listCalls++
		body = `<d:response><d:href>/cals/</d:href><d:propstat><d:prop>
<d:resourcetype><d:collection/></d:resourcetype>
</d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>
<d:response><d:href>/cals/home/</d:href><d:propstat><d:prop>
<d:resourcetype><d:collection/><c:calendar/></d:resourcetype>
<d:displayname>Home</d:displayname>
</d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`
	default:
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(http.StatusMultiStatus)
	io.WriteString(w, `<?xml version="1.0" encoding="utf-8"?>
<d:multistatus xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav">`+body+`</d:multistatus>`)
}

func TestListCalendars_CachesDiscovery(t *testing.T) {
	fake := &discoveryCalDAV{}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	c, err := newClient(srv.Client(), srv.URL, "user@example.com", "pw")
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}

	for i := 0; i < 2; i++ {
		cals, err := c.ListCalendars(context.Background())
		if err != nil {
			t.Fatalf("ListCalendars #%d: %v", i+1, err)
		}
		if len(cals) != 1 || cals[0].Path != "/cals/home/" || cals[0].Name != "Home" {
			t.Fatalf("ListCalendars #%d = %+v, want the Home calendar", i+1, cals)
		}
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.principalCalls != 1 || fake.homeSetCalls != 1 {
		t.Errorf("discovery ran principal=%d home-set=%d times over two calls, want once each", fake.principalCalls, fake.homeSetCalls)
	}
	if fake.listCalls != 2 {
		t.Errorf("calendar list fetched %d times, want 2 (it isn't cached)", fake.listCalls)
	}
}

func TestListCalendars_RediscoversHomeSetAfterTTL(t *testing.T) {
	fake := &discoveryCalDAV{}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	c, err := newClient(srv.Client(), srv.URL, "user@example.com", "pw")
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }

	list := func() {
		t.Helper()
		if _, err := c.ListCalendars(context.Background()); err != nil {
			t.Fatalf("ListCalendars: %v", err)
		}
	}
	list()
	now = now.Add(homeSetTTL - time.Minute)
	list() // still cached
	now = now.Add(2 * time.Minute)
	list() // expired: rediscovered, even though every call succeeded

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.principalCalls != 2 || fake.homeSetCalls != 2 {
		t.Errorf("discovery ran principal=%d home-set=%d times, want twice each (initially, then after the TTL)", fake.principalCalls, fake.homeSetCalls)
	}
}

// partitionCalDAV is a discovery server whose calendar home set is an
// absolute URL on another host (as iCloud's is, on pNN-caldav.icloud.com).
type partitionCalDAV struct {
	homeURL string
}

func (f *partitionCalDAV) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var prop string
	switch r.URL.Path {
	case "/":
		prop = `<d:current-user-principal><d:href>/123/principal/</d:href></d:current-user-principal>`
	case "/123/principal/":
		prop = `<c:calendar-home-set><d:href>` + f.homeURL + `</d:href></c:calendar-home-set>`
	default:
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(http.StatusMultiStatus)
	io.WriteString(w, `<?xml version="1.0" encoding="utf-8"?>
<d:multistatus xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:response><d:href>`+r.URL.Path+`</d:href>
<d:propstat><d:prop>`+prop+`</d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`)
}

func TestClient_UsesHomeSetHost(t *testing.T) {
	home := &discoveryCalDAV{}
	homeSrv := httptest.NewServer(home)
	t.Cleanup(homeSrv.Close)
	disc := httptest.NewServer(&partitionCalDAV{homeURL: homeSrv.URL + "/cals/"})
	t.Cleanup(disc.Close)

	c, err := newClient(http.DefaultClient, disc.URL, "user@example.com", "pw")
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	c.trustHost = func(*url.URL) bool { return true }

	cals, err := c.ListCalendars(context.Background())
	if err != nil {
		t.Fatalf("ListCalendars: %v", err)
	}
	if len(cals) != 1 || cals[0].Path != "/cals/home/" {
		t.Fatalf("ListCalendars = %+v, want the Home calendar from the home-set host", cals)
	}
	home.mu.Lock()
	defer home.mu.Unlock()
	if home.listCalls != 1 {
		t.Errorf("home-set host got %d calendar listings, want 1", home.listCalls)
	}
}

func TestClient_RefusesUntrustedHomeSetHost(t *testing.T) {
	other := httptest.NewServer(&discoveryCalDAV{})
	t.Cleanup(other.Close)
	disc := httptest.NewServer(&partitionCalDAV{homeURL: other.URL + "/cals/"})
	t.Cleanup(disc.Close)

	c, err := newClient(http.DefaultClient, disc.URL, "user@example.com", "pw")
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	// Default trustHost: a plain-HTTP, non-icloud.com host is refused.
	if _, err := c.ListCalendars(context.Background()); err == nil || !strings.Contains(err.Error(), "unexpected host") {
		t.Fatalf("ListCalendars = %v, want an unexpected-host error", err)
	}
}

func TestIsICloudHost(t *testing.T) {
	for raw, want := range map[string]bool{
		"https://p42-caldav.icloud.com:443/1/calendars/": true,
		"https://caldav.icloud.com/":                     true,
		"http://p42-caldav.icloud.com/":                  false,
		"https://evilicloud.com/":                        false,
		"https://icloud.com.evil.example/":               false,
	} {
		u, _ := url.Parse(raw)
		if got := isICloudHost(u); got != want {
			t.Errorf("isICloudHost(%s) = %v, want %v", raw, got, want)
		}
	}
}
