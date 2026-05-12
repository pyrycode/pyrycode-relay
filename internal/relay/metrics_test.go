package relay

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
)

// textFormatPrefix is the substring of the Prometheus text exposition
// Content-Type that the AC pins on every /metrics response: "text/plain;
// version=0.0.4; charset=utf-8". Newer prometheus/common versions append
// "; escaping=<scheme>" (default "underscores") to advertise the name-escape
// policy used when serialising metric/label names — that suffix is an
// additive, backward-compatible extension, so we substring-match the AC
// literal instead of equality-matching. The substring is the canary the
// spec calls out: a release that renames the version or drops the charset
// would still surface here as a test failure rather than a tautological
// pass.
const textFormatPrefix = "text/plain; version=0.0.4; charset=utf-8"

func TestMetricsHandler_EmptyRegistry_ResponseShape(t *testing.T) {
	t.Parallel()

	reg := NewMetricsRegistry()
	h := NewMetricsHandler(reg)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, textFormatPrefix) {
		t.Errorf("Content-Type: got %q, want prefix %q", got, textFormatPrefix)
	}

	// "Parseable body" — round-trip the response through expfmt's text
	// decoder. Decoding to io.EOF without an error is the contract; the
	// number of MetricFamilies is *not* zero because NewMetricsHandler
	// passes HandlerOpts.Registry=reg, which registers promhttp's own
	// self-instrumentation collectors (promhttp_metric_handler_*) on the
	// private registry — that's the load-bearing design choice in
	// metrics.go (ADR-0008 § Scope of use: nothing on DefaultRegisterer,
	// not even promhttp's own bookkeeping).
	dec := expfmt.NewDecoder(rec.Body, expfmt.NewFormat(expfmt.TypeTextPlain))
	for {
		var mf dto.MetricFamily
		if err := dec.Decode(&mf); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("decode: %v (body=%q)", err, rec.Body.String())
		}
	}
}

func TestMetricsHandler_RegisterAndScrape_RoundTrip(t *testing.T) {
	t.Parallel()

	reg := NewMetricsRegistry()
	c := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "relay_test_counter_total",
		Help: "Sole purpose: prove the registry → handler seam works.",
	})
	reg.MustRegister(c)
	c.Add(2)

	h := NewMetricsHandler(reg)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "relay_test_counter_total 2") {
		t.Errorf("body missing counter+value; got:\n%s", body)
	}
}

func TestMetricsRegistry_NoGlobalRegistrarLeak(t *testing.T) {
	// Structural defence for ADR-0008's "never prometheus.DefaultRegisterer"
	// rule. Snapshot the default gatherer's MetricFamily count before and
	// after constructing the relay's registry and handler — the delta MUST
	// be zero. Absolute count is whatever the Go runtime or client_golang
	// itself has pre-registered; this test asserts the delta only, so it is
	// stable across version bumps.
	t.Parallel()

	before := defaultGathererSize(t)
	_ = NewMetricsHandler(NewMetricsRegistry())
	after := defaultGathererSize(t)

	if before != after {
		t.Fatalf("default registerer changed: before=%d, after=%d "+
			"(metrics.go must never register on prometheus.DefaultRegisterer; "+
			"see ADR-0008 § Scope of use)", before, after)
	}
}

func defaultGathererSize(t *testing.T) int {
	t.Helper()
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("DefaultGatherer.Gather: %v", err)
	}
	return len(mfs)
}
