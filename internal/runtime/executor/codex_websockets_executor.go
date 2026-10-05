// Package executor provides runtime execution capabilities for various AI service providers.
// This file implements a Codex executor that uses the Responses API WebSocket transport.
package executor

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

// CodexWebsocketsExecutor executes Codex Responses requests using a WebSocket transport.
//
// It preserves the existing CodexExecutor HTTP implementation as a fallback for endpoints
// not available over WebSocket (e.g. /responses/compact) and for websocket upgrade failures.
type CodexWebsocketsExecutor struct {
	*CodexExecutor

	store *codexWebsocketSessionStore
}

func NewCodexWebsocketsExecutor(cfg *config.Config) *CodexWebsocketsExecutor {
	return &CodexWebsocketsExecutor{
		CodexExecutor: NewCodexExecutor(cfg),
		store:         globalCodexWebsocketSessionStore,
	}
}

// CodexAutoExecutor routes Codex requests to the websocket transport only when:
//  1. The downstream transport is websocket, and
//  2. The selected auth enables websockets (Codex OAuth credentials do by default).
//
// While a credential is backing off after an upstream websocket failure, new turns use
// HTTP/SSE and turns bound to an existing upstream socket keep using it.
// For non-websocket downstream requests, it always uses the legacy HTTP implementation.
type CodexAutoExecutor struct {
	httpExec *CodexExecutor
	wsExec   *CodexWebsocketsExecutor
}

func NewCodexAutoExecutor(cfg *config.Config) *CodexAutoExecutor {
	return &CodexAutoExecutor{
		httpExec: NewCodexExecutor(cfg),
		wsExec:   NewCodexWebsocketsExecutor(cfg),
	}
}

func (e *CodexAutoExecutor) Identifier() string { return "codex" }

func (e *CodexAutoExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if e == nil || e.httpExec == nil {
		return nil
	}
	return e.httpExec.PrepareRequest(req, auth)
}

func (e *CodexAutoExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if e == nil || e.httpExec == nil {
		return nil, fmt.Errorf("codex auto executor: http executor is nil")
	}
	return e.httpExec.HttpRequest(ctx, auth, req)
}

func (e *CodexAutoExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if e == nil || e.httpExec == nil || e.wsExec == nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("codex auto executor: executor is nil")
	}
	if cliproxyexecutor.DownstreamWebsocket(ctx) && cliproxyauth.WebsocketsEnabled(auth) {
		if cliproxyexecutor.RequiredUpstreamWebsocket(ctx) || cliproxyauth.UpstreamWebsocketDialAllowed(auth) {
			return e.wsExec.Execute(ctx, auth, req, opts)
		}
		return e.wsExec.executeHTTPFallback(ctx, auth, req, opts)
	}
	if cliproxyexecutor.RequiredUpstreamWebsocket(ctx) {
		return cliproxyexecutor.Response{}, cliproxyexecutor.NewUpstreamWebsocketReplayRequiredError()
	}
	return e.httpExec.Execute(ctx, auth, req, opts)
}

func (e *CodexAutoExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	if e == nil || e.httpExec == nil || e.wsExec == nil {
		return nil, fmt.Errorf("codex auto executor: executor is nil")
	}
	if cliproxyexecutor.DownstreamWebsocket(ctx) && cliproxyauth.WebsocketsEnabled(auth) {
		if cliproxyexecutor.RequiredUpstreamWebsocket(ctx) || cliproxyauth.UpstreamWebsocketDialAllowed(auth) {
			return e.wsExec.ExecuteStream(ctx, auth, req, opts)
		}
		return e.wsExec.executeStreamHTTPFallback(ctx, auth, req, opts)
	}
	if cliproxyexecutor.RequiredUpstreamWebsocket(ctx) {
		return nil, cliproxyexecutor.NewUpstreamWebsocketReplayRequiredError()
	}
	return e.httpExec.ExecuteStream(ctx, auth, req, opts)
}

