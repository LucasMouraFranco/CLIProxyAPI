package usage

import (
	"path/filepath"
	"testing"
	"time"
)

func TestCredentialTokenStoreKeepsSevenDaysOfHourlyBuckets(t *testing.T) {
	start := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	store, clock := newTestStore(start)

	store.Record("claude-a", TokenCounts{Requests: 1, OutputTokens: 5}, start.Add(5*time.Minute))
	store.Record("claude-a", TokenCounts{Requests: 1, Failed: 1}, start.Add(50*time.Minute))
	store.Record("claude-a", TokenCounts{Requests: 1, OutputTokens: 7}, start.Add(3*24*time.Hour))

	clock.now = start.Add(3*24*time.Hour + 30*time.Minute)
	snapshot, _ := store.Snapshot("claude-a")
	if len(snapshot.Hourly) != 2 {
		t.Fatalf("hourly = %+v, want the 09:00 hour and the hour three days later", snapshot.Hourly)
	}
	first := snapshot.Hourly[0]
	if !first.Time.Equal(start) || first.Requests != 2 || first.Failed != 1 || first.OutputTokens != 5 {
		t.Fatalf("first hour = %+v", first)
	}

	// Eight days on, the first hour has left the window.
	clock.now = start.Add(8 * 24 * time.Hour)
	snapshot, _ = store.Snapshot("claude-a")
	if len(snapshot.Hourly) != 1 || snapshot.Hourly[0].OutputTokens != 7 {
		t.Fatalf("hourly after a week = %+v, want only the later hour", snapshot.Hourly)
	}
}

func TestCredentialHistorySurvivesSaveAndLoad(t *testing.T) {
	start := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	saved, _ := newTestStore(start)
	saved.Record("claude-a", TokenCounts{Requests: 3, Failed: 1, CacheReadTokens: 900}, start.Add(10*time.Minute))

	previous := defaultCredentialTokens
	t.Cleanup(func() { defaultCredentialTokens = previous })
	defaultCredentialTokens = saved
	path := filepath.Join(t.TempDir(), CredentialHistoryFileName)
	if err := SaveCredentialHistory(path); err != nil {
		t.Fatal(err)
	}
	if saved.dirty {
		t.Fatal("saving should clear the dirty flag")
	}

	restored, _ := newTestStore(start.Add(2 * time.Hour))
	defaultCredentialTokens = restored
	if err := LoadCredentialHistory(path); err != nil {
		t.Fatal(err)
	}
	snapshot, ok := restored.Snapshot("claude-a")
	if !ok || len(snapshot.Hourly) != 1 {
		t.Fatalf("restored snapshot = %+v, ok=%v", snapshot, ok)
	}
	if got := snapshot.Hourly[0]; got.Requests != 3 || got.Failed != 1 || got.CacheReadTokens != 900 {
		t.Fatalf("restored hour = %+v", got)
	}
	if snapshot.Requests != 0 {
		t.Fatalf("totals = %d, want 0: they count only this process", snapshot.Requests)
	}

	if err := LoadCredentialHistory(filepath.Join(t.TempDir(), "missing.json")); err != nil {
		t.Fatalf("missing file should not be an error: %v", err)
	}
}
