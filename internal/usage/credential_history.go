package usage

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	log "github.com/sirupsen/logrus"
)

// CredentialHistoryFileName is the file, next to the config, that keeps each
// credential's hourly buckets across restarts.
const CredentialHistoryFileName = "usage-history.json"

// credentialHistorySaveInterval bounds how much history a crash can lose.
const credentialHistorySaveInterval = time.Minute

type credentialHistoryFile struct {
	Version     int                      `json:"version"`
	Credentials map[string][]TokenBucket `json:"credentials"`
}

// exportHourly returns every credential's active hourly buckets and clears the dirty flag.
func (s *CredentialTokenStore) exportHourly() map[string][]TokenBucket {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	out := make(map[string][]TokenBucket, len(s.entries))
	for id, entry := range s.entries {
		if hourly := activeHourlyLocked(entry, now); len(hourly) > 0 {
			out[id] = hourly
		}
	}
	s.dirty = false
	return out
}

// importHourly restores saved hourly buckets. Buckets older than 7 days are
// harmless: reads only return hours inside the window.
func (s *CredentialTokenStore) importHourly(credentials map[string][]TokenBucket) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for id, buckets := range credentials {
		if id == "" || len(buckets) == 0 {
			continue
		}
		entry := s.entries[id]
		if entry == nil {
			s.evictLocked()
			// Totals count only this process's requests, so they start empty.
			entry = &credentialTokenEntry{since: now}
			s.entries[id] = entry
		}
		for _, bucket := range buckets {
			start := bucket.Time.Truncate(time.Hour)
			addToRing(entry.hourly[:], start, time.Hour, bucket.TokenCounts)
		}
	}
}

// LoadCredentialHistory restores hourly buckets saved by SaveCredentialHistory.
// A missing file is not an error.
func LoadCredentialHistory(path string) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var file credentialHistoryFile
	if err := json.Unmarshal(data, &file); err != nil {
		return err
	}
	defaultCredentialTokens.importHourly(file.Credentials)
	return nil
}

// SaveCredentialHistory writes the hourly buckets atomically, so a crash
// mid-write leaves the previous file intact.
func SaveCredentialHistory(path string) error {
	data, err := json.Marshal(credentialHistoryFile{
		Version:     1,
		Credentials: defaultCredentialTokens.exportHourly(),
	})
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// RunCredentialHistory loads the saved history, then saves it every minute
// while it changes, until ctx ends. Call SaveCredentialHistory on shutdown
// to keep the last minute too.
func RunCredentialHistory(ctx context.Context, path string) {
	if path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		log.Warnf("usage history: %v", err)
		return
	}
	if err := LoadCredentialHistory(path); err != nil {
		log.Warnf("usage history: load %s: %v", path, err)
	}
	go func() {
		ticker := time.NewTicker(credentialHistorySaveInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if !credentialHistoryDirty() {
					continue
				}
				if err := SaveCredentialHistory(path); err != nil {
					log.Warnf("usage history: save %s: %v", path, err)
				}
			}
		}
	}()
}

func credentialHistoryDirty() bool {
	defaultCredentialTokens.mu.Lock()
	defer defaultCredentialTokens.mu.Unlock()
	return defaultCredentialTokens.dirty
}
