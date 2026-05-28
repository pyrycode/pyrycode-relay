package relay

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// pollUntil polls fn every 5ms until it returns true or deadline passes.
// Wraps the same shape used in server_endpoint_test.go to keep this file
// self-contained (no new test-helper file just for a 5-line loop).
func pollUntil(deadline time.Time, fn func() bool) bool {
	for time.Now().Before(deadline) {
		if fn() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return fn()
}

// newUpgradeBundle returns the full per-test wiring. The metrics
// registry is private to this bundle, so counter assertions are
// hermetic across t.Parallel runs.
func newUpgradeBundle() (*Registry, *UpgradeMetrics, http.Handler) {
	reg := NewRegistry()
	mreg := NewMetricsRegistry()
	m := NewUpgradeMetrics(mreg)
	return reg, m, NewMetricsHandler(mreg)
}

// allServerOutcomes / allClientOutcomes pin the 6-outcome label
// surface. Tests assert "exactly one cell at N, every other cell at 0"
// against these slices so that a future outcome added without a
// corresponding test exposure surfaces as a missing-cell zero
// assertion.
var allServerOutcomes = []string{
	"accept", "reject_headers", "reject_409", "reject_404", "reject_429", "reject_rate_limit",
}
var allClientOutcomes = []string{
	"accept", "reject_headers", "reject_409", "reject_404", "reject_429", "reject_rate_limit",
}
var allFailureKinds = []string{
	"no_server", "server_in_use", "phones_at_cap", "ratelimit",
}

// assertServerOutcomes asserts every (server, outcome) cell against
// the want map; missing keys are asserted as 0. Same shape for client.
func assertServerOutcomes(t *testing.T, h http.Handler, want map[string]int) {
	t.Helper()
	for _, outcome := range allServerOutcomes {
		v := want[outcome]
		assertCounter(t, h, "pyrycode_relay_ws_upgrade_attempts_total",
			`endpoint="server",outcome="`+outcome+`"`, v)
	}
}

func assertClientOutcomes(t *testing.T, h http.Handler, want map[string]int) {
	t.Helper()
	for _, outcome := range allClientOutcomes {
		v := want[outcome]
		assertCounter(t, h, "pyrycode_relay_ws_upgrade_attempts_total",
			`endpoint="client",outcome="`+outcome+`"`, v)
	}
}

func assertFailureKinds(t *testing.T, h http.Handler, want map[string]int) {
	t.Helper()
	for _, kind := range allFailureKinds {
		v := want[kind]
		assertCounter(t, h, "pyrycode_relay_register_failures_total",
			`kind="`+kind+`"`, v)
	}
}

func TestUpgradeMetrics_ServerEndpoint_TerminalPaths(t *testing.T) {
	t.Parallel()

	t.Run("accept", func(t *testing.T) {
		t.Parallel()
		reg, m, scrape := newUpgradeBundle()
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		srv := httptest.NewServer(ServerHandler(reg, logger, 100*time.Millisecond, 256*1024, m))
		defer srv.Close()
		wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		c, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: validHeaders("s1")})
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer c.Close(websocket.StatusNormalClosure, "")

		if !pollUntil(time.Now().Add(time.Second), func() bool {
			_, ok := reg.BinaryFor("s1")
			return ok
		}) {
			t.Fatalf("BinaryFor(s1) never registered")
		}

		assertServerOutcomes(t, scrape, map[string]int{"accept": 1})
		assertFailureKinds(t, scrape, nil)
	})

	t.Run("reject_headers", func(t *testing.T) {
		t.Parallel()
		reg, m, scrape := newUpgradeBundle()
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		srv := httptest.NewServer(ServerHandler(reg, logger, 100*time.Millisecond, 256*1024, m))
		defer srv.Close()

		// Missing X-Pyrycode-Server → header-gate 400.
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/", nil)
		req.Header.Set("X-Pyrycode-Version", "0.1.0-test")
		req.Header.Set("User-Agent", "pyry-test/0.1.0")
		resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
		if err != nil {
			t.Fatalf("do: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", resp.StatusCode)
		}

		assertServerOutcomes(t, scrape, map[string]int{"reject_headers": 1})
		assertFailureKinds(t, scrape, nil)
		_ = reg
	})

	t.Run("reject_409", func(t *testing.T) {
		t.Parallel()
		reg, m, scrape := newUpgradeBundle()
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		srv := httptest.NewServer(ServerHandler(reg, logger, 100*time.Millisecond, 256*1024, m))
		defer srv.Close()
		wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		c1, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: validHeaders("s1")})
		if err != nil {
			t.Fatalf("dial #1: %v", err)
		}
		defer c1.Close(websocket.StatusNormalClosure, "")
		if !pollUntil(time.Now().Add(time.Second), func() bool {
			_, ok := reg.BinaryFor("s1")
			return ok
		}) {
			t.Fatalf("first claim did not register")
		}

		c2, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: validHeaders("s1")})
		if err != nil {
			t.Fatalf("dial #2: %v", err)
		}
		defer c2.Close(websocket.StatusNormalClosure, "")
		readCtx, cancelRead := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelRead()
		_, _, readErr := c2.Read(readCtx)
		var ce websocket.CloseError
		if !errors.As(readErr, &ce) || ce.Code != websocket.StatusCode(4409) {
			t.Fatalf("close err = %v, want 4409", readErr)
		}

		assertServerOutcomes(t, scrape, map[string]int{"accept": 1, "reject_409": 1})
		assertFailureKinds(t, scrape, map[string]int{"server_in_use": 1})
	})

	t.Run("reject_rate_limit", func(t *testing.T) {
		t.Parallel()
		reg, m, scrape := newUpgradeBundle()
		_ = reg
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		// Burst 1 → the second request from the same IP is denied with
		// HTTP 429 BEFORE the upgrade handler is reached. The deny
		// observer sits OUTSIDE the rate-limit middleware so the 429
		// from the middleware is what the observer sees.
		limiter := newMiddlewareTestLimiter(t, 1)
		rateLimit := NewRateLimitMiddleware(limiter, logger, false)
		// Real ServerHandler in the inner position so the wiring shape
		// matches main.go; the bucket-deny ensures it is never reached.
		inner := rateLimit(ServerHandler(NewRegistry(), logger, time.Second, 256*1024, m))
		wrapped := m.WrapServerRateLimitDeny(inner)

		// One allowed (the inner ServerHandler will 400 on missing
		// headers — the observer does NOT increment on that), one
		// denied.
		rr1 := httptest.NewRecorder()
		wrapped.ServeHTTP(rr1, newMiddlewareRequest("1.1.1.1:1", ""))
		rr2 := httptest.NewRecorder()
		wrapped.ServeHTTP(rr2, newMiddlewareRequest("1.1.1.1:1", ""))
		if rr2.Code != http.StatusTooManyRequests {
			t.Fatalf("denied attempt: status = %d, want 429", rr2.Code)
		}

		assertServerOutcomes(t, scrape, map[string]int{
			"reject_headers":    1, // the first attempt header-gated
			"reject_rate_limit": 1, // the second attempt rate-limited
		})
		assertFailureKinds(t, scrape, map[string]int{"ratelimit": 1})
	})
}

