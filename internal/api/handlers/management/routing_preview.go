package management

import (
	"net/http"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// GetRoutingNext reports which credential the routing strategy would choose next for
// requests without a session binding, per provider.
//
//	GET /v8/management/routing/next?provider=claude,codex&model=claude-fable-5-1
//
// provider is optional and defaults to every provider with registered credentials.
// model is optional; it selects quota buckets (the Claude Fable 7-day bucket for Fable
// models) and per-model cooldowns.
func (h *Handler) GetRoutingNext(c *gin.Context) {
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}
	model := strings.TrimSpace(c.Query("model"))
	var providers []string
	for _, raw := range strings.Split(c.Query("provider"), ",") {
		if provider := strings.ToLower(strings.TrimSpace(raw)); provider != "" {
			providers = append(providers, provider)
		}
	}
	if len(providers) == 0 {
		seen := make(map[string]struct{})
		for _, auth := range h.authManager.List() {
			provider := strings.ToLower(strings.TrimSpace(auth.Provider))
			if provider == "" || auth.Disabled {
				continue
			}
			if _, ok := seen[provider]; !ok {
				seen[provider] = struct{}{}
				providers = append(providers, provider)
			}
		}
		sort.Strings(providers)
	}

	previews := make([]coreauth.SelectionPreview, 0, len(providers))
	for _, provider := range providers {
		preview, ok := h.authManager.PreviewNextPick(provider, model)
		if !ok {
			continue
		}
		if preview.Candidates == nil {
			preview.Candidates = []coreauth.SelectionCandidate{}
		}
		previews = append(previews, preview)
	}
	c.JSON(http.StatusOK, gin.H{"previews": previews})
}
