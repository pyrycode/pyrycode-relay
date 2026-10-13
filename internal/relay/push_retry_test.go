package relay

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func retryInfo(err error) *fcmRetryError {
	var r *fcmRetryError
	errors.As(fmt.Errorf("outer: %w", err), &r)
	return r
}

func TestFCMSender_RetryClassification(t *testing.T) {
	for _, phase := range []string{"oauth", "fcm"} {
		for _, status := range []int{400, 401, 403, 404, 408, 429, 500, 501, 502, 503, 504} {
			t.Run(fmt.Sprintf("%s/%d", phase, status), func(t *testing.T) {
				oauth, _ := fakeOAuth(t, 0)
				peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.Copy(io.Discard, r.Body)
					w.Header().Set("Retry-After", "15")
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(status)
					_, _ = io.WriteString(w, `{"error":"`+testFCMMarker+`"}`)
				}))
				defer peer.Close()
				tokenURL, fcmURL := oauth.URL, peer.URL
				sentinel := ErrFCMSend
				if phase == "oauth" {
					tokenURL = peer.URL
					sentinel = ErrFCMTokenFetch
				}
				s, err := newFCMSender(testServiceAccountJSON(t, tokenURL), fcmURL)
				if err != nil {
					t.Fatal(err)
				}
				before := time.Now()
				_, err = s.Send(context.Background(), testFCMDeviceToken)
				after := time.Now()
				want := status == 429 || status == 500 || status == 503 || phase == "oauth" && (status == 502 || status == 504)
				info := retryInfo(err)
				if !errors.Is(err, sentinel) || (info != nil && info.retry) != want {
					t.Fatalf("error=%v info=%+v want retry=%v", err, info, want)
				}
				if want {
					delay := 15 * time.Second
					if status == 429 {
						delay = 60 * time.Second
					}
					if info.notBefore.Before(before.Add(delay)) || info.notBefore.After(after.Add(delay)) {
						t.Fatalf("guidance=%v", info.notBefore)
					}
				}
				if strings.Contains(err.Error(), testFCMMarker) || strings.Contains(err.Error(), peer.URL) {
					t.Fatalf("leaked error: %v", err)
				}
			})
		}
	}
}

func TestFCMRetryAfter(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	for _, tc := range []struct {
		value string
		delay time.Duration
	}{
		{"", 0}, {"-1", 0}, {"junk", 0}, {"1.5", 0}, {"0", 0}, {"20", 20 * time.Second},
		{now.Add(22 * time.Second).Format(http.TimeFormat), 22 * time.Second},
		{now.Add(-time.Second).Format(http.TimeFormat), 0},
		{"9223372036854775807", time.Duration(1<<63 - 1)},
		{strings.Repeat("9", 100), time.Duration(1<<63 - 1)},
	} {
		t.Run(tc.value, func(t *testing.T) {
			info := retryInfo(fcmHTTPFailure(ErrFCMSend, 503, tc.value, now, false))
			if !info.notBefore.Equal(now.Add(tc.delay)) {
				t.Fatalf("not-before %v want %v", info.notBefore, now.Add(tc.delay))
			}
		})
	}
}

type retryNetError struct{ timeout, temporary bool }

func (e retryNetError) Error() string   { return "secret-url-token" }
func (e retryNetError) Timeout() bool   { return e.timeout }
func (e retryNetError) Temporary() bool { return e.temporary }

func TestFCMRetryTransport(t *testing.T) {
	for _, tc := range []struct {
		err   error
		retry bool
	}{
		{retryNetError{timeout: true}, true}, {retryNetError{temporary: true}, true},
		{retryNetError{}, false}, {errors.New("secret-url-token"), false},
		{context.DeadlineExceeded, true}, {context.Canceled, false},
	} {
		for _, phase := range []error{ErrFCMSend, ErrFCMTokenFetch} {
			err := fcmTransportFailure(phase, fmt.Errorf("credentials: %w", tc.err))
			info := retryInfo(err)
			if !errors.Is(err, phase) || (info != nil && info.retry) != tc.retry {
				t.Fatalf("error=%v retry=%+v", err, info)
			}
			if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "credentials") {
				t.Fatalf("leak: %v", err)
			}
			if errors.Is(tc.err, context.DeadlineExceeded) && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("lost deadline identity")
			}
		}
	}
}

