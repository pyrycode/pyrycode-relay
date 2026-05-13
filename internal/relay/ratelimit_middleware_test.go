package relay

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// sentinelHandler is a minimal http.Handler that increments a counter on
// every invocation. The middleware tests assert the counter to lock in the
// "deny short-circuits before the wrapped handler runs" contract.
type sentinelHandler struct {
	calls atomic.Int64
}

func (s *sentinelHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.calls.Add(1)
	w.WriteHeader(http.StatusOK)
}

// newMiddlewareTestLimiter builds a limiter that never refills during the
// test (refillEvery=time.Hour) and registers cleanup. The middleware test
// suite is time-independent — burst values alone drive the allow/deny
// transitions.
func newMiddlewareTestLimiter(t *testing.T, burst int) *IPRateLimiter {
	t.Helper()
	l := NewIPRateLimiter(time.Hour, burst, time.Hour)
	t.Cleanup(l.Close)
	return l
}

// newMiddlewareRequest builds a request with RemoteAddr set as given. XFF is
// set only when xff is non-empty.
func newMiddlewareRequest(remoteAddr, xff string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = remoteAddr
	if xff != "" {
		r.Header.Set("X-Forwarded-For", xff)
	}
	return r
}

func TestRateLimitMiddleware_BurstThenDeny(t *testing.T) {
	t.Parallel()
	l := newMiddlewareTestLimiter(t, 2)
	sentinel := &sentinelHandler{}
	mw := NewRateLimitMiddleware(l, discardLogger(), false)
	h := mw(sentinel)

	for i := 0; i < 2; i++ {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, newMiddlewareRequest("1.1.1.1:1234", ""))
		if rr.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200", i+1, rr.Code)
		}
	}
	if got := sentinel.calls.Load(); got != 2 {
		t.Fatalf("sentinel calls after burst: got %d, want 2", got)
	}

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, newMiddlewareRequest("1.1.1.1:1234", ""))
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("denied request: status = %d, want 429", rr.Code)
	}
	if got := sentinel.calls.Load(); got != 2 {
		t.Fatalf("sentinel calls after deny: got %d, want 2 (handler must not run on deny)", got)
	}
}

func TestRateLimitMiddleware_PerIPIsolation(t *testing.T) {
	t.Parallel()
	l := newMiddlewareTestLimiter(t, 1)
	sentinel := &sentinelHandler{}
	h := NewRateLimitMiddleware(l, discardLogger(), false)(sentinel)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, newMiddlewareRequest("1.1.1.1:1", ""))
	if rr.Code != http.StatusOK {
		t.Fatalf("first 1.1.1.1: status = %d, want 200", rr.Code)
	}

	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, newMiddlewareRequest("1.1.1.1:1", ""))
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("second 1.1.1.1: status = %d, want 429", rr.Code)
	}

	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, newMiddlewareRequest("2.2.2.2:1", ""))
	if rr.Code != http.StatusOK {
		t.Fatalf("first 2.2.2.2: status = %d, want 200 (per-IP isolation)", rr.Code)
	}

	if got := sentinel.calls.Load(); got != 2 {
		t.Fatalf("sentinel calls: got %d, want 2", got)
	}
}

func TestRateLimitMiddleware_TrustXFF_BucketsByForgedIP(t *testing.T) {
	t.Parallel()
	l := newMiddlewareTestLimiter(t, 1)
	sentinel := &sentinelHandler{}
	h := NewRateLimitMiddleware(l, discardLogger(), true)(sentinel)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, newMiddlewareRequest("1.1.1.1:1", "9.9.9.9"))
	if rr.Code != http.StatusOK {
		t.Fatalf("first XFF=9.9.9.9: status = %d, want 200", rr.Code)
	}

	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, newMiddlewareRequest("1.1.1.1:1", "9.9.9.9"))
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("second XFF=9.9.9.9: status = %d, want 429 (bucket keyed by XFF)", rr.Code)
	}

	// Same RemoteAddr but a different XFF lands in a different bucket — the
	// trust-the-header model means the relay does not key by RemoteAddr.
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, newMiddlewareRequest("1.1.1.1:1", "8.8.8.8"))
	if rr.Code != http.StatusOK {
		t.Fatalf("first XFF=8.8.8.8: status = %d, want 200", rr.Code)
	}
}

func TestRateLimitMiddleware_TrustXFFOff_IgnoresHeader(t *testing.T) {
	t.Parallel()
	l := newMiddlewareTestLimiter(t, 1)
	sentinel := &sentinelHandler{}
	h := NewRateLimitMiddleware(l, discardLogger(), false)(sentinel)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, newMiddlewareRequest("1.1.1.1:1", "9.9.9.9"))
	if rr.Code != http.StatusOK {
		t.Fatalf("first request: status = %d, want 200", rr.Code)
	}

	// Default mode keys by RemoteAddr; the second request from 1.1.1.1
	// exhausts the bucket regardless of XFF.
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, newMiddlewareRequest("1.1.1.1:1", "9.9.9.9"))
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("second request: status = %d, want 429 (XFF must be ignored)", rr.Code)
	}
}

