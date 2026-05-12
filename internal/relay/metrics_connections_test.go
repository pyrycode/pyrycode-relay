package relay

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// assertGauge scrapes h and asserts that body contains the literal line
// `<name> <want>` in the Prometheus text format. Substring-match (not
// equality on the full body) because the body also contains the
// promhttp_metric_handler_* self-instrumentation counters from
// HandlerOpts.Registry=reg.
func assertGauge(t *testing.T, h http.Handler, name string, want int) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("scrape status: got %d, want 200", rec.Code)
	}
	wantLine := fmt.Sprintf("%s %d", name, want)
	if !strings.Contains(rec.Body.String(), wantLine) {
		t.Errorf("scrape body missing %q; got:\n%s", wantLine, rec.Body.String())
	}
}

func TestConnectionsMetrics_ReflectLiveCounts(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	mreg := NewMetricsRegistry()
	NewConnectionsMetrics(mreg, r)
	h := NewMetricsHandler(mreg)

	// Empty registry: both gauges should be 0.
	assertGauge(t, h, "pyrycode_relay_connected_binaries", 0)
	assertGauge(t, h, "pyrycode_relay_connected_phones", 0)

	if err := r.ClaimServer("s1", &fakeConn{id: "b-1"}); err != nil {
		t.Fatalf("ClaimServer: %v", err)
	}
	if err := r.RegisterPhone("s1", &fakeConn{id: "p-1"}); err != nil {
		t.Fatalf("RegisterPhone: %v", err)
	}
	if err := r.RegisterPhone("s1", &fakeConn{id: "p-2"}); err != nil {
		t.Fatalf("RegisterPhone: %v", err)
	}

	assertGauge(t, h, "pyrycode_relay_connected_binaries", 1)
	assertGauge(t, h, "pyrycode_relay_connected_phones", 2)

	// Unregister one phone and release the server; gauges follow.
	r.UnregisterPhone("s1", "p-1")
	if !r.ReleaseServer("s1") {
		t.Fatal("ReleaseServer: got false, want true")
	}
	assertGauge(t, h, "pyrycode_relay_connected_binaries", 0)
	// Phone p-2 outlives the release per ReleaseServer's contract (orphan
	// phones survive until explicit unregister or grace expiry).
	assertGauge(t, h, "pyrycode_relay_connected_phones", 1)
}

func TestConnectionsMetrics_GraceStaleFireDoesNotMoveGauge(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	mreg := NewMetricsRegistry()
	NewConnectionsMetrics(mreg, r)
	h := NewMetricsHandler(mreg)

	if err := r.ClaimServer("s1", &fakeConn{id: "b-1"}); err != nil {
		t.Fatalf("ClaimServer b-1: %v", err)
	}
	if err := r.RegisterPhone("s1", &fakeConn{id: "p-1"}); err != nil {
		t.Fatalf("RegisterPhone p-1: %v", err)
	}

	// Arm grace; immediately cancel-and-replace via reclaim.
	r.ScheduleReleaseServer("s1", 5*time.Millisecond)
	if err := r.ClaimServer("s1", &fakeConn{id: "b-2"}); err != nil {
		t.Fatalf("ClaimServer b-2 (reclaim): %v", err)
	}

	// Wait past the original grace window. If the stale fire were to
	// move the gauge, this is when it would.
	time.Sleep(30 * time.Millisecond)

	assertGauge(t, h, "pyrycode_relay_connected_binaries", 1)
	assertGauge(t, h, "pyrycode_relay_connected_phones", 1)
}

func TestConnectionsMetrics_RaceFreedom(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	mreg := NewMetricsRegistry()
	NewConnectionsMetrics(mreg, r)
	h := NewMetricsHandler(mreg)

	const goroutines = 16
	const opsPer = 200

	stopScrape := make(chan struct{})
	var scrapeCount atomic.Int64
	scrapeDone := make(chan struct{})

	// Scraper: periodic gauge reads via the public handler — the same
	// path Prometheus would hit.
	go func() {
		defer close(scrapeDone)
		for {
			select {
			case <-stopScrape:
				return
			default:
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
				if rec.Code != http.StatusOK {
					t.Errorf("scrape status: got %d, want 200", rec.Code)
					return
				}
				scrapeCount.Add(1)
				// Tight loop — no sleep — to maximise interleaving under
				// -race. The race detector reports on memory accesses, not
				// on iteration count.
			}
		}
	}()

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		g := g
		go func() {
			defer wg.Done()
			sid := fmt.Sprintf("s-%d", g%4)
			for i := 0; i < opsPer; i++ {
				_ = r.ClaimServer(sid, &fakeConn{id: fmt.Sprintf("b-%d-%d", g, i)})
				_ = r.RegisterPhone(sid, &fakeConn{id: fmt.Sprintf("p-%d-%d", g, i)})
				r.ScheduleReleaseServer(sid, time.Millisecond)
				_ = r.ClaimServer(sid, &fakeConn{id: fmt.Sprintf("b2-%d-%d", g, i)})
				r.UnregisterPhone(sid, fmt.Sprintf("p-%d-%d", g, i))
			}
		}()
	}
	wg.Wait()
	close(stopScrape)
	<-scrapeDone

	// Sleep past max grace duration so any in-flight timers fire and exit.
	time.Sleep(50 * time.Millisecond)

	// Final state need not be empty (some claims won the cancellation
	// race); we only assert the scraper ran AND a final scrape returns 200
	// with parseable gauge lines. The race detector's verdict is the real
	// assertion — this test exists to feed -race interleavings, not to
	// check exact values.
	if scrapeCount.Load() == 0 {
		t.Error("scraper never ran")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("final scrape status: got %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "pyrycode_relay_connected_binaries ") {
		t.Errorf("final scrape body missing binaries gauge; got:\n%s", body)
	}
	if !strings.Contains(body, "pyrycode_relay_connected_phones ") {
		t.Errorf("final scrape body missing phones gauge; got:\n%s", body)
	}
}