// The scheduling clock starts in real time so the immutable context deadline
// stays live while simulated waits advance allowance and scheduling time.
func retryWaker(t *testing.T, s pushSender) (*PushWaker, *lockedBuffer, func(time.Duration)) {
	t.Helper()
	logger, logs := captureLogger()
	w := NewPushWaker(s, logger)
	now := time.Now()
	w.mu.Lock()
	w.now = func() time.Time { return now }
	w.jobNow = w.now
	w.global.refillLast = now
	w.mu.Unlock()
	w.jitter = func() time.Duration { return 0 }
	advance := func(d time.Duration) { w.mu.Lock(); now = now.Add(d); w.mu.Unlock() }
	w.wait = func(ctx context.Context, d time.Duration) bool { advance(d); return ctx.Err() == nil }
	t.Cleanup(w.Close)
	return w, logs, advance
}

func TestPushWaker_RetrySequence(t *testing.T) {
	for _, tc := range []struct {
		phase    string
		failures int
	}{{"fcm", 1}, {"fcm", 3}, {"oauth", 1}, {"oauth", 3}} {
		t.Run(fmt.Sprintf("%s/%d", tc.phase, tc.failures), func(t *testing.T) {
			var hits atomic.Int32
			entered, release := make(chan struct{}, 1), make(chan struct{})
			peer := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				n := int(hits.Add(1))
				if n == 1 {
					entered <- struct{}{}
					<-release
				}
				rw.Header().Set("Content-Type", "application/json")
				if n <= tc.failures {
					rw.WriteHeader(503)
					_, _ = io.WriteString(rw, testFCMMarker)
					return
				}
				if tc.phase == "oauth" {
					_, _ = io.WriteString(rw, `{"access_token":"synthetic","token_type":"Bearer","expires_in":3600}`)
				}
			}))
			defer peer.Close()
			oauth, _ := fakeOAuth(t, 0)
			fcm, _ := fakeFCM(t, 200)
			tokenURL, fcmURL := oauth.URL, peer.URL
			if tc.phase == "oauth" {
				tokenURL, fcmURL = peer.URL, fcm.URL
			}
			s, err := newFCMSender(testServiceAccountJSON(t, tokenURL), fcmURL)
			if err != nil {
				t.Fatal(err)
			}
			w, logs, advance := retryWaker(t, s)
			var outcomes [pushWakeOutcomeCount]atomic.Int32
			w.setOutcomeHook(func(o pushWakeOutcome) { outcomes[o].Add(1) })
			var waits []time.Duration
			w.jitter = func() time.Duration { return time.Duration(len(waits)+1) * 250 * time.Millisecond }
			duplicate := func() {
				for _, daemon := range []string{"origin", "other"} {
					if err := w.Request(daemon, wakeJSON("fcm", testWakeToken)); err != nil {
						t.Error(err)
					}
				}
			}
			w.wait = func(ctx context.Context, d time.Duration) bool {
				waits = append(waits, d)
				duplicate()
				advance(d)
				duplicate()
				return true
			}
			if err := w.Request("origin", wakeJSON("fcm", testWakeToken)); err != nil {
				t.Fatal(err)
			}
			awaitRefreshSignal(t, entered)
			duplicate()
			close(release)
			w.wg.Wait()
			want := tc.failures + 1
			if want > 3 {
				want = 3
			}
			if int(hits.Load()) != want || len(waits) != want-1 {
				t.Fatalf("calls=%d waits=%v", hits.Load(), waits)
			}
			for i, d := range waits {
				if d != time.Duration(i+1)*10*time.Second+time.Duration(i+1)*250*time.Millisecond {
					t.Fatalf("backoff=%v", waits)
				}
			}
			failure := pushWakeSendFailed
			if tc.phase == "oauth" {
				failure = pushWakeTokenFetchFailed
			}
			if int(outcomes[failure].Load()) != tc.failures || int(outcomes[pushWakeSent].Load()) != want-tc.failures || outcomes[pushWakeDropped].Load() != 0 {
				t.Fatal("wrong per-attempt outcomes")
			}
			text := logs.String()
			if strings.Count(text, "server_id=origin") != want || strings.Contains(text, "server_id=other") || strings.Contains(text, testFCMMarker) || strings.Contains(text, testWakeToken) {
				t.Fatalf("logs=%s", text)
			}
			w.mu.Lock()
			other := w.daemons[sha256.Sum256([]byte("other"))]
			jobs := len(w.jobs)
			w.mu.Unlock()
			if other != nil || jobs != 0 {
				t.Fatal("duplicate gained identity or completed retry job retained past window")
			}
		})
	}
}

