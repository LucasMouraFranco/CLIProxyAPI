package auth

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// QuotaGuardKey is the per-account metadata key for the shared-account guard.
// The same JSON shape is stored in the auth file, e.g.
// {"enabled":true,"steps":[{"hours":48,"min_remaining":50},{"hours":24,"min_remaining":30},{"hours":0,"min_remaining":5}]}
const QuotaGuardKey = "quota_guard"

// SoonestResetSkipQuotaGuard reports a credential held back by its quota guard.
const SoonestResetSkipQuotaGuard = "quota_guard"

// QuotaGuardStep keeps at least MinRemaining percent once the time left
// until reset drops to Hours or below the next larger step threshold.
// Steps are evaluated from the largest Hours to the smallest.
type QuotaGuardStep struct {
	Hours        float64 `json:"hours"`
	MinRemaining float64 `json:"min_remaining"`
}

// QuotaGuard is the parsed per-account guard. Enabled=false means no guard.
type QuotaGuard struct {
	Enabled bool             `json:"enabled"`
	Steps   []QuotaGuardStep `json:"steps"`
}

func isFinite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

func quotaGuardNumber(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, isFinite(v)
	case float32:
		f := float64(v)
		return f, isFinite(f)
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case int32:
		return float64(v), true
	case json.Number:
		f, err := v.Float64()
		return f, err == nil && isFinite(f)
	case string:
		var f float64
		if _, err := fmt.Sscan(strings.TrimSpace(v), &f); err == nil {
			return f, isFinite(f)
		}
	}
	return 0, false
}

func quotaGuardMap(value any) (map[string]any, bool) {
	if m, ok := value.(map[string]any); ok {
		return m, true
	}
	// Values decoded without UseNumber use map[string]any already; be lenient
	// with a raw JSON string stored as text.
	if s, ok := value.(string); ok && strings.TrimSpace(s) != "" {
		var m map[string]any
		if err := json.Unmarshal([]byte(s), &m); err == nil {
			return m, true
		}
	}
	return nil, false
}

// ParseQuotaGuard reads and validates a raw metadata value. A nil value means
// no guard was configured and returns (nil, nil).
func ParseQuotaGuard(value any) (*QuotaGuard, error) {
	if value == nil {
		return nil, nil
	}
	raw, ok := quotaGuardMap(value)
	if !ok {
		return nil, fmt.Errorf("quota_guard must be an object")
	}
	enabled := true
	if rawEnabled, present := raw["enabled"]; present {
		b, isBool := rawEnabled.(bool)
		if !isBool {
			return nil, fmt.Errorf("quota_guard.enabled must be a boolean")
		}
		enabled = b
	}
	rawSteps, present := raw["steps"]
	if !present || rawSteps == nil {
		if enabled {
			return nil, fmt.Errorf("quota_guard.steps is required")
		}
		return &QuotaGuard{Enabled: false}, nil
	}
	list, isList := rawSteps.([]any)
	if !isList {
		return nil, fmt.Errorf("quota_guard.steps must be an array")
	}
	if enabled && len(list) == 0 {
		return nil, fmt.Errorf("quota_guard.steps must not be empty")
	}
	if len(list) > 8 {
		return nil, fmt.Errorf("quota_guard.steps must have at most 8 entries")
	}
	steps := make([]QuotaGuardStep, 0, len(list))
	seenHours := make(map[float64]bool, len(list))
	for i, item := range list {
		row, isMap := quotaGuardMap(item)
		if !isMap {
			return nil, fmt.Errorf("quota_guard.steps[%d] must be an object", i)
		}
		hours, okHours := quotaGuardNumber(row["hours"])
		minRemaining, okMin := quotaGuardNumber(row["min_remaining"])
		if !okHours || !okMin {
			return nil, fmt.Errorf("quota_guard.steps[%d] needs numeric hours and min_remaining", i)
		}
		if hours < 0 || hours > 24*366 {
			return nil, fmt.Errorf("quota_guard.steps[%d].hours out of range", i)
		}
		if minRemaining < 0 || minRemaining > 100 {
			return nil, fmt.Errorf("quota_guard.steps[%d].min_remaining out of range", i)
		}
		if seenHours[hours] {
			return nil, fmt.Errorf("quota_guard.steps has duplicate hours %v", hours)
		}
		seenHours[hours] = true
		steps = append(steps, QuotaGuardStep{Hours: hours, MinRemaining: minRemaining})
	}
	sort.Slice(steps, func(i, j int) bool { return steps[i].Hours > steps[j].Hours })
	// Floors must relax (or hold) as the reset approaches; a floor that rises
	// with time would pause accounts by the clock alone, which surprises.
	for i := 1; i < len(steps); i++ {
		if steps[i].MinRemaining > steps[i-1].MinRemaining {
			return nil, fmt.Errorf("quota_guard.steps must keep less or equal as reset approaches")
		}
	}
	return &QuotaGuard{Enabled: enabled, Steps: steps}, nil
}