func TestUpgradeMetrics_ClientEndpoint_TerminalPaths(t *testing.T) {
	t.Parallel()

	t.Run("accept", func(t *testing.T) {
		t.Parallel()
		reg, m, scrape := newUpgradeBundle()
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		srv := httptest.NewServer(ClientHandler(reg, logger, 256*1024, 0, m))
		defer srv.Close()
		seedBinary(t, reg, "s1")
		wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		c, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: validClientHeaders("s1")})
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer c.Close(websocket.StatusNormalClosure, "")
		waitForPhones(t, reg, "s1", 1, time.Second)

		assertClientOutcomes(t, scrape, map[string]int{"accept": 1})
		assertFailureKinds(t, scrape, nil)
	})

	t.Run("reject_headers", func(t *testing.T) {
		t.Parallel()
		reg, m, scrape := newUpgradeBundle()
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		srv := httptest.NewServer(ClientHandler(reg, logger, 256*1024, 0, m))
		defer srv.Close()

		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/", nil)
		req.Header.Set("X-Pyrycode-Token", "phone-token-opaque")
		req.Header.Set("User-Agent", "pyry-test/0.1.0")
		resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
		if err != nil {
			t.Fatalf("do: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", resp.StatusCode)
		}

		assertClientOutcomes(t, scrape, map[string]int{"reject_headers": 1})
		assertFailureKinds(t, scrape, nil)
		_ = reg
	})

	t.Run("reject_404", func(t *testing.T) {
		t.Parallel()
		reg, m, scrape := newUpgradeBundle()
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		srv := httptest.NewServer(ClientHandler(reg, logger, 256*1024, 0, m))
		defer srv.Close()
		// No seedBinary — RegisterPhoneCapped returns ErrNoServer.
		wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		c, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: validClientHeaders("s1")})
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer c.Close(websocket.StatusNormalClosure, "")
		readCtx, cancelRead := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelRead()
		_, _, readErr := c.Read(readCtx)
		var ce websocket.CloseError
		if !errors.As(readErr, &ce) || ce.Code != websocket.StatusCode(4404) {
			t.Fatalf("close err = %v, want 4404", readErr)
		}

		assertClientOutcomes(t, scrape, map[string]int{"reject_404": 1})
		assertFailureKinds(t, scrape, map[string]int{"no_server": 1})
		_ = reg
	})

	t.Run("reject_429", func(t *testing.T) {
		t.Parallel()
		const cap = 1
		reg, m, scrape := newUpgradeBundle()
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		srv := httptest.NewServer(ClientHandler(reg, logger, 256*1024, cap, m))
		defer srv.Close()
		seedBinary(t, reg, "s1")
		wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// First phone fills the cap.
		c1, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: validClientHeaders("s1")})
		if err != nil {
			t.Fatalf("dial first: %v", err)
		}
		defer c1.Close(websocket.StatusNormalClosure, "")
		waitForPhones(t, reg, "s1", cap, time.Second)

		// Second phone trips ErrPhonesAtCap.
		c2, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: validClientHeaders("s1")})
		if err != nil {
			t.Fatalf("dial over-cap: %v", err)
		}
		defer c2.Close(websocket.StatusNormalClosure, "")
		readCtx, cancelRead := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelRead()
		_, _, readErr := c2.Read(readCtx)
		var ce websocket.CloseError
		if !errors.As(readErr, &ce) || ce.Code != websocket.StatusCode(4429) {
			t.Fatalf("close err = %v, want 4429", readErr)
		}

		assertClientOutcomes(t, scrape, map[string]int{
			"accept":     1, // first phone
			"reject_429": 1, // second phone
		})
		assertFailureKinds(t, scrape, map[string]int{"phones_at_cap": 1})
	})

	t.Run("reject_rate_limit", func(t *testing.T) {
		t.Parallel()
		_, m, scrape := newUpgradeBundle()
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		limiter := newMiddlewareTestLimiter(t, 1)
		rateLimit := NewRateLimitMiddleware(limiter, logger, false)
		inner := rateLimit(ClientHandler(NewRegistry(), logger, 256*1024, 0, m))
		wrapped := m.WrapClientRateLimitDeny(inner)

		rr1 := httptest.NewRecorder()
		wrapped.ServeHTTP(rr1, newMiddlewareRequest("1.1.1.1:1", ""))
		rr2 := httptest.NewRecorder()
		wrapped.ServeHTTP(rr2, newMiddlewareRequest("1.1.1.1:1", ""))
		if rr2.Code != http.StatusTooManyRequests {
			t.Fatalf("denied attempt: status = %d, want 429", rr2.Code)
		}

		assertClientOutcomes(t, scrape, map[string]int{
			"reject_headers":    1,
			"reject_rate_limit": 1,
		})
		assertFailureKinds(t, scrape, map[string]int{"ratelimit": 1})
	})
}