func TestPushWaker_RetryRefusal(t *testing.T) {
	for _, kind := range []string{"daemon", "device", "global"} {
		t.Run(kind, func(t *testing.T) {
			s := newFakePushSender()
			s.err = fcmHTTPFailure(ErrFCMSend, 503, "", time.Now(), false)
			w, logs, _ := retryWaker(t, s)
			var drops atomic.Int32
			w.setOutcomeHook(func(o pushWakeOutcome) {
				if o == pushWakeDropped {
					drops.Add(1)
				}
			})
			w.wait = func(context.Context, time.Duration) bool {
				w.mu.Lock()
				defer w.mu.Unlock()
				switch kind {
				case "daemon":
					w.daemons[sha256.Sum256([]byte("origin"))].tokens = 0
				case "device":
					w.devices[sha256.Sum256([]byte(testWakeToken))].tokens = 0
				case "global":
					w.global.tokens = 0
				}
				return true
			}
			if err := w.Request("origin", wakeJSON("fcm", testWakeToken)); err != nil {
				t.Fatal(err)
			}
			w.wg.Wait()
			if len(s.sentTokens()) != 1 || drops.Load() != 1 || strings.Count(logs.String(), "push_wake_send_failed") != 1 {
				t.Fatal("refusal sent or added attempt outcome")
			}
			w.mu.Lock()
			defer w.mu.Unlock()
			daemon, device := w.daemons[sha256.Sum256([]byte("origin"))].tokens, w.devices[sha256.Sum256([]byte(testWakeToken))].tokens
			if kind != "daemon" && daemon != 5 || kind != "device" && device != 5 || kind != "global" && w.global.tokens != 31 {
				t.Fatal("partial charging")
			}
		})
	}
}

func TestPushWaker_RetryBounds(t *testing.T) {
	for _, kind := range []string{"exact", "beyond", "overflow", "429", "cancel", "expired", "unregistered"} {
		t.Run(kind, func(t *testing.T) {
			s := newFakePushSender()
			w, logs, advance := retryWaker(t, s)
			w.mu.Lock()
			now := w.jobNow()
			w.mu.Unlock()
			guidance := "45"
			status := 503
			switch kind {
			case "beyond":
				guidance = "46"
			case "overflow":
				guidance = strings.Repeat("9", 100)
			case "429":
				status = 429
				guidance = "0"
			default:
			}
			s.err = fcmHTTPFailure(ErrFCMSend, status, guidance, now, false)
			if kind == "cancel" || kind == "expired" {
				s.err = fcmHTTPFailure(ErrFCMSend, 503, "", now, false)
			}
			if kind == "unregistered" {
				s.err = fcmUnregisteredError{s.err}
			}
			var waits atomic.Int32
			w.wait = func(context.Context, time.Duration) bool {
				waits.Add(1)
				if kind == "cancel" {
					w.cancel()
				} else {
					advance(45 * time.Second)
				}
				return true
			}
			if err := w.Request("origin", wakeJSON("fcm", testWakeToken)); err != nil {
				t.Fatal(err)
			}
			w.wg.Wait()
			if len(s.sentTokens()) != 1 || strings.Count(logs.String(), "push_wake_send_failed") != 1 {
				t.Fatal("sent beyond bound or synthetic outcome")
			}
			wantWait := int32(0)
			if kind == "cancel" || kind == "expired" {
				wantWait = 1
			}
			if waits.Load() != wantWait {
				t.Fatalf("waits=%d", waits.Load())
			}
		})
	}
}

func TestPushWaker_RetryClose(t *testing.T) {
	for _, waiting := range []bool{false, true} {
		t.Run(fmt.Sprint(waiting), func(t *testing.T) {
			s := newFakePushSender()
			s.err = fcmHTTPFailure(ErrFCMSend, 503, "", time.Now(), false)
			if !waiting {
				s.block = make(chan struct{})
			}
			w, logs, _ := retryWaker(t, s)
			started := make(chan struct{}, 1)
			w.wait = func(ctx context.Context, d time.Duration) bool { started <- struct{}{}; return waitPushRetry(ctx, d) }
			if err := w.Request("origin", wakeJSON("fcm", testWakeToken)); err != nil {
				t.Fatal(err)
			}
			if waiting {
				awaitRefreshSignal(t, started)
			} else {
				s.waitStarted(t, 1)
			}
			done := make(chan struct{}, 2)
			for i := 0; i < 2; i++ {
				go func() { w.Close(); done <- struct{}{} }()
			}
			awaitRefreshSignal(t, done)
			awaitRefreshSignal(t, done)
			if len(s.sentTokens()) != 1 || len(w.jobs) != 0 || strings.Count(logs.String(), "push_wake_send_failed") != 1 {
				t.Fatal("shutdown failed to join job")
			}
			if err := w.Request("origin", wakeJSON("fcm", testWakeToken)); !errors.Is(err, ErrPushOff) {
				t.Fatal(err)
			}
		})
	}
}

