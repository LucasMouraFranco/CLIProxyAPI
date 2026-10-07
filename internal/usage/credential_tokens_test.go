package usage

import (
	"context"
	"testing"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

type testClock struct{ now time.Time }

func (c *testClock) Now() time.Time { return c.now }

func newTestStore(start time.Time) (*CredentialTokenStore, *testClock) {
	clock := &testClock{now: start}
	store := NewCredentialTokenStore()
	store.nowFunc = clock.Now
	return store, clock
}

func TestCredentialTokenStoreAggregatesTotalsAndRecentBuckets(t *testing.T) {
	start := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	store, clock := newTestStore(start)

	store.Record("claude-a", TokenCounts{Requests: 1, UncachedInputTokens: 10, OutputTokens: 5, CacheReadTokens: 1000, CacheWriteTokens: 200}, start.Add(time.Minute))
	store.Record("claude-a", TokenCounts{Requests: 1, UncachedInputTokens: 20, OutputTokens: 7, CacheReadTokens: 3000}, start.Add(2*time.Minute))
	store.Record("claude-a", TokenCounts{Requests: 1, CacheWriteTokens: 50000}, start.Add(25*time.Minute))
	store.Record("claude-b", TokenCounts{Requests: 1, CacheReadTokens: 9}, start.Add(3*time.Minute))

	clock.now = start.Add(29 * time.Minute)
	snapshot, ok := store.Snapshot("claude-a")
	if !ok {
		t.Fatal("expected a snapshot for claude-a")
	}
	want := TokenCounts{Requests: 3, UncachedInputTokens: 30, OutputTokens: 12, CacheReadTokens: 4000, CacheWriteTokens: 50200}
	if snapshot.TokenCounts != want {
		t.Fatalf("totals = %+v, want %+v", snapshot.TokenCounts, want)
	}
	if !snapshot.Since.Equal(start.Add(time.Minute)) || !snapshot.LastSeen.Equal(start.Add(25*time.Minute)) {
		t.Fatalf("since/last seen = %s/%s", snapshot.Since, snapshot.LastSeen)
	}
	if len(snapshot.Recent) != CredentialTokenBucketCount {
		t.Fatalf("recent buckets = %d, want %d", len(snapshot.Recent), CredentialTokenBucketCount)
	}
	last := snapshot.Recent[len(snapshot.Recent)-1]
	if !last.Time.Equal(start.Add(20*time.Minute)) || last.CacheWriteTokens != 50000 {
		t.Fatalf("newest bucket = %+v, want the 12:20 cache-write spike", last)
	}
	first12 := snapshot.Recent[len(snapshot.Recent)-3]
	if !first12.Time.Equal(start) || first12.CacheReadTokens != 4000 || first12.CacheWriteTokens != 200 || first12.Requests != 2 {
		t.Fatalf("12:00 bucket = %+v", first12)
	}
	if empty := snapshot.Recent[len(snapshot.Recent)-2]; empty.Requests != 0 || !empty.Time.Equal(start.Add(10*time.Minute)) {
		t.Fatalf("idle bucket = %+v, want zero counts at 12:10", empty)
	}

	other, ok := store.Snapshot("claude-b")
	if !ok || other.CacheReadTokens != 9 {
		t.Fatalf("claude-b snapshot = %+v, ok=%v", other, ok)
	}
	if _, ok := store.Snapshot("unknown"); ok {
		t.Fatal("unknown credential should have no snapshot")
	}
}

func TestCredentialTokenStoreExpiresOldBuckets(t *testing.T) {
	start := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	store, clock := newTestStore(start)
	store.Record("codex-a", TokenCounts{Requests: 1, CacheWriteTokens: 700}, start)

	// Exactly one full ring later the slot is reused; the old bucket must not leak into the window.
	clock.now = start.Add(CredentialTokenBucketCount * CredentialTokenBucketWidth)
	store.Record("codex-a", TokenCounts{Requests: 1, CacheReadTokens: 5}, clock.now)

	snapshot, ok := store.Snapshot("codex-a")
	if !ok {
		t.Fatal("expected a snapshot")
	}
	var recentWrites, recentReads int64
	for _, bucket := range snapshot.Recent {
		recentWrites += bucket.CacheWriteTokens
		recentReads += bucket.CacheReadTokens
	}
	if recentWrites != 0 || recentReads != 5 {
		t.Fatalf("recent writes/reads = %d/%d, want 0/5", recentWrites, recentReads)
	}
	if snapshot.CacheWriteTokens != 700 {
		t.Fatalf("lifetime cache writes = %d, want 700", snapshot.CacheWriteTokens)
	}
}

func TestCredentialTokenPluginUsesProviderAccounting(t *testing.T) {
	start := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	store, _ := newTestStore(start)
	plugin := credentialTokenPlugin{store: store}

	// Claude reports cache buckets independently of input_tokens.
	plugin.HandleUsage(context.Background(), coreusage.Record{
		Provider:    "claude",
		AuthID:      "claude-a",
		RequestedAt: start,
		Detail: coreusage.Detail{
			InputTokens:         12,
			OutputTokens:        40,
			CacheReadTokens:     90000,
			CacheCreationTokens: 3000,
		},
	})
	// Codex reports cached tokens as a subset of input_tokens.
	plugin.HandleUsage(context.Background(), coreusage.Record{
		Provider:    "codex",
		AuthID:      "codex-a",
		RequestedAt: start,
		Detail: coreusage.Detail{
			InputTokens:     10000,
			OutputTokens:    300,
			CachedTokens:    8000,
			CacheReadTokens: 8000,
		},
	})
	// Records without a credential are ignored.
	plugin.HandleUsage(context.Background(), coreusage.Record{Provider: "claude", Detail: coreusage.Detail{InputTokens: 1}})

	claude, ok := store.Snapshot("claude-a")
	if !ok {
		t.Fatal("expected claude-a snapshot")
	}
	if claude.UncachedInputTokens != 12 || claude.CacheReadTokens != 90000 || claude.CacheWriteTokens != 3000 || claude.OutputTokens != 40 {
		t.Fatalf("claude counts = %+v", claude.TokenCounts)
	}
	codex, ok := store.Snapshot("codex-a")
	if !ok {
		t.Fatal("expected codex-a snapshot")
	}
	if codex.UncachedInputTokens != 2000 || codex.CacheReadTokens != 8000 || codex.CacheWriteTokens != 0 || codex.OutputTokens != 300 {
		t.Fatalf("codex counts = %+v", codex.TokenCounts)
	}
}

func TestCredentialTokenStoreEvictsLeastRecentlySeen(t *testing.T) {
	start := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	store, _ := newTestStore(start)
	for i := 0; i < credentialTokenMaxEntries; i++ {
		store.Record(string(rune('a'+i%26))+time.Duration(i).String(), TokenCounts{Requests: 1}, start.Add(time.Duration(i)*time.Second))
	}
	store.Record("newest", TokenCounts{Requests: 1}, start.Add(48*time.Hour))
	if len(store.entries) > credentialTokenMaxEntries {
		t.Fatalf("entries = %d, want at most %d", len(store.entries), credentialTokenMaxEntries)
	}
	if _, ok := store.Snapshot("newest"); !ok {
		t.Fatal("newest credential should be kept")
	}
	if _, ok := store.Snapshot("a0s"); ok {
		t.Fatal("least recently seen credential should be evicted")
	}
}