// TestUpgradeMetrics_RegisterFailures_CoIncrement pins the AC invariant
// that pyrycode_relay_register_failures_total{kind} co-increments in
// lockstep with the matching pyrycode_relay_ws_upgrade_attempts_total
// {endpoint,outcome=reject_*} cell — the composite event-named methods
// are the increment contract, so a single method bumps both.
func TestUpgradeMetrics_RegisterFailures_CoIncrement(t *testing.T) {
	t.Parallel()
	_, m, scrape := newUpgradeBundle()

	// Drive each failure kind once via the event methods (no handler
	// machinery needed — the methods are the contract under test).
	m.ServerIDConflict()
	m.ClientNoServer()
	m.ClientPhonesAtCap()
	m.ServerRateLimitDeny()
	m.ClientRateLimitDeny()

	// server_in_use ↔ server,reject_409
	assertCounter(t, scrape, "pyrycode_relay_register_failures_total", `kind="server_in_use"`, 1)
	assertCounter(t, scrape, "pyrycode_relay_ws_upgrade_attempts_total", `endpoint="server",outcome="reject_409"`, 1)
	// no_server ↔ client,reject_404
	assertCounter(t, scrape, "pyrycode_relay_register_failures_total", `kind="no_server"`, 1)
	assertCounter(t, scrape, "pyrycode_relay_ws_upgrade_attempts_total", `endpoint="client",outcome="reject_404"`, 1)
	// phones_at_cap ↔ client,reject_429
	assertCounter(t, scrape, "pyrycode_relay_register_failures_total", `kind="phones_at_cap"`, 1)
	assertCounter(t, scrape, "pyrycode_relay_ws_upgrade_attempts_total", `endpoint="client",outcome="reject_429"`, 1)
	// ratelimit ↔ {server,client},reject_rate_limit (sum on kind = sum
	// on the matching outcome value).
	assertCounter(t, scrape, "pyrycode_relay_register_failures_total", `kind="ratelimit"`, 2)
	assertCounter(t, scrape, "pyrycode_relay_ws_upgrade_attempts_total", `endpoint="server",outcome="reject_rate_limit"`, 1)
	assertCounter(t, scrape, "pyrycode_relay_ws_upgrade_attempts_total", `endpoint="client",outcome="reject_rate_limit"`, 1)
}

