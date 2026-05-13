package relay

import (
	"log/slog"
	"net/http"
	"strconv"
)

// NewRateLimitMiddleware returns an http.Handler-wrapping middleware that
// rejects requests whose source IP has exhausted its token bucket. On a
// denied request, the middleware writes 429 Too Many Requests, emits one
// slog Warn line ("rate_limited", "remote", strconv.Quote(host)), and does
// NOT invoke the wrapped handler. Empty-IP requests (ClientIP returns "")
// are denied the same way before Allow is called — refusing un-attributable
// traffic without throttling matches the loud-failure rule in
// docs/PROJECT-MEMORY.md § Project-level conventions.
//
// trustForwardedFor is threaded into relay.ClientIP. Callers MUST NOT set
// this true unless a trusted reverse proxy fronts the relay — see the
// --trust-x-forwarded-for CLI flag's Usage string in cmd/pyrycode-relay.
func NewRateLimitMiddleware(limiter *IPRateLimiter, logger *slog.Logger, trustForwardedFor bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := ClientIP(r, trustForwardedFor)
			// Empty-IP guard must precede Allow: Allow("") is a normal map
			// key in the limiter's contract, not a deny.
			if ip == "" {
				http.Error(w, "", http.StatusTooManyRequests)
				logger.Warn("rate_limited", "remote", strconv.Quote(ip))
				return
			}
			if !limiter.Allow(ip) {
				http.Error(w, "", http.StatusTooManyRequests)
				logger.Warn("rate_limited", "remote", strconv.Quote(ip))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
