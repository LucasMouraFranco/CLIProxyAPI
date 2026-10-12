package auth

import (
	"strconv"
	"testing"
	"time"
)

func guardAuth(used float64, resetIn time.Duration, now time.Time) *Auth {
	reset := now.Add(resetIn).Unix()
	return &Auth{
		ID:       "luis",
		Provider: "codex",
		Status:   StatusActive,
		Metadata: map[string]any{
			QuotaGuardKey: map[string]any{
				"enabled": true,
				"steps": []any{
					map[string]any{"hours": float64(48), "min_remaining": float64(50)},
					map[string]any{"hours": float64(24), "min_remaining": float64(30)},
					map[string]any{"hours": float64(6), "min_remaining": float64(15)},
					map[string]any{"hours": float64(0), "min_remaining": float64(5)},
				},
			},
		},
		Quota: QuotaState{
			ObservedAt: now,
			Signals: map[string]string{
				"x-codex-primary-window-minutes":   "10080",
				"x-codex-primary-used-percent":     formatFloat(used),
				"x-codex-primary-reset-at":         formatUnix(reset),
				"x-codex-secondary-window-minutes": "300",
				"x-codex-secondary-used-percent":   "0",
			},
		},
	}
}

func formatFloat(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

func formatUnix(v int64) string {
	return strconv.FormatInt(v, 10)
}

func TestQuotaGuardExampleFromSpec(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	// 24h left, 30% remaining (70% used): exactly at the 24h floor of 30.
	if !IsQuotaGuarded(guardAuth(70, 24*time.Hour, now), "gpt-5", now) {
		t.Fatal("expected guarded at 24h left with 30% remaining")
	}
	// Same remaining, 1h left: floor relaxes to 5, so not guarded.
	if IsQuotaGuarded(guardAuth(70, time.Hour, now), "gpt-5", now) {
		t.Fatal("expected not guarded at 1h left with 30% remaining")
	}
	// 3 days left, 40% remaining: floor is 50, so guarded.
	if !IsQuotaGuarded(guardAuth(60, 72*time.Hour, now), "gpt-5", now) {
		t.Fatal("expected guarded at 72h left with 40% remaining")
	}
}

func TestQuotaGuardUnknownQuotaNeverGuarded(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	auth := guardAuth(99, 24*time.Hour, now)
	auth.Quota.Signals = nil
	if IsQuotaGuarded(auth, "gpt-5", now) {
		t.Fatal("unknown quota must fail open")
	}
}

func TestQuotaGuardFloorBoundaries(t *testing.T) {
	guard, err := ParseQuotaGuard(map[string]any{
		"enabled": true,
		"steps": []any{
			map[string]any{"hours": float64(24), "min_remaining": float64(30)},
			map[string]any{"hours": float64(0), "min_remaining": float64(5)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if floor, _ := guard.FloorFor(24 * time.Hour); floor != 30 {
		t.Fatalf("floor at exactly 24h = %v, want 30", floor)
	}
	if floor, _ := guard.FloorFor(23 * time.Hour); floor != 5 {
		t.Fatalf("floor at 23h = %v, want 5", floor)
	}
}

func TestParseQuotaGuardRejectsBadValues(t *testing.T) {
	for _, raw := range []any{
		"nope",
		map[string]any{"enabled": true, "steps": []any{}},
		map[string]any{"enabled": true, "steps": []any{map[string]any{"hours": float64(1), "min_remaining": float64(101)}}},
		map[string]any{"enabled": true, "steps": []any{map[string]any{"hours": float64(1), "min_remaining": float64(10)}, map[string]any{"hours": float64(1), "min_remaining": float64(20)}}},
	} {
		if _, err := ParseQuotaGuard(raw); err == nil {
			t.Fatalf("expected error for %v", raw)
		}
	}
}

func TestQuotaGuardClaudeFableUsesOverallWeekly(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	reset := now.Add(24 * time.Hour).Unix()
	auth := guardAuth(0, 24*time.Hour, now)
	auth.Provider = "claude"
	auth.Quota.Signals = map[string]string{
		"anthropic-ratelimit-unified-7d-utilization":    "0.8",
		"anthropic-ratelimit-unified-7d-reset":          formatUnix(reset),
		"anthropic-ratelimit-unified-7d_oi-utilization": "0.1",
		"anthropic-ratelimit-unified-7d_oi-reset":       formatUnix(reset),
		"anthropic-ratelimit-unified-5h-utilization":    "0",
		"anthropic-ratelimit-unified-5h-reset":          formatUnix(reset),
	}
	// Overall weekly has 20% remaining (floor 30 at 24h) even though Fable has 90%.
	if !IsQuotaGuarded(auth, "claude-fable-5", now) {
		t.Fatal("expected Fable request to be guarded by overall weekly floor")
	}
}

func TestQuotaGuardRecoveryAtLaterThreshold(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	// 72h left, 10% remaining: only the 5% floor under 6h frees this account.
	_, _, next := isAuthBlockedForModel(guardAuth(90, 72*time.Hour, now), "gpt-5", now)
	want := 66*time.Hour + time.Second
	if next.Sub(now) != want {
		t.Fatalf("recovery in %v, want %v (just past the 6h crossing)", next.Sub(now), want)
	}
}

func TestQuotaGuardBlocksViaAvailability(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	blocked, reason, next := isAuthBlockedForModel(guardAuth(70, 24*time.Hour, now), "gpt-5", now)
	if !blocked || reason != blockReasonCooldown {
		t.Fatalf("blocked=%v reason=%v, want cooldown block", blocked, reason)
	}
	// At exactly 24h left the next moment drops under the 24h step, where the
	// 15% floor no longer holds 30% remaining back, so recovery is 1s out.
	if next.Sub(now) != time.Second {
		t.Fatalf("recovery in %v, want 1s (just past the 24h crossing)", next.Sub(now))
	}
}
