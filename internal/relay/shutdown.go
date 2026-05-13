package relay

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"

	"nhooyr.io/websocket"
)

// gracefulCloser is the narrow contract that lets Shutdown emit a
// specific WebSocket close code on each live conn. *WSConn satisfies
// it; the registry's Conn interface intentionally does not, so tests
// (fakeConn) and any future Conn impls that do not need code-aware
// close fall back to the plain Close() path.
type gracefulCloser interface {
	CloseWithCode(code websocket.StatusCode, reason string)
}

// Shutdown initiates a graceful drain. It concurrently invokes
// http.Server.Shutdown on each server (which closes their listeners
// and waits for non-hijacked handlers), snapshots every WS conn from
// reg, and emits a 1001 StatusGoingAway close frame on each. It
// returns once both fan-outs complete or ctx expires.
//
// On ctx expiry it force-closes each server via http.Server.Close
// before returning ctx.Err(); the helper does not wait for the
// per-conn close goroutines that are still mid-handshake — those
// will be reaped by process exit.
//
// Errors from srv.Shutdown are logged but do not abort the drain.
// Every server gets a Shutdown call, every conn gets a close call.
// The function's return reflects only deadline outcome:
//
//   - nil on clean drain within deadline,
//   - ctx.Err() on deadline expiry.
//
// Safe to call exactly once per process. Concurrent or repeat
// invocations are undefined.
func Shutdown(ctx context.Context, logger *slog.Logger, reg *Registry, servers ...*http.Server) error {
	var wg sync.WaitGroup

	for _, srv := range servers {
		srv := srv
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := srv.Shutdown(ctx); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				logger.Error("http server shutdown", "err", err)
			}
		}()
	}

	conns := reg.Snapshot()
	for _, c := range conns {
		c := c
		wg.Add(1)
		go func() {
			defer wg.Done()
			if gc, ok := c.(gracefulCloser); ok {
				gc.CloseWithCode(websocket.StatusGoingAway, "shutting down")
				return
			}
			c.Close()
		}()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		logger.Info("shutdown complete")
		return nil
	case <-ctx.Done():
		for _, srv := range servers {
			_ = srv.Close()
		}
		logger.Warn("shutdown deadline exceeded; force-closed listeners", "err", ctx.Err())
		return ctx.Err()
	}
}