// TestUpgradeMetrics_AllSixteenSeries_Exposed asserts the full label
// surface is visible after a per-method exercise: 12 endpoint×outcome
// cells (9 reachable advanced to 1, 3 structurally unreachable at 0)
// plus 4 failure-kind cells (all advanced to 1) = 16 series.
func TestUpgradeMetrics_AllSixteenSeries_Exposed(t *testing.T) {
	t.Parallel()
	_, m, scrape := newUpgradeBundle()

	// Advance every reachable method exactly once. The composite
	// methods take care of the failure-kind cells.
	m.ServerAccept()
	m.ServerHeaderReject()
	m.ServerIDConflict()
	m.ServerRateLimitDeny()
	m.ClientAccept()
	m.ClientHeaderReject()
	m.ClientNoServer()
	m.ClientPhonesAtCap()
	m.ClientRateLimitDeny()

	// 9 reachable endpoint×outcome cells at 1.
	assertServerOutcomes(t, scrape, map[string]int{
		"accept":            1,
		"reject_headers":    1,
		"reject_409":        1,
		"reject_rate_limit": 1,
	})
	assertClientOutcomes(t, scrape, map[string]int{
		"accept":            1,
		"reject_headers":    1,
		"reject_404":        1,
		"reject_429":        1,
		"reject_rate_limit": 1,
	})

	// 3 structurally unreachable cells exposed at 0.
	assertCounter(t, scrape, "pyrycode_relay_ws_upgrade_attempts_total", `endpoint="server",outcome="reject_404"`, 0)
	assertCounter(t, scrape, "pyrycode_relay_ws_upgrade_attempts_total", `endpoint="server",outcome="reject_429"`, 0)
	assertCounter(t, scrape, "pyrycode_relay_ws_upgrade_attempts_total", `endpoint="client",outcome="reject_409"`, 0)

	// 4 failure-kind cells at the totals implied by the composite
	// methods: server_in_use=1, no_server=1, phones_at_cap=1, ratelimit=2.
	assertFailureKinds(t, scrape, map[string]int{
		"server_in_use": 1,
		"no_server":     1,
		"phones_at_cap": 1,
		"ratelimit":     2,
	})
}

// TestUpgradeMetrics_NoGlobalRegistrarLeak pins ADR-0008 § Scope of use
// locally — same shape as TestMetricsRegistry_NoGlobalRegistrarLeak.
// Constructing the upgrade vectors against a private registry MUST NOT
// touch prometheus.DefaultRegisterer.
func TestUpgradeMetrics_NoGlobalRegistrarLeak(t *testing.T) {
	t.Parallel()
	before := defaultGathererSize(t)
	_ = NewUpgradeMetrics(NewMetricsRegistry())
	after := defaultGathererSize(t)
	if before != after {
		t.Fatalf("default registerer changed: before=%d, after=%d "+
			"(NewUpgradeMetrics must never register on prometheus.DefaultRegisterer; "+
			"see ADR-0008 § Scope of use)", before, after)
	}
}

