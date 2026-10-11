package relay

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
)

// ErrInvalidSourceConnCap identifies a non-positive source allowance.
var ErrInvalidSourceConnCap = errors.New("relay: source connection allowance must be positive")

// SourceConnCap shares a live-handler allowance across upgrade endpoints.
// Entries exist only while handlers are admitted; no source history survives
// their cleanup. The outer rate limiter must reject empty sources first.
type SourceConnCap struct {
	mu                sync.Mutex
	counts            map[string]int
	max               int
	logger            *slog.Logger
	trustForwardedFor bool
}

// NewSourceConnCap constructs a positive source allowance. The caller must
// also enforce that max is below the relay-wide cap before starting workers.
func NewSourceConnCap(max int, logger *slog.Logger, trustForwardedFor bool) (*SourceConnCap, error) {
	if max <= 0 {
		return nil, fmt.Errorf("max-connections-per-source %d: %w", max, ErrInvalidSourceConnCap)
	}
	return &SourceConnCap{counts: make(map[string]int), max: max, logger: logger, trustForwardedFor: trustForwardedFor}, nil
}

// Wrap reserves before next runs and releases only after its deferred cleanup.
// Place it inside rate limiting and outside ConnCap.Wrap: refused attempts
// take no global slot, and global denials return their source reservation.
func (c *SourceConnCap) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		source := ClientIP(r, c.trustForwardedFor)
		c.mu.Lock()
		if c.counts[source] >= c.max {
			c.mu.Unlock()
			http.Error(w, "", http.StatusServiceUnavailable)
			c.logger.Warn("source_conn_cap_reached", "remote", strconv.Quote(source))
			return
		}
		c.counts[source]++
		c.mu.Unlock()
		defer func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.counts[source]--
			if c.counts[source] == 0 {
				delete(c.counts, source)
			}
		}()
		next.ServeHTTP(w, r)
	})
}