func TestFCMSender_RetryPermanentReplies(t *testing.T) {
	for _, tc := range []struct {
		phase  string
		status int
		body   string
	}{
		{"oauth", 503, `{"error":"invalid_grant"}`},
		{"oauth", 200, `{"access_token":123}`},
		{"oauth", 200, `{}`},
		{"fcm", 503, `{"error":{"details":[{"@type":"` + fcmErrorType + `","errorCode":"UNREGISTERED"}]}}`},
		{"fcm", 200, `{"error":{"details":[{"@type":"` + fcmErrorType + `","errorCode":"UNREGISTERED"}]}}`},
	} {
		t.Run(tc.phase+tc.body, func(t *testing.T) {
			oauth, _ := fakeOAuth(t, 0)
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer peer.Close()
			tokenURL, fcmURL := oauth.URL, peer.URL
			if tc.phase == "oauth" {
				tokenURL = peer.URL
			}
			s, err := newFCMSender(testServiceAccountJSON(t, tokenURL), fcmURL)
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.Send(context.Background(), testWakeToken)
			info := retryInfo(err)
			if err == nil || info != nil && info.retry {
				t.Fatalf("permanent reply retryable: %v", err)
			}
			if tc.phase == "fcm" && !errors.Is(err, ErrFCMUnregistered) {
				t.Fatal("lost unregistered identity")
			}
		})
	}
}

func TestFCMSender_RetrySharedReceipt(t *testing.T) {
	headers, release := make(chan struct{}, 1), make(chan struct{})
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "15")
		w.WriteHeader(503)
		w.(http.Flusher).Flush()
		headers <- struct{}{}
		<-release
	}))
	defer peer.Close()
	s, err := newFCMSender(testServiceAccountJSON(t, peer.URL), peer.URL)
	if err != nil {
		t.Fatal(err)
	}
	first := asyncFCMSend(s, context.Background())
	awaitRefreshSignal(t, headers)
	second := asyncFCMSend(s, context.Background())
	awaitFCMWaiters(t, s, 2)
	time.Sleep(50 * time.Millisecond)
	completion := time.Now()
	close(release)
	a, b := retryInfo(awaitFCMSend(t, first)), retryInfo(awaitFCMSend(t, second))
	if a == nil || a != b || !a.retry || !a.notBefore.Before(completion.Add(15*time.Second-25*time.Millisecond)) {
		t.Fatal("shared receipt guidance reset at publication")
	}
}

func TestPushWaker_RetryAuthenticationExpiry(t *testing.T) {
	for _, survivor := range []bool{false, true} {
		t.Run(fmt.Sprint(survivor), func(t *testing.T) {
			h := newHeldOAuth(t)
			s, requests := newHeldSender(t, h)
			var other <-chan error
			if survivor {
				other = asyncFCMSend(s, context.Background())
				awaitRefreshSignal(t, h.started)
			}
			w, logs, _ := retryWaker(t, s)
			w.jobNow = func() time.Time { return time.Now().Add(-pushWakeJobTimeout + 200*time.Millisecond) }
			if err := w.Request("origin", wakeJSON("fcm", testWakeToken)); err != nil {
				t.Fatal(err)
			}
			r := awaitFCMWaiters(t, s, map[bool]int{false: 1, true: 2}[survivor])
			done := make(chan struct{}, 1)
			go func() { w.wg.Wait(); done <- struct{}{} }()
			awaitRefreshSignal(t, done)
			if len(requests()) != 0 || strings.Count(logs.String(), "push_wake_send_failed") != 1 {
				t.Fatal("expired authentication sent or synthesized outcome")
			}
			if survivor {
				awaitFCMWaiters(t, s, 1)
				select {
				case <-r.done:
					t.Fatal("expired job cancelled surviving refresh")
				default:
				}
				h.unblock()
				if err := awaitFCMSend(t, other); err != nil {
					t.Fatal(err)
				}
			} else {
				awaitRefreshSignal(t, h.cancelled)
				awaitRefreshSignal(t, r.done)
			}
		})
	}
}

