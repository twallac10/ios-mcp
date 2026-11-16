// Self-hosted MCP server for iCloud Calendar, deployed as an Azure
// Functions custom handler (see host.json) — the same hosting model as the
// Concept2 MCP server.
//
// Client-facing OAuth (Claude <-> this server) is handled entirely by
// Azure's built-in authentication ("Easy Auth") against Microsoft Entra ID,
// configured at the platform level in the Azure Portal — no auth code lives
// here. The separate server <-> iCloud CalDAV authentication (HTTP Basic
// Auth with an Apple ID + app-specific password, loaded from Key Vault) is
// unrelated and untouched by this file; see internal/credentials and
// internal/icloud.
//
// Local dev: `func start` (Azure Functions Core Tools) reads host.json /
// local.settings.json and proxies HTTP requests to this process on the port
// declared in host.json. You can also run it directly with
// `go run ./cmd/server` for a quick transport-level smoke test.
package main

import (
	"log/slog"
	"net/http"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	// Ensure the IANA timezone database is embedded in the binary, so
	// America/Chicago (and any TZID in a fetched event) resolves correctly
	// regardless of whether the host OS ships tzdata.
	_ "time/tzdata"

	"github.com/twallac10/ios-mcp/internal/config"
	"github.com/twallac10/ios-mcp/internal/credentials"
	"github.com/twallac10/ios-mcp/internal/mcptools"
)

const defaultPort = "8000"

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	cfg, err := config.Load()
	if err != nil {
		log.Error("failed to load configuration", "error", err)
		os.Exit(1)
	}
	if len(cfg.CalendarAllowlist) == 0 {
		log.Warn("ICLOUD_CALENDAR_ALLOWLIST is empty; no calendars will be exposed")
	}

	store, err := credentials.NewStore(cfg.KeyVaultName)
	if err != nil {
		log.Error("failed to create Key Vault credential store", "error", err)
		os.Exit(1)
	}

	svc := mcptools.NewService(cfg, store, log)

	server := mcp.NewServer(&mcp.Implementation{Name: "icloud-calendar", Version: "0.1.0"}, nil)
	svc.Register(server)

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return server
	}, &mcp.StreamableHTTPOptions{Stateless: true})

	mux := http.NewServeMux()
	mux.Handle("/mcp", handler)

	port := os.Getenv("FUNCTIONS_CUSTOMHANDLER_PORT")
	if port == "" {
		port = defaultPort
	}

	log.Info("starting iCloud Calendar MCP server", "port", port, "calendar_count", len(cfg.CalendarAllowlist))
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Error("server exited", "error", err)
		os.Exit(1)
	}
}
