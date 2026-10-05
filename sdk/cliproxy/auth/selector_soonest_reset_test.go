package auth

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	internallogging "github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

var soonestResetTestNow = time.Date(2026, 9, 11, 20, 0, 0, 0, time.UTC)

func newSoonestResetTestSelector() *SoonestResetSelector {
	return &SoonestResetSelector{nowFunc: func() time.Time { return soonestResetTestNow }}
}

func unixSignal(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }

// claudeAuth builds a Claude credential whose observed unified rate-limit headers
// report the given weekly (7d), 5-hour and optional Fable (7d_oi) windows.
func claudeAuth(id string, weeklyUsed float64, weeklyReset time.Duration, fiveHourUsed float64, fiveHourReset time.Duration) *Auth {
	signals := map[string]string{
		"Anthropic-Ratelimit-Unified-7d-Utilization": strconv.FormatFloat(weeklyUsed, 'f', 2, 64),
		"Anthropic-Ratelimit-Unified-7d-Reset":       unixSignal(soonestResetTestNow.Add(weeklyReset)),
		"Anthropic-Ratelimit-Unified-5h-Utilization": strconv.FormatFloat(fiveHourUsed, 'f', 2, 64),
		"Anthropic-Ratelimit-Unified-5h-Reset":       unixSignal(soonestResetTestNow.Add(fiveHourReset)),
	}
	return &Auth{ID: id, Provider: "claude", Quota: QuotaState{ObservedAt: soonestResetTestNow, Signals: signals}}
}

func withFableWindow(auth *Auth, used float64, reset time.Duration) *Auth {
	auth.Quota.Signals["Anthropic-Ratelimit-Unified-7d_oi-Utilization"] = strconv.FormatFloat(used, 'f', 2, 64)
	auth.Quota.Signals["Anthropic-Ratelimit-Unified-7d_oi-Reset"] = unixSignal(soonestResetTestNow.Add(reset))
	return auth
}