func TestRateLimitMiddleware_EmptyIPDeniesWithoutTouchingLimiter(t *testing.T) {
	t.Parallel()
	l := newMiddlewareTestLimiter(t, 1)
	sentinel := &sentinelHandler{}
	h := NewRateLimitMiddleware(l, discardLogger(), false)(sentinel)

	// RemoteAddr=="" causes net.SplitHostPort to fail; ClientIP returns "".
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, newMiddlewareRequest("", ""))
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("empty-IP request: status = %d, want 429", rr.Code)
	}
	if got := sentinel.calls.Load(); got != 0 {
		t.Fatalf("sentinel calls: got %d, want 0 (handler must not run)", got)
	}

	// The limiter must not have been consulted on the empty-IP path: an
	// empty-key bucket would have been created had Allow("") run. This
	// pins the AC's "deny before calling Allow" contract.
	l.mu.Lock()
	n := len(l.buckets)
	l.mu.Unlock()
	if n != 0 {
		t.Fatalf("limiter buckets after empty-IP request: got %d, want 0 (Allow must not have been called)", n)
	}
}

func TestRateLimitMiddleware_DenyEmitsOneLogLine(t *testing.T) {
	t.Parallel()
	l := newMiddlewareTestLimiter(t, 1)
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	h := NewRateLimitMiddleware(l, logger, false)(&sentinelHandler{})

	// Burn the bucket.
	h.ServeHTTP(httptest.NewRecorder(), newMiddlewareRequest("1.2.3.4:5555", ""))
	// Allowed request must not log.
	if buf.Len() != 0 {
		t.Fatalf("buffer after allowed request: got %q, want empty", buf.String())
	}

	// Denied request.
	h.ServeHTTP(httptest.NewRecorder(), newMiddlewareRequest("1.2.3.4:5555", ""))

	out := buf.String()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("log lines after one deny: got %d (%q), want 1", len(lines), out)
	}
	line := lines[0]
	if !strings.Contains(line, "level=WARN") {
		t.Errorf("log line missing level=WARN: %q", line)
	}
	if !strings.Contains(line, "msg=rate_limited") {
		t.Errorf("log line missing msg=rate_limited: %q", line)
	}
	// slog text handler renders the strconv.Quote-wrapped value as a
	// double-quoted string whose inner quotes are escaped: `remote="\"1.2.3.4\""`.
	if !strings.Contains(line, `remote="\"1.2.3.4\""`) {
		t.Errorf("log line missing quoted remote field: %q", line)
	}
	// Field set must be exactly {remote} — no token, no User-Agent, no
	// server-id leaked from the request.
	for _, forbidden := range []string{"server_id=", "binary_version=", "conn_id=", "device_name=", "user_agent="} {
		if strings.Contains(line, forbidden) {
			t.Errorf("log line contains forbidden field %q: %q", forbidden, line)
		}
	}
}

func TestRateLimitMiddleware_EmptyIPDenyLogsQuotedEmptyString(t *testing.T) {
	t.Parallel()
	l := newMiddlewareTestLimiter(t, 1)
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	h := NewRateLimitMiddleware(l, logger, false)(&sentinelHandler{})

	h.ServeHTTP(httptest.NewRecorder(), newMiddlewareRequest("", ""))

	line := buf.String()
	if !strings.Contains(line, `remote="\"\""`) {
		t.Errorf("empty-IP deny: log line missing remote=\"\\\"\\\"\": %q", line)
	}
}

func TestRateLimitMiddleware_AllowEmitsNoLogLine(t *testing.T) {
	t.Parallel()
	l := newMiddlewareTestLimiter(t, 2)
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	h := NewRateLimitMiddleware(l, logger, false)(&sentinelHandler{})

	h.ServeHTTP(httptest.NewRecorder(), newMiddlewareRequest("1.2.3.4:1", ""))
	h.ServeHTTP(httptest.NewRecorder(), newMiddlewareRequest("1.2.3.4:1", ""))

	if buf.Len() != 0 {
		t.Fatalf("buffer after allowed requests: got %q, want empty", buf.String())
	}
}

// TestRateLimitMiddleware_RegistryNotTouchedOnDeny pins the AC's structural
// guarantee that a denied attempt never reaches code paths that mutate the
// shared registry. The sentinel-counter assertion in the burst test already
// proves the wrapped handler does not run on deny; this is a belt-and-
// suspenders regression guard wired against a real ServerHandler.
func TestRateLimitMiddleware_RegistryNotTouchedOnDeny(t *testing.T) {
	t.Parallel()
	l := newMiddlewareTestLimiter(t, 1)
	reg := NewRegistry()
	logger := discardLogger()
	wrapped := NewRateLimitMiddleware(l, logger, false)(ServerHandler(reg, logger, time.Second, 256*1024))

	// Exhaust the bucket without going through the WS handler (no headers,
	// so even if it did run it would 400 — but the deny must short-circuit
	// before that, and the registry must remain pristine throughout).
	wrapped.ServeHTTP(httptest.NewRecorder(), newMiddlewareRequest("1.1.1.1:1", ""))
	// Denied attempt.
	rr := httptest.NewRecorder()
	wrapped.ServeHTTP(rr, newMiddlewareRequest("1.1.1.1:1", ""))
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("denied attempt: status = %d, want 429", rr.Code)
	}

	b, p := reg.Counts()
	if b != 0 || p != 0 {
		t.Fatalf("registry counts after denied attempt: binaries=%d phones=%d, want 0,0", b, p)
	}
}
