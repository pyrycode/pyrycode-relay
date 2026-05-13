package relay

import (
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
)

// ErrUnexpectedListener is returned by CheckListenerPorts when the set
// of ports the process is about to bind contains any port outside the
// expected set (derived from parsed flags). Stray listeners
// (net/http/pprof on :6060, a forgotten debug exporter, a metrics
// endpoint accidentally enabled) are the threat shape: an unauthenticated
// HTTP surface on the public internet. The relay refuses to start so the
// misconfiguration fails the deploy's health check rather than serving
// traffic on the surplus port.
//
// Branchable via errors.Is. The wrapped error names the offending
// port(s) and the expected-ports set.
var ErrUnexpectedListener = errors.New("relay: process is about to bind a TCP port outside the expected set")

// ListenerPort extracts the TCP port from an http.Server Addr value
// such as ":443", "127.0.0.1:8080", or "[::1]:443". Returns a wrapped
// error if addr is empty or does not contain a parseable port in
// 1..65535. Used by cmd/pyrycode-relay to canonicalise both the expected
// (flag-derived) and actual (http.Server.Addr-derived) port sets onto
// the same uint16 key.
//
// Port 0 is rejected explicitly: it means "pick an ephemeral port" in
// net.Listen semantics, which the relay never wants and which would
// defeat the asymmetric set check by smuggling an unknown bound port
// past the actual-set construction.
func ListenerPort(addr string) (uint16, error) {
	if addr == "" {
		return 0, fmt.Errorf("relay: listener address is empty")
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return 0, fmt.Errorf("relay: parsing listener address %q: %w", addr, err)
	}
	n, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return 0, fmt.Errorf("relay: parsing listener address %q: %w", addr, err)
	}
	if n == 0 {
		return 0, fmt.Errorf("relay: parsing listener address %q: port 0 is not permitted (ephemeral-port placeholder)", addr)
	}
	return uint16(n), nil
}

// CheckListenerPorts returns ErrUnexpectedListener (wrapped, naming the
// offending port(s) in ascending order plus the expected-set contents)
// when any element of actual is absent from expected. Returns nil
// otherwise.
//
// The check is asymmetric: missing ports (actual ⊊ expected) are NOT an
// error — a failure to bind an expected port surfaces as a runtime
// listener error from http.Server.ListenAndServe, which has its own
// exit path. This check exists to catch the surplus-listener case
// (actual ⊋ expected) where an unauthenticated debug endpoint would
// otherwise be exposed on the internet.
//
// Both arguments are sets keyed by TCP port (uint16). Empty sets are
// valid inputs: CheckListenerPorts(∅, ∅) returns nil.
func CheckListenerPorts(expected, actual map[uint16]struct{}) error {
	var surplus []uint16
	for p := range actual {
		if _, ok := expected[p]; !ok {
			surplus = append(surplus, p)
		}
	}
	if len(surplus) == 0 {
		return nil
	}
	slices.Sort(surplus)

	sortedExpected := make([]uint16, 0, len(expected))
	for p := range expected {
		sortedExpected = append(sortedExpected, p)
	}
	slices.Sort(sortedExpected)

	return fmt.Errorf("%w: unexpected ports %v; expected ports %v", ErrUnexpectedListener, surplus, sortedExpected)
}
