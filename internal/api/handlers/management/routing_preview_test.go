package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestGetRoutingNextReportsSoonestResetPick(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	manager := coreauth.NewManager(nil, &coreauth.SoonestResetSelector{}, nil)
	now := time.Now()
	weeklyReset := func(after time.Duration) map[string]string {
		return map[string]string{
			"Anthropic-Ratelimit-Unified-7d-Utilization": "0.40",
			"Anthropic-Ratelimit-Unified-7d-Reset":       strconv.FormatInt(now.Add(after).Unix(), 10),
		}
	}
	for _, auth := range []*coreauth.Auth{
		{ID: "claude-later", Provider: "claude", Status: coreauth.StatusActive, Quota: coreauth.QuotaState{ObservedAt: now, Signals: weeklyReset(96 * time.Hour)}},
		{ID: "claude-sooner", Provider: "claude", Status: coreauth.StatusActive, Quota: coreauth.QuotaState{ObservedAt: now, Signals: weeklyReset(20 * time.Hour)}},
		{ID: "codex-only", Provider: "codex", Status: coreauth.StatusActive},
	} {
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("Register(%s): %v", auth.ID, errRegister)
		}
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)

	rec := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rec)
	ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v8/management/routing/next", nil)
	h.GetRoutingNext(ginCtx)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}

	var payload struct {
		Previews []coreauth.SelectionPreview `json:"previews"`
	}
	if errUnmarshal := json.Unmarshal(rec.Body.Bytes(), &payload); errUnmarshal != nil {
		t.Fatalf("decode: %v", errUnmarshal)
	}
	byProvider := map[string]coreauth.SelectionPreview{}
	for _, preview := range payload.Previews {
		byProvider[preview.Provider] = preview
	}
	claude := byProvider["claude"]
	if claude.Strategy != coreauth.RoutingStrategySoonestReset || claude.AuthID != "claude-sooner" {
		t.Fatalf("claude preview = %+v, want soonest-reset picking claude-sooner", claude)
	}
	if len(claude.Candidates) != 2 || claude.Candidates[0].AuthID != "claude-sooner" || claude.Candidates[0].AuthIndex == "" ||
		claude.Candidates[0].Weekly == nil || claude.Candidates[0].Weekly.UsedPercent == nil || *claude.Candidates[0].Weekly.UsedPercent != 40 {
		t.Fatalf("claude candidates = %+v", claude.Candidates)
	}
	if codex := byProvider["codex"]; codex.AuthID != "codex-only" {
		t.Fatalf("codex preview = %+v, want codex-only", codex)
	}
}
