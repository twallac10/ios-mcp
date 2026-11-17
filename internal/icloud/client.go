// Package icloud is a thin CalDAV client for iCloud calendars, built on
// emersion/go-webdav and emersion/go-ical, plus this server's own
// RRULE/EXDATE/RECURRENCE-ID expansion (see recurrence.go) rather than
// iCloud's server-side expansion.
package icloud

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	ical "github.com/emersion/go-ical"
	"github.com/emersion/go-webdav"
	"github.com/emersion/go-webdav/caldav"
)

// ServerURL is iCloud's well-known CalDAV endpoint.
const ServerURL = "https://caldav.icloud.com"

// Calendar is one of the user's iCloud calendars.
type Calendar struct {
	// Path is the calendar's CalDAV collection href, used internally to
	// query it. Opaque to callers outside this package.
	Path string
	Name string
}

// Client talks to iCloud's CalDAV server, authenticated via HTTP Basic Auth
// with an Apple ID and an app-specific password (iCloud doesn't support
// OAuth for CalDAV).
//
// A Client is safe for concurrent use, and is meant to be reused for as
// long as its credentials are: it caches calendar discovery (see
// ListCalendars).
type Client struct {
	// http is the authenticated transport every request goes through.
	http webdav.HTTPClient
	// endpoint is the discovery endpoint (ServerURL in production).
	endpoint *url.URL
	// dav talks to endpoint; used only for discovery.
	dav *caldav.Client
	// trustHost reports whether credentials may be sent to a calendar
	// home set on a host other than endpoint's (see discover).
	trustHost func(*url.URL) bool

	// homeSet caches the account's calendar-home-set path, homeDAV a client
	// for the host it lives on, and homeBase that host's root URL (for the
	// requests this package sends itself), found via two discovery round trips
	// (current-user-principal, then calendar-home-set) that almost always
	// give the same answer — so a warm Function instance doesn't repeat
	// them on every MCP tool call, mirroring credentials.Store's cache.
	// They're dropped on a failed calendar listing, and in any case after
	// homeSetTTL (see session).
	mu        sync.Mutex
	homeSet   string
	homeDAV   *caldav.Client
	homeBase  *url.URL
	homeSetAt time.Time
	now       func() time.Time
}

// homeSetTTL bounds how long a discovered calendar-home-set href is
// trusted. If the account's home set moves (e.g. an iCloud-side migration)
// while the old href still answers successfully — with an empty or stale
// calendar list rather than an error — nothing else would ever invalidate
// the cache, and one Client lives as long as its credentials do. An hour
// costs two extra PROPFINDs per hour of warm-instance uptime.
const homeSetTTL = time.Hour

// ErrUnauthorized is returned (wrapped) by every Client method when iCloud
// answers a request with HTTP 401, i.e. it rejected the Apple ID /
// app-specific password. Detect it with errors.Is.
var ErrUnauthorized = errors.New("icloud: unauthorized (HTTP 401)")

// unauthorizedDetector wraps the HTTP client handed to go-webdav and turns a
// raw 401 response into ErrUnauthorized before go-webdav sees it.
//
// go-webdav reports non-2xx responses as an *internal.HTTPError, a type in
// its internal package that this module can't import (Go's internal-package
// rule) and that has no public equivalent, so the status code can't be
// recovered reliably from the error go-webdav returns. Checking the status
// on the raw *http.Response here, and returning a sentinel error that
// go-webdav passes through unchanged, avoids matching on rendered error text.
type unauthorizedDetector struct {
	next webdav.HTTPClient
}

func (d unauthorizedDetector) Do(req *http.Request) (*http.Response, error) {
	resp, err := d.next.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, ErrUnauthorized
	}
	return resp, nil
}

// NewClient creates a Client authenticated as appleID using the given
// app-specific password.
func NewClient(appleID, appSpecificPassword string) (*Client, error) {
	return newClient(&http.Client{Timeout: requestTimeout}, ServerURL, appleID, appSpecificPassword)
}

// requestTimeout bounds each HTTP request to iCloud, so a stalled
// connection can't hang a tool call (http.DefaultClient has no timeout).
const requestTimeout = 30 * time.Second

// newClient is NewClient with an injectable transport and endpoint, for
// tests against an httptest.Server.
func newClient(httpClient webdav.HTTPClient, endpoint, appleID, appSpecificPassword string) (*Client, error) {
	authed := unauthorizedDetector{next: webdav.HTTPClientWithBasicAuth(httpClient, appleID, appSpecificPassword)}
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("icloud: parsing CalDAV endpoint: %w", err)
	}
	dav, err := caldav.NewClient(authed, endpoint)
	if err != nil {
		return nil, fmt.Errorf("icloud: creating CalDAV client: %w", err)
	}
	return &Client{http: authed, endpoint: u, dav: dav, trustHost: isICloudHost, now: time.Now}, nil
}