// QuotaGuardFor returns the guard configured on an auth, or nil when absent
// or invalid. Invalid stored data fails open (no guard) so a bad edit can
// never block traffic on its own; the management API rejects it on write.
func QuotaGuardFor(auth *Auth) *QuotaGuard {
	if auth == nil {
		return nil
	}
	var raw any
	if auth.Metadata != nil {
		if v, ok := auth.Metadata[QuotaGuardKey]; ok {
			raw = v
		} else if v, ok := auth.Metadata["quota-guard"]; ok {
			raw = v
		}
	}
	if raw == nil {
		return nil
	}
	guard, err := ParseQuotaGuard(raw)
	if err != nil || guard == nil || !guard.Enabled || len(guard.Steps) == 0 {
		return nil
	}
	return guard
}

// FloorFor returns the minimum remaining percent for a time-to-reset.
// Steps are sorted largest Hours first; the first step whose threshold the
// remaining time still reaches wins, so exactly 24h left matches the 24h step.
func (g *QuotaGuard) FloorFor(timeToReset time.Duration) (float64, bool) {
	if g == nil || !g.Enabled || len(g.Steps) == 0 {
		return 0, false
	}
	hoursLeft := timeToReset.Hours()
	for _, step := range g.Steps {
		if hoursLeft >= step.Hours {
			return step.MinRemaining, true
		}
	}
	// Below every threshold: the smallest step still applies.
	return g.Steps[len(g.Steps)-1].MinRemaining, true
}

// guardWindow returns the window the guard judges. For Claude the overall
// 7-day window is used even for Fable models, whose candidate weekly window
// is the Fable bucket; the guard protects the shared account as a whole.
func guardWindow(auth *Auth, candidateWeekly QuotaWindow, now time.Time) QuotaWindow {
	if auth != nil && strings.EqualFold(strings.TrimSpace(auth.Provider), "claude") {
		overall := claudeWindow(auth.Quota.Signals, "7d", "7d")
		expireQuotaWindow(&overall, now)
		// Never fall back to the Fable bucket: unknown overall quota fails open.
		return overall
	}
	return candidateWeekly
}

// nextEligibleAt returns when a guarded auth may become usable again: the
// first step threshold crossing that lowers the floor below the remaining
// percent, or the reset when no crossing helps. Returning the reset for
// every guard block would keep the scheduler from rechecking relaxed floors.
func (g *QuotaGuard) nextEligibleAt(remaining float64, resetAt time.Time, now time.Time) time.Time {
	if g == nil {
		return resetAt
	}
	for _, step := range g.Steps {
		threshold := time.Duration(step.Hours * float64(time.Hour))
		crossing := resetAt.Add(-threshold)
		if crossing.Before(now) {
			continue
		}
		// Just after the crossing the time to reset is just under this step's
		// threshold, so judge that floor. Return a moment after the crossing:
		// at the exact crossing the old floor still applies.
		if floor, ok := g.FloorFor(threshold - time.Second); ok && remaining > floor {
			return crossing.Add(time.Second)
		}
	}
	return resetAt
}

// GuardStatus describes why an auth is held back, for previews and errors.
type GuardStatus struct {
	Guarded   bool
	Floor     float64
	Remaining float64
	ResetAt   time.Time
	// NextEligibleAt is when the floor may relax enough to use the auth again.
	NextEligibleAt time.Time
}

// quotaGuardStatus evaluates the weekly window guard for an auth.
// Unknown quota (no used percent or no future reset) is never guarded:
// after a restart the proxy has not heard from the account yet, and a
// guard that blocks on missing data would strand traffic.
func quotaGuardStatus(auth *Auth, model string, now time.Time) GuardStatus {
	guard := QuotaGuardFor(auth)
	if guard == nil || auth == nil {
		return GuardStatus{}
	}
	candidate := soonestResetCandidateFor(auth, model, now)
	weekly := guardWindow(auth, candidate.weekly, now)
	if weekly.UsedPercent == nil || weekly.ResetAt == nil || !weekly.ResetAt.After(now) {
		return GuardStatus{}
	}
	remaining := math.Max(0, 100-*weekly.UsedPercent)
	floor, ok := guard.FloorFor(weekly.ResetAt.Sub(now))
	if !ok {
		return GuardStatus{}
	}
	guarded := remaining <= floor
	next := *weekly.ResetAt
	if guarded {
		next = guard.nextEligibleAt(remaining, *weekly.ResetAt, now)
	}
	return GuardStatus{
		Guarded:        guarded,
		Floor:          floor,
		Remaining:      remaining,
		ResetAt:        *weekly.ResetAt,
		NextEligibleAt: next,
	}
}

// IsQuotaGuarded reports whether the guard currently holds this auth back.
func IsQuotaGuarded(auth *Auth, model string, now time.Time) bool {
	return quotaGuardStatus(auth, model, now).Guarded
}
