package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestListAuthFiles_IncludesCredentialTokenUsage(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	observed := time.Date(2026, 10, 5, 12, 20, 0, 0, time.UTC)
	original := credentialTokenSnapshot
	credentialTokenSnapshot = func(authID string) (usage.CredentialTokens, bool) {
		if authID != "claude-with-usage" {
			return usage.CredentialTokens{}, false
		}
		return usage.CredentialTokens{
			TokenCounts: usage.TokenCounts{
				Requests:            4,
				UncachedInputTokens: 120,
				OutputTokens:        900,
				CacheReadTokens:     480000,
				CacheWriteTokens:    35000,
			},
			Since:    observed.Add(-time.Hour),
			LastSeen: observed,
			Recent: []usage.TokenBucket{{
				Time:        observed,
				TokenCounts: usage.TokenCounts{Requests: 1, CacheWriteTokens: 35000},
			}},
		}, true
	}
	t.Cleanup(func() { credentialTokenSnapshot = original })

	manager := coreauth.NewManager(nil, nil, nil)
	for _, auth := range []*coreauth.Auth{
		{ID: "claude-with-usage", Provider: "claude", Attributes: map[string]string{"runtime_only": "true"}},
		{ID: "claude-idle", Provider: "claude", Attributes: map[string]string{"runtime_only": "true"}},
	} {
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("Register(%s): %v", auth.ID, errRegister)
		}
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	h.tokenStore = &memoryAuthStore{}

	rec := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rec)
	ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v8/management/credentials", nil)
	h.ListAuthFiles(ginCtx)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, body %s", rec.Code, rec.Body.String())
	}

	var payload struct {
		Files []map[string]any `json:"files"`
	}
	if errUnmarshal := json.Unmarshal(rec.Body.Bytes(), &payload); errUnmarshal != nil {
		t.Fatalf("decode payload: %v", errUnmarshal)
	}
	byID := make(map[string]map[string]any, len(payload.Files))
	for _, file := range payload.Files {
		id, _ := file["id"].(string)
		byID[id] = file
	}

	withUsage, ok := byID["claude-with-usage"]["token_usage"].(map[string]any)
	if !ok {
		t.Fatalf("expected token_usage object, entry: %#v", byID["claude-with-usage"])
	}
	for field, want := range map[string]float64{
		"requests":              4,
		"uncached_input_tokens": 120,
		"output_tokens":         900,
		"cache_read_tokens":     480000,
		"cache_write_tokens":    35000,
	} {
		if got, _ := withUsage[field].(float64); got != want {
			t.Fatalf("token_usage.%s = %v, want %v", field, withUsage[field], want)
		}
	}
	recent, ok := withUsage["recent"].([]any)
	if !ok || len(recent) != 1 {
		t.Fatalf("token_usage.recent = %#v", withUsage["recent"])
	}
	if bucket, _ := recent[0].(map[string]any); bucket["cache_write_tokens"] != float64(35000) || bucket["time"] != "2026-10-05T12:20:00Z" {
		t.Fatalf("recent bucket = %#v", recent[0])
	}

	if _, present := byID["claude-idle"]["token_usage"]; present {
		t.Fatalf("idle credential should omit token_usage: %#v", byID["claude-idle"])
	}
}
