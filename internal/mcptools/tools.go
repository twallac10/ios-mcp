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
	"sync"
	"time"

	ical "github.com/emersion/go-ical"
	"github.com/emersion/go-webdav/caldav"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/sync/errgroup"

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
	return &Service{cfg: cfg, log: log, connect: storeConnector(store, icloud.NewClient)}
}

// credentialSource is the subset of *credentials.Store storeConnector uses.
type credentialSource interface {
	Load(ctx context.Context) (*credentials.Credentials, error)
	Invalidate()
}

// storeConnector returns a connect func that reuses one client for as long
// as store keeps returning the same cached credentials, so the client's
// calendar-discovery cache (see icloud.Client.ListCalendars) survives
// across tool calls. A refresh invalidates the credentials, and the
// re-fetched ones get a fresh client with an empty discovery cache.
func storeConnector[C calendarClient](store credentialSource, newClient func(appleID, appSpecificPassword string) (C, error)) func(context.Context, bool) (calendarClient, error) {
	var (
		mu          sync.Mutex
		clientCreds *credentials.Credentials
		client      calendarClient
	)
	return func(ctx context.Context, refresh bool) (calendarClient, error) {
		mu.Lock()
		defer mu.Unlock()

		if refresh {
			store.Invalidate()
			clientCreds, client = nil, nil
		}
		creds, err := store.Load(ctx)
		if err != nil {
			if errors.Is(err, credentials.ErrNotFound) {
				return nil, fmt.Errorf("no iCloud credentials are configured; run cmd/seed-credentials once to store an Apple ID and app-specific password")
			}
			return nil, fmt.Errorf("loading iCloud credentials: %w", err)
		}
		if client != nil && creds == clientCreds {
			return client, nil
		}
		c, err := newClient(creds.AppleID, creds.AppSpecificPassword)
		if err != nil {
			return nil, err
		}
		clientCreds, client = creds, c
		return client, nil
	}
}

// Register adds every iCloud Calendar tool to s.
func (svc *Service) Register(s *mcp.Server) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "list_calendars",
		Description: "List the iCloud calendars this server is configured to expose (see the server's calendar allowlist). Call this first to get the exact calendar names to pass to list_events and create_event. " +
			"Each calendar also has an opaque `id`, which create_event accepts in place of the name; use the id when two calendars share a name.",
		Annotations: readOnly,
	}, svc.listCalendars)

	mcp.AddTool(s, &mcp.Tool{
		Name: "list_events",
		Description: "List calendar events in a time window, with recurring events already expanded into their individual occurrences (one entry per occurrence, not one per series). " +
			"Each returned event has an opaque `id` to pass to get_event. Times are always full RFC3339 timestamps with a UTC offset. " +
			"IMPORTANT: if the result's `incomplete` is true, some events could not be retrieved (e.g. one calendar failed to load; `warnings` says which), so the list is partial: " +
			"tell the user, and don't conclude from it that a time is free or that an event doesn't exist.",
		Annotations: readOnly,
	}, svc.listEvents)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_event",
		Description: "Get full detail for a single event instance by the `id` returned from list_events or create_event.",
		Annotations: readOnly,
	}, svc.getEvent)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "create_event",
		Description: "Create a single new (non-recurring) event on one of the allow-listed iCloud calendars, given by name or by its `id` from list_calendars. There is no update or delete tool in this server; to change or remove an event, do so in the Calendar app.",
		Annotations: writeNonDestructive,
	}, svc.createEvent)
}

// --- list_calendars ---

type ListCalendarsArgs struct{}

type CalendarOut struct {
	Name string `json:"name"`
	// ID is an opaque token for this calendar (see icloud.CalendarToken),
	// which create_event accepts in place of Name — needed when two
	// calendars share a name.
	ID string `json:"id" jsonschema:"Opaque calendar id; create_event accepts it in place of the name, which is needed when two calendars share a name."`
}

