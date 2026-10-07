package cliproxy

import (
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestSoonestResetRoutingSelector(t *testing.T) {
	for _, strategy := range []string{"soonest-reset", " Soonest-Reset ", "soonestreset", "sr"} {
		state := normalizedRoutingRuntimeState(&internalconfig.Config{
			Routing: internalconfig.RoutingConfig{Strategy: strategy},
		})
		if state.strategy != coreauth.RoutingStrategySoonestReset {
			t.Fatalf("strategy %q normalized to %q, want soonest-reset", strategy, state.strategy)
		}
		// The selector may be wrapped by session affinity; previews report the strategy underneath.
		selector := newRoutingSelector(state)
		previewer, ok := selector.(coreauth.SelectionPreviewer)
		if !ok {
			t.Fatalf("strategy %q selector %T cannot preview", strategy, selector)
		}
		if got := previewer.PreviewPick("claude", "", nil).Strategy; got != coreauth.RoutingStrategySoonestReset {
			t.Fatalf("strategy %q previews as %q", strategy, got)
		}
		if stoppable, ok := selector.(coreauth.StoppableSelector); ok {
			stoppable.Stop()
		}
	}
}