// isICloudHost reports whether u is an HTTPS URL on an icloud.com host,
// e.g. the per-account partition host (https://p42-caldav.icloud.com)
// iCloud hands out as a calendar home set.
func isICloudHost(u *url.URL) bool {
	host := strings.ToLower(u.Hostname())
	return u.Scheme == "https" && (host == "icloud.com" || strings.HasSuffix(host, ".icloud.com"))
}

// ListCalendars returns every calendar in the account's calendar home set,
// unfiltered — callers apply the configured allowlist themselves.
//
// The home set's location is discovered once per Client and cached (for up
// to homeSetTTL); the calendar list itself is re-fetched on every call (one
// round trip), since the user may add or remove calendars at any time.
func (c *Client) ListCalendars(ctx context.Context) ([]Calendar, error) {
	dav, homeSet, _, err := c.session(ctx)
	if err != nil {
		return nil, err
	}
	cals, err := dav.FindCalendars(ctx, homeSet)
	if err != nil {
		// Rediscover next time, in case the cached home set is what's wrong.
		c.mu.Lock()
		c.homeSet, c.homeDAV = "", nil
		c.mu.Unlock()
		return nil, fmt.Errorf("icloud: listing calendars: %w", err)
	}

	out := make([]Calendar, 0, len(cals))
	for _, cal := range cals {
		out = append(out, Calendar{Path: cal.Path, Name: cal.Name})
	}
	return out, nil
}

// session returns a CalDAV client for the host the account's calendars
// live on, the calendar-home-set path on it, and that host's root URL,
// discovering and caching them first if there are none or they're older
// than homeSetTTL. Only success is cached.
//
// iCloud answers discovery on caldav.icloud.com but returns the calendar
// home set as an absolute URL on a per-account partition host (e.g.
// https://p42-caldav.icloud.com:443/1234567/calendars/). go-webdav's
// FindCalendarHomeSet keeps only that URL's path, so every later request
// would go to the wrong host; this keeps the host too, and every calendar
// request (listing, querying, fetching, creating) goes there.
func (c *Client) session(ctx context.Context) (*caldav.Client, string, *url.URL, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.homeSet != "" && c.homeDAV != nil && c.now().Sub(c.homeSetAt) < homeSetTTL {
		return c.homeDAV, c.homeSet, c.homeBase, nil
	}
	dav, homeSet, base, err := c.discover(ctx)
	if err != nil {
		return nil, "", nil, err
	}
	c.homeDAV, c.homeSet, c.homeBase, c.homeSetAt = dav, homeSet, base, c.now()
	return dav, homeSet, base, nil
}

func (c *Client) discover(ctx context.Context) (*caldav.Client, string, *url.URL, error) {
	principal, err := c.dav.FindCurrentUserPrincipal(ctx)
	if err != nil {
		return nil, "", nil, fmt.Errorf("icloud: finding current user principal: %w", err)
	}
	principalURL := c.endpoint.ResolveReference(&url.URL{Path: principal})
	href, err := c.findHomeSetHref(ctx, principalURL)
	if err != nil {
		return nil, "", nil, fmt.Errorf("icloud: finding calendar home set: %w", err)
	}
	home, err := principalURL.Parse(href)
	if err != nil {
		return nil, "", nil, fmt.Errorf("icloud: finding calendar home set: unparseable href %q: %w", href, err)
	}
	if home.Path == "" {
		home.Path = "/"
	}

	base := &url.URL{Scheme: home.Scheme, Host: home.Host, Path: "/"}
	if strings.EqualFold(home.Host, c.endpoint.Host) && home.Scheme == c.endpoint.Scheme {
		return c.dav, home.Path, base, nil
	}
	if !c.trustHost(home) {
		return nil, "", nil, fmt.Errorf("icloud: calendar home set is on unexpected host %q; refusing to send credentials there", home.Host)
	}
	dav, err := caldav.NewClient(c.http, base.String())
	if err != nil {
		return nil, "", nil, fmt.Errorf("icloud: creating CalDAV client for %q: %w", home.Host, err)
	}
	return dav, home.Path, base, nil
}

const homeSetPropfind = `<?xml version="1.0" encoding="utf-8"?>
<d:propfind xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:prop><c:calendar-home-set/></d:prop></d:propfind>`

// homeSetMultistatus is the part of a PROPFIND calendar-home-set response
// that findHomeSetHref reads.
type homeSetMultistatus struct {
	XMLName   xml.Name `xml:"DAV: multistatus"`
	Responses []struct {
		Propstats []struct {
			Status string `xml:"DAV: status"`
			Prop   struct {
				HomeSet struct {
					Hrefs []string `xml:"DAV: href"`
				} `xml:"urn:ietf:params:xml:ns:caldav calendar-home-set"`
			} `xml:"DAV: prop"`
		} `xml:"DAV: propstat"`
	} `xml:"DAV: response"`
}