type ListCalendarsResult struct {
	Calendars []CalendarOut `json:"calendars"`
	// Warnings notes allow-listed names shared by more than one calendar,
	// which create_event can only address by id.
	Warnings []string `json:"warnings,omitempty"`
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
	counts := map[string]int{}
	for _, cal := range allowedCalendars(cals, svc.cfg) {
		counts[cal.Name]++
		out.Calendars = append(out.Calendars, CalendarOut{Name: cal.Name, ID: icloud.CalendarToken(cal.Path)})
	}
	sort.Slice(out.Calendars, func(i, j int) bool {
		a, b := out.Calendars[i], out.Calendars[j]
		return a.Name < b.Name || (a.Name == b.Name && a.ID < b.ID)
	})
	for i, c := range out.Calendars {
		if n := counts[c.Name]; n > 1 && (i == 0 || out.Calendars[i-1].Name != c.Name) {
			out.Warnings = append(out.Warnings, fmt.Sprintf(
				"%d calendars are named %q; list_events includes all of them, but create_event can't tell them apart by name, so pass the id of the one you mean instead", n, c.Name))
		}
	}

	svc.log.Info("list_calendars done", "allowed_count", len(out.Calendars))
	return nil, out, nil
}

// --- list_events ---

type ListEventsArgs struct {
	Calendar string `json:"calendar,omitempty" jsonschema:"Restrict results to this calendar's exact name (from list_calendars). Omit to search every allow-listed calendar."`
	Start    string `json:"start" jsonschema:"Inclusive start of the time window, as a full RFC3339 timestamp with a UTC offset, e.g. 2026-11-17T00:00:00-06:00."`
	End      string `json:"end" jsonschema:"Exclusive end of the time window, as a full RFC3339 timestamp with a UTC offset. At most 366 days after start."`
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
	// TimezoneWarning is set when the event's time zone couldn't be
	// identified and its times were interpreted in the server's default
	// zone instead, so Start/End may be off by some hours.
	TimezoneWarning string `json:"timezone_warning,omitempty"`
}

type ListEventsResult struct {
	Events []EventOut `json:"events"`
	// Incomplete is set whenever events may be missing from Events (a
	// calendar couldn't be queried, or entries couldn't be parsed). It's
	// always present, unlike Warnings, so a caller checking only for an
	// error can't mistake a partial listing for a complete one as easily.
	Incomplete bool `json:"incomplete" jsonschema:"True when some events could not be retrieved and may be missing from events (see warnings for which calendars). Treat the list as partial: don't conclude that a time is free or that an event doesn't exist."`
	// Warnings notes anything left out of Events, e.g. calendar entries
	// that couldn't be parsed. Never contains event content.
	Warnings []string `json:"warnings,omitempty" jsonschema:"What was left out of events and why (e.g. a calendar that failed to load). Non-empty means the listing is incomplete; see incomplete."`
}

// maxListWindow caps list_events' window, so one call can't expand e.g. a
// daily series across decades into a response of tens of thousands of
// occurrences.
const maxListWindow = 366 * 24 * time.Hour

// maxConcurrentCalendarFetches caps how many calendars list_events queries
// at once: enough to overlap most of the latency for a typical account,
// few enough not to hammer one account's iCloud quota (or get throttled)
// when it has dozens of shared/subscribed calendars.
const maxConcurrentCalendarFetches = 4

