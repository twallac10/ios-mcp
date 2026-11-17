# iCloud Calendar MCP Server

A read-only-by-default MCP server for your own iCloud calendars. Talks to
iCloud's CalDAV server (`caldav.icloud.com`) directly — no third-party
service in the middle. Runs as an Azure Functions self-hosted MCP server,
reachable from Claude web, mobile, and desktop as a custom connector — the
same hosting model as this account's Concept2 Logbook MCP server
([twallac10/concept2-mcp](https://github.com/twallac10/concept2-mcp)), adapted
from Python to Go.

## What it exposes

- `list_calendars` — the allow-listed iCloud calendars
- `list_events(calendar?, start, end)` — events in a time window, with
  recurring events already expanded into individual occurrences (one entry
  per occurrence, not one per series); the window may span at most 366
  days
- `get_event(id)` — full detail for one event instance
- `create_event(calendar, title, start, end, location?, notes?)` — create a
  single (non-recurring) event

There is **no update or delete tool in v1**, by design — see "Notes" below.

Only calendars on the configured allowlist (`ICLOUD_CALENDAR_ALLOWLIST`) are
visible to any tool, including `list_calendars` itself.

## Architecture

Two independent authentication layers, same shape as the Concept2 server:

1. **Claude ↔ this server**: Microsoft Entra ID, via Azure's built-in "Easy
   Auth" — no app code, configured entirely in the Azure Portal. See
   [docs/runbook.md](docs/runbook.md).
2. **This server ↔ iCloud's CalDAV API**: HTTP Basic Auth with an Apple ID
   and an **app-specific password** (generated at
   [appleid.apple.com](https://appleid.apple.com), under "App-Specific
   Passwords" — iCloud's CalDAV endpoint doesn't support OAuth). The
   credential is stored in Azure Key Vault and loaded via
   `azure-identity`'s `DefaultAzureCredential`, which resolves to the local
   `az login` session when run locally and to the Function App's Managed
   Identity in Azure — no environment-specific branching in code
   (`internal/credentials`).

This is simpler than Concept2's auth layer 2: an app-specific password is a
static credential with no refresh flow (it doesn't expire on a schedule; it
stays valid until revoked), so there's no bootstrap-browser-login dance —
just a one-time `go run ./cmd/seed-credentials` (see below).

```
Claude (web/mobile/desktop)
      │  MCP over Streamable HTTP + OAuth (Entra ID as AS)
      ▼
Azure Function App  ← Easy Auth (Entra ID) gates every request at the platform level
      │  (the Go binary only runs after Easy Auth validates the token)
      ▼
internal/mcptools (tool definitions)
      │  reads the iCloud credential, talks CalDAV
      ▼
Key Vault (iCloud app-specific password)  +  iCloud CalDAV (caldav.icloud.com)
```

### Recurrence is expanded in server code

Per the design brief, this server does **not** rely on iCloud's server-side
recurrence expansion (CalDAV's `CalendarExpandRequest`) — its handling of
`EXDATE` and `RECURRENCE-ID` overrides is inconsistent across providers.
Instead, `list_events` queries the raw VEVENTs (a recurring master plus zero
or more override instances sharing its UID) and expands them itself with
[`teambition/rrule-go`](https://github.com/teambition/rrule-go) via
[`emersion/go-ical`](https://github.com/emersion/go-ical)'s
`Component.RecurrenceSet`, applying `RECURRENCE-ID` overrides (including
`STATUS:CANCELLED` deletions) and `EXDATE` exclusions itself. See
`internal/icloud/recurrence.go` and its tests for the exact behavior,
including the weekly-series-with-exceptions, all-day, and DST-crossover
cases called out in the design brief.

### Why Go instead of Python (unlike Concept2)

Hosting model, MCP transport shape, both auth layers, and the deploy
approach (self-hosted Azure Functions custom handler, no CI/CD, manual
Portal provisioning) all mirror Concept2 directly. The implementation
language doesn't: Go, using the maintained
[`emersion/go-webdav`](https://github.com/emersion/go-webdav) +
[`emersion/go-ical`](https://github.com/emersion/go-ical) CalDAV/iCalendar
libraries and the official
[`modelcontextprotocol/go-sdk`](https://github.com/modelcontextprotocol/go-sdk)
in place of Concept2's Python `mcp` SDK. One consequence worth knowing
before you touch the deploy step: Go is compiled, so there's no Oryx
pip-install equivalent to go wrong at deploy time (see Concept2's Python
3.12-vs-3.14 Oryx footnote) — you ship a single static binary instead. See
[docs/runbook.md](docs/runbook.md) for exactly what that changes in the
deploy step (a zip deploy of `host.json` + the binary via
`az functionapp deployment source config-zip`).

## Local development

```bash
go build ./...
go test ./...
```

Run the server as a custom handler locally (requires [Azure Functions Core
Tools](https://learn.microsoft.com/en-us/azure/azure-functions/functions-run-local)
and the [Azurite](https://learn.microsoft.com/en-us/azure/storage/common/storage-install-azurite)
storage emulator):

```bash
go build -o ios-mcp-server ./cmd/server
func start
```

Or run the transport layer directly without the Functions host, for a quick
smoke test:

```bash
ICLOUD_KEY_VAULT_NAME=your-vault-name ICLOUD_CALENDAR_ALLOWLIST="Home,Family" go run ./cmd/server
```

`local.settings.json` (gitignored, not committed) is what `func start`
reads locally:

```json
{
    "IsEncrypted": false,
    "Values": {
        "FUNCTIONS_WORKER_RUNTIME": "custom",
        "AzureWebJobsFeatureFlags": "EnableMcpCustomHandlerPreview",
        "AzureWebJobsStorage": "UseDevelopmentStorage=true",
        "ICLOUD_KEY_VAULT_NAME": "REPLACE_WITH_KEYVAULT_NAME_FROM_RUNBOOK",
        "ICLOUD_CALENDAR_ALLOWLIST": "Home,Family",
        "ICLOUD_DEFAULT_TIMEZONE": "America/Chicago"
    }
}
```

## Configuration

| Setting | Required | Default | Notes |
|---|---|---|---|
| `ICLOUD_KEY_VAULT_NAME` | yes | — | Key Vault holding the `icloud-credentials` secret. |
| `ICLOUD_CALENDAR_ALLOWLIST` | yes (empty = nothing exposed) | — | Comma-separated exact calendar display names. |
| `ICLOUD_DEFAULT_TIMEZONE` | no | `America/Chicago` | IANA zone used for all-day events and any date without an explicit offset. |

## One-time iCloud login

```bash
ICLOUD_KEY_VAULT_NAME=your-vault-name go run ./cmd/seed-credentials
```

Prompts for your Apple ID and an app-specific password (generate one at
appleid.apple.com → Sign-In and Security → App-Specific Passwords) and
saves both to Key Vault. You must be signed in locally via `az login` with
write access to that vault (only this local step needs write access; the
deployed Function App's Managed Identity gets read-only `Key Vault Secrets
User`). Re-run this any time you revoke/rotate the
app-specific password — the server picks up the new value on its next
credential load, retrying once automatically if iCloud rejects a cached
one.

## Deploying to Azure

See [docs/runbook.md](docs/runbook.md) for the full manual Azure Portal
setup: Function App + Key Vault + Managed Identity provisioning, Easy Auth /
Entra ID configuration, registering "Claude" as an authorized OAuth client,
and the Go-specific build/publish steps.

## Connecting Claude

Add a custom connector in Claude (web: Settings → Connectors → Add custom
connector; syncs to mobile automatically) pointing at your Function App's
**custom domain** URL, e.g. `https://icloud-cal.mcp.t-wallace.com/mcp` — not
the default `azurewebsites.net` hostname, which Entra ID rejects for token
requests (see the runbook's custom-domain step; this is the same
`AADSTS9010010` issue Concept2's runbook hit). Enter the Client ID/Secret
from the "Claude" Entra ID App Registration created during the Azure
runbook. Claude Desktop uses the same custom connector.

## Yearly maintenance

| Credential | Expires | Renew with |
|---|---|---|
| iCloud app-specific password | Only if you revoke it | `go run ./cmd/seed-credentials` (see above) |
| Server app registration secret (`MICROSOFT_PROVIDER_AUTHENTICATION_SECRET`, used by Easy Auth) | Set at creation (180-day default) | `az ad app credential reset --id <server-app-id> --display-name easy-auth --years 1` then update the app setting via `az functionapp config appsettings set` |
| "Claude" app registration secret (entered in Claude's connector settings) | Set at creation | `az ad app credential reset --id <claude-app-id> --display-name claude-connector --years 1` then paste the new secret into Claude's connector settings — no CLI for that side |

If either Entra secret lapses, sign-in starts failing with a generic
Entra/Claude error, not an obvious "secret expired" message — if the
connector suddenly stops authenticating and nothing else changed, check
these dates first (same gotcha as Concept2).

## Logging

Structured JSON logs (`log/slog`) to stdout → Application Insights. At
Info level, logs carry tool names, calendar *names* (since they're also
config, not private event content), and counts/timestamps — never event
titles, locations, notes, or attendee data. See `internal/mcptools`.

## Notes

- No `update_event` / `delete_event` in v1 — edit or remove events in the
  Calendar app. If you want these later, gate them behind a config flag
  (an explicit opt-in env var, checked before registering the tools) rather
  than exposing them unconditionally, since the agent runs unattended.
- `get_event`/`create_event` IDs are opaque (base64 of calendar path +
  CalDAV object href + UID + optional recurrence instant) — don't try to
  parse them, they're not guaranteed stable across server versions.
- A `RECURRENCE-ID` override that reschedules an occurrence far outside the
  originally-requested window is best-effort: `list_events` pads its
  internal RRULE expansion by 48h on each side of the requested window to
  catch typical reschedules, but a large enough time shift could still miss
  it. Not expected to matter in practice for a personal calendar.