// findHomeSetHref PROPFINDs principal's calendar-home-set and returns its
// href exactly as the server gave it (possibly an absolute URL).
func (c *Client) findHomeSetHref(ctx context.Context, principal *url.URL) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "PROPFIND", principal.String(), strings.NewReader(homeSetPropfind))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	req.Header.Set("Depth", "0")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusMultiStatus {
		return "", fmt.Errorf("PROPFIND %s: HTTP %d", principal.Path, resp.StatusCode)
	}
	var ms homeSetMultistatus
	if err := xml.Unmarshal(body, &ms); err != nil {
		return "", fmt.Errorf("decoding PROPFIND response: %w", err)
	}
	for _, r := range ms.Responses {
		for _, ps := range r.Propstats {
			if ps.Status != "" && !strings.Contains(ps.Status, " 200") {
				continue
			}
			for _, h := range ps.Prop.HomeSet.Hrefs {
				if h = strings.TrimSpace(h); h != "" {
					return h, nil
				}
			}
		}
	}
	return "", errors.New("no calendar-home-set in PROPFIND response")
}

// FetchObjects runs a CalDAV time-range REPORT against one calendar,
// returning the raw calendar objects (each one a .ics resource, which may
// contain a recurring master plus its RECURRENCE-ID overrides) whose
// VEVENTs intersect [start, end). The caller is responsible for expanding
// recurrence — see ExpandEvents.
//
// This sends the REPORT itself rather than through go-webdav's
// QueryCalendar, which always nests a <C:comp> inside <C:calendar-data>.
// iCloud ignores <C:allprop/> there: asked for VCALENDAR with allprop and
// allcomp, it answers with an empty VCALENDAR, and naming VEVENT explicitly
// only gets VEVENTs with every property stripped — so every event would
// silently disappear. A bare <C:calendar-data/> (RFC 4791 section 9.6: no
// comp means the whole object) is the one form iCloud answers in full.
func (c *Client) FetchObjects(ctx context.Context, calendarPath string, start, end time.Time) ([]caldav.CalendarObject, error) {
	_, _, base, err := c.session(ctx)
	if err != nil {
		return nil, err
	}
	objs, err := c.queryCalendar(ctx, base.ResolveReference(&url.URL{Path: calendarPath}), start, end)
	if err != nil {
		return nil, fmt.Errorf("icloud: querying calendar %q: %w", calendarPath, err)
	}
	return objs, nil
}

const calendarQueryReport = `<?xml version="1.0" encoding="utf-8"?>
<c:calendar-query xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:prop><d:getetag/><c:calendar-data/></d:prop><c:filter><c:comp-filter name="VCALENDAR"><c:comp-filter name="VEVENT"><c:time-range start="%s" end="%s"/></c:comp-filter></c:comp-filter></c:filter></c:calendar-query>`

// calendarQueryMultistatus is the part of a calendar-query REPORT response
// that queryCalendar reads.
type calendarQueryMultistatus struct {
	XMLName   xml.Name `xml:"DAV: multistatus"`
	Responses []struct {
		Href      string `xml:"DAV: href"`
		Propstats []struct {
			Status string `xml:"DAV: status"`
			Prop   struct {
				ETag         string `xml:"DAV: getetag"`
				CalendarData string `xml:"urn:ietf:params:xml:ns:caldav calendar-data"`
			} `xml:"DAV: prop"`
		} `xml:"DAV: propstat"`
	} `xml:"DAV: response"`
}