// calInstances is one calendar's contribution to a list_events call.
type calInstances struct {
	name           string
	instances      []icloud.Instance
	skipped        int
	ignoredMasters int
	fetchErr       error
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
	if end.Sub(start) > maxListWindow {
		return nil, ListEventsResult{}, fmt.Errorf("the time window is too long (at most %d days); split it into smaller windows", int(maxListWindow/(24*time.Hour)))
	}

	if args.Calendar != "" && !svc.cfg.IsCalendarAllowed(args.Calendar) {
		return nil, ListEventsResult{}, fmt.Errorf("calendar %q is not on this server's allowlist", args.Calendar)
	}

	svc.log.Info("list_events", "calendar", args.Calendar, "start", args.Start, "end", args.End)

	// Credentials are refreshed (by withClient) only if listing the
	// calendars themselves is rejected; a 401 on a per-calendar query is
	// retried below for just the calendars that got one, so one rejected
	// query doesn't redo every other calendar's successful one.
	type listing struct {
		client calendarClient
		cals   []icloud.Calendar
	}
	l, err := withClient(ctx, svc, func(c calendarClient) (listing, error) {
		cals, err := c.ListCalendars(ctx)
		return listing{client: c, cals: cals}, err
	})
	if err != nil {
		return nil, ListEventsResult{}, err
	}

	var targets []icloud.Calendar
	for _, cal := range allowedCalendars(l.cals, svc.cfg) {
		if args.Calendar == "" || cal.Name == args.Calendar {
			targets = append(targets, cal)
		}
	}
	if args.Calendar != "" && len(targets) == 0 {
		// Allow-listed, but not in the account (e.g. deleted): say so, as
		// create_event does, rather than look like a calendar with no events.
		return nil, ListEventsResult{}, fmt.Errorf("calendar %q was not found in the iCloud account", args.Calendar)
	}

	results := make([]calInstances, len(targets))
	every := make([]int, len(targets))
	for i := range every {
		every[i] = i
	}
	svc.fetchCalendars(ctx, l.client, targets, every, results, start, end)

	var authFailed []int
	for i, r := range results {
		if r.fetchErr != nil && isAuthError(r.fetchErr) {
			authFailed = append(authFailed, i)
		}
	}
	if len(authFailed) > 0 && ctx.Err() == nil {
		svc.log.Info("iCloud rejected cached credentials for some calendars, retrying only those once after refetch",
			"calendars", len(authFailed), "calendars_total", len(targets))
		c, err := svc.connect(ctx, true)
		if err != nil {
			return nil, ListEventsResult{}, fmt.Errorf("reconnecting after iCloud rejected credentials: %w", err)
		}
		svc.fetchCalendars(ctx, c, targets, authFailed, results, start, end)
	}

	// Like a malformed object, one calendar that can't be queried mustn't
	// sink the rest — unless it's (still) an auth failure, or cancellation,
	// or nothing could be queried.
	failed := 0
	for _, ci := range results {
		if ci.fetchErr == nil {
			continue
		}
		if isAuthError(ci.fetchErr) || ctx.Err() != nil {
			return nil, ListEventsResult{}, fmt.Errorf("calendar %q: %w", ci.name, ci.fetchErr)
		}
		failed++
	}
	if failed > 0 && failed == len(results) {
		return nil, ListEventsResult{}, fmt.Errorf("calendar %q: %w", results[0].name, results[0].fetchErr)
	}

	type namedInstance struct {
		calendar string
		inst     icloud.Instance
	}
	var all []namedInstance
	var out ListEventsResult
	skipped := 0
	for _, cr := range results {
		if cr.fetchErr != nil {
			svc.log.Warn("list_events: skipping calendar that could not be queried", "calendar", cr.name, "error", cr.fetchErr)
			out.Warnings = append(out.Warnings, fmt.Sprintf("calendar %q could not be queried, so its events are omitted", cr.name))
			out.Incomplete = true
			continue
		}
		for _, inst := range cr.instances {
			all = append(all, namedInstance{calendar: cr.name, inst: inst})
		}
		if cr.skipped > 0 {
			skipped += cr.skipped
			out.Incomplete = true
			out.Warnings = append(out.Warnings, fmt.Sprintf("%d %s in calendar %q could not be parsed and %s omitted",
				cr.skipped, plural(cr.skipped, "entry", "entries"), cr.name, plural(cr.skipped, "was", "were")))
		}
		if cr.ignoredMasters > 0 {
			out.Incomplete = true
			out.Warnings = append(out.Warnings, fmt.Sprintf("%d %s in calendar %q held more than one main event; only the first of each was used",
				cr.ignoredMasters, plural(cr.ignoredMasters, "entry", "entries"), cr.name))
		}
	}
	// Sort by instant, not by the formatted RFC3339 string: strings with
	// different UTC offsets don't sort chronologically.
	sort.SliceStable(all, func(i, j int) bool { return all[i].inst.Start.Before(all[j].inst.Start) })
	for _, ni := range all {
		out.Events = append(out.Events, svc.toEventOut(ni.calendar, ni.inst))
	}

	svc.log.Info("list_events done", "event_count", len(out.Events), "skipped_objects", skipped, "incomplete", out.Incomplete)
	return nil, out, nil
}

// fetchCalendars queries targets[i] for each i in indices with client c,
// at most maxConcurrentCalendarFetches at a time, storing each calendar's
// outcome in results[i]. Each goroutine writes only its own slot, so
// results keeps targets' order regardless of completion order.
func (svc *Service) fetchCalendars(ctx context.Context, c calendarClient, targets []icloud.Calendar, indices []int, results []calInstances, start, end time.Time) {
	var g errgroup.Group
	g.SetLimit(maxConcurrentCalendarFetches)
	for _, i := range indices {
		g.Go(func() error {
			results[i] = svc.fetchCalendar(ctx, c, targets[i], start, end)
			return nil
		})
	}
	g.Wait() // never returns an error: each one is kept in its result
}