type retrySendFunc func(context.Context, string) (string, error)

func (f retrySendFunc) Send(ctx context.Context, token string) (string, error) { return f(ctx, token) }

func TestPushWaker_RetryGuidanceAndCallTimeout(t *testing.T) {
	var calls int
	var deadline time.Time
	s := retrySendFunc(func(ctx context.Context, _ string) (string, error) {
		calls++
		d, ok := ctx.Deadline()
		if !ok {
			t.Error("job lacks deadline")
		}
		if calls == 1 {
			deadline = d
			return "", fcmTransportFailure(ErrFCMTokenFetch, context.DeadlineExceeded)
		}
		if !d.Equal(deadline) {
			t.Error("deadline reset")
		}
		if calls == 2 {
			return "", fcmHTTPFailure(ErrFCMSend, 503, "40", deadline.Add(-45*time.Second), false)
		}
		return "", nil
	})
	w, _, _ := retryWaker(t, s)
	if err := w.Request("origin", wakeJSON("fcm", testWakeToken)); err != nil {
		t.Fatal(err)
	}
	w.wg.Wait()
	if calls != 3 {
		t.Fatalf("own timeout/guidance retry calls=%d", calls)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.jobNow().Equal(deadline.Add(-5 * time.Second)) {
		t.Fatal("guidance did not override ordinary backoff")
	}
}

type retryTransport func(*http.Request) (*http.Response, error)

func (f retryTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestFCMSender_RetryTransportBoundary(t *testing.T) {
	for _, phase := range []string{"oauth", "fcm"} {
		for _, temporary := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%v", phase, temporary), func(t *testing.T) {
				s, err := newFCMSender(testServiceAccountJSON(t, "https://synthetic.invalid/secret"), "https://synthetic.invalid/secret")
				if err != nil {
					t.Fatal(err)
				}
				s.client.Transport = retryTransport(func(r *http.Request) (*http.Response, error) {
					if phase == "fcm" && !strings.Contains(r.URL.Path, "messages:send") {
						return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"access_token":"synthetic","token_type":"Bearer"}`))}, nil
					}
					return nil, fmt.Errorf("secret: %w", retryNetError{temporary: temporary})
				})
				_, err = s.Send(context.Background(), testWakeToken)
				info := retryInfo(err)
				if info == nil || info.retry != temporary || strings.Contains(err.Error(), "secret") {
					t.Fatalf("classification lost or leaked: %v", err)
				}
			})
		}
	}
}

func TestFCMSender_RetryResponseReadFailure(t *testing.T) {
	for _, phase := range []string{"oauth", "fcm"} {
		for _, tc := range []struct {
			name, guidance, body string
			status               int
			cause                error
			retry, permanent     bool
			minimum              time.Duration
		}{
			{"quota", "", "", 429, retryNetError{timeout: true}, true, false, time.Minute},
			{"unavailable", "60", "", 503, retryNetError{timeout: true}, true, false, time.Minute},
			{"unauthorized", "60", "", 401, retryNetError{timeout: true}, false, false, time.Minute},
			{"success-body-timeout", "60", "", 200, retryNetError{timeout: true}, true, false, time.Minute},
			{"overflow", strings.Repeat("9", 100), "", 503, retryNetError{timeout: true}, true, false, pushWakeJobTimeout},
			{"deadline-identity", "60", "", 503, context.DeadlineExceeded, true, false, time.Minute},
			{"cancelled", "60", "", 503, context.Canceled, false, false, time.Minute},
			{"unclassified-read", "60", "", 503, errors.New("secret-read-error"), true, false, time.Minute},
			{"permanent-503", "60", "permanent", 503, retryNetError{timeout: true}, false, true, time.Minute},
			{"permanent-200", "60", "permanent", 200, retryNetError{timeout: true}, false, true, time.Minute},
			{"permanent-429", "60", "permanent", 429, retryNetError{timeout: true}, false, true, time.Minute},
		} {
			t.Run(phase+"/"+tc.name, func(t *testing.T) {
				s, err := newFCMSender(testServiceAccountJSON(t, "https://synthetic.invalid/secret"), "https://synthetic.invalid/secret")
				if err != nil {
					t.Fatal(err)
				}
				body := tc.body
				if tc.permanent {
					body = `{"error":"invalid_grant","error_description":"secret-reply"}`
					if phase == "fcm" {
						body = fcmErrorBody("NOT_FOUND", fcmErrorType, "UNREGISTERED")
					}
				}
				s.client.Transport = retryTransport(func(r *http.Request) (*http.Response, error) {
					if phase == "fcm" && !strings.Contains(r.URL.Path, "messages:send") {
						return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"access_token":"synthetic","token_type":"Bearer"}`))}, nil
					}
					reader := io.MultiReader(strings.NewReader(body), retryReadError{tc.cause})
					return &http.Response{StatusCode: tc.status, Header: http.Header{"Retry-After": []string{tc.guidance}}, Body: io.NopCloser(reader)}, nil
				})
				before := time.Now()
				_, err = s.Send(context.Background(), testWakeToken)
				after := time.Now()
				info := retryInfo(err)
				wantPhase := ErrFCMSend
				if phase == "oauth" {
					wantPhase = ErrFCMTokenFetch
				}
				if !errors.Is(err, wantPhase) || info == nil || info.retry != tc.retry {
					t.Fatalf("error=%v classification=%+v want retry=%v", err, info, tc.retry)
				}
				if info.notBefore.Before(before.Add(tc.minimum)) || tc.name != "overflow" && info.notBefore.After(after.Add(tc.minimum)) {
					t.Fatalf("lost response guidance: %v", info.notBefore)
				}
				if tc.permanent && phase == "fcm" && !errors.Is(err, ErrFCMUnregistered) {
					t.Fatal("lost UNREGISTERED identity")
				}
				for _, sentinel := range []error{context.Canceled, context.DeadlineExceeded} {
					if errors.Is(err, sentinel) != errors.Is(tc.cause, sentinel) {
						t.Fatalf("lost context identity: %v", err)
					}
				}
				if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "synthetic.invalid") {
					t.Fatalf("leaked response/read error: %v", err)
				}
			})
		}
	}
}

