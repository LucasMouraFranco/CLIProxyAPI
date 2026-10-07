package auth

import (
	"strings"
	"time"
)

// PreviewPick reports the next round-robin pick without advancing the rotation.
func (s *RoundRobinSelector) PreviewPick(provider, model string, auths []*Auth) SelectionPreview {
	preview := orderedSelectionPreview("round-robin", provider, model, auths)
	if len(auths) == 0 {
		return preview
	}
	s.mu.Lock()
	last := s.lastPicked[provider+":"+canonicalModelKey(model)]
	s.mu.Unlock()
	preview.AuthID = auths[successorIndex(auths, last)].ID
	return preview
}

// PreviewPick reports the first available credential, which fill-first always picks.
func (s *FillFirstSelector) PreviewPick(provider, model string, auths []*Auth) SelectionPreview {
	preview := orderedSelectionPreview("fill-first", provider, model, auths)
	if len(auths) > 0 {
		preview.AuthID = auths[0].ID
	}
	return preview
}

// PreviewPick reports where a request without a session binding would go. Bound
// sessions keep their credential while it stays available.
func (s *SessionAffinitySelector) PreviewPick(provider, model string, auths []*Auth) SelectionPreview {
	if s == nil {
		return SelectionPreview{Provider: provider, Model: model}
	}
	previewer, ok := s.fallback.(SelectionPreviewer)
	if !ok {
		return SelectionPreview{Provider: provider, Model: model, SessionAffinity: true}
	}
	preview := previewer.PreviewPick(provider, model, auths)
	preview.SessionAffinity = true
	return preview
}

func orderedSelectionPreview(strategy, provider, model string, auths []*Auth) SelectionPreview {
	preview := SelectionPreview{Strategy: strategy, Provider: provider, Model: model}
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		preview.Candidates = append(preview.Candidates, SelectionCandidate{AuthID: auth.ID, AuthIndex: auth.Index, Usable: true})
	}
	return preview
}

// PreviewNextPick reports which credential the configured routing strategy would pick
// next for a request without a session binding, without changing selection state.
// The model selects quota buckets and per-model cooldowns; it does not filter
// credentials by model support. It returns false when the configured selector cannot
// preview its choice.
func (m *Manager) PreviewNextPick(provider, model string) (SelectionPreview, bool) {
	provider = strings.TrimSpace(provider)
	model = strings.TrimSpace(model)
	if m == nil || provider == "" {
		return SelectionPreview{}, false
	}

	m.mu.RLock()
	selector := m.selector
	previewer, ok := selector.(SelectionPreviewer)
	if !ok {
		m.mu.RUnlock()
		return SelectionPreview{}, false
	}
	targetKey := canonicalSchedulingProvider(provider)
	candidates := make([]*Auth, 0, len(m.auths))
	for _, candidate := range m.auths {
		if candidate == nil || candidate.Disabled || canonicalSchedulingProvider(executorKeyFromAuth(candidate)) != targetKey {
			continue
		}
		candidates = append(candidates, candidate)
	}
	var available []*Auth
	if len(candidates) > 0 {
		if found, errAvailable := m.availableAuthsForRouteModel(candidates, provider, model, time.Now()); errAvailable == nil {
			available = cloneAuthSlice(found)
			for _, auth := range available {
				auth.EnsureIndex()
			}
		}
	}
	m.mu.RUnlock()

	preview := previewer.PreviewPick(provider, selectionArgForSelector(selector, model), available)
	preview.Provider = provider
	preview.Model = model
	return preview, true
}
