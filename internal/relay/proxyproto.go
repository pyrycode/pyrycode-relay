package relay

import (
	"errors"
	"fmt"
	"net"
	"time"

	proxyproto "github.com/pires/go-proxyproto"
)

// ErrProxyHeaderRejected is returned (wrapped) by validateProxyHeader for a
// syntactically valid PROXY header that is not a v2 PROXY-command header
// carrying a TCP source address. Such a header would otherwise leave the
// library's RemoteAddr on the socket peer while reads succeed — the
// fallback the listener exists to rule out.
var ErrProxyHeaderRejected = errors.New("relay: PROXY header rejected")

// NewProxyProtoListener wraps inner so that every accepted connection must
// open with a PROXY protocol v2 header, received within headerTimeout,
// before any other byte (in production: before the TLS handshake). The
// header's source address becomes the connection's RemoteAddr, and so the
// request's RemoteAddr that ClientIP and the per-IP rate limiter key on.
//
// A connection whose header is missing, malformed, rejected by
// validateProxyHeader, or late fails its first Read; http.Server then
// closes it without invoking a handler. The header is read lazily on the
// connection's own goroutine (http.Server's per-conn serve), never in
// Accept, so a stalled header cannot hold up other connections.
//
// headerTimeout must be positive: the library reads 0 as its 10s default
// and a negative value as "no timeout".
func NewProxyProtoListener(inner net.Listener, headerTimeout time.Duration) (net.Listener, error) {
	if headerTimeout <= 0 {
		return nil, fmt.Errorf("relay: PROXY header timeout must be positive, got %v", headerTimeout)
	}
	return &proxyproto.Listener{
		Listener: inner,
		// Explicit REQUIRE rather than proxyproto.DefaultPolicy, a mutable
		// package global.
		ConnPolicy: func(proxyproto.ConnPolicyOptions) (proxyproto.Policy, error) {
			return proxyproto.REQUIRE, nil
		},
		ValidateHeader:    validateProxyHeader,
		ReadHeaderTimeout: headerTimeout,
	}, nil
}

// validateProxyHeader accepts only v2 PROXY-command headers whose source is
// a TCP address (TCPv4 or TCPv6). LOCAL commands, UNSPEC and unix
// transports, and v1 text headers are rejected.
func validateProxyHeader(h *proxyproto.Header) error {
	if h.Version != 2 {
		return fmt.Errorf("%w: version %d", ErrProxyHeaderRejected, h.Version)
	}
	if !h.Command.IsProxy() {
		return fmt.Errorf("%w: not a PROXY command", ErrProxyHeaderRejected)
	}
	if _, ok := h.SourceAddr.(*net.TCPAddr); !ok {
		return fmt.Errorf("%w: source is not a TCP address", ErrProxyHeaderRejected)
	}
	return nil
}