// fetchCalendar queries and expands one calendar's events in [start, end).
func (svc *Service) fetchCalendar(ctx context.Context, c calendarClient, cal icloud.Calendar, start, end time.Time) calInstances {
	ci := calInstances{name: cal.Name}
	objs, err := c.FetchObjects(ctx, cal.Path, start, end)
	if err != nil {
		ci.fetchErr = err
		return ci
	}
	for _, obj := range objs {
		if obj.Data == nil {
			continue
		}
		events := obj.Data.Events()
		x, err := icloud.Expand(cal.Path, obj.Path, events, icloud.Timezones(obj.Data), start, end, svc.cfg.DefaultLocation)
		if err != nil {
			// One malformed calendar object mustn't sink the whole
			// listing. Log only identifiers, never event content.
			svc.log.Warn("list_events: skipping unparseable calendar object",
				"object_path", obj.Path, "uid", firstUID(events), "error", err)
			ci.skipped++
			continue
		}
		svc.logExpansionDiagnostics("list_events", obj.Path, events, x)
		if x.IgnoredMasters > 0 {
			ci.ignoredMasters++
		}
		ci.instances = append(ci.instances, x.Instances...)
	}
	return ci
}

// logExpansionDiagnostics logs, by identifiers only (never event content),
// anything Expand had to guess at or ignore in one calendar object.
func (svc *Service) logExpansionDiagnostics(tool, objectPath string, events []ical.Event, x icloud.Expansion) {
	if x.IgnoredMasters > 0 {
		svc.log.Warn(tool+": calendar object has more than one VEVENT without RECURRENCE-ID; only the first was used",
			"object_path", objectPath, "uid", firstUID(events), "ignored", x.IgnoredMasters)
	}
	for _, tzid := range x.FallbackTZIDs {
		svc.log.Warn(tool+": unrecognized TZID; interpreted in the default time zone",
			"object_path", objectPath, "uid", firstUID(events), "tzid", tzid, "fallback_zone", svc.cfg.DefaultLocation.String())
	}
	for _, tzid := range x.ExclusionFallbackTZIDs {
		svc.log.Warn(tool+": unrecognized TZID on an EXDATE; interpreted in the default time zone, so an excluded occurrence may still be listed",
			"object_path", objectPath, "uid", firstUID(events), "tzid", tzid, "fallback_zone", svc.cfg.DefaultLocation.String())
	}
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
		cal, ok := findCalendar(allowedCalendars(cals, svc.cfg), func(cal icloud.Calendar) bool {
			return cal.Path == id.CalendarPath
		})
		if !ok || !objectInCalendar(id.ObjectPath, id.CalendarPath) {
			return found{}, errEventNotAllowed
		}

		obj, err := c.GetObject(ctx, id.ObjectPath)
		if err != nil {
			return found{}, err
		}
		if obj.Data == nil {
			return found{}, fmt.Errorf("event not found")
		}

		// A non-recurring event is returned wherever in time it falls; so
		// is one occurrence of a recurring series, looked up by its slot
		// (RECURRENCE-ID) — even if an override moved it arbitrarily far.
		events := obj.Data.Events()
		timezones := icloud.Timezones(obj.Data)
		var x icloud.Expansion
		if id.RecurrenceID == nil {
			x, err = icloud.SingleEvent(id.CalendarPath, id.ObjectPath, events, timezones, svc.cfg.DefaultLocation)
		} else {
			x, err = icloud.Occurrence(id.CalendarPath, id.ObjectPath, events, timezones, *id.RecurrenceID, svc.cfg.DefaultLocation)
		}
		if err != nil {
			return found{}, err
		}
		svc.logExpansionDiagnostics("get_event", id.ObjectPath, events, x)
		for _, candidate := range x.Instances {
			if matchesEventID(candidate, id) {
				return found{calendar: cal.Name, inst: candidate}, nil
			}
		}
		return found{}, fmt.Errorf("event not found")
	})
	if err != nil {
		return nil, EventOut{}, err
	}

	return nil, svc.toEventOut(res.calendar, res.inst), nil
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
	Calendar string `json:"calendar" jsonschema:"The calendar to create the event on: its exact name, or its id, both from list_calendars. Use the id when two calendars share a name."`
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
	// A calendar id's name is only known once it's resolved, so its
	// allowlist check happens then (see resolveCalendar).
	if _, isID := icloud.DecodeCalendarToken(args.Calendar); !isID && !svc.cfg.IsCalendarAllowed(args.Calendar) {
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

	type created struct {
		calendar string
		inst     icloud.Instance
	}
	res, err := withClient(ctx, svc, func(c calendarClient) (created, error) {
		cals, err := c.ListCalendars(ctx)
		if err != nil {
			return created{}, err
		}
		cal, err := resolveCalendar(cals, svc.cfg, args.Calendar)
		if err != nil {
			return created{}, err
		}
		inst, err := c.CreateEvent(ctx, cal.Path, draft)
		return created{calendar: cal.Name, inst: inst}, err
	})
	if err != nil {
		return nil, EventOut{}, err
	}

	svc.log.Info("create_event done")
	return nil, svc.toEventOut(res.calendar, res.inst), nil
}

