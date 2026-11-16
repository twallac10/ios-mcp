// Package icloud is a thin CalDAV client for iCloud calendars, built on
// emersion/go-webdav and emersion/go-ical, plus this server's own
// RRULE/EXDATE/RECURRENCE-ID expansion (see recurrence.go) rather than
// iCloud's server-side expansion.
package icloud

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
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
	dav *caldav.Client

	// homeSet caches the account's calendar-home-set href, found via two
	// discovery round trips (current-user-principal, then
	// calendar-home-set) that almost always give the same answer — so a
	// warm Function instance doesn't repeat them on every MCP tool call,
	// mirroring credentials.Store's cache. It's dropped on a failed calendar
	// listing, and in any case after homeSetTTL (see calendarHomeSet).
	mu        sync.Mutex
	homeSet   string
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
	return newClient(http.DefaultClient, ServerURL, appleID, appSpecificPassword)
}

// newClient is NewClient with an injectable transport and endpoint, for
// tests against an httptest.Server.
func newClient(httpClient webdav.HTTPClient, endpoint, appleID, appSpecificPassword string) (*Client, error) {
	authed := webdav.HTTPClientWithBasicAuth(httpClient, appleID, appSpecificPassword)
	dav, err := caldav.NewClient(unauthorizedDetector{next: authed}, endpoint)
	if err != nil {
		return nil, fmt.Errorf("icloud: creating CalDAV client: %w", err)
	}
	return &Client{dav: dav, now: time.Now}, nil
}

// ListCalendars returns every calendar in the account's calendar home set,
// unfiltered — callers apply the configured allowlist themselves.
//
// The home set's location is discovered once per Client and cached (for up
// to homeSetTTL); the calendar list itself is re-fetched on every call (one
// round trip), since the user may add or remove calendars at any time.
func (c *Client) ListCalendars(ctx context.Context) ([]Calendar, error) {
	homeSet, err := c.calendarHomeSet(ctx)
	if err != nil {
		return nil, err
	}
	cals, err := c.dav.FindCalendars(ctx, homeSet)
	if err != nil {
		// Rediscover next time, in case the cached home set is what's wrong.
		c.mu.Lock()
		c.homeSet = ""
		c.mu.Unlock()
		return nil, fmt.Errorf("icloud: listing calendars: %w", err)
	}

	out := make([]Calendar, 0, len(cals))
	for _, cal := range cals {
		out = append(out, Calendar{Path: cal.Path, Name: cal.Name})
	}
	return out, nil
}

// calendarHomeSet returns the cached calendar-home-set href, discovering
// and caching it first if there is none or it's older than homeSetTTL.
// Only success is cached.
func (c *Client) calendarHomeSet(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.homeSet != "" && c.now().Sub(c.homeSetAt) < homeSetTTL {
		return c.homeSet, nil
	}
	principal, err := c.dav.FindCurrentUserPrincipal(ctx)
	if err != nil {
		return "", fmt.Errorf("icloud: finding current user principal: %w", err)
	}
	homeSet, err := c.dav.FindCalendarHomeSet(ctx, principal)
	if err != nil {
		return "", fmt.Errorf("icloud: finding calendar home set: %w", err)
	}
	c.homeSet, c.homeSetAt = homeSet, c.now()
	return homeSet, nil
}

// FetchObjects runs a CalDAV time-range REPORT against one calendar,
// returning the raw calendar objects (each one a .ics resource, which may
// contain a recurring master plus its RECURRENCE-ID overrides) whose
// VEVENTs intersect [start, end). The caller is responsible for expanding
// recurrence — see ExpandEvents.
func (c *Client) FetchObjects(ctx context.Context, calendarPath string, start, end time.Time) ([]caldav.CalendarObject, error) {
	query := &caldav.CalendarQuery{
		// Ask for the whole calendar object. go-webdav always encodes
		// CompRequest as a <C:comp> inside <C:calendar-data>; left as its
		// zero value that becomes <C:comp name=""/> with no allprop/allcomp,
		// i.e. a request for a component with no name and no properties
		// (RFC 4791 section 9.6.1), which a server may answer with empty or
		// stripped calendar data.
		CompRequest: caldav.CalendarCompRequest{
			Name:     ical.CompCalendar,
			AllProps: true,
			AllComps: true,
		},
		CompFilter: caldav.CompFilter{
			Name: ical.CompCalendar,
			Comps: []caldav.CompFilter{
				{
					Name:  ical.CompEvent,
					Start: start,
					End:   end,
				},
			},
		},
	}
	objs, err := c.dav.QueryCalendar(ctx, calendarPath, query)
	if err != nil {
		return nil, fmt.Errorf("icloud: querying calendar %q: %w", calendarPath, err)
	}
	return objs, nil
}

// GetObject fetches a single calendar object by its href, e.g. for
// get_event once the calendar + object path are known from a previously
// returned event ID.
func (c *Client) GetObject(ctx context.Context, objectPath string) (*caldav.CalendarObject, error) {
	obj, err := c.dav.GetCalendarObject(ctx, objectPath)
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

	objectPath := strings.TrimSuffix(calendarPath, "/") + "/" + draft.UID + ".ics"
	if _, err := c.dav.PutCalendarObject(ctx, objectPath, cal); err != nil {
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
