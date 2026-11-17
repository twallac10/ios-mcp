// Package config loads this server's settings from the process environment.
//
// Local dev reads these from local.settings.json (via `func start`) or a
// `.env` file; in Azure they're Function App application settings.
package config

import (
	"fmt"
	"os"
	"strings"
	"time"
)

const DefaultTimezoneName = "America/Chicago"

// Config holds the server's runtime settings.
type Config struct {
	// KeyVaultName is the Azure Key Vault holding the iCloud credentials
	// secret (see internal/credentials). Required.
	KeyVaultName string

	// CalendarAllowlist is the set of iCloud calendar display names exposed
	// through the MCP tools. Calendars not on this list are invisible to
	// every tool, including list_calendars. Empty means nothing is exposed.
	CalendarAllowlist []string

	// DefaultLocation is used to interpret all-day events and any input
	// timestamp that omits a UTC offset. Defaults to America/Chicago.
	DefaultLocation *time.Location
}

// Load reads Config from the environment, returning an error if a required
// setting is missing or invalid.
func Load() (*Config, error) {
	vaultName := os.Getenv("ICLOUD_KEY_VAULT_NAME")
	if vaultName == "" {
		return nil, fmt.Errorf("ICLOUD_KEY_VAULT_NAME must be set")
	}

	tzName := os.Getenv("ICLOUD_DEFAULT_TIMEZONE")
	if tzName == "" {
		tzName = DefaultTimezoneName
	}
	loc, err := time.LoadLocation(tzName)
	if err != nil {
		return nil, fmt.Errorf("ICLOUD_DEFAULT_TIMEZONE %q is not a known timezone: %w", tzName, err)
	}

	return &Config{
		KeyVaultName:      vaultName,
		CalendarAllowlist: parseAllowlist(os.Getenv("ICLOUD_CALENDAR_ALLOWLIST")),
		DefaultLocation:   loc,
	}, nil
}

func parseAllowlist(raw string) []string {
	var names []string
	for _, part := range strings.Split(raw, ",") {
		name := strings.TrimSpace(part)
		if name != "" {
			names = append(names, name)
		}
	}
	return names
}

// IsCalendarAllowed reports whether the named calendar is on the allowlist.
// Matching is case-sensitive and exact, against the calendar's display name.
func (c *Config) IsCalendarAllowed(name string) bool {
	for _, allowed := range c.CalendarAllowlist {
		if allowed == name {
			return true
		}
	}
	return false
}
