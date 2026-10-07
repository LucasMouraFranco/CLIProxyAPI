// Package usage keeps in-memory per-credential token totals so the management
// API can show how each account's prompt cache behaves.
package usage

import (
	"context"
	"strings"
	"sync"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

const (
	// CredentialTokenBucketWidth is the width of one recent-activity bucket.
	CredentialTokenBucketWidth = 10 * time.Minute
	// CredentialTokenBucketCount is the number of recent buckets kept per credential (3 hours).
	CredentialTokenBucketCount = 18

	credentialTokenPluginName  = "credential-token-stats"
	credentialTokenMaxEntries  = 4096
	credentialTokenEvictBatchN = 64
)

// TokenCounts holds non-overlapping token buckets for a set of requests.
type TokenCounts struct {
	Requests            int64 `json:"requests"`
	UncachedInputTokens int64 `json:"uncached_input_tokens"`
	OutputTokens        int64 `json:"output_tokens"`
	CacheReadTokens     int64 `json:"cache_read_tokens"`
	CacheWriteTokens    int64 `json:"cache_write_tokens"`
}

// TokenBucket is one fixed-width slice of recent activity.
type TokenBucket struct {
	Time time.Time `json:"time"`
	TokenCounts
}

// CredentialTokens is a snapshot of one credential's token usage since the process started.
type CredentialTokens struct {
	TokenCounts
	Since    time.Time     `json:"since"`
	LastSeen time.Time     `json:"last_seen"`
	Recent   []TokenBucket `json:"recent"`
}

type credentialTokenEntry struct {
	totals   TokenCounts
	since    time.Time
	lastSeen time.Time
	buckets  [CredentialTokenBucketCount]TokenBucket
}

// CredentialTokenStore aggregates usage records by credential ID.
type CredentialTokenStore struct {
	mu      sync.Mutex
	nowFunc func() time.Time
	entries map[string]*credentialTokenEntry
}

// NewCredentialTokenStore creates an empty store using the wall clock.
func NewCredentialTokenStore() *CredentialTokenStore {
	return &CredentialTokenStore{nowFunc: time.Now, entries: make(map[string]*credentialTokenEntry)}
}

func (s *CredentialTokenStore) now() time.Time {
	if s.nowFunc == nil {
		return time.Now()
	}
	return s.nowFunc()
}

// Record adds one usage record to the credential's totals and recent buckets.
func (s *CredentialTokenStore) Record(authID string, counts TokenCounts, at time.Time) {
	authID = strings.TrimSpace(authID)
	if s == nil || authID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if at.IsZero() {
		at = s.now()
	}
	entry := s.entries[authID]
	if entry == nil {
		s.evictLocked()
		entry = &credentialTokenEntry{since: at}
		s.entries[authID] = entry
	}
	entry.totals.add(counts)
	if at.After(entry.lastSeen) {
		entry.lastSeen = at
	}
	if at.Before(entry.since) {
		entry.since = at
	}

	start := at.Truncate(CredentialTokenBucketWidth)
	slot := &entry.buckets[bucketSlot(start)]
	if !slot.Time.Equal(start) {
		if slot.Time.After(start) {
			// The slot already holds a newer window; this record is too old to bucket.
			return
		}
		*slot = TokenBucket{Time: start}
	}
	slot.add(counts)
}

// Snapshot returns the credential's totals and its recent buckets ordered oldest
// first. Buckets with no activity are returned as zero-valued entries.
func (s *CredentialTokenStore) Snapshot(authID string) (CredentialTokens, bool) {
	authID = strings.TrimSpace(authID)
	if s == nil || authID == "" {
		return CredentialTokens{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.entries[authID]
	if entry == nil {
		return CredentialTokens{}, false
	}
	current := s.now().Truncate(CredentialTokenBucketWidth)
	recent := make([]TokenBucket, CredentialTokenBucketCount)
	for i := range recent {
		start := current.Add(-time.Duration(CredentialTokenBucketCount-1-i) * CredentialTokenBucketWidth)
		recent[i] = TokenBucket{Time: start}
		if slot := entry.buckets[bucketSlot(start)]; slot.Time.Equal(start) {
			recent[i] = slot
		}
	}
	return CredentialTokens{
		TokenCounts: entry.totals,
		Since:       entry.since,
		LastSeen:    entry.lastSeen,
		Recent:      recent,
	}, true
}

// evictLocked drops the least recently seen credentials once the store is full.
func (s *CredentialTokenStore) evictLocked() {
	if len(s.entries) < credentialTokenMaxEntries {
		return
	}
	for n := 0; n < credentialTokenEvictBatchN && len(s.entries) > 0; n++ {
		oldestID := ""
		var oldest time.Time
		for id, entry := range s.entries {
			if oldestID == "" || entry.lastSeen.Before(oldest) {
				oldestID, oldest = id, entry.lastSeen
			}
		}
		delete(s.entries, oldestID)
	}
}

func bucketSlot(start time.Time) int {
	index := (start.Unix() / int64(CredentialTokenBucketWidth/time.Second)) % CredentialTokenBucketCount
	if index < 0 {
		index += CredentialTokenBucketCount
	}
	return int(index)
}

func (c *TokenCounts) add(other TokenCounts) {
	c.Requests += other.Requests
	c.UncachedInputTokens += other.UncachedInputTokens
	c.OutputTokens += other.OutputTokens
	c.CacheReadTokens += other.CacheReadTokens
	c.CacheWriteTokens += other.CacheWriteTokens
}

// TokenCountsFromRecord converts a usage record into non-overlapping token buckets
// using the provider-aware v2 accounting breakdown.
func TokenCountsFromRecord(record coreusage.Record) TokenCounts {
	detail := coreusage.EnsureTokenBreakdownForProvider(record.Detail, record.Provider, record.ExecutorType)
	breakdown := detail.TokenBreakdown
	return TokenCounts{
		Requests:            1,
		UncachedInputTokens: breakdown.Input.UncachedTokens,
		OutputTokens:        breakdown.Output.TotalTokens,
		CacheReadTokens:     breakdown.Input.CacheReadTokens,
		CacheWriteTokens:    breakdown.Input.CacheWriteTokens,
	}
}

type credentialTokenPlugin struct {
	store *CredentialTokenStore
}

// HandleUsage implements coreusage.Plugin.
func (p credentialTokenPlugin) HandleUsage(_ context.Context, record coreusage.Record) {
	if strings.TrimSpace(record.AuthID) == "" {
		return
	}
	p.store.Record(record.AuthID, TokenCountsFromRecord(record), record.RequestedAt)
}

var defaultCredentialTokens = NewCredentialTokenStore()

func init() {
	coreusage.RegisterNamedPlugin(credentialTokenPluginName, credentialTokenPlugin{store: defaultCredentialTokens})
}

// CredentialTokenSnapshot returns the process-wide token statistics for a credential.
func CredentialTokenSnapshot(authID string) (CredentialTokens, bool) {
	return defaultCredentialTokens.Snapshot(authID)
}
