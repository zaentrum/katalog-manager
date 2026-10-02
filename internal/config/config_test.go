package config

import (
	"testing"
	"time"
)

// TMDB_REFRESH_INTERVAL is a Go duration, 24h when unset or unreadable; 0 and
// off turn the change-list refresh off.
func TestTMDBRefreshInterval(t *testing.T) {
	for v, want := range map[string]time.Duration{
		"": 24 * time.Hour, "6h": 6 * time.Hour, "90m": 90 * time.Minute, "0": 0, "off": 0, "OFF": 0,
		"false": 0, "disabled": 0, "daily": 24 * time.Hour, "-1h": 0,
	} {
		t.Setenv("TMDB_REFRESH_INTERVAL", v)
		if got := Load().TMDBRefreshInterval; got != want {
			t.Errorf("TMDB_REFRESH_INTERVAL=%q: %s, want %s", v, got, want)
		}
	}
}
