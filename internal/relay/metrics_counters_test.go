package relay

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// assertCounter scrapes h and asserts that body contains the literal line
// `<metric><labels?> <want>` in the Prometheus text format. labelStr may be
// empty for unlabelled counters. Substring-match posture mirrors
// assertGauge in metrics_connections_test.go — the body also carries
// promhttp_metric_handler_* self-instrumentation lines.
func assertCounter(t *testing.T, h http.Handler, metric, labelStr string, want int) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("scrape status: got %d, want 200", rec.Code)
	}
	var wantLine string
	if labelStr == "" {
		wantLine = fmt.Sprintf("%s %d", metric, want)
	} else {
		wantLine = fmt.Sprintf("%s{%s} %d", metric, labelStr, want)
	}
	if !strings.Contains(rec.Body.String(), wantLine) {
		t.Errorf("scrape body missing %q; got:\n%s", wantLine, rec.Body.String())
	}
}

func TestForwardMetrics_PhoneToBinary_OnlyOnSuccess(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	mreg := NewMetricsRegistry()
	NewForwardMetrics(mreg, reg)
	h := NewMetricsHandler(mreg)

	bin := &fakeBinary{id: "bin-s1"}
	if err := reg.ClaimServer("s1", bin); err != nil {
		t.Fatalf("ClaimServer: %v", err)
	}
	phone := newFakePhone("client-s1-aa00bb11")
	if err := reg.RegisterPhone("s1", &registryConn{phone}); err != nil {
		t.Fatalf("RegisterPhone: %v", err)
	}

	done, cancel := runForwarder(reg, "s1", phone)
	defer cancel()

	for i := 0; i < 3; i++ {
		phone.frames <- []byte(fmt.Sprintf(`{"i":%d}`, i))
	}
	waitForSent(t, bin, 3, 2*time.Second)

	assertCounter(t, h, "pyrycode_relay_frames_forwarded_total", `direction="phone_to_binary"`, 3)
	// binary_to_phone direction registered (label cardinality 2) but never
	// incremented — assert it stays at 0.
	assertCounter(t, h, "pyrycode_relay_frames_forwarded_total", `direction="binary_to_phone"`, 0)

	// Close to drain forwarder cleanly before the next subscenario.
	close(phone.frames)
	<-done

	// Negative case: Send returns an error → no increment. New forwarder,
	// new phone; reuse the same binary with a sticky sendErr.
	bin.mu.Lock()
	bin.sendErr = errors.New("nope")
	bin.mu.Unlock()
	phone2 := newFakePhone("client-s1-aa00bb12")
	if err := reg.RegisterPhone("s1", &registryConn{phone2}); err != nil {
		t.Fatalf("RegisterPhone phone2: %v", err)
	}
	done2, cancel2 := runForwarder(reg, "s1", phone2)
	defer cancel2()
	phone2.frames <- []byte(`{"send":"will-fail"}`)
	select {
	case <-done2:
	case <-time.After(time.Second):
		t.Fatal("forwarder did not return after Send error")
	}

	// Still 3; the failed Send did not increment.
	assertCounter(t, h, "pyrycode_relay_frames_forwarded_total", `direction="phone_to_binary"`, 3)

	// No-binary case: no ClaimServer for s2; counter unaffected.
	phone3 := newFakePhone("client-s2-cc00dd11")
	done3, cancel3 := runForwarder(reg, "s2", phone3)
	defer cancel3()
	phone3.frames <- []byte(`{"no":"binary"}`)
	select {
	case err := <-done3:
		if err != nil {
			t.Fatalf("forwarder return = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("forwarder did not return on missing binary")
	}
	assertCounter(t, h, "pyrycode_relay_frames_forwarded_total", `direction="phone_to_binary"`, 3)
}

func TestForwardMetrics_BinaryToPhone_OnlyOnSuccess(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	mreg := NewMetricsRegistry()
	NewForwardMetrics(mreg, reg)
	h := NewMetricsHandler(mreg)

	p1 := newFakePhone("client-s1-aaaa1111")
	p2 := newFakePhone("client-s1-bbbb2222")
	pErr := newFakePhone("client-s1-cccc3333")
	pErr.sendErr = errors.New("phone wedged")
	claimAndRegister(t, reg, "s1", p1, p2, pErr)

	src := newFakeBinarySource("bin-s1")
	done, cancel := runBinaryForwarder(reg, "s1", src)
	defer cancel()

	// 2 well-formed envelopes to a registered phone → +2.
	src.frames <- mustMarshal(t, p1.ConnID(), []byte(`{"i":1}`))
	src.frames <- mustMarshal(t, p2.ConnID(), []byte(`{"i":2}`))
	waitForPhoneSent(t, p1, 1, 2*time.Second)
	waitForPhoneSent(t, p2, 1, 2*time.Second)

	// Malformed envelope → continue, no increment.
	src.frames <- []byte("not-json")
	// Unknown conn_id → continue, no increment.
	src.frames <- mustMarshal(t, "client-s1-deadbeef", []byte(`{"dropped":true}`))
	// Phone send error → continue, no increment.
	src.frames <- mustMarshal(t, pErr.ConnID(), []byte(`{"wedged":true}`))

	// Send a final well-formed envelope so we can synchronise on its
	// delivery — once it lands, the three preceding continue paths must
	// have already been processed.
	src.frames <- mustMarshal(t, p1.ConnID(), []byte(`{"i":3}`))
	waitForPhoneSent(t, p1, 2, 2*time.Second)

	close(src.frames)
	<-done

	// Three real successes (i=1 to p1, i=2 to p2, i=3 to p1).
	assertCounter(t, h, "pyrycode_relay_frames_forwarded_total", `direction="binary_to_phone"`, 3)
	// phone_to_binary direction registered but unused.
	assertCounter(t, h, "pyrycode_relay_frames_forwarded_total", `direction="phone_to_binary"`, 0)
}

func TestGraceMetrics_OnlyOnRealEviction(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	mreg := NewMetricsRegistry()
	NewGraceMetrics(mreg, r)
	h := NewMetricsHandler(mreg)

	if err := r.ClaimServer("s1", &fakeConn{id: "b-1"}); err != nil {
		t.Fatalf("ClaimServer s1: %v", err)
	}
	r.ScheduleReleaseServer("s1", 5*time.Millisecond)

	// Wait for the real eviction. Poll until BinaryFor("s1") clears.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, ok := r.BinaryFor("s1"); !ok {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if _, ok := r.BinaryFor("s1"); ok {
		t.Fatal("grace timer did not fire within timeout")
	}

	assertCounter(t, h, "pyrycode_relay_grace_expiries_total", "", 1)

	// Stale-fire scenario: arm + cancel-replace before timer fires.
	if err := r.ClaimServer("s2", &fakeConn{id: "b-2"}); err != nil {
		t.Fatalf("ClaimServer s2: %v", err)
	}
	r.ScheduleReleaseServer("s2", 5*time.Millisecond)
	if err := r.ClaimServer("s2", &fakeConn{id: "b-2-prime"}); err != nil {
		t.Fatalf("ClaimServer s2 reclaim: %v", err)
	}

	// Sleep past the original grace window so the stale fire reaches the
	// AfterFunc body and is then short-circuited by the pointer-identity
	// guard. If the guard were broken the counter would move to 2.
	time.Sleep(30 * time.Millisecond)

	assertCounter(t, h, "pyrycode_relay_grace_expiries_total", "", 1)
}

func TestGraceMetrics_RaceFreedom(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	mreg := NewMetricsRegistry()
	NewGraceMetrics(mreg, r)
	h := NewMetricsHandler(mreg)

	const goroutines = 16
	const opsPer = 200

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		g := g
		go func() {
			defer wg.Done()
			sid := fmt.Sprintf("s-%d", g%4)
			for i := 0; i < opsPer; i++ {
				_ = r.ClaimServer(sid, &fakeConn{id: fmt.Sprintf("b-%d-%d", g, i)})
				r.ScheduleReleaseServer(sid, time.Millisecond)
				if i%2 == 0 {
					_ = r.ClaimServer(sid, &fakeConn{id: fmt.Sprintf("b2-%d-%d", g, i)})
				}
			}
		}()
	}
	wg.Wait()

	// Drain in-flight timers.
	time.Sleep(50 * time.Millisecond)

	// Race detector is the real assertion. Sanity-check the body parses
	// and the metric is present with a non-negative integer value (the
	// exact value depends on the cancel-replace race window, between 0
	// and the total schedule count).
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("scrape status: got %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "pyrycode_relay_grace_expiries_total ") {
		t.Errorf("scrape body missing grace counter; got:\n%s", rec.Body.String())
	}
}
