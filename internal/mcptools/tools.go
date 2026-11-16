// Package mcptools registers the iCloud Calendar MCP tools on a FastMCP-style
// *mcp.Server, mirroring the Concept2 MCP server's concept2_tools.go: tool
// definitions here, authentication (both layers) handled elsewhere.
package mcptools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"sort"
	"strings"
	"time"

	ical "github.com/emersion/go-ical"
	"github.com/emersion/go-webdav/caldav"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/twallac10/ios-mcp/internal/config"
	"github.com/twallac10/ios-mcp/internal/credentials"
	"github.com/twallac10/ios-mcp/internal/icloud"
)

func boolPtr(b bool) *bool { return &b }

var readOnly = &mcp.ToolAnnotations{
	ReadOnlyHint:    true,
	DestructiveHint: boolPtr(false),
	IdempotentHint:  true,
	OpenWorldHint:   boolPtr(true),
}

var writeNonDestructive = &mcp.ToolAnnotations{
	ReadOnlyHint:    false,
	DestructiveHint: boolPtr(false),
	IdempotentHint:  false,
	OpenWorldHint:   boolPtr(true),
}

// calendarClient is the subset of *icloud.Client the tool handlers use;
// a seam so the handlers can be tested without iCloud.
type calendarClient interface {
	ListCalendars(ctx context.Context) ([]icloud.Calendar, error)
	FetchObjects(ctx context.Context, calendarPath string, start, end time.Time) ([]caldav.CalendarObject, error)
	GetObject(ctx context.Context, objectPath string) (*caldav.CalendarObject, error)
	CreateEvent(ctx context.Context, calendarPath string, draft icloud.EventDraft) (icloud.Instance, error)
}

// Service holds this server's dependencies and implements each tool's
// handler as a method.
type Service struct {
	cfg *config.Config
	log *slog.Logger

	// connect builds a client from the stored iCloud credentials. With
	// refresh set, it first drops any cached credentials so they're
	// re-read from Key Vault.
	connect func(ctx context.Context, refresh bool) (calendarClient, error)
}

// NewService creates a Service. log must never be given event content
// (titles, locations, notes) to write at Info level or above — see
// package-level docs and the README's logging notes.
func NewService(cfg *config.Config, store *credentials.Store, log *slog.Logger) *Service {
	return &Service{cfg: cfg, log: log, connect: storeConnector(store)}
}

func storeConnector(store *credentials.Store) func(context.Context, bool) (calendarClient, error) {
	return func(ctx context.Context, refresh bool) (calendarClient, error) {
		if refresh {
			store.Invalidate()
		}
		creds, err := store.Load(ctx)
		if err != nil {
			if errors.Is(err, credentials.ErrNotFound) {
				return nil, fmt.Errorf("no iCloud credentials are configured; run cmd/seed-credentials once to store an Apple ID and app-specific password")
			}
			return nil, fmt.Errorf("loading iCloud credentials: %w", err)
		}
		return icloud.NewClient(creds.AppleID, creds.AppSpecificPassword)
	}
}

// Register adds every iCloud Calendar tool to s.
func (svc *Service) Register(s *mcp.Server) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_calendars",
		Description: "List the iCloud calendars this server is configured to expose (see the server's calendar allowlist). Call this first to get the exact calendar names to pass to list_events and create_event.",
		Annotations: readOnly,
	}, svc.listCalendars)

	mcp.AddTool(s, &mcp.Tool{
		Name: "list_events",
		Description: "List calendar events in a time window, with recurring events already expanded into their individual occurrences (one entry per occurrence, not one per series). " +
			"Each returned event has an opaque `id` to pass to get_event. Times are always full RFC3339 timestamps with a UTC offset.",
		Annotations: readOnly,
	}, svc.listEvents)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_event",
		Description: "Get full detail for a single event instance by the `id` returned from list_events or create_event.",
		Annotations: readOnly,
	}, svc.getEvent)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "create_event",
		Description: "Create a single new (non-recurring) event on one of the allow-listed iCloud calendars. There is no update or delete tool in this server; to change or remove an event, do so in the Calendar app.",
		Annotations: writeNonDestructive,
	}, svc.createEvent)
}

// --- list_calendars ---

type ListCalendarsArgs struct{}

type CalendarOut struct {
	Name string `json:"name"`
}

type ListCalendarsResult struct {
	Calendars []CalendarOut `json:"calendars"`
}

