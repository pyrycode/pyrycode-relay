package relay

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

// ErrNonLoopbackBind is returned by CheckLoopbackBind and NewMetricsServer
// when --metrics-listen names a host that does not parse as a loopback IP
// literal. The relay refuses to start so a misconfiguration (typo,
// copy-paste from a non-loopback bind doc, accidental "0.0.0.0") does not
// silently publish operational state on the internet-exposed surface.
//
// Branchable via errors.Is.
var ErrNonLoopbackBind = errors.New("relay: metrics listener bind address must be a loopback IP literal")

// CheckLoopbackBind validates a --metrics-listen address. It returns nil
// only if addr parses as <ip-literal>:<port> with the host portion being a
// loopback IP (IPv4 127.0.0.0/8 or IPv6 ::1) and the port satisfying
// ListenerPort's rules (1..65535, port 0 rejected).
//
// Hostnames are deliberately refused even when they resolve to a loopback
// IP at validation time. A hostname resolves at validation time and again
// at bind time (different syscalls); between the two, DNS rebinding, an
// /etc/hosts race, or a resolver reconfigure can make a hostname that
// validated as loopback bind to a non-loopback IP. Refusing the entire
// shape is the only TOCTOU-proof defence. Do not "fix" this validator by
// adding net.LookupHost — that reintroduces the very race the IP-literal
// rule exists to close.
//
// The empty-addr case returns a non-nil error as defence in depth: the
// operator opt-out (--metrics-listen="") is structurally handled in
// NewMetricsServer before this function runs, so a caller that reaches
// CheckLoopbackBind with an empty string has already lost the opt-out
// shape and should fail.
func CheckLoopbackBind(addr string) error {
	if addr == "" {
		return fmt.Errorf("relay: metrics listener bind address is empty")
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("relay: parsing metrics listener address %q: %w", addr, err)
	}
	if _, err := ListenerPort(addr); err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("%w: %q has a non-IP-literal host (hostnames are rejected to avoid DNS-time TOCTOU; provide a loopback IP literal such as 127.0.0.1 or [::1])", ErrNonLoopbackBind, addr)
	}
	if !ip.IsLoopback() {
		return fmt.Errorf("%w: %q resolves to non-loopback IP %s", ErrNonLoopbackBind, addr, ip)
	}
	return nil
}

// NewMetricsServer constructs the *http.Server that serves /metrics on
// the operator-supplied loopback address. The opt-out path (addr == "")
// returns (nil, nil): the caller checks srv != nil before launching the
// goroutine, adding the port to the listener-allowlist set, or otherwise
// referencing the metrics listener — no listener, no goroutine, no error.
//
// Validation order: addr == "" is the structural opt-out and short-
// circuits before CheckLoopbackBind runs. Otherwise CheckLoopbackBind
// gates the construction; its error is returned verbatim so callers can
// branch on errors.Is(err, ErrNonLoopbackBind).
//
// Timeouts match the public listener (cmd/pyrycode-relay/main.go) so the
// metrics surface has the same DoS-resistance shape. They are duplicated
// here rather than exported as constants — each listener may drift
// independently if a future ticket has reason; today they match because
// that is the safest default.
//
// The handler argument is http.Handler, not *http.ServeMux: the caller
// chooses whether to wire the bare NewMetricsHandler(reg) result or wrap
// it in a mux so /metrics is distinguishable from a future sibling path.
func NewMetricsServer(addr string, h http.Handler) (*http.Server, error) {
	if addr == "" {
		return nil, nil
	}
	if err := CheckLoopbackBind(addr); err != nil {
		return nil, err
	}
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}, nil
}
