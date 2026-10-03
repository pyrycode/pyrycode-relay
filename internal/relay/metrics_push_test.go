package relay

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// allPushWakeOutcomes pins the closed outcome label set.
var allPushWakeOutcomes = []string{"sent", "unregistered", "send_failed", "token_fetch_failed", "dropped"}

// assertPushWakeOutcomes asserts every outcome cell against want; missing
// keys are asserted as 0. It also checks no test server id or token reached
// the scrape.
func assertPushWakeOutcomes(t *testing.T, h http.Handler, want map[string]int) {
	t.Helper()
	for _, outcome := range allPushWakeOutcomes {
		assertCounter(t, h, "pyrycode_relay_push_wakes_total", `outcome="`+outcome+`"`, want[outcome])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for _, secret := range []string{testWakeToken, testFCMDeviceToken, "push-sid-"} {
		if strings.Contains(rec.Body.String(), secret) {
			t.Errorf("scrape contains %q", secret)
		}
	}
}

func newPushMetricsBundle(w *PushWaker) http.Handler {
	mreg := NewMetricsRegistry()
	NewPushMetrics(mreg, w)
	return NewMetricsHandler(mreg)
}

func TestPushMetrics_ExposesAllOutcomesAtZero(t *testing.T) {
	t.Parallel()
	w := NewPushWaker(newFakePushSender(), discardLogger())
	defer w.Close()
	assertPushWakeOutcomes(t, newPushMetricsBundle(w), nil)
}

// TestPushMetrics_FCMOutcomes drives a real FCMSender against in-test OAuth
// and FCM servers, so classification runs on the errors Send really returns.
func TestPushMetrics_FCMOutcomes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		oauthFail int
		status    int
		body      string
		want      string
	}{
		{"2xx with id", 0, http.StatusOK, `{"name":"` + testFCMMessageID + `"}`, "sent"},
		{"2xx without id", 0, http.StatusOK, `{}`, "sent"},
		{"unregistered", 0, http.StatusNotFound, fcmErrorBody("NOT_FOUND", fcmErrorType, "UNREGISTERED"), "unregistered"},
		{"other FCM error", 0, http.StatusBadRequest, `{"error":{"status":"INVALID_ARGUMENT"}}`, "send_failed"},
		{"token fetch failure", http.StatusUnauthorized, http.StatusOK, `{}`, "token_fetch_failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			oauth, _ := fakeOAuth(t, tc.oauthFail)
			fcm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			t.Cleanup(fcm.Close)
			s, err := newFCMSender(testServiceAccountJSON(t, oauth.URL), fcm.URL)
			if err != nil {
				t.Fatalf("newFCMSender: %v", err)
			}

			logger, logs := captureLogger()
			w := NewPushWaker(s, logger)
			defer w.Close()
			h := newPushMetricsBundle(w)
			if err := w.Request("push-sid-1", wakeJSON("fcm", testFCMDeviceToken)); err != nil {
				t.Fatalf("Request: %v", err)
			}
			// Close would cancel the send, so wait for its outcome line; the
			// outcome is recorded before the line is logged.
			if !pollUntil(time.Now().Add(5*time.Second), func() bool { return len(outcomeLines(logs.String())) == 1 }) {
				t.Fatalf("send did not finish; logs:\n%s", logs.String())
			}
			assertPushWakeOutcomes(t, h, map[string]int{tc.want: 1})
		})
	}
}

func TestPushMetrics_CancelledByCloseIsSendFailed(t *testing.T) {
	t.Parallel()
	sender := newFakePushSender()
	sender.block = make(chan struct{})
	w := NewPushWaker(sender, discardLogger())
	h := newPushMetricsBundle(w)

	if err := w.Request("push-sid-1", wakeJSON("fcm", testWakeToken)); err != nil {
		t.Fatalf("Request: %v", err)
	}
	sender.waitStarted(t, 1)
	w.Close()
	assertPushWakeOutcomes(t, h, map[string]int{"send_failed": 1})
}

func TestPushMetrics_RateLimitIsDropped(t *testing.T) {
	t.Parallel()
	w := newPushWaker(newFakePushSender(), time.Hour, 1, 16, discardLogger())
	h := newPushMetricsBundle(w)

	if err := w.Request("push-sid-1", wakeJSON("fcm", testWakeToken)); err != nil {
		t.Fatalf("first wake: %v", err)
	}
	if err := w.Request("push-sid-1", wakeJSON("fcm", testWakeToken)); err == nil {
		t.Fatal("second wake admitted, want rate-limited")
	}
	w.Close()
	assertPushWakeOutcomes(t, h, map[string]int{"sent": 1, "dropped": 1})
}

func TestPushMetrics_InFlightCapIsDropped(t *testing.T) {
	t.Parallel()
	sender := newFakePushSender()
	sender.block = make(chan struct{})
	w := newPushWaker(sender, time.Millisecond, 100, 1, discardLogger())
	h := newPushMetricsBundle(w)

	if err := w.Request("push-sid-1", wakeJSON("fcm", testWakeToken)); err != nil {
		t.Fatalf("first wake: %v", err)
	}
	sender.waitStarted(t, 1)
	if err := w.Request("push-sid-2", wakeJSON("fcm", testWakeToken)); err == nil {
		t.Fatal("wake over cap admitted, want refused")
	}
	assertPushWakeOutcomes(t, h, map[string]int{"dropped": 1})
	w.Close()
	assertPushWakeOutcomes(t, h, map[string]int{"dropped": 1, "send_failed": 1})
}

func TestPushMetrics_MalformedAndPushOffCountNothing(t *testing.T) {
	t.Parallel()
	sender := newFakePushSender()
	w := NewPushWaker(sender, discardLogger())
	h := newPushMetricsBundle(w)

	for _, raw := range []json.RawMessage{json.RawMessage(`5`), wakeJSON("apns", testWakeToken), wakeJSON("fcm", "")} {
		if err := w.Request("push-sid-1", raw); err == nil {
			t.Fatalf("malformed wake %s admitted", raw)
		}
	}
	w.Close()
	if err := w.Request("push-sid-1", wakeJSON("fcm", testWakeToken)); err == nil {
		t.Fatal("wake after Close admitted")
	}
	assertPushWakeOutcomes(t, h, nil)
}