func (svc *Service) listCalendars(ctx context.Context, _ *mcp.CallToolRequest, _ ListCalendarsArgs) (*mcp.CallToolResult, ListCalendarsResult, error) {
	svc.log.Info("list_calendars")

	cals, err := withClient(ctx, svc, func(c calendarClient) ([]icloud.Calendar, error) {
		return c.ListCalendars(ctx)
	})
	if err != nil {
		return nil, ListCalendarsResult{}, err
	}

	out := ListCalendarsResult{}
	for _, cal := range cals {
		if svc.cfg.IsCalendarAllowed(cal.Name) {
			out.Calendars = append(out.Calendars, CalendarOut{Name: cal.Name})
		}
	}
	sort.Slice(out.Calendars, func(i, j int) bool { return out.Calendars[i].Name < out.Calendars[j].Name })

	svc.log.Info("list_calendars done", "allowed_count", len(out.Calendars))
	return nil, out, nil
}

// --- list_events ---

type ListEventsArgs struct {
	Calendar string `json:"calendar,omitempty" jsonschema:"Restrict results to this calendar's exact name (from list_calendars). Omit to search every allow-listed calendar."`
	Start    string `json:"start" jsonschema:"Inclusive start of the time window, as a full RFC3339 timestamp with a UTC offset, e.g. 2026-11-17T00:00:00-06:00."`
	End      string `json:"end" jsonschema:"Exclusive end of the time window, as a full RFC3339 timestamp with a UTC offset."`
}

type EventOut struct {
	ID           string `json:"id"`
	Calendar     string `json:"calendar"`
	Title        string `json:"title"`
	Start        string `json:"start"`
	End          string `json:"end"`
	AllDay       bool   `json:"all_day"`
	Location     string `json:"location,omitempty"`
	Notes        string `json:"notes,omitempty"`
	Recurring    bool   `json:"recurring"`
	RecurrenceID string `json:"recurrence_id,omitempty"`
}

type ListEventsResult struct {
	Events []EventOut `json:"events"`
	// Warnings notes anything left out of Events, e.g. calendar entries
	// that couldn't be parsed. Never contains event content.
	Warnings []string `json:"warnings,omitempty"`
}