func (e *CodexAutoExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if e == nil || e.httpExec == nil {
		return nil, fmt.Errorf("codex auto executor: http executor is nil")
	}
	return e.httpExec.Refresh(ctx, auth)
}

func (e *CodexAutoExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if e == nil || e.httpExec == nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("codex auto executor: http executor is nil")
	}
	return e.httpExec.CountTokens(ctx, auth, req, opts)
}

func (e *CodexAutoExecutor) CloseExecutionSession(sessionID string) {
	if e == nil || e.wsExec == nil {
		return
	}
	e.wsExec.CloseExecutionSession(sessionID)
}

func (e *CodexAutoExecutor) UpstreamDisconnectChan(sessionID string) <-chan error {
	if e == nil || e.wsExec == nil {
		return nil
	}
	return e.wsExec.UpstreamDisconnectChan(sessionID)
}

// codexWebsocketDialFailureAllowsHTTPFallback reports whether a failed upstream
// websocket dial is a transport problem that HTTP/SSE can work around. Credential,
// payment, and quota rejections keep the normal cooldown handling because the HTTP
// transport would be rejected the same way.
func codexWebsocketDialFailureAllowsHTTPFallback(ctx context.Context, opts cliproxyexecutor.Options, respHS *http.Response, body []byte, modelLevelCooling bool) bool {
	if opts.ExecutionLifecycle != nil || ctx.Err() != nil {
		return false
	}
	if respHS == nil || respHS.StatusCode <= 0 {
		return true
	}
	switch newCodexStatusErrWithCooling(respHS.StatusCode, body, modelLevelCooling).StatusCode() {
	case http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden, http.StatusTooManyRequests:
		return false
	}
	return true
}

// codexWebsocketWarmupRequest reports whether the request is a websocket warm-up
// (generate:false). Warm-ups have no HTTP/SSE equivalent and must never generate.
func codexWebsocketWarmupRequest(payload []byte) bool {
	generate := gjson.GetBytes(payload, "generate")
	return generate.Exists() && generate.Type == gjson.False
}

// startCodexWebsocketHTTPFallback records an upstream websocket dial failure and
// starts the credential's backoff, during which new turns use HTTP/SSE.
func startCodexWebsocketHTTPFallback(authID string, respHS *http.Response, errDial error) {
	retryAt := cliproxyauth.MarkUpstreamWebsocketFailure(authID)
	status := 0
	if respHS != nil {
		status = respHS.StatusCode
	}
	log.Warnf("codex websockets: upstream websocket unavailable for auth %s (status=%d error=%v); using HTTP/SSE until %s",
		authID, status, errDial, retryAt.Format(time.RFC3339))
}

func (e *CodexWebsocketsExecutor) executeHTTPFallback(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if codexWebsocketWarmupRequest(req.Payload) {
		return cliproxyexecutor.Response{}, cliproxyexecutor.NewUpstreamWebsocketPrewarmFallbackError()
	}
	if auth != nil {
		cliproxyexecutor.MarkUpstreamHTTPFallback(ctx, auth.ID)
	}
	return e.CodexExecutor.Execute(ctx, auth, req, opts)
}

func (e *CodexWebsocketsExecutor) executeStreamHTTPFallback(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	if codexWebsocketWarmupRequest(req.Payload) {
		return nil, cliproxyexecutor.NewUpstreamWebsocketPrewarmFallbackError()
	}
	if auth != nil {
		cliproxyexecutor.MarkUpstreamHTTPFallback(ctx, auth.ID)
	}
	return e.CodexExecutor.ExecuteStream(ctx, auth, req, opts)
}

// SupportsApplyPatch requires both selectable transports to support the tool.
func (e *CodexAutoExecutor) SupportsApplyPatch() bool {
	return e != nil && e.httpExec != nil && e.wsExec != nil && e.httpExec.SupportsApplyPatch() && e.wsExec.SupportsApplyPatch()
}
