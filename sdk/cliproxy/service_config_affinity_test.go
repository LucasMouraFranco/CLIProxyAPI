package cliproxy

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func boolPtr(value bool) *bool { return &value }

func TestRoutingDefaultsEnableSessionAffinity(t *testing.T) {
	for name, cfg := range map[string]*internalconfig.Config{
		"nil config":   nil,
		"empty config": {},
		"strategy only": {Routing: internalconfig.RoutingConfig{
			Strategy: "fill-first",
		}},
	} {
		t.Run(name, func(t *testing.T) {
			state := normalizedRoutingRuntimeState(cfg)
			if !state.sessionAffinity {
				t.Fatal("session affinity should be enabled when routing.session-affinity is unset")
			}
			if state.sessionAffinityTTL != time.Hour {
				t.Fatalf("session affinity TTL = %s, want 1h", state.sessionAffinityTTL)
			}
			if !state.sessionAffinitySubagents {
				t.Fatal("subagent affinity should default to enabled")
			}
			selector := newRoutingSelector(state)
			affinity, ok := selector.(*coreauth.SessionAffinitySelector)
			if !ok {
				t.Fatalf("selector type = %T, want *auth.SessionAffinitySelector", selector)
			}
			affinity.Stop()
		})
	}
}

func TestRoutingSessionAffinityExplicitFalseDisablesAffinity(t *testing.T) {
	state := normalizedRoutingRuntimeState(&internalconfig.Config{
		Routing: internalconfig.RoutingConfig{SessionAffinity: boolPtr(false)},
	})
	if state.sessionAffinity {
		t.Fatal("session affinity should be disabled when routing.session-affinity is false")
	}
	if _, ok := newRoutingSelector(state).(*coreauth.RoundRobinSelector); !ok {
		t.Fatalf("selector type = %T, want *auth.RoundRobinSelector", newRoutingSelector(state))
	}
}

// affinityRecordingExecutor records which credential served each call and
// can be told to fail for a specific credential.
type affinityRecordingExecutor struct {
	provider string

	mu       sync.Mutex
	failures map[string]error
}

func (e *affinityRecordingExecutor) Identifier() string { return e.provider }

func (e *affinityRecordingExecutor) failFor(authID string, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.failures == nil {
		e.failures = make(map[string]error)
	}
	if err == nil {
		delete(e.failures, authID)
		return
	}
	e.failures[authID] = err
}

func (e *affinityRecordingExecutor) Execute(_ context.Context, auth *coreauth.Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.mu.Lock()
	err := e.failures[auth.ID]
	e.mu.Unlock()
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	return cliproxyexecutor.Response{Payload: []byte(auth.ID)}, nil
}

func (e *affinityRecordingExecutor) ExecuteStream(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, fmt.Errorf("streaming not used in this test")
}

