package auth

import (
	"context"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// RoutingStrategySoonestReset is the routing.strategy value for SoonestResetSelector.
const RoutingStrategySoonestReset = "soonest-reset"

const (
	codexWeeklyWindowMinutes   = 7 * 24 * 60
	codexFiveHourWindowMinutes = 5 * 60
	// Reset times are compared at minute precision so accounts sharing a reset
	// (for example after a provider-wide reset) tie and fall through to the next rule.
	soonestResetTiePrecision = time.Minute
)

// Skip reasons reported for credentials the soonest-reset strategy avoids.
const (
	SoonestResetSkipFiveHourLimit = "five_hour_limit"
	SoonestResetSkipWeeklyLimit   = "weekly_limit"
)

// SoonestResetSelector drains the credential whose weekly quota window resets soonest,
// so quota that is about to expire is used before quota with days left.
//
// Among available credentials it skips those whose observed quota says they are at
// their 5-hour or weekly limit, then orders by weekly reset time (unknown resets last),
// then by most remaining weekly quota, and rotates round-robin among exact ties.
// Reset times come from the provider quota headers recorded on each credential
// (Anthropic unified rate-limit headers and Codex x-codex-* headers). For Claude
// requests to a Fable model the Fable-specific 7-day bucket is used when known.
type SoonestResetSelector struct {
	mu         sync.Mutex
	lastPicked map[string]string
	nowFunc    func() time.Time
}

// QuotaWindow is a parsed quota window from a credential's observed quota signals.
type QuotaWindow struct {
	Name        string     `json:"name"`
	UsedPercent *float64   `json:"used_percent,omitempty"`
	ResetAt     *time.Time `json:"reset_at,omitempty"`
}

// SelectionCandidate describes one credential in a routing preview, in selection order.
type SelectionCandidate struct {
	AuthID     string       `json:"auth_id"`
	AuthIndex  string       `json:"auth_index,omitempty"`
	Usable     bool         `json:"usable"`
	SkipReason string       `json:"skip_reason,omitempty"`
	Weekly     *QuotaWindow `json:"weekly,omitempty"`
	FiveHour   *QuotaWindow `json:"five_hour,omitempty"`
}

// SelectionPreview reports which credential a routing strategy would choose next for a
// request without a session binding.
type SelectionPreview struct {
	Strategy string `json:"strategy"`
	Provider string `json:"provider"`
	Model    string `json:"model,omitempty"`
	// SessionAffinity reports that bound sessions keep their credential; the preview
	// describes requests that do not have a binding yet.
	SessionAffinity bool                 `json:"session_affinity"`
	AuthID          string               `json:"auth_id,omitempty"`
	Candidates      []SelectionCandidate `json:"candidates"`
}

// SelectionPreviewer is implemented by selectors that can report their next pick
// without changing selection state.
type SelectionPreviewer interface {
	PreviewPick(provider, model string, auths []*Auth) SelectionPreview
}

type soonestResetCandidate struct {
	auth       *Auth
	weekly     QuotaWindow
	fiveHour   QuotaWindow
	skipReason string
}

func (s *SoonestResetSelector) now() time.Time {
	if s == nil || s.nowFunc == nil {
		return time.Now()
	}
	return s.nowFunc()
}

// Pick selects the usable credential whose weekly quota resets soonest.
func (s *SoonestResetSelector) Pick(ctx context.Context, provider, model string, _ cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	now := s.now()
	available, err := getSelectorAvailableAuths(ctx, auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)
	ranked := rankSoonestReset(available, model, now)
	ties := leadingSoonestResetTies(ranked)

	key := provider + ":" + canonicalModelKey(model)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastPicked == nil {
		s.lastPicked = make(map[string]string)
	}
	if len(s.lastPicked) >= 4096 {
		s.lastPicked = make(map[string]string)
	}
	picked := ties[successorIndex(ties, s.lastPicked[key])]
	s.lastPicked[key] = picked.ID
	return picked, nil
}

// PreviewPick reports the ranked candidates and the next pick without advancing the
// round-robin state used for ties.
func (s *SoonestResetSelector) PreviewPick(provider, model string, auths []*Auth) SelectionPreview {
	now := s.now()
	ranked := rankSoonestReset(auths, model, now)
	preview := SelectionPreview{Strategy: RoutingStrategySoonestReset, Provider: provider, Model: model}
	for _, candidate := range ranked {
		entry := SelectionCandidate{
			AuthID:     candidate.auth.ID,
			AuthIndex:  candidate.auth.Index,
			Usable:     candidate.skipReason == "",
			SkipReason: candidate.skipReason,
		}
		if candidate.weekly.UsedPercent != nil || candidate.weekly.ResetAt != nil {
			weekly := candidate.weekly
			entry.Weekly = &weekly
		}
		if candidate.fiveHour.UsedPercent != nil || candidate.fiveHour.ResetAt != nil {
			fiveHour := candidate.fiveHour
			entry.FiveHour = &fiveHour
		}
		preview.Candidates = append(preview.Candidates, entry)
	}
	if ties := leadingSoonestResetTies(ranked); len(ties) > 0 {
		s.mu.Lock()
		last := s.lastPicked[provider+":"+canonicalModelKey(model)]
		s.mu.Unlock()
		preview.AuthID = ties[successorIndex(ties, last)].ID
	}
	return preview
}

// rankSoonestReset orders credentials by the soonest-reset rules. Credentials the
// quota signals mark as limited sort after usable ones; when every credential is
// limited they are still ranked so selection never fails on stale signals alone.
func rankSoonestReset(auths []*Auth, model string, now time.Time) []soonestResetCandidate {
	ranked := make([]soonestResetCandidate, 0, len(auths))
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		ranked = append(ranked, soonestResetCandidateFor(auth, model, now))
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		return soonestResetLess(ranked[i], ranked[j])
	})
	return ranked
}

