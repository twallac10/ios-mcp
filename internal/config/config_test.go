package config

import "testing"

func TestParseAllowlist(t *testing.T) {
	cases := []struct {
		raw  string
		want []string
	}{
		{"", nil},
		{"Home", []string{"Home"}},
		{"Home, Work ,Family", []string{"Home", "Work", "Family"}},
		{" , ,", nil},
	}
	for _, c := range cases {
		got := parseAllowlist(c.raw)
		if len(got) != len(c.want) {
			t.Fatalf("parseAllowlist(%q) = %v, want %v", c.raw, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("parseAllowlist(%q) = %v, want %v", c.raw, got, c.want)
			}
		}
	}
}

func TestIsCalendarAllowed(t *testing.T) {
	cfg := &Config{CalendarAllowlist: []string{"Home", "Family"}}

	if !cfg.IsCalendarAllowed("Home") {
		t.Error("expected Home to be allowed")
	}
	if cfg.IsCalendarAllowed("Work") {
		t.Error("expected Work to be rejected")
	}
	if cfg.IsCalendarAllowed("home") {
		t.Error("matching should be case-sensitive and exact")
	}
}

func TestLoadRequiresKeyVaultName(t *testing.T) {
	t.Setenv("ICLOUD_KEY_VAULT_NAME", "")
	if _, err := Load(); err == nil {
		t.Fatal("expected an error when ICLOUD_KEY_VAULT_NAME is unset")
	}
}

func TestLoadDefaultsTimezoneToChicago(t *testing.T) {
	t.Setenv("ICLOUD_KEY_VAULT_NAME", "test-vault")
	t.Setenv("ICLOUD_DEFAULT_TIMEZONE", "")
	t.Setenv("ICLOUD_CALENDAR_ALLOWLIST", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.DefaultLocation.String() != "America/Chicago" {
		t.Errorf("DefaultLocation = %v, want America/Chicago", cfg.DefaultLocation)
	}
}

func TestLoadRejectsUnknownTimezone(t *testing.T) {
	t.Setenv("ICLOUD_KEY_VAULT_NAME", "test-vault")
	t.Setenv("ICLOUD_DEFAULT_TIMEZONE", "Not/ARealZone")

	if _, err := Load(); err == nil {
		t.Fatal("expected an error for an unknown timezone")
	}
}
