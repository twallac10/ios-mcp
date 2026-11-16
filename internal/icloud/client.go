// Package icloud is a thin CalDAV client for iCloud calendars, built on
// emersion/go-webdav and emersion/go-ical, plus this server's own
// RRULE/EXDATE/RECURRENCE-ID expansion (see recurrence.go) rather than
// iCloud's server-side expansion.
package icloud

import (
	"context"
	"fmt"
	"strings"
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
type Client struct {
	dav *caldav.Client
}

// NewClient creates a Client authenticated as appleID using the given
// app-specific password.
func NewClient(appleID, appSpecificPassword string) (*Client, error) {
	httpClient := webdav.HTTPClientWithBasicAuth(nil, appleID, appSpecificPassword)
	dav, err := caldav.NewClient(httpClient, ServerURL)
	if err != nil {
		return nil, fmt.Errorf("icloud: creating CalDAV client: %w", err)
	}
	return &Client{dav: dav}, nil
}

// ListCalendars returns every calendar in the account's calendar home set,
// unfiltered — callers apply the configured allowlist themselves.
func (c *Client) ListCalendars(ctx context.Context) ([]Calendar, error) {
	principal, err := c.dav.FindCurrentUserPrincipal(ctx)
	if err != nil {
		return nil, fmt.Errorf("icloud: finding current user principal: %w", err)
	}
	homeSet, err := c.dav.FindCalendarHomeSet(ctx, principal)
	if err != nil {
		return nil, fmt.Errorf("icloud: finding calendar home set: %w", err)
	}
	cals, err := c.dav.FindCalendars(ctx, homeSet)
	if err != nil {
		return nil, fmt.Errorf("icloud: listing calendars: %w", err)
	}

	out := make([]Calendar, 0, len(cals))
	for _, cal := range cals {
		out = append(out, Calendar{Path: cal.Path, Name: cal.Name})
	}
	return out, nil
}

// FetchObjects runs a CalDAV time-range REPORT against one calendar,
// returning the raw calendar objects (each one a .ics resource, which may
// contain a recurring master plus its RECURRENCE-ID overrides) whose
// VEVENTs intersect [start, end). The caller is responsible for expanding
// recurrence — see ExpandEvents.
func (c *Client) FetchObjects(ctx context.Context, calendarPath string, start, end time.Time) ([]caldav.CalendarObject, error) {
	query := &caldav.CalendarQuery{
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

// CreateEvent creates a new, single (non-recurring) VEVENT in the given
// calendar and returns the resulting Instance.
func (c *Client) CreateEvent(ctx context.Context, calendarPath string, title string, start, end time.Time, location, notes string) (Instance, error) {
	uid, err := newUID()
	if err != nil {
		return Instance{}, err
	}

	event := ical.NewEvent()
	event.Props.SetText(ical.PropUID, uid)
	event.Props.SetDateTime(ical.PropDateTimeStamp, time.Now().UTC())
	event.Props.SetDateTime(ical.PropDateTimeStart, start)
	event.Props.SetDateTime(ical.PropDateTimeEnd, end)
	event.Props.SetText(ical.PropSummary, title)
	if location != "" {
		event.Props.SetText(ical.PropLocation, location)
	}
	if notes != "" {
		event.Props.SetText(ical.PropDescription, notes)
	}

	cal := ical.NewCalendar()
	cal.Props.SetText(ical.PropProductID, "-//twallac10//ios-mcp//EN")
	cal.Props.SetText(ical.PropVersion, "2.0")
	cal.Children = append(cal.Children, event.Component)

	objectPath := strings.TrimSuffix(calendarPath, "/") + "/" + uid + ".ics"
	if _, err := c.dav.PutCalendarObject(ctx, objectPath, cal); err != nil {
		return Instance{}, fmt.Errorf("icloud: creating event: %w", err)
	}

	return Instance{
		UID:          uid,
		ObjectPath:   objectPath,
		CalendarPath: calendarPath,
		Title:        title,
		Start:        start,
		End:          end,
		AllDay:       false,
		Location:     location,
		Notes:        notes,
	}, nil
}