func soonestResetLess(a, b soonestResetCandidate) bool {
	if (a.skipReason == "") != (b.skipReason == "") {
		return a.skipReason == ""
	}
	if c := compareSoonestResetTime(a.weekly.ResetAt, b.weekly.ResetAt); c != 0 {
		return c < 0
	}
	if ra, rb := remainingPercent(a.weekly), remainingPercent(b.weekly); ra != rb {
		return ra > rb
	}
	return a.auth.ID < b.auth.ID
}

// compareSoonestResetTime orders known reset times before unknown ones and earlier
// before later, at minute precision.
func compareSoonestResetTime(a, b *time.Time) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return 1
	case b == nil:
		return -1
	}
	ta, tb := a.Truncate(soonestResetTiePrecision), b.Truncate(soonestResetTiePrecision)
	switch {
	case ta.Before(tb):
		return -1
	case ta.After(tb):
		return 1
	default:
		return 0
	}
}

func remainingPercent(window QuotaWindow) float64 {
	if window.UsedPercent == nil {
		return -1
	}
	return math.Max(0, 100-*window.UsedPercent)
}

// leadingSoonestResetTies returns the ID-ordered credentials that tie with the best
// ranked credential on every rule, so round-robin rotates only among exact ties.
func leadingSoonestResetTies(ranked []soonestResetCandidate) []*Auth {
	if len(ranked) == 0 {
		return nil
	}
	best := ranked[0]
	ties := []*Auth{best.auth}
	for _, candidate := range ranked[1:] {
		if (candidate.skipReason == "") != (best.skipReason == "") ||
			compareSoonestResetTime(candidate.weekly.ResetAt, best.weekly.ResetAt) != 0 ||
			remainingPercent(candidate.weekly) != remainingPercent(best.weekly) {
			break
		}
		ties = append(ties, candidate.auth)
	}
	sort.Slice(ties, func(i, j int) bool { return ties[i].ID < ties[j].ID })
	return ties
}

func soonestResetCandidateFor(auth *Auth, model string, now time.Time) soonestResetCandidate {
	candidate := soonestResetCandidate{auth: auth}
	signals := auth.Quota.Signals
	observedAt := auth.Quota.ObservedAt
	switch strings.ToLower(strings.TrimSpace(auth.Provider)) {
	case "claude":
		candidate.weekly, candidate.fiveHour, candidate.skipReason = claudeQuotaWindows(signals, model, now)
	case "codex":
		candidate.weekly, candidate.fiveHour, candidate.skipReason = codexQuotaWindows(signals, observedAt, now)
	}
	// A window whose reset time has passed has started over: its old usage no longer
	// applies and its next reset is unknown until the next observation.
	expireQuotaWindow(&candidate.weekly, now)
	expireQuotaWindow(&candidate.fiveHour, now)
	return candidate
}

func expireQuotaWindow(window *QuotaWindow, now time.Time) {
	if window.ResetAt != nil && !window.ResetAt.After(now) {
		window.ResetAt = nil
		window.UsedPercent = nil
	}
}

