package executor

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// codexTransportTestUpstream serves the Codex Responses API over both transports and
// can be told to reject websocket upgrades with a status code.
type codexTransportTestUpstream struct {
	t *testing.T

	mu             sync.Mutex
	rejectUpgrade  int
	upgradeCount   atomic.Int32
	websocketTurns atomic.Int32
	httpRequests   atomic.Int32
}

func (u *codexTransportTestUpstream) setRejectUpgrade(status int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.rejectUpgrade = status
}

func (u *codexTransportTestUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	completed := `{"type":"response.completed","response":{"id":"resp_1","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		u.httpRequests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: " + completed + "\n\n"))
		return
	}
	u.upgradeCount.Add(1)
	u.mu.Lock()
	reject := u.rejectUpgrade
	u.mu.Unlock()
	if reject != 0 {
		w.WriteHeader(reject)
		_, _ = w.Write([]byte(`{"error":{"message":"websocket rejected"}}`))
		return
	}
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	conn, errUpgrade := upgrader.Upgrade(w, r, nil)
	if errUpgrade != nil {
		u.t.Errorf("upgrade websocket: %v", errUpgrade)
		return
	}
	defer func() { _ = conn.Close() }()
	if _, _, errRead := conn.ReadMessage(); errRead != nil {
		return
	}
	u.websocketTurns.Add(1)
	_ = conn.WriteMessage(websocket.TextMessage, []byte(completed))
}

func newCodexOAuthTestAuth(id, baseURL string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		ID:         id,
		Provider:   "codex",
		Status:     cliproxyauth.StatusActive,
		Attributes: map[string]string{"base_url": baseURL},
		Metadata: map[string]any{
			"type":         "codex",
			"access_token": "test-access-token",
			"email":        "user@example.test",
		},
	}
}

func runCodexAutoStream(t *testing.T, exec *CodexAutoExecutor, ctx context.Context, auth *cliproxyauth.Auth, payload string) (string, error) {
	t.Helper()
	result, errExecute := exec.ExecuteStream(ctx, auth, cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(payload),
	}, cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FromString("openai-response"),
		ResponseFormat: sdktranslator.FromString("openai-response"),
		Stream:         true,
	})
	if errExecute != nil {
		return "", errExecute
	}
	var output bytes.Buffer
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			return output.String(), chunk.Err
		}
		output.Write(chunk.Payload)
	}
	return output.String(), nil
}

func TestCodexAutoExecutorUsesWebsocketsForOAuthByDefault(t *testing.T) {
	upstream := &codexTransportTestUpstream{t: t}
	server := httptest.NewServer(upstream)
	defer server.Close()

	exec := NewCodexAutoExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
	ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())

	oauth := newCodexOAuthTestAuth("codex-oauth-default-ws", server.URL)
	t.Cleanup(func() { cliproxyauth.MarkUpstreamWebsocketSuccess(oauth.ID) })
	if output, errRun := runCodexAutoStream(t, exec, ctx, oauth, `{"model":"gpt-5.4","input":[{"type":"message","role":"user","content":"hi"}]}`); errRun != nil {
		t.Fatalf("OAuth stream error: %v (output %s)", errRun, output)
	}
	if upstream.websocketTurns.Load() != 1 || upstream.httpRequests.Load() != 0 {
		t.Fatalf("OAuth credential used websocket turns=%d http=%d, want 1/0", upstream.websocketTurns.Load(), upstream.httpRequests.Load())
	}

	apiKey := &cliproxyauth.Auth{
		ID:         "codex-api-key-default-http",
		Provider:   "codex",
		Attributes: map[string]string{"api_key": "sk-test", "base_url": server.URL},
	}
	if output, errRun := runCodexAutoStream(t, exec, ctx, apiKey, `{"model":"gpt-5.4","input":[{"type":"message","role":"user","content":"hi"}]}`); errRun != nil {
		t.Fatalf("API key stream error: %v (output %s)", errRun, output)
	}
	if upstream.websocketTurns.Load() != 1 || upstream.httpRequests.Load() != 1 {
		t.Fatalf("API key credential used websocket turns=%d http=%d, want 1/1", upstream.websocketTurns.Load(), upstream.httpRequests.Load())
	}

	optedOut := newCodexOAuthTestAuth("codex-oauth-opted-out", server.URL)
	optedOut.Metadata["websockets"] = false
	if output, errRun := runCodexAutoStream(t, exec, ctx, optedOut, `{"model":"gpt-5.4","input":[{"type":"message","role":"user","content":"hi"}]}`); errRun != nil {
		t.Fatalf("opted-out stream error: %v (output %s)", errRun, output)
	}
	if upstream.websocketTurns.Load() != 1 || upstream.httpRequests.Load() != 2 {
		t.Fatalf("opted-out credential used websocket turns=%d http=%d, want 1/2", upstream.websocketTurns.Load(), upstream.httpRequests.Load())
	}
}

func TestCodexAutoExecutorFallsBackToHTTPWhenWebsocketDialFails(t *testing.T) {
	upstream := &codexTransportTestUpstream{t: t}
	upstream.setRejectUpgrade(http.StatusBadGateway)
	server := httptest.NewServer(upstream)
	defer server.Close()

	exec := NewCodexAutoExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
	auth := newCodexOAuthTestAuth("codex-oauth-ws-fallback", server.URL)
	t.Cleanup(func() { cliproxyauth.MarkUpstreamWebsocketSuccess(auth.ID) })
	request := `{"model":"gpt-5.4","input":[{"type":"message","role":"user","content":"hi"}]}`
	newTurnCtx := func() context.Context {
		return cliproxyexecutor.WithUpstreamHTTPFallbackTracker(cliproxyexecutor.WithDownstreamWebsocket(context.Background()))
	}

	// 1. The upgrade fails, so the same turn is served over HTTP/SSE and flagged.
	firstCtx := newTurnCtx()
	output, errRun := runCodexAutoStream(t, exec, firstCtx, auth, request)
	if errRun != nil {
		t.Fatalf("first turn error: %v", errRun)
	}
	if !strings.Contains(output, "response.completed") {
		t.Fatalf("first turn output missing completion: %s", output)
	}
	if upstream.upgradeCount.Load() != 1 || upstream.httpRequests.Load() != 1 {
		t.Fatalf("first turn upgrades=%d http=%d, want 1/1", upstream.upgradeCount.Load(), upstream.httpRequests.Load())
	}
	if !cliproxyexecutor.UpstreamHTTPFallback(firstCtx, auth.ID) {
		t.Fatal("first turn was not flagged as an HTTP fallback")
	}
	if cliproxyauth.UpstreamWebsocketDialAllowed(auth) {
		t.Fatal("credential should back off from websocket dials after a failure")
	}

	// 2. While backing off, new turns go straight to HTTP without another upgrade attempt.
	secondCtx := newTurnCtx()
	if _, errRun = runCodexAutoStream(t, exec, secondCtx, auth, request); errRun != nil {
		t.Fatalf("second turn error: %v", errRun)
	}
	if upstream.upgradeCount.Load() != 1 || upstream.httpRequests.Load() != 2 {
		t.Fatalf("second turn upgrades=%d http=%d, want 1/2", upstream.upgradeCount.Load(), upstream.httpRequests.Load())
	}
	if !cliproxyexecutor.UpstreamHTTPFallback(secondCtx, auth.ID) {
		t.Fatal("second turn was not flagged as an HTTP fallback")
	}

	// 3. A warm-up is never sent over HTTP, where it would generate a real response.
	if _, errRun = runCodexAutoStream(t, exec, newTurnCtx(), auth, `{"model":"gpt-5.4","generate":false,"input":[]}`); !cliproxyexecutor.IsUpstreamWebsocketPrewarmFallback(errRun) {
		t.Fatalf("warm-up error = %T %v, want the prewarm fallback signal", errRun, errRun)
	}
	if upstream.httpRequests.Load() != 2 {
		t.Fatalf("warm-up reached HTTP: http requests = %d, want 2", upstream.httpRequests.Load())
	}

	// 4. Once the backoff ends and the upstream accepts websockets again, turns use them.
	upstream.setRejectUpgrade(0)
	cliproxyauth.MarkUpstreamWebsocketSuccess(auth.ID)
	recoveredCtx := newTurnCtx()
	if _, errRun = runCodexAutoStream(t, exec, recoveredCtx, auth, request); errRun != nil {
		t.Fatalf("recovered turn error: %v", errRun)
	}
	if upstream.websocketTurns.Load() != 1 || upstream.httpRequests.Load() != 2 {
		t.Fatalf("recovered turn websocket=%d http=%d, want 1/2", upstream.websocketTurns.Load(), upstream.httpRequests.Load())
	}
	if cliproxyexecutor.UpstreamHTTPFallback(recoveredCtx, auth.ID) {
		t.Fatal("recovered websocket turn was flagged as an HTTP fallback")
	}
}

func TestCodexAutoExecutorKeepsCredentialRejectionsOnWebsocket(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			upstream := &codexTransportTestUpstream{t: t}
			upstream.setRejectUpgrade(status)
			server := httptest.NewServer(upstream)
			defer server.Close()

			exec := NewCodexAutoExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
			auth := newCodexOAuthTestAuth("codex-oauth-ws-rejected", server.URL)
			t.Cleanup(func() { cliproxyauth.MarkUpstreamWebsocketSuccess(auth.ID) })
			ctx := cliproxyexecutor.WithUpstreamHTTPFallbackTracker(cliproxyexecutor.WithDownstreamWebsocket(context.Background()))

			_, errRun := runCodexAutoStream(t, exec, ctx, auth, `{"model":"gpt-5.4","input":[{"type":"message","role":"user","content":"hi"}]}`)
			statusErr, ok := errRun.(interface{ StatusCode() int })
			if !ok || statusErr.StatusCode() != status {
				t.Fatalf("error = %T %v, want status %d", errRun, errRun, status)
			}
			if upstream.httpRequests.Load() != 0 {
				t.Fatalf("credential rejection fell back to HTTP %d times", upstream.httpRequests.Load())
			}
			if !cliproxyauth.UpstreamWebsocketDialAllowed(auth) {
				t.Fatal("credential rejection must not start a websocket backoff")
			}
			if cliproxyexecutor.UpstreamHTTPFallback(ctx, auth.ID) {
				t.Fatal("credential rejection was flagged as an HTTP fallback")
			}
		})
	}
}
