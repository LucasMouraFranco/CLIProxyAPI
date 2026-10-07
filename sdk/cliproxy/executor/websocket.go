package executor

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
)

// UpstreamWebsocketReplayRequiredError indicates that an incremental request
// cannot safely continue because its upstream websocket is no longer reusable.
type UpstreamWebsocketReplayRequiredError struct{}

func (*UpstreamWebsocketReplayRequiredError) Error() string {
	return `{"error":{"message":"upstream transport requires full HTTP replay","type":"server_error","code":"upstream_http_replay_required","status":426}}`
}

func (*UpstreamWebsocketReplayRequiredError) StatusCode() int { return http.StatusUpgradeRequired }

func (*UpstreamWebsocketReplayRequiredError) IsRequestScoped() bool { return true }

// NewUpstreamWebsocketReplayRequiredError creates a request-scoped replay signal.
func NewUpstreamWebsocketReplayRequiredError() error {
	return &UpstreamWebsocketReplayRequiredError{}
}

// IsUpstreamWebsocketReplayRequired reports whether err is the internal replay signal.
func IsUpstreamWebsocketReplayRequired(err error) bool {
	var replayErr *UpstreamWebsocketReplayRequiredError
	return errors.As(err, &replayErr)
}

// UpstreamWebsocketPrewarmFallbackError reports that a warm-up request
// (generate:false) could not reach the upstream WebSocket. HTTP/SSE has no warm-up
// equivalent, so the downstream handler acknowledges the warm-up locally instead.
type UpstreamWebsocketPrewarmFallbackError struct{}

func (*UpstreamWebsocketPrewarmFallbackError) Error() string {
	return `{"error":{"message":"upstream websocket unavailable for warm-up request","type":"server_error","code":"upstream_websocket_prewarm_unavailable","status":503}}`
}

func (*UpstreamWebsocketPrewarmFallbackError) StatusCode() int { return http.StatusServiceUnavailable }

func (*UpstreamWebsocketPrewarmFallbackError) IsRequestScoped() bool { return true }

// NewUpstreamWebsocketPrewarmFallbackError creates a request-scoped warm-up fallback signal.
func NewUpstreamWebsocketPrewarmFallbackError() error {
	return &UpstreamWebsocketPrewarmFallbackError{}
}

// IsUpstreamWebsocketPrewarmFallback reports whether err is the warm-up fallback signal.
func IsUpstreamWebsocketPrewarmFallback(err error) bool {
	var prewarmErr *UpstreamWebsocketPrewarmFallbackError
	return errors.As(err, &prewarmErr)
}

type upstreamHTTPFallbackContextKey struct{}

type upstreamHTTPFallbackTracker struct {
	mu      sync.Mutex
	authIDs map[string]struct{}
}

// WithUpstreamHTTPFallbackTracker lets a downstream WebSocket handler learn which
// credentials served a request over HTTP/SSE although they are configured for the
// upstream WebSocket transport.
func WithUpstreamHTTPFallbackTracker(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, upstreamHTTPFallbackContextKey{}, &upstreamHTTPFallbackTracker{})
}

// MarkUpstreamHTTPFallback records that the credential served the request over HTTP/SSE
// instead of the upstream WebSocket.
func MarkUpstreamHTTPFallback(ctx context.Context, authID string) {
	if ctx == nil {
		return
	}
	tracker, ok := ctx.Value(upstreamHTTPFallbackContextKey{}).(*upstreamHTTPFallbackTracker)
	if !ok || tracker == nil {
		return
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if tracker.authIDs == nil {
		tracker.authIDs = make(map[string]struct{})
	}
	tracker.authIDs[strings.TrimSpace(authID)] = struct{}{}
}

// UpstreamHTTPFallback reports whether the credential served the tracked request over
// HTTP/SSE instead of the upstream WebSocket.
func UpstreamHTTPFallback(ctx context.Context, authID string) bool {
	if ctx == nil {
		return false
	}
	tracker, ok := ctx.Value(upstreamHTTPFallbackContextKey{}).(*upstreamHTTPFallbackTracker)
	if !ok || tracker == nil {
		return false
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	_, fellBack := tracker.authIDs[strings.TrimSpace(authID)]
	return fellBack
}