// queryCalendar sends the calendar-query REPORT described on FetchObjects
// to the calendar collection at u and decodes every returned object.
func (c *Client) queryCalendar(ctx context.Context, u *url.URL, start, end time.Time) ([]caldav.CalendarObject, error) {
	const utcDateTime = "20060102T150405Z"
	body := fmt.Sprintf(calendarQueryReport, start.UTC().Format(utcDateTime), end.UTC().Format(utcDateTime))
	req, err := http.NewRequestWithContext(ctx, "REPORT", u.String(), strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	req.Header.Set("Depth", "1")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMultiStatus {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("REPORT %s: HTTP %d", u.Path, resp.StatusCode)
	}
	var ms calendarQueryMultistatus
	if err := xml.NewDecoder(resp.Body).Decode(&ms); err != nil {
		return nil, fmt.Errorf("decoding REPORT response: %w", err)
	}

	objs := make([]caldav.CalendarObject, 0, len(ms.Responses))
	for _, r := range ms.Responses {
		href, err := url.Parse(strings.TrimSpace(r.Href))
		if err != nil {
			return nil, fmt.Errorf("unparseable href %q: %w", r.Href, err)
		}
		for _, ps := range r.Propstats {
			if ps.Status != "" && !strings.Contains(ps.Status, " 200") {
				continue
			}
			if strings.TrimSpace(ps.Prop.CalendarData) == "" {
				continue
			}
			data, err := ical.NewDecoder(strings.NewReader(ps.Prop.CalendarData)).Decode()
			if err != nil {
				return nil, fmt.Errorf("decoding %s: %w", href.Path, err)
			}
			objs = append(objs, caldav.CalendarObject{Path: href.Path, ETag: ps.Prop.ETag, Data: data})
			break
		}
	}
	return objs, nil
}

// GetObject fetches a single calendar object by its href, e.g. for
// get_event once the calendar + object path are known from a previously
// returned event ID.
func (c *Client) GetObject(ctx context.Context, objectPath string) (*caldav.CalendarObject, error) {
	dav, _, _, err := c.session(ctx)
	if err != nil {
		return nil, err
	}
	obj, err := dav.GetCalendarObject(ctx, objectPath)
	if err != nil {
		return nil, fmt.Errorf("icloud: fetching calendar object %q: %w", objectPath, err)
	}
	return obj, nil
}

// NewEventUID generates a UID for a new VEVENT, suffixed per RFC 5545's
// recommendation to scope UIDs to a domain.
//
// Callers that may retry CreateEvent must generate the UID once, up front,
// and reuse it on every attempt; see CreateEvent.
func NewEventUID() (string, error) {
	return newUID()
}

// EventDraft describes a new, single (non-recurring) event to create.
type EventDraft struct {
	// UID becomes both the VEVENT's UID and the object's file name. It must
	// be stable across retries of the same logical create; use NewEventUID.
	UID      string
	Title    string
	Start    time.Time
	End      time.Time
	Location string
	Notes    string
}

// CreateEvent creates a new, single (non-recurring) VEVENT in the given
// calendar and returns the resulting Instance.
//
// The object is written with an unconditional PUT to
// <calendarPath>/<UID>.ics, so retrying with the same draft (same UID) is
// idempotent: if an earlier attempt actually reached iCloud, the retry
// overwrites that same object with identical content instead of creating a
// second event. (go-webdav v0.7.0's PutCalendarObject doesn't support
// If-None-Match, and with a stable UID a conditional create would only turn
// a retry-after-success into a spurious 412.)
func (c *Client) CreateEvent(ctx context.Context, calendarPath string, draft EventDraft) (Instance, error) {
	if draft.UID == "" {
		return Instance{}, fmt.Errorf("icloud: creating event: empty UID")
	}
	event := buildEvent(draft, time.Now())

	cal := ical.NewCalendar()
	cal.Props.SetText(ical.PropProductID, "-//twallac10//ios-mcp//EN")
	cal.Props.SetText(ical.PropVersion, "2.0")
	cal.Children = append(cal.Children, event.Component)

	dav, _, _, err := c.session(ctx)
	if err != nil {
		return Instance{}, err
	}
	objectPath := strings.TrimSuffix(calendarPath, "/") + "/" + draft.UID + ".ics"
	if _, err := dav.PutCalendarObject(ctx, objectPath, cal); err != nil {
		return Instance{}, fmt.Errorf("icloud: creating event: %w", err)
	}

	return Instance{
		UID:          draft.UID,
		ObjectPath:   objectPath,
		CalendarPath: calendarPath,
		Title:        draft.Title,
		Start:        draft.Start,
		End:          draft.End,
		AllDay:       false,
		Location:     draft.Location,
		Notes:        draft.Notes,
	}, nil
}

// buildEvent renders draft as a VEVENT.
//
// DTSTART/DTEND are written in UTC ("...Z" form). go-ical's SetDateTime
// writes any non-UTC time as TZID=<Location().String()>; a time parsed
// from an RFC3339 string with a numeric offset (e.g. -06:00) has an unnamed
// fixed-zone Location, which would produce an invalid "DTSTART;TZID=:..."
// (or "TZID=Local"). UTC is unambiguous and always round-trips.
func buildEvent(draft EventDraft, now time.Time) *ical.Event {
	event := ical.NewEvent()
	event.Props.SetText(ical.PropUID, draft.UID)
	event.Props.SetDateTime(ical.PropDateTimeStamp, now.UTC())
	event.Props.SetDateTime(ical.PropDateTimeStart, draft.Start.UTC())
	event.Props.SetDateTime(ical.PropDateTimeEnd, draft.End.UTC())
	event.Props.SetText(ical.PropSummary, draft.Title)
	if draft.Location != "" {
		event.Props.SetText(ical.PropLocation, draft.Location)
	}
	if draft.Notes != "" {
		event.Props.SetText(ical.PropDescription, draft.Notes)
	}
	return event
}