func pickSoonestReset(t *testing.T, selector *SoonestResetSelector, provider, model string, auths ...*Auth) string {
	t.Helper()
	picked, err := selector.Pick(context.Background(), provider, model, cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	return picked.ID
}

func TestSoonestResetPicksAccountWhoseWeeklyWindowResetsFirst(t *testing.T) {
	selector := newSoonestResetTestSelector()
	day := 24 * time.Hour
	auths := []*Auth{
		claudeAuth("claude-a", 0.00, 4*day, 0, 5*time.Hour),
		claudeAuth("claude-b", 0.42, 1*day, 0.10, 3*time.Hour),
		claudeAuth("claude-c", 0.00, 5*day, 0, 5*time.Hour),
		{ID: "claude-unknown", Provider: "claude"},
	}
	for i := 0; i < 3; i++ {
		if got := pickSoonestReset(t, selector, "claude", "claude-sonnet-5", auths...); got != "claude-b" {
			t.Fatalf("pick %d = %s, want claude-b (resets in 1 day)", i, got)
		}
	}
}

func TestSoonestResetSkipsFiveHourAndWeeklyLimits(t *testing.T) {
	selector := newSoonestResetTestSelector()
	day := 24 * time.Hour
	fiveHourLimited := claudeAuth("claude-a", 0.30, 1*day, 1.00, 2*time.Hour)
	weeklyRejected := claudeAuth("claude-b", 0.97, 2*day, 0, 5*time.Hour)
	weeklyRejected.Quota.Signals["Anthropic-Ratelimit-Unified-7d-Status"] = "rejected"
	healthy := claudeAuth("claude-c", 0.10, 3*day, 0, 5*time.Hour)

	if got := pickSoonestReset(t, selector, "claude", "claude-sonnet-5", fiveHourLimited, weeklyRejected, healthy); got != "claude-c" {
		t.Fatalf("pick = %s, want claude-c (the others are at a limit)", got)
	}

	preview := selector.PreviewPick("claude", "claude-sonnet-5", []*Auth{fiveHourLimited, weeklyRejected, healthy})
	reasons := map[string]string{}
	for _, candidate := range preview.Candidates {
		reasons[candidate.AuthID] = candidate.SkipReason
	}
	if reasons["claude-a"] != SoonestResetSkipFiveHourLimit || reasons["claude-b"] != SoonestResetSkipWeeklyLimit || reasons["claude-c"] != "" {
		t.Fatalf("skip reasons = %#v", reasons)
	}
	if preview.AuthID != "claude-c" {
		t.Fatalf("preview pick = %s, want claude-c", preview.AuthID)
	}

	// When every account is at a limit the strategy still answers, soonest reset first,
	// and lets the upstream response and cooldowns decide.
	if got := pickSoonestReset(t, selector, "claude", "claude-sonnet-5", fiveHourLimited, weeklyRejected); got != "claude-a" {
		t.Fatalf("all-limited pick = %s, want claude-a", got)
	}
}

func TestSoonestResetUsesFableBucketForFableModels(t *testing.T) {
	selector := newSoonestResetTestSelector()
	day := 24 * time.Hour
	// Overall 7d resets soonest on A, but A's Fable bucket resets last.
	a := withFableWindow(claudeAuth("claude-a", 0.20, 1*day, 0, 5*time.Hour), 0.30, 5*day)
	b := withFableWindow(claudeAuth("claude-b", 0.20, 3*day, 0, 5*time.Hour), 0.60, 2*day)

	if got := pickSoonestReset(t, selector, "claude", "claude-fable-5-1", a, b); got != "claude-b" {
		t.Fatalf("Fable pick = %s, want claude-b (Fable bucket resets in 2 days)", got)
	}
	if got := pickSoonestReset(t, selector, "claude", "claude-sonnet-5", a, b); got != "claude-a" {
		t.Fatalf("non-Fable pick = %s, want claude-a (overall 7d resets in 1 day)", got)
	}

	// An exhausted Fable bucket only blocks Fable requests.
	a.Quota.Signals["Anthropic-Ratelimit-Unified-7d_oi-Utilization"] = "1.00"
	a.Quota.Signals["Anthropic-Ratelimit-Unified-7d_oi-Reset"] = unixSignal(soonestResetTestNow.Add(day / 2))
	if got := pickSoonestReset(t, selector, "claude", "claude-fable-5-1", a, b); got != "claude-b" {
		t.Fatalf("Fable pick with exhausted bucket = %s, want claude-b", got)
	}
	if got := pickSoonestReset(t, selector, "claude", "claude-sonnet-5", a, b); got != "claude-a" {
		t.Fatalf("non-Fable pick with exhausted Fable bucket = %s, want claude-a", got)
	}
}

func TestSoonestResetTieBreaksByRemainingQuotaThenRoundRobin(t *testing.T) {
	selector := newSoonestResetTestSelector()
	day := 24 * time.Hour
	// Same reset minute (seconds apart); B has more weekly quota left.
	a := claudeAuth("claude-a", 0.50, 2*day, 0, 5*time.Hour)
	b := claudeAuth("claude-b", 0.10, 2*day+20*time.Second, 0, 5*time.Hour)
	if got := pickSoonestReset(t, selector, "claude", "claude-sonnet-5", a, b); got != "claude-b" {
		t.Fatalf("pick = %s, want claude-b (more remaining quota)", got)
	}

	// Equal reset and remaining quota rotate round-robin.
	c := claudeAuth("claude-c", 0.10, 2*day, 0, 5*time.Hour)
	var picks []string
	for i := 0; i < 4; i++ {
		picks = append(picks, pickSoonestReset(t, selector, "claude", "claude-sonnet-5", a, b, c))
	}
	if fmt.Sprint(picks) != "[claude-c claude-b claude-c claude-b]" && fmt.Sprint(picks) != "[claude-b claude-c claude-b claude-c]" {
		t.Fatalf("tie picks = %v, want alternating claude-b/claude-c", picks)
	}
}

func TestSoonestResetTreatsUnknownAndExpiredResetsAsLast(t *testing.T) {
	selector := newSoonestResetTestSelector()
	day := 24 * time.Hour
	expired := claudeAuth("claude-a", 0.90, -time.Hour, 0, -time.Hour)
	unknown := &Auth{ID: "claude-b", Provider: "claude"}
	known := claudeAuth("claude-c", 0.10, 6*day, 0, 5*time.Hour)
	if got := pickSoonestReset(t, selector, "claude", "claude-sonnet-5", expired, unknown, known); got != "claude-c" {
		t.Fatalf("pick = %s, want claude-c (the only known reset)", got)
	}

	// Without any known reset, accounts rotate round-robin.
	first := pickSoonestReset(t, selector, "claude", "claude-sonnet-5", expired, unknown)
	second := pickSoonestReset(t, selector, "claude", "claude-sonnet-5", expired, unknown)
	if first == second {
		t.Fatalf("unknown resets did not rotate: %s then %s", first, second)
	}
}

func TestSoonestResetReadsCodexWeeklyWindow(t *testing.T) {
	selector := newSoonestResetTestSelector()
	observed := soonestResetTestNow.Add(-10 * time.Minute)
	// A: weekly in the secondary slot, resetting in 2 days via reset-after-seconds.
	a := &Auth{ID: "codex-a", Provider: "codex", Quota: QuotaState{ObservedAt: observed, Signals: map[string]string{
		"X-Codex-Primary-Window-Minutes":        "300",
		"X-Codex-Primary-Used-Percent":          "20",
		"X-Codex-Primary-Reset-After-Seconds":   "3600",
		"X-Codex-Secondary-Window-Minutes":      "10080",
		"X-Codex-Secondary-Used-Percent":        "83",
		"X-Codex-Secondary-Reset-After-Seconds": strconv.Itoa(int((2*24*time.Hour + 10*time.Minute).Seconds())),
	}}}
	// B: weekly-only plan in the primary slot, resetting in 3 days via reset-at.
	b := &Auth{ID: "codex-b", Provider: "codex", Quota: QuotaState{ObservedAt: observed, Signals: map[string]string{
		"X-Codex-Primary-Window-Minutes": "10080",
		"X-Codex-Primary-Used-Percent":   "0",
		"X-Codex-Primary-Reset-At":       unixSignal(soonestResetTestNow.Add(3 * 24 * time.Hour)),
	}}}
	if got := pickSoonestReset(t, selector, "codex", "gpt-5.4", a, b); got != "codex-a" {
		t.Fatalf("pick = %s, want codex-a (weekly resets in 2 days)", got)
	}

	preview := selector.PreviewPick("codex", "gpt-5.4", []*Auth{a, b})
	if preview.Candidates[0].AuthID != "codex-a" || preview.Candidates[0].Weekly == nil || preview.Candidates[0].Weekly.ResetAt == nil ||
		!preview.Candidates[0].Weekly.ResetAt.Equal(soonestResetTestNow.Add(2*24*time.Hour)) {
		t.Fatalf("codex-a weekly window = %+v", preview.Candidates[0].Weekly)
	}

	// At the 5-hour limit, the next account is used instead.
	a.Quota.Signals["X-Codex-Primary-Used-Percent"] = "100"
	if got := pickSoonestReset(t, selector, "codex", "gpt-5.4", a, b); got != "codex-b" {
		t.Fatalf("pick with codex-a at its 5-hour limit = %s, want codex-b", got)
	}
}

func TestSoonestResetSkipsAccountsInCooldown(t *testing.T) {
	selector := newSoonestResetTestSelector()
	day := 24 * time.Hour
	cooling := claudeAuth("claude-a", 0.10, 1*day, 0, 5*time.Hour)
	cooling.ModelStates = map[string]*ModelState{"claude-sonnet-5": {
		Unavailable:    true,
		NextRetryAfter: soonestResetTestNow.Add(10 * time.Minute),
		Quota:          QuotaState{Exceeded: true, NextRecoverAt: soonestResetTestNow.Add(10 * time.Minute)},
	}}
	healthy := claudeAuth("claude-b", 0.10, 3*day, 0, 5*time.Hour)
	if got := pickSoonestReset(t, selector, "claude", "claude-sonnet-5", cooling, healthy); got != "claude-b" {
		t.Fatalf("pick = %s, want claude-b (claude-a is cooling down)", got)
	}
}

func TestSoonestResetKeepsAffinityBindingWhileAccountIsUsable(t *testing.T) {
	day := 24 * time.Hour
	a := claudeAuth("claude-a", 0.10, 3*day, 0, 5*time.Hour)
	b := claudeAuth("claude-b", 0.10, 2*day, 0, 5*time.Hour)
	affinity := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: newSoonestResetTestSelector(), TTL: time.Hour})
	defer affinity.Stop()
	opts := func() cliproxyexecutor.Options {
		return cliproxyexecutor.Options{
			Headers:  http.Header{"X-Claude-Code-Session-Id": []string{"soonest-reset-session"}},
			Metadata: map[string]any{},
		}
	}
	pick := func(auths ...*Auth) string {
		t.Helper()
		picked, err := affinity.Pick(context.Background(), "claude", "claude-sonnet-5", opts(), auths)
		if err != nil {
			t.Fatalf("Pick() error = %v", err)
		}
		return picked.ID
	}

	if got := pick(a, b); got != "claude-b" {
		t.Fatalf("cold binding = %s, want claude-b (soonest reset)", got)
	}
	// Another account now resets sooner, but the bound session stays put while claude-b is usable.
	c := claudeAuth("claude-c", 0.10, 1*day, 0, 5*time.Hour)
	if got := pick(a, b, c); got != "claude-b" {
		t.Fatalf("bound session moved to %s while claude-b was usable", got)
	}
	// claude-b drops out (cooldown): the session fails over to the soonest reset left.
	if got := pick(a, c); got != "claude-c" {
		t.Fatalf("failover = %s, want claude-c", got)
	}
}