// --- shared helpers ---

// allowedCalendars returns the calendars in cals that are on cfg's
// allowlist, in their original order.
func allowedCalendars(cals []icloud.Calendar, cfg *config.Config) []icloud.Calendar {
	var out []icloud.Calendar
	for _, cal := range cals {
		if cfg.IsCalendarAllowed(cal.Name) {
			out = append(out, cal)
		}
	}
	return out
}

// findCalendar returns the first calendar in cals matching pred.
func findCalendar(cals []icloud.Calendar, pred func(icloud.Calendar) bool) (icloud.Calendar, bool) {
	for _, cal := range cals {
		if pred(cal) {
			return cal, true
		}
	}
	return icloud.Calendar{}, false
}

// resolveCalendar resolves create_event's calendar argument, which is
// either a calendar id from list_calendars (see icloud.CalendarToken) or a
// display name (see calendarByName), to an allow-listed calendar in cals.
func resolveCalendar(cals []icloud.Calendar, cfg *config.Config, ref string) (icloud.Calendar, error) {
	if calPath, ok := icloud.DecodeCalendarToken(ref); ok {
		cal, found := findCalendar(cals, func(cal icloud.Calendar) bool { return cal.Path == calPath })
		if !found {
			return icloud.Calendar{}, fmt.Errorf("calendar id %q doesn't match any calendar in the iCloud account; call list_calendars for current ids", ref)
		}
		if !cfg.IsCalendarAllowed(cal.Name) {
			return icloud.Calendar{}, fmt.Errorf("calendar %q is not on this server's allowlist", cal.Name)
		}
		return cal, nil
	}
	if !cfg.IsCalendarAllowed(ref) {
		return icloud.Calendar{}, fmt.Errorf("calendar %q is not on this server's allowlist", ref)
	}
	return calendarByName(cals, ref)
}

// errAmbiguousCalendar is returned (wrapped) by calendarByName when more
// than one calendar has the requested name.
var errAmbiguousCalendar = errors.New("calendar name is ambiguous")

// calendarByName resolves a calendar display name to the single calendar
// with that name. iCloud lets two calendars (e.g. one's own and one shared
// from another account) share a name, so rather than arbitrarily picking
// one — and, say, writing an event to the wrong calendar — an ambiguous
// name is an error; the caller can use the calendar's id instead (see
// resolveCalendar).
func calendarByName(cals []icloud.Calendar, name string) (icloud.Calendar, error) {
	var match icloud.Calendar
	n := 0
	for _, cal := range cals {
		if cal.Name == name {
			match = cal
			n++
		}
	}
	switch n {
	case 0:
		return icloud.Calendar{}, fmt.Errorf("calendar %q was not found in the iCloud account", name)
	case 1:
		return match, nil
	default:
		return icloud.Calendar{}, fmt.Errorf("%w: %d calendars in the iCloud account are named %q, so it's unclear which one to use; pass the id of the one you mean (from list_calendars) instead", errAmbiguousCalendar, n, name)
	}
}

func (svc *Service) toEventOut(calendarName string, inst icloud.Instance) EventOut {
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
	if inst.TZIDFallback != "" {
		out.TimezoneWarning = fmt.Sprintf("the event's time zone %q isn't recognized, so its times were read as %s time and may be wrong",
			inst.TZIDFallback, svc.cfg.DefaultLocation)
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