func (svc *Service) listEvents(ctx context.Context, _ *mcp.CallToolRequest, args ListEventsArgs) (*mcp.CallToolResult, ListEventsResult, error) {
	start, err := parseRFC3339(args.Start)
	if err != nil {
		return nil, ListEventsResult{}, fmt.Errorf("start: %w", err)
	}
	end, err := parseRFC3339(args.End)
	if err != nil {
		return nil, ListEventsResult{}, fmt.Errorf("end: %w", err)
	}
	if !end.After(start) {
		return nil, ListEventsResult{}, fmt.Errorf("end (%s) must be after start (%s)", args.End, args.Start)
	}

	if args.Calendar != "" && !svc.cfg.IsCalendarAllowed(args.Calendar) {
		return nil, ListEventsResult{}, fmt.Errorf("calendar %q is not on this server's allowlist", args.Calendar)
	}

	svc.log.Info("list_events", "calendar", args.Calendar, "start", args.Start, "end", args.End)

	type calInstances struct {
		name      string
		instances []icloud.Instance
		skipped   int
	}

	results, err := withClient(ctx, svc, func(c calendarClient) ([]calInstances, error) {
		cals, err := c.ListCalendars(ctx)
		if err != nil {
			return nil, err
		}

		var out []calInstances
		for _, cal := range cals {
			if !svc.cfg.IsCalendarAllowed(cal.Name) {
				continue
			}
			if args.Calendar != "" && cal.Name != args.Calendar {
				continue
			}

			objs, err := c.FetchObjects(ctx, cal.Path, start, end)
			if err != nil {
				return nil, fmt.Errorf("calendar %q: %w", cal.Name, err)
			}

			ci := calInstances{name: cal.Name}
			for _, obj := range objs {
				if obj.Data == nil {
					continue
				}
				events := obj.Data.Events()
				expanded, err := icloud.ExpandEvents(cal.Path, obj.Path, events, start, end, svc.cfg.DefaultLocation)
				if err != nil {
					// One malformed calendar object mustn't sink the whole
					// listing. Log only identifiers, never event content.
					svc.log.Warn("list_events: skipping unparseable calendar object",
						"object_path", obj.Path, "uid", firstUID(events), "error", err)
					ci.skipped++
					continue
				}
				ci.instances = append(ci.instances, expanded...)
			}
			out = append(out, ci)
		}
		return out, nil
	})
	if err != nil {
		return nil, ListEventsResult{}, err
	}

	type namedInstance struct {
		calendar string
		inst     icloud.Instance
	}
	var all []namedInstance
	var out ListEventsResult
	skipped := 0
	for _, cr := range results {
		for _, inst := range cr.instances {
			all = append(all, namedInstance{calendar: cr.name, inst: inst})
		}
		if cr.skipped > 0 {
			skipped += cr.skipped
			out.Warnings = append(out.Warnings, fmt.Sprintf("%d %s in calendar %q could not be parsed and %s omitted",
				cr.skipped, plural(cr.skipped, "entry", "entries"), cr.name, plural(cr.skipped, "was", "were")))
		}
	}
	// Sort by instant, not by the formatted RFC3339 string: strings with
	// different UTC offsets don't sort chronologically.
	sort.SliceStable(all, func(i, j int) bool { return all[i].inst.Start.Before(all[j].inst.Start) })
	for _, ni := range all {
		out.Events = append(out.Events, toEventOut(ni.calendar, ni.inst))
	}

	svc.log.Info("list_events done", "event_count", len(out.Events), "skipped_objects", skipped)
	return nil, out, nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func firstUID(events []ical.Event) string {
	for _, e := range events {
		if prop := e.Props.Get(ical.PropUID); prop != nil {
			return prop.Value
		}
	}
	return ""
}

// --- get_event ---

type GetEventArgs struct {
	ID string `json:"id" jsonschema:"The opaque event id returned by list_events or create_event."`
}

func (svc *Service) getEvent(ctx context.Context, _ *mcp.CallToolRequest, args GetEventArgs) (*mcp.CallToolResult, EventOut, error) {
	id, err := icloud.DecodeEventID(args.ID)
	if err != nil {
		return nil, EventOut{}, fmt.Errorf("invalid id: %w", err)
	}

	svc.log.Info("get_event")

	type found struct {
		calendar string
		inst     icloud.Instance
	}
	res, err := withClient(ctx, svc, func(c calendarClient) (found, error) {
		// Authorize before fetching anything: the id is client-supplied, so
		// its calendar must be an allow-listed calendar in this account and
		// its object must actually live in that calendar. Otherwise an id
		// pairing an allowed CalendarPath with another calendar's
		// ObjectPath would read that calendar's event.
		cals, err := c.ListCalendars(ctx)
		if err != nil {
			return found{}, err
		}
		calName := ""
		for _, cal := range cals {
			if cal.Path == id.CalendarPath && svc.cfg.IsCalendarAllowed(cal.Name) {
				calName = cal.Name
				break
			}
		}
		if calName == "" || !objectInCalendar(id.ObjectPath, id.CalendarPath) {
			return found{}, errEventNotAllowed
		}

		obj, err := c.GetObject(ctx, id.ObjectPath)
		if err != nil {
			return found{}, err
		}
		if obj.Data == nil {
			return found{}, fmt.Errorf("event not found")
		}

		// Expand over a generous window around the requested instant (or,
		// for a non-recurring event, around "now") so the single matching
		// occurrence is found regardless of how far out it is.
		anchor := time.Now()
		if id.RecurrenceID != nil {
			anchor = *id.RecurrenceID
		}
		windowStart := anchor.AddDate(-1, 0, 0)
		windowEnd := anchor.AddDate(1, 0, 0)

		instances, err := icloud.ExpandEvents(id.CalendarPath, id.ObjectPath, obj.Data.Events(), windowStart, windowEnd, svc.cfg.DefaultLocation)
		if err != nil {
			return found{}, err
		}
		for _, candidate := range instances {
			if matchesEventID(candidate, id) {
				return found{calendar: calName, inst: candidate}, nil
			}
		}
		return found{}, fmt.Errorf("event not found")
	})
	if err != nil {
		return nil, EventOut{}, err
	}

	return nil, toEventOut(res.calendar, res.inst), nil
}

var errEventNotAllowed = errors.New("event's calendar is not on this server's allowlist")

// objectInCalendar reports whether objectPath names a resource directly
// inside the calendar collection at calendarPath (CalDAV calendar objects
// are immediate children of their collection). An objectPath that isn't
// already in clean form (e.g. contains "..") is rejected outright rather
// than trusted to resolve the way it looks.
func objectInCalendar(objectPath, calendarPath string) bool {
	if !strings.HasPrefix(objectPath, "/") || !strings.HasPrefix(calendarPath, "/") {
		return false
	}
	if path.Clean(objectPath) != objectPath {
		return false
	}
	return path.Dir(objectPath) == path.Clean(calendarPath)
}

func matchesEventID(inst icloud.Instance, id icloud.EventID) bool {
	if inst.UID != id.UID {
		return false
	}
	if id.RecurrenceID == nil {
		return inst.RecurrenceID == nil
	}
	return inst.RecurrenceID != nil && inst.RecurrenceID.Equal(*id.RecurrenceID)
}

// --- create_event ---

type CreateEventArgs struct {
	Calendar string `json:"calendar" jsonschema:"The exact calendar name (from list_calendars) to create the event on."`
	Title    string `json:"title" jsonschema:"The event's title."`
	Start    string `json:"start" jsonschema:"Start time, as a full RFC3339 timestamp with a UTC offset."`
	End      string `json:"end" jsonschema:"End time, as a full RFC3339 timestamp with a UTC offset. Must be after start."`
	Location string `json:"location,omitempty" jsonschema:"Optional location text."`
	Notes    string `json:"notes,omitempty" jsonschema:"Optional free-text notes/description."`
}

func (svc *Service) createEvent(ctx context.Context, _ *mcp.CallToolRequest, args CreateEventArgs) (*mcp.CallToolResult, EventOut, error) {
	if strings.TrimSpace(args.Title) == "" {
		return nil, EventOut{}, fmt.Errorf("title must not be empty")
	}
	if !svc.cfg.IsCalendarAllowed(args.Calendar) {
		return nil, EventOut{}, fmt.Errorf("calendar %q is not on this server's allowlist", args.Calendar)
	}
	start, err := parseRFC3339(args.Start)
	if err != nil {
		return nil, EventOut{}, fmt.Errorf("start: %w", err)
	}
	end, err := parseRFC3339(args.End)
	if err != nil {
		return nil, EventOut{}, fmt.Errorf("end: %w", err)
	}
	if !end.After(start) {
		return nil, EventOut{}, fmt.Errorf("end (%s) must be after start (%s)", args.End, args.Start)
	}

	svc.log.Info("create_event", "calendar", args.Calendar)

	// Generate the UID once, outside the retried closure: if the first
	// attempt reached iCloud before a 401-triggered retry, the retry PUTs
	// the same <UID>.ics again (an idempotent overwrite) instead of
	// creating a duplicate event under a fresh UID.
	uid, err := icloud.NewEventUID()
	if err != nil {
		return nil, EventOut{}, fmt.Errorf("generating event UID: %w", err)
	}
	draft := icloud.EventDraft{
		UID:      uid,
		Title:    args.Title,
		Start:    start,
		End:      end,
		Location: args.Location,
		Notes:    args.Notes,
	}

	inst, err := withClient(ctx, svc, func(c calendarClient) (icloud.Instance, error) {
		cals, err := c.ListCalendars(ctx)
		if err != nil {
			return icloud.Instance{}, err
		}
		var calPath string
		for _, cal := range cals {
			if cal.Name == args.Calendar {
				calPath = cal.Path
				break
			}
		}
		if calPath == "" {
			return icloud.Instance{}, fmt.Errorf("calendar %q was not found in the iCloud account", args.Calendar)
		}
		return c.CreateEvent(ctx, calPath, draft)
	})
	if err != nil {
		return nil, EventOut{}, err
	}

	svc.log.Info("create_event done")
	return nil, toEventOut(args.Calendar, inst), nil
}

// --- shared helpers ---

func toEventOut(calendarName string, inst icloud.Instance) EventOut {
	out := EventOut{
		ID:        inst.ID(),
		Calendar:  calendarName,
		Title:     inst.Title,
		Start:     inst.Start.Format(time.RFC3339),
		End:       inst.End.Format(time.RFC3339),
		AllDay:    inst.AllDay,
		Location:  inst.Location,
		Notes:     inst.Notes,
		Recurring: inst.Recurring,
	}
	if inst.RecurrenceID != nil {
		out.RecurrenceID = inst.RecurrenceID.Format(time.RFC3339)
	}
	return out
}

func parseRFC3339(value string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("%q is not a valid RFC3339 timestamp with a UTC offset: %w", value, err)
	}
	return t, nil
}

