package auth

import (
	"testing"
	"time"
)

func TestWebsocketsEnabledDefaultsOnForCodexOAuthOnly(t *testing.T) {
	codexOAuth := func(metadata map[string]any) *Auth {
		meta := map[string]any{"type": "codex", "access_token": "token", "email": "user@example.test"}
		for key, value := range metadata {
			meta[key] = value
		}
		return &Auth{ID: "codex-oauth", Provider: "codex", Metadata: meta}
	}
	for _, tc := range []struct {
		name string
		auth *Auth
		want bool
	}{
		{name: "nil", auth: nil, want: false},
		{name: "codex oauth without setting", auth: codexOAuth(nil), want: true},
		{name: "codex oauth metadata false", auth: codexOAuth(map[string]any{"websockets": false}), want: false},
		{name: "codex oauth metadata string false", auth: codexOAuth(map[string]any{"websockets": "false"}), want: false},
		{
			name: "attribute overrides metadata",
			auth: func() *Auth {
				auth := codexOAuth(map[string]any{"websockets": true})
				auth.Attributes = map[string]string{"websockets": "false"}
				return auth
			}(),
			want: false,
		},
		{
			name: "codex api key without setting",
			auth: &Auth{ID: "codex-key", Provider: "codex", Attributes: map[string]string{"api_key": "sk-test"}},
			want: false,
		},
		{
			name: "codex api key opted in",
			auth: &Auth{ID: "codex-key", Provider: "codex", Attributes: map[string]string{"api_key": "sk-test", "websockets": "true"}},
			want: true,
		},
		{
			name: "codex credential of unknown kind",
			auth: &Auth{ID: "codex-unknown", Provider: "codex"},
			want: false,
		},
		{
			name: "xai oauth keeps the opt-in default",
			auth: &Auth{ID: "xai-oauth", Provider: "xai", Metadata: map[string]any{"access_token": "token"}},
			want: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := WebsocketsEnabled(tc.auth); got != tc.want {
				t.Fatalf("WebsocketsEnabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestUpstreamWebsocketBackoffDoublesAndResets(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	health := &upstreamWebsocketHealth{
		nowFunc: func() time.Time { return now },
		entries: make(map[string]upstreamWebsocketBackoff),
	}
	previous := upstreamWebsocketTransportHealth
	upstreamWebsocketTransportHealth = health
	t.Cleanup(func() { upstreamWebsocketTransportHealth = previous })

	auth := &Auth{ID: "codex-oauth", Provider: "codex", Metadata: map[string]any{"access_token": "token"}}
	if !UpstreamWebsocketDialAllowed(auth) {
		t.Fatal("a healthy credential should dial websockets")
	}

	wantWaits := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, 30 * time.Minute, 30 * time.Minute}
	for i, want := range wantWaits {
		retryAt := MarkUpstreamWebsocketFailure(auth.ID)
		if got := retryAt.Sub(now); got != want {
			t.Fatalf("failure %d backoff = %s, want %s", i+1, got, want)
		}
		if UpstreamWebsocketDialAllowed(auth) {
			t.Fatalf("failure %d: dial allowed during backoff", i+1)
		}
		now = retryAt.Add(-time.Second)
		if UpstreamWebsocketDialAllowed(auth) {
			t.Fatalf("failure %d: dial allowed one second before the backoff ends", i+1)
		}
		now = retryAt
		if !UpstreamWebsocketDialAllowed(auth) {
			t.Fatalf("failure %d: dial still blocked after the backoff ended", i+1)
		}
	}

	MarkUpstreamWebsocketSuccess(auth.ID)
	if got := MarkUpstreamWebsocketFailure(auth.ID).Sub(now); got != time.Minute {
		t.Fatalf("backoff after success = %s, want 1m", got)
	}

	disabled := &Auth{ID: "codex-oauth-off", Provider: "codex", Metadata: map[string]any{"access_token": "token", "websockets": false}}
	if UpstreamWebsocketDialAllowed(disabled) {
		t.Fatal("a credential with websockets disabled must never dial")
	}
}