type retryReadError struct{ err error }

func (r retryReadError) Read([]byte) (int, error) { return 0, r.err }

func TestFCMSender_RetrySharedReceiptReadFailure(t *testing.T) {
	started, release := make(chan struct{}, 1), make(chan struct{})
	s, err := newFCMSender(testServiceAccountJSON(t, "https://synthetic.invalid/token"), "https://synthetic.invalid")
	if err != nil {
		t.Fatal(err)
	}
	s.client.Transport = retryTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 503, Header: http.Header{"Retry-After": []string{"15"}}, Body: retryHeldReadFailure{started, release}}, nil
	})
	before := time.Now()
	first := asyncFCMSend(s, context.Background())
	awaitRefreshSignal(t, started)
	received := time.Now()
	second := asyncFCMSend(s, context.Background())
	awaitFCMWaiters(t, s, 2)
	time.Sleep(50 * time.Millisecond)
	close(release)
	a, b := retryInfo(awaitFCMSend(t, first)), retryInfo(awaitFCMSend(t, second))
	if a == nil || a != b || !a.retry || a.notBefore.Before(before.Add(15*time.Second)) || a.notBefore.After(received.Add(15*time.Second)) {
		t.Fatalf("shared response receipt lost on read failure: %+v / %+v", a, b)
	}
}

type retryHeldReadFailure struct{ started, release chan struct{} }

func (b retryHeldReadFailure) Read([]byte) (int, error) {
	b.started <- struct{}{}
	<-b.release
	return 0, retryNetError{timeout: true}
}
func (retryHeldReadFailure) Close() error { return nil }

func TestFCMSender_RetryRealBodyTimeout(t *testing.T) {
	for _, phase := range []string{"oauth", "fcm"} {
		t.Run(phase, func(t *testing.T) {
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Retry-After", "60")
				w.WriteHeader(429)
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			}))
			defer peer.Close()
			oauth, _ := fakeOAuth(t, 0)
			tokenURL := oauth.URL
			if phase == "oauth" {
				tokenURL = peer.URL
			}
			s, err := newFCMSender(testServiceAccountJSON(t, tokenURL), peer.URL)
			if err != nil {
				t.Fatal(err)
			}
			s.client.Timeout = 100 * time.Millisecond
			before := time.Now()
			_, err = s.Send(context.Background(), testWakeToken)
			info := retryInfo(err)
			if info == nil || !info.retry || info.notBefore.Before(before.Add(time.Minute)) || !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("real body timeout lost quota floor or deadline identity: %v / %+v", err, info)
			}
		})
	}
}
