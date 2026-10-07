package openai

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	_ "github.com/router-for-me/CLIProxyAPI/v8/internal/translator"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"github.com/tidwall/gjson"
)

// TestResponsesWebsocketCodexOAuthFallsBackToHTTPWhenUpstreamWebsocketFails runs a
// Codex-style session (warm-up, follow-up, next turn) through the real Codex executor
// against an upstream that rejects websocket upgrades but serves HTTP/SSE.
func TestResponsesWebsocketCodexOAuthFallsBackToHTTPWhenUpstreamWebsocketFails(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const model = "gpt-5.4"

	var (
		mu              sync.Mutex
		httpBodies      [][]byte
		upgradeAttempts atomic.Int32
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			upgradeAttempts.Add(1)
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":{"message":"websocket gateway unavailable"}}`))
			return
		}
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		httpBodies = append(httpBodies, body)
		turn := len(httpBodies)
		mu.Unlock()
		item := fmt.Sprintf(`{"type":"message","id":"msg_answer_%d","role":"assistant","status":"completed","content":[{"type":"output_text","text":"answer %d","annotations":[]}]}`, turn, turn)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_http_%d\",\"status\":\"in_progress\",\"output\":[]}}\n\n", turn)
		_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":%s}\n\n", item)
		_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_http_%d\",\"status\":\"completed\",\"output\":[%s],\"usage\":{\"input_tokens\":3,\"output_tokens\":2,\"total_tokens\":5}}}\n\n", turn, item)
	}))
	defer upstream.Close()

	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(runtimeexecutor.NewCodexAutoExecutor(&config.Config{
		SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll},
	}))
	auth := &coreauth.Auth{
		ID:         "codex-oauth-handler-ws-fallback",
		Provider:   "codex",
		Status:     coreauth.StatusActive,
		Attributes: map[string]string{"base_url": upstream.URL},
		Metadata: map[string]any{
			"type":         "codex",
			"access_token": "test-access-token",
			"email":        "user@example.test",
			"expired":      time.Now().Add(time.Hour).Format(time.RFC3339),
		},
	}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("Register: %v", errRegister)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
		coreauth.MarkUpstreamWebsocketSuccess(auth.ID)
	})

	h := NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager))
	router := gin.New()
	router.GET("/v1/responses", h.ResponsesWebsocket)
	server := httptest.NewServer(router)
	defer server.Close()

	conn, _, errDial := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", nil)
	if errDial != nil {
		t.Fatalf("dial downstream websocket: %v", errDial)
	}
	defer func() { _ = conn.Close() }()

	readCompleted := func(step string) string {
		t.Helper()
		_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		for {
			_, payload, errRead := conn.ReadMessage()
			if errRead != nil {
				t.Fatalf("%s: read downstream websocket: %v", step, errRead)
			}
			switch gjson.GetBytes(payload, "type").String() {
			case wsEventTypeCompleted:
				return gjson.GetBytes(payload, "response.id").String()
			case "error":
				t.Fatalf("%s: downstream error event: %s", step, payload)
			}
		}
	}
	send := func(step, message string) {
		t.Helper()
		if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(message)); errWrite != nil {
			t.Fatalf("%s: write downstream websocket: %v", step, errWrite)
		}
	}
	userMessage := func(text string) string {
		return `{"type":"message","role":"user","content":[{"type":"input_text","text":"` + text + `"}]}`
	}

	// 1. Warm-up: the upstream websocket fails, so the proxy acknowledges the warm-up locally.
	send("warm-up", `{"type":"response.create","model":"`+model+`","generate":false,"instructions":"be brief","input":[`+userMessage("first question")+`]}`)
	prewarmID := readCompleted("warm-up")
	if !strings.HasPrefix(prewarmID, "resp_prewarm_") {
		t.Fatalf("warm-up response id = %q, want a local warm-up acknowledgement", prewarmID)
	}
	mu.Lock()
	httpAfterWarmup := len(httpBodies)
	mu.Unlock()
	if upgradeAttempts.Load() != 1 || httpAfterWarmup != 0 {
		t.Fatalf("after warm-up: upgrade attempts=%d http requests=%d, want 1/0", upgradeAttempts.Load(), httpAfterWarmup)
	}

	// 2. The follow-up references the warm-up and is served over HTTP/SSE with the full transcript.
	send("follow-up", `{"type":"response.create","model":"`+model+`","previous_response_id":"`+prewarmID+`","input":[]}`)
	if got := readCompleted("follow-up"); got != "resp_http_1" {
		t.Fatalf("follow-up response id = %q, want resp_http_1", got)
	}

	// 3. The next turn replays the whole conversation over HTTP/SSE without redialing the websocket.
	send("second turn", `{"type":"response.create","model":"`+model+`","previous_response_id":"resp_http_1","input":[`+userMessage("second question")+`]}`)
	if got := readCompleted("second turn"); got != "resp_http_2" {
		t.Fatalf("second turn response id = %q, want resp_http_2", got)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(httpBodies) != 2 {
		t.Fatalf("HTTP requests = %d, want 2", len(httpBodies))
	}
	if upgradeAttempts.Load() != 1 {
		t.Fatalf("upgrade attempts = %d, want 1 (backoff should skip redials)", upgradeAttempts.Load())
	}
	for i, body := range httpBodies {
		if gjson.GetBytes(body, "generate").Exists() {
			t.Fatalf("HTTP request %d leaked generate: %s", i+1, body)
		}
		if gjson.GetBytes(body, "previous_response_id").Exists() {
			t.Fatalf("HTTP request %d kept previous_response_id instead of replaying: %s", i+1, body)
		}
	}
	firstInput := gjson.GetBytes(httpBodies[0], "input").Raw
	if !strings.Contains(firstInput, "first question") {
		t.Fatalf("follow-up HTTP input lost the warm-up transcript: %s", httpBodies[0])
	}
	secondInput := gjson.GetBytes(httpBodies[1], "input").Raw
	for _, want := range []string{"first question", "answer 1", "second question"} {
		if !strings.Contains(secondInput, want) {
			t.Fatalf("second turn HTTP input missing %q: %s", want, httpBodies[1])
		}
	}
}