// withClient loads the stored iCloud credentials and runs fn against a
// freshly built CalDAV client, retrying exactly once — with the credential
// cache invalidated first — if iCloud rejects them as unauthorized. This
// covers the case where the app-specific password was rotated via
// cmd/seed-credentials since this server instance last started.
//
// fn may therefore run twice, so it must be safe to repeat (see
// createEvent's UID handling).
func withClient[T any](ctx context.Context, svc *Service, fn func(calendarClient) (T, error)) (T, error) {
	var zero T

	client, err := svc.connect(ctx, false)
	if err != nil {
		return zero, err
	}

	result, err := fn(client)
	if err == nil || !isAuthError(err) {
		return result, err
	}

	svc.log.Info("iCloud rejected cached credentials, retrying once after refetch")
	client, err = svc.connect(ctx, true)
	if err != nil {
		return zero, fmt.Errorf("reconnecting after iCloud rejected credentials: %w", err)
	}
	return fn(client)
}

// isAuthError reports whether err is iCloud rejecting the credentials with
// HTTP 401. icloud.Client detects the status on the raw HTTP response (see
// icloud.ErrUnauthorized), so this doesn't depend on rendered error text,
// which can contain "401" for unrelated reasons (e.g. in a path).
func isAuthError(err error) bool {
	return errors.Is(err, icloud.ErrUnauthorized)
}
