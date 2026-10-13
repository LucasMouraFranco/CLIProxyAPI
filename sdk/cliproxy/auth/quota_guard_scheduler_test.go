package auth

import (
	"context"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// The guard is account-wide: a result on model-a that trips it must pause
// model-b's shard too, not leave a stale ready entry behind.
func TestQuotaGuardSyncsAllModelShards(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	reg := registry.GetGlobalRegistry()
	authID := "auth-quota-guard-shards"
	provider := "codex"
	reg.RegisterClient(authID, provider, []*registry.ModelInfo{{ID: "model-a"}, {ID: "model-b"}})
	t.Cleanup(func() { reg.UnregisterClient(authID) })

	now := time.Now()
	auth := guardAuth(60, 30*time.Hour, now) // 40% remaining, floor 30: usable
	auth.ID = authID
	registered, errRegister := manager.Register(WithSkipPersist(context.Background()), auth)
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	for _, m := range []string{"model-a", "model-b"} {
		picked, errPick := manager.scheduler.pickSingle(context.Background(), provider, m, cliproxyexecutor.Options{}, nil)
		if errPick != nil || picked == nil || picked.ID != authID {
			t.Fatalf("pickSingle(%s) = (%v, %v), want %s", m, picked, errPick, authID)
		}
	}

	// A model-a result now reports 20% remaining: under the 30% floor.
	updated := registered.Clone()
	updated.Quota.Signals = map[string]string{
		"x-codex-primary-window-minutes":   "10080",
		"x-codex-primary-used-percent":     "80",
		"x-codex-primary-reset-at":         formatUnix(time.Now().Add(30 * time.Hour).Unix()),
		"x-codex-secondary-window-minutes": "300",
		"x-codex-secondary-used-percent":   "0",
	}
	updated.Quota.ObservedAt = time.Now()
	updated.Generation += 10
	updated.UpdatedAt = time.Now()
	manager.scheduler.upsertAuthResult(updated, []string{"model-a"}, false)

	manager.scheduler.mu.Lock()
	defer manager.scheduler.mu.Unlock()
	pState := manager.scheduler.providers[provider]
	if pState == nil {
		t.Fatal("provider state missing")
	}
	for _, m := range []string{"model-a", "model-b"} {
		shard := pState.modelShards[m]
		if shard == nil || shard.entries[authID] == nil {
			t.Fatalf("shard %s missing entry", m)
		}
		if shard.entries[authID].state == scheduledStateReady {
			t.Fatalf("shard %s still ready after guard tripped on model-a", m)
		}
	}
}
