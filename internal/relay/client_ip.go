package relay

import (
	"net"
	"net/http"
	"strings"
)

// ClientIP extracts the client's source IP from an incoming HTTP request.
//
// When trustForwardedFor is false, ClientIP returns the host portion of
// r.RemoteAddr with the port stripped. Use this in deployments where the
// relay is directly internet-facing: X-Forwarded-For is attacker-controlled
// and must be ignored.
//
// When trustForwardedFor is true, ClientIP returns the left-most entry of
// X-Forwarded-For (the original client per the de-facto convention) with
// surrounding whitespace stripped. If the header is absent or yields no
// non-empty entry, ClientIP falls back to the host portion of r.RemoteAddr.
// Use this only when the relay is fronted by a reverse proxy the operator
// trusts to set XFF correctly.
//
// ClientIP returns the empty string only when no usable source is available
// (RemoteAddr cannot be parsed and either XFF was not consulted or yielded
// nothing). Callers MUST treat the empty string as "deny": the rate-limit
// wiring ticket enforces this. ClientIP itself performs no policy.
//
// The returned string is the raw host portion as it appears on the wire —
// no canonicalisation (no IPv6 lower-casing, no zone-id stripping). Callers
// that emit the string in log lines must quote it (e.g. via strconv.Quote)
// to avoid log injection from embedded control bytes.
func ClientIP(r *http.Request, trustForwardedFor bool) string {
	if trustForwardedFor {
		if v := r.Header.Get("X-Forwarded-For"); v != "" {
			if comma := strings.IndexByte(v, ','); comma >= 0 {
				v = v[:comma]
			}
			if first := strings.TrimSpace(v); first != "" {
				return first
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return ""
	}
	return host
}