// TestManagerSoonestResetLearnsResetTimesFromResponseHeaders checks the backend
// end to end: Anthropic rate-limit headers on responses are recorded by MarkResult
// and then drive the soonest-reset choice, with no dashboard involved.
func TestManagerSoonestResetLearnsResetTimesFromResponseHeaders(t *testing.T) {
	ctx := context.Background()
	const model = "claude-soonest-reset-model"
	manager := NewManager(nil, nil, nil)
	manager.SetSelector(&SoonestResetSelector{})
	manager.RegisterExecutor(schedulerTestExecutor{provider: "claude"})
	now := time.Now()
	resets := map[string]time.Duration{
		"claude-soonest-a": 4 * 24 * time.Hour,
		"claude-soonest-b": 30 * time.Hour,
		"claude-soonest-c": 6 * 24 * time.Hour,
	}
	for id := range resets {
		if _, errRegister := manager.Register(WithSkipPersist(ctx), &Auth{ID: id, Provider: "claude", Status: StatusActive}); errRegister != nil {
			t.Fatalf("Register(%s): %v", id, errRegister)
		}
		registry.GetGlobalRegistry().RegisterClient(id, "claude", []*registry.ModelInfo{{ID: model}})
		authID := id
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(authID) })
	}
	for id, reset := range resets {
		headerCtx := internallogging.WithResponseHeadersHolder(ctx)
		internallogging.SetResponseHeaders(headerCtx, http.Header{
			"Anthropic-Ratelimit-Unified-Status":         []string{"allowed"},
			"Anthropic-Ratelimit-Unified-7d-Utilization": []string{"0.25"},
			"Anthropic-Ratelimit-Unified-7d-Reset":       []string{unixSignal(now.Add(reset))},
			"Anthropic-Ratelimit-Unified-5h-Utilization": []string{"0.05"},
			"Anthropic-Ratelimit-Unified-5h-Reset":       []string{unixSignal(now.Add(3 * time.Hour))},
		})
		manager.MarkResult(headerCtx, Result{AuthID: id, Provider: "claude", Model: model, Success: true})
	}

	for i := 0; i < 3; i++ {
		picked, _, errPick := manager.pickNext(ctx, "claude", model, cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("pickNext: %v", errPick)
		}
		if picked.ID != "claude-soonest-b" {
			t.Fatalf("pick %d = %s, want claude-soonest-b (resets in 30h)", i, picked.ID)
		}
	}

	preview, ok := manager.PreviewNextPick("claude", model)
	if !ok || preview.AuthID != "claude-soonest-b" || preview.Strategy != RoutingStrategySoonestReset {
		t.Fatalf("preview = %+v ok=%v", preview, ok)
	}
	if len(preview.Candidates) != 3 || preview.Candidates[2].AuthID != "claude-soonest-c" {
		t.Fatalf("preview order = %+v, want claude-soonest-c last", preview.Candidates)
	}
}
