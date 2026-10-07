package auth

import (
	"strconv"
	"strings"
	"sync"
	"time"
)

// AttributeWebsockets is the attribute and metadata key that opts a credential in or
// out of the upstream Responses WebSocket transport.
const AttributeWebsockets = "websockets"

const (
	upstreamWebsocketBackoffBase = time.Minute
	upstreamWebsocketBackoffMax  = 30 * time.Minute
)

// WebsocketsEnabled reports whether a credential is configured to use the upstream
// Responses WebSocket transport. An explicit "websockets" attribute or metadata value
// always wins. Without one, Codex OAuth (subscription) credentials default to enabled
// and every other credential defaults to disabled.
func WebsocketsEnabled(auth *Auth) bool {
	if auth == nil {
		return false
	}
	if enabled, ok := explicitWebsocketsSetting(auth); ok {
		return enabled
	}
	return strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") && auth.AuthKind() == AuthKindOAuth
}

func explicitWebsocketsSetting(auth *Auth) (bool, bool) {
	if raw := authAttribute(auth, AttributeWebsockets); raw != "" {
		if parsed, errParse := strconv.ParseBool(raw); errParse == nil {
			return parsed, true
		}
	}
	if len(auth.Metadata) == 0 {
		return false, false
	}
	switch value := auth.Metadata[AttributeWebsockets].(type) {
	case bool:
		return value, true
	case string:
		if parsed, errParse := strconv.ParseBool(strings.TrimSpace(value)); errParse == nil {
			return parsed, true
		}
	}
	return false, false
}

// UpstreamWebsocketDialAllowed reports whether a new upstream WebSocket connection
// should be dialed for the credential. It is false while the credential is backing
// off after a WebSocket transport failure; requests then use HTTP/SSE instead.
func UpstreamWebsocketDialAllowed(auth *Auth) bool {
	if !WebsocketsEnabled(auth) {
		return false
	}
	_, backingOff := upstreamWebsocketTransportHealth.backoffUntil(auth.ID)
	return !backingOff
}

// MarkUpstreamWebsocketFailure records a WebSocket transport failure for the
// credential and returns when new connections may be attempted again. Repeated
// failures double the wait up to 30 minutes.
func MarkUpstreamWebsocketFailure(authID string) time.Time {
	return upstreamWebsocketTransportHealth.markFailure(authID)
}

// MarkUpstreamWebsocketSuccess clears the credential's WebSocket failure backoff.
func MarkUpstreamWebsocketSuccess(authID string) {
	upstreamWebsocketTransportHealth.markSuccess(authID)
}

type upstreamWebsocketBackoff struct {
	until    time.Time
	failures int
}

type upstreamWebsocketHealth struct {
	mu      sync.Mutex
	nowFunc func() time.Time
	entries map[string]upstreamWebsocketBackoff
}

var upstreamWebsocketTransportHealth = &upstreamWebsocketHealth{
	nowFunc: time.Now,
	entries: make(map[string]upstreamWebsocketBackoff),
}

func (h *upstreamWebsocketHealth) now() time.Time {
	if h.nowFunc == nil {
		return time.Now()
	}
	return h.nowFunc()
}

func (h *upstreamWebsocketHealth) backoffUntil(authID string) (time.Time, bool) {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return time.Time{}, false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	entry, ok := h.entries[authID]
	if !ok || !entry.until.After(h.now()) {
		return time.Time{}, false
	}
	return entry.until, true
}

func (h *upstreamWebsocketHealth) markFailure(authID string) time.Time {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return time.Time{}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	entry := h.entries[authID]
	entry.failures++
	wait := upstreamWebsocketBackoffBase
	for i := 1; i < entry.failures && wait < upstreamWebsocketBackoffMax; i++ {
		wait *= 2
	}
	if wait > upstreamWebsocketBackoffMax {
		wait = upstreamWebsocketBackoffMax
	}
	entry.until = h.now().Add(wait)
	h.entries[authID] = entry
	return entry.until
}

func (h *upstreamWebsocketHealth) markSuccess(authID string) {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.entries, authID)
}