func (e *affinityRecordingExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *affinityRecordingExecutor) CountTokens(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *affinityRecordingExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

// TestDefaultRoutingKeepsClientSessionsOnOneAccount drives the fork's default
// routing configuration (no routing keys set) through Manager.Execute and checks
// that Claude Code and Codex sessions, including their subagents, stay on one
// credential and only move when that credential becomes unavailable.
func TestDefaultRoutingKeepsClientSessionsOnOneAccount(t *testing.T) {
	coreauth.SetQuotaCooldownDisabled(false)
	t.Cleanup(func() { coreauth.SetQuotaCooldownDisabled(false) })

	for _, tc := range []struct {
		name          string
		provider      string
		model         string
		parent        http.Header
		subagents     []http.Header
		otherSession  http.Header
		parentRequest string
	}{
		{
			name:     "claude code",
			provider: "claude",
			model:    "claude-affinity-default-model",
			parent:   http.Header{"X-Claude-Code-Session-Id": []string{"cc-session-main"}},
			subagents: []http.Header{
				{
					"X-Claude-Code-Session-Id": []string{"cc-session-main"},
					"X-Claude-Code-Agent-Id":   []string{"explore-agent"},
				},
				{
					"X-Claude-Code-Session-Id": []string{"cc-session-main"},
					"X-Claude-Code-Agent-Id":   []string{"review-agent"},
				},
			},
			otherSession:  http.Header{"X-Claude-Code-Session-Id": []string{"cc-session-other"}},
			parentRequest: `{"messages":[{"role":"user","content":"refactor the module"}]}`,
		},
		{
			name:     "codex",
			provider: "codex",
			model:    "codex-affinity-default-model",
			parent:   http.Header{"Session-Id": []string{"codex-thread-main"}},
			subagents: []http.Header{
				{
					"Session-Id":               []string{"codex-thread-child-1"},
					"X-Codex-Parent-Thread-Id": []string{"codex-thread-main"},
					"X-Openai-Subagent":        []string{"true"},
				},
				{
					"Session-Id":               []string{"codex-thread-child-2"},
					"X-Codex-Parent-Thread-Id": []string{"codex-thread-main"},
					"X-Openai-Subagent":        []string{"true"},
				},
			},
			otherSession:  http.Header{"Session-Id": []string{"codex-thread-other"}},
			parentRequest: `{"input":[{"role":"user","content":"refactor the module"}]}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			state := normalizedRoutingRuntimeState(&internalconfig.Config{})
			selector := newRoutingSelector(state)
			affinity, ok := selector.(*coreauth.SessionAffinitySelector)
			if !ok {
				t.Fatalf("default selector = %T, want *auth.SessionAffinitySelector", selector)
			}
			defer affinity.Stop()

			manager := coreauth.NewManager(nil, selector, nil)
			executor := &affinityRecordingExecutor{provider: tc.provider}
			manager.RegisterExecutor(executor)

			authIDs := []string{tc.provider + "-account-1", tc.provider + "-account-2", tc.provider + "-account-3"}
			for _, authID := range authIDs {
				auth := &coreauth.Auth{ID: authID, Provider: tc.provider, Status: coreauth.StatusActive}
				if _, errRegister := manager.Register(coreauth.WithSkipPersist(ctx), auth); errRegister != nil {
					t.Fatalf("Register(%s): %v", authID, errRegister)
				}
				registry.GetGlobalRegistry().RegisterClient(authID, tc.provider, []*registry.ModelInfo{{ID: tc.model}})
				id := authID
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
			}

			execute := func(headers http.Header, body string) (string, error) {
				t.Helper()
				resp, errExec := manager.Execute(ctx, []string{tc.provider}, cliproxyexecutor.Request{
					Model:   tc.model,
					Payload: []byte(body),
				}, cliproxyexecutor.Options{
					Headers:         headers.Clone(),
					OriginalRequest: []byte(body),
					Metadata:        map[string]any{},
				})
				return string(resp.Payload), errExec
			}
			mustExecute := func(headers http.Header, body string) string {
				t.Helper()
				authID, errExec := execute(headers, body)
				if errExec != nil {
					t.Fatalf("Execute: %v", errExec)
				}
				return authID
			}
			subagentBody := func(i int) string {
				return fmt.Sprintf(`{"messages":[{"role":"user","content":"subagent task %d"}],"input":[{"role":"user","content":"subagent task %d"}]}`, i, i)
			}

			// Every turn of the main session lands on one account.
			bound := mustExecute(tc.parent, tc.parentRequest)
			for turn := 0; turn < 5; turn++ {
				if got := mustExecute(tc.parent, tc.parentRequest); got != bound {
					t.Fatalf("main session turn %d moved from %s to %s", turn, bound, got)
				}
			}

			// Subagents inherit the parent's account.
			for i, headers := range tc.subagents {
				for turn := 0; turn < 3; turn++ {
					if got := mustExecute(headers, subagentBody(i)); got != bound {
						t.Fatalf("subagent %d turn %d used %s, want parent account %s", i, turn, got, bound)
					}
				}
			}

			// An independent session gets its own stable binding.
			otherBound := mustExecute(tc.otherSession, tc.parentRequest)
			for turn := 0; turn < 3; turn++ {
				if got := mustExecute(tc.otherSession, tc.parentRequest); got != otherBound {
					t.Fatalf("other session turn %d moved from %s to %s", turn, otherBound, got)
				}
			}

			// A request-scoped failure is not the account's fault and must not move the session.
			executor.failFor(bound, coreauth.NewRequestScopedError("invalid request", http.StatusBadRequest))
			if _, errExec := execute(tc.parent, tc.parentRequest); errExec == nil {
				t.Fatal("expected the request-scoped failure to surface")
			}
			executor.failFor(bound, nil)
			if got := mustExecute(tc.parent, tc.parentRequest); got != bound {
				t.Fatalf("session moved from %s to %s after a request-scoped error", bound, got)
			}

			// The bound account runs out of quota: the session fails over once and
			// then stays on the new account.
			executor.failFor(bound, &coreauth.Error{
				Code:       "rate_limit",
				Message:    "quota exhausted",
				Retryable:  true,
				HTTPStatus: http.StatusTooManyRequests,
			})
			moved, errExec := execute(tc.parent, tc.parentRequest)
			if errExec != nil {
				// The failing attempt may surface before the retry; the next turn must fail over.
				moved = mustExecute(tc.parent, tc.parentRequest)
			}
			if moved == bound {
				t.Fatalf("session stayed on exhausted account %s", bound)
			}
			for turn := 0; turn < 3; turn++ {
				if got := mustExecute(tc.parent, tc.parentRequest); got != moved {
					t.Fatalf("after failover, turn %d moved from %s to %s", turn, moved, got)
				}
			}

			// Subagents leave the exhausted account too and settle on one account each.
			for i, headers := range tc.subagents {
				first := mustExecute(headers, subagentBody(i))
				if first == bound {
					t.Fatalf("subagent %d stayed on exhausted account %s", i, bound)
				}
				for turn := 0; turn < 2; turn++ {
					if got := mustExecute(headers, subagentBody(i)); got != first {
						t.Fatalf("subagent %d turn %d moved from %s to %s after failover", i, turn, first, got)
					}
				}
			}

			// The independent session keeps its account unless that account was the exhausted one.
			if otherBound != bound {
				if got := mustExecute(tc.otherSession, tc.parentRequest); got != otherBound {
					t.Fatalf("unrelated session moved from %s to %s", otherBound, got)
				}
			}
		})
	}
}