// claudeQuotaWindows reads the Anthropic unified rate-limit headers. Utilization is a
// fraction (1.0 = 100%) and resets are Unix seconds. The 7d_oi bucket is Fable-specific.
func claudeQuotaWindows(signals map[string]string, model string, now time.Time) (weekly, fiveHour QuotaWindow, skipReason string) {
	fiveHour = claudeWindow(signals, "5h", "5h")
	overall := claudeWindow(signals, "7d", "7d")
	weekly = overall
	if isFableModel(model) {
		if fable := claudeWindow(signals, "7d_oi", "7d_fable"); fable.UsedPercent != nil || fable.ResetAt != nil {
			weekly = fable
		}
	}
	switch {
	case claudeWindowExhausted(signals, "5h", fiveHour, now):
		skipReason = SoonestResetSkipFiveHourLimit
	case claudeWindowExhausted(signals, "7d", overall, now):
		skipReason = SoonestResetSkipWeeklyLimit
	case weekly.Name == "7d_fable" && claudeWindowExhausted(signals, "7d_oi", weekly, now):
		skipReason = SoonestResetSkipWeeklyLimit
	}
	return weekly, fiveHour, skipReason
}

func claudeWindow(signals map[string]string, header, name string) QuotaWindow {
	window := QuotaWindow{Name: name}
	prefix := "anthropic-ratelimit-unified-" + header + "-"
	if raw := quotaSignal(signals, prefix+"utilization"); raw != "" {
		if utilization, errParse := strconv.ParseFloat(raw, 64); errParse == nil && !math.IsNaN(utilization) && !math.IsInf(utilization, 0) {
			used := utilization * 100
			window.UsedPercent = &used
		}
	}
	if resetAt, ok := parseQuotaUnixTime(quotaSignal(signals, prefix+"reset")); ok {
		window.ResetAt = &resetAt
	}
	return window
}

func claudeWindowExhausted(signals map[string]string, header string, window QuotaWindow, now time.Time) bool {
	if window.ResetAt == nil || !window.ResetAt.After(now) {
		return false
	}
	if strings.EqualFold(quotaSignal(signals, "anthropic-ratelimit-unified-"+header+"-status"), "rejected") {
		return true
	}
	return window.UsedPercent != nil && *window.UsedPercent >= 100
}

func isFableModel(model string) bool {
	return strings.Contains(strings.ToLower(model), "fable")
}

// codexQuotaWindows reads the x-codex-* rate-limit headers. The weekly window is the
// one with a 10080-minute window (primary or secondary), the 5-hour window the one with
// a 300-minute window; used percent is 0-100.
func codexQuotaWindows(signals map[string]string, observedAt, now time.Time) (weekly, fiveHour QuotaWindow, skipReason string) {
	weekly = QuotaWindow{Name: "weekly"}
	fiveHour = QuotaWindow{Name: "5h"}
	for _, slot := range []string{"primary", "secondary"} {
		prefix := "x-codex-" + slot + "-"
		minutes, errMinutes := strconv.Atoi(quotaSignal(signals, prefix+"window-minutes"))
		if errMinutes != nil {
			continue
		}
		window := QuotaWindow{}
		if raw := quotaSignal(signals, prefix+"used-percent"); raw != "" {
			if used, errParse := strconv.ParseFloat(raw, 64); errParse == nil && !math.IsNaN(used) && !math.IsInf(used, 0) {
				window.UsedPercent = &used
			}
		}
		if resetAt, ok := parseQuotaUnixTime(quotaSignal(signals, prefix+"reset-at")); ok {
			window.ResetAt = &resetAt
		} else if seconds, errSeconds := strconv.ParseFloat(quotaSignal(signals, prefix+"reset-after-seconds"), 64); errSeconds == nil && seconds >= 0 && !observedAt.IsZero() {
			resetAt := observedAt.Add(time.Duration(seconds * float64(time.Second)))
			window.ResetAt = &resetAt
		}
		switch minutes {
		case codexWeeklyWindowMinutes:
			window.Name = weekly.Name
			weekly = window
		case codexFiveHourWindowMinutes:
			window.Name = fiveHour.Name
			fiveHour = window
		}
	}
	switch {
	case codexWindowExhausted(fiveHour, now):
		skipReason = SoonestResetSkipFiveHourLimit
	case codexWindowExhausted(weekly, now):
		skipReason = SoonestResetSkipWeeklyLimit
	}
	return weekly, fiveHour, skipReason
}

func codexWindowExhausted(window QuotaWindow, now time.Time) bool {
	return window.ResetAt != nil && window.ResetAt.After(now) && window.UsedPercent != nil && *window.UsedPercent >= 100
}

// quotaSignal looks up a quota signal case-insensitively; signals are stored with
// canonical header keys.
func quotaSignal(signals map[string]string, name string) string {
	if len(signals) == 0 {
		return ""
	}
	for key, value := range signals {
		if strings.EqualFold(key, name) {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func parseQuotaUnixTime(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	seconds, errParse := strconv.ParseFloat(raw, 64)
	if errParse != nil || seconds <= 0 || math.IsNaN(seconds) || math.IsInf(seconds, 0) {
		return time.Time{}, false
	}
	whole := math.Floor(seconds)
	return time.Unix(int64(whole), int64((seconds-whole)*1e9)).UTC(), true
}
