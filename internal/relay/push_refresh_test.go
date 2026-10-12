package relay

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// heldOAuth lets tests release exactly one shared attempt after admission.
// Reading the POST body first lets the server observe client cancellation.
type heldOAuth struct {
	server    *httptest.Server
	hits      atomic.Int32
	status    atomic.Int32
	started   chan struct{}
	cancelled chan struct{}
	release   chan struct{}
	once      sync.Once
}

func newHeldOAuth(t *testing.T) *heldOAuth {
	t.Helper()
	h := &heldOAuth{started: make(chan struct{}, 16), cancelled: make(chan struct{}, 16), release: make(chan struct{})}
	h.status.Store(http.StatusOK)
	h.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		h.hits.Add(1)
		h.started <- struct{}{}
		select {
		case <-r.Context().Done():
			h.cancelled <- struct{}{}
			return
		case <-h.release:
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(int(h.status.Load()))
		if h.status.Load() != http.StatusOK {
			_, _ = io.WriteString(w, `{"error":"invalid_grant","error_description":"`+testFCMMarker+` `+testFCMAccessToken+`"}`)
			return
		}
		_, _ = io.WriteString(w, `{"access_token":"`+testFCMAccessToken+`","token_type":"Bearer","expires_in":3600}`)
	}))
	t.Cleanup(func() { h.unblock(); h.server.Close() })
	return h
}

func (h *heldOAuth) unblock() { h.once.Do(func() { close(h.release) }) }

func awaitRefreshSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for endpoint/cleanup signal")
	}
}

func newHeldSender(t *testing.T, h *heldOAuth) (*FCMSender, func() []fcmRequest) {
	t.Helper()
	fcm, requests := fakeFCM(t, http.StatusOK)
	s, err := newFCMSender(testServiceAccountJSON(t, h.server.URL), fcm.URL)
	if err != nil {
		t.Fatal(err)
	}
	return s, requests
}

func asyncFCMSend(s *FCMSender, ctx context.Context) <-chan error {
	done := make(chan error, 1)
	go func() { _, err := s.Send(ctx, testFCMDeviceToken); done <- err }()
	return done
}

func awaitFCMSend(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("Send did not return promptly")
		return nil
	}
}

func TestFCMSender_AlreadyCancelledNoRequests(t *testing.T) {
	t.Parallel()
	for _, warm := range []bool{false, true} {
		t.Run(map[bool]string{false: "cold", true: "warm"}[warm], func(t *testing.T) {
			oauth, hits := fakeOAuth(t, 0)
			fcm, requests := fakeFCM(t, http.StatusOK)
			s, err := newFCMSender(testServiceAccountJSON(t, oauth.URL), fcm.URL)
			if err != nil {
				t.Fatal(err)
			}
			if warm {
				if _, err := s.Send(context.Background(), testFCMDeviceToken); err != nil {
					t.Fatal(err)
				}
			}
			beforeAuth, beforeFCM := hits.Load(), len(requests())
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			_, err = s.Send(ctx, testFCMDeviceToken)
			if !errors.Is(err, ErrFCMSend) || !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled Send = %v", err)
			}
			if hits.Load() != beforeAuth || len(requests()) != beforeFCM {
				t.Fatal("already-cancelled Send started an outbound request")
			}
		})
	}
}

func TestFCMSender_WholeOperationDeadline(t *testing.T) {
	t.Parallel()
	for _, callerBound := range []bool{false, true} {
		t.Run(map[bool]string{false: "sender budget", true: "earlier caller deadline"}[callerBound], func(t *testing.T) {
			h := newHeldOAuth(t)
			fcmStarted := make(chan struct{}, 1)
			fcm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				fcmStarted <- struct{}{}
				<-r.Context().Done()
			}))
			t.Cleanup(fcm.Close)
			s, err := newFCMSender(testServiceAccountJSON(t, h.server.URL), fcm.URL)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			budget, delay := fcmSendTimeout, 2*time.Second
			if callerBound {
				var cancel context.CancelFunc
				budget, delay = time.Second, 200*time.Millisecond
				ctx, cancel = context.WithTimeout(ctx, budget)
				defer cancel()
			}
			start := time.Now()
			done := asyncFCMSend(s, ctx)
			awaitRefreshSignal(t, h.started)
			time.Sleep(delay)
			h.unblock()
			awaitRefreshSignal(t, fcmStarted)
			select {
			case err := <-done:
				if !errors.Is(err, ErrFCMSend) || !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("Send = %v, want FCM deadline failure", err)
				}
			case <-time.After(budget):
				t.Fatal("Send exceeded operation budget")
			}
			if elapsed := time.Since(start); elapsed > budget+500*time.Millisecond {
				t.Fatalf("whole send took %v, budget %v", elapsed, budget)
			}
		})
	}
}

// Inspect under the same lock as admission so releasing OAuth never races a
// send that has started but has not joined the shared attempt yet.
func awaitFCMWaiters(t *testing.T, s *FCMSender, n int) *fcmTokenRefresh {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.tokens.mu.Lock()
		r := s.tokens.refresh
		ready := r != nil && r.waiters == n
		s.tokens.mu.Unlock()
		if ready {
			return r
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("did not admit %d refresh waiters", n)
	return nil
}

func TestFCMSender_SharedRefresh(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusOK, http.StatusBadRequest} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			h := newHeldOAuth(t)
			h.status.Store(int32(status))
			s, requests := newHeldSender(t, h)
			var sends []<-chan error
			for i := 0; i < 6; i++ {
				sends = append(sends, asyncFCMSend(s, context.Background()))
			}
			r := awaitFCMWaiters(t, s, 6)
			awaitRefreshSignal(t, h.started)
			h.unblock()
			var sharedError error
			for _, send := range sends {
				err := awaitFCMSend(t, send)
				if status == http.StatusOK {
					if err != nil {
						t.Fatal(err)
					}
				} else {
					if !errors.Is(err, ErrFCMTokenFetch) || errors.Is(err, ErrFCMSend) {
						t.Fatalf("Send = %v, want token failure", err)
					}
					assertNoSecrets(t, err.Error())
					if sharedError != nil && err != sharedError {
						t.Fatal("waiters did not receive the same failure")
					}
					sharedError = err
				}
			}
			awaitRefreshSignal(t, r.done)
			wantFCM := 6
			if status != http.StatusOK {
				wantFCM = 0
			}
			if h.hits.Load() != 1 || len(requests()) != wantFCM {
				t.Fatalf("auth=%d, FCM=%d, want 1/%d", h.hits.Load(), len(requests()), wantFCM)
			}
			// A later send retries a failed attempt, or reuses a successful token.
			h.status.Store(http.StatusOK)
			if _, err := s.Send(context.Background(), testFCMDeviceToken); err != nil {
				t.Fatal(err)
			}
			wantAuth := int32(1)
			if status != http.StatusOK {
				wantAuth = 2
			}
			if h.hits.Load() != wantAuth || len(requests()) != wantFCM+1 {
				t.Fatal("incorrect retry/cache behaviour")
			}
		})
	}
}

func TestFCMSender_IndependentCancellation(t *testing.T) {
	t.Parallel()
	for _, starter := range []bool{true, false} {
		for _, expiry := range []bool{false, true} {
			name := map[bool]string{true: "starter", false: "other waiter"}[starter] + "/" + map[bool]string{false: "cancel", true: "expire"}[expiry]
			t.Run(name, func(t *testing.T) {
				h := newHeldOAuth(t)
				s, requests := newHeldSender(t, h)
				ctx, cancel := context.WithCancel(context.Background())
				if expiry {
					cancel()
					ctx, cancel = context.WithTimeout(context.Background(), time.Second)
				}
				defer cancel()
				firstCtx, secondCtx := context.Background(), context.Background()
				if starter {
					firstCtx = ctx
				} else {
					secondCtx = ctx
				}
				first := asyncFCMSend(s, firstCtx)
				awaitFCMWaiters(t, s, 1)
				awaitRefreshSignal(t, h.started)
				second := asyncFCMSend(s, secondCtx)
				r := awaitFCMWaiters(t, s, 2)
				cancelledSend, survivor := second, first
				if starter {
					cancelledSend, survivor = first, second
				}
				if !expiry {
					cancel()
				}
				err := awaitFCMSend(t, cancelledSend)
				want := context.Canceled
				if expiry {
					want = context.DeadlineExceeded
				}
				if !errors.Is(err, ErrFCMTokenFetch) || !errors.Is(err, want) {
					t.Fatalf("cancelled waiter = %v", err)
				}
				awaitFCMWaiters(t, s, 1)
				select {
				case <-h.cancelled:
					t.Fatal("one waiter cancelled shared authentication")
				default:
				}
				select {
				case <-r.done:
					t.Fatal("shared refresh stopped before survivor completed")
				default:
				}
				h.unblock()
				if err := awaitFCMSend(t, survivor); err != nil {
					t.Fatal(err)
				}
				if h.hits.Load() != 1 || len(requests()) != 1 {
					t.Fatal("incorrect request counts after independent cancellation")
				}
			})
		}
	}
}

func TestFCMSender_AllWaitersCancel(t *testing.T) {
	t.Parallel()
	for _, expiry := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel", true: "expire"}[expiry], func(t *testing.T) {
			h := newHeldOAuth(t)
			s, requests := newHeldSender(t, h)
			ctx, cancel := context.WithCancel(context.Background())
			if expiry {
				cancel()
				ctx, cancel = context.WithTimeout(context.Background(), time.Second)
			}
			defer cancel()
			var sends []<-chan error
			for i := 0; i < 6; i++ {
				sends = append(sends, asyncFCMSend(s, ctx))
			}
			r := awaitFCMWaiters(t, s, 6)
			awaitRefreshSignal(t, h.started)
			if !expiry {
				cancel()
			}
			for _, done := range sends {
				if err := awaitFCMSend(t, done); !errors.Is(err, ErrFCMTokenFetch) {
					t.Fatalf("Send = %v", err)
				}
			}
			// Cleanup is joined before the last send returns, not merely scheduled.
			select {
			case <-r.done:
			default:
				t.Fatal("sends left refresh work running")
			}
			awaitRefreshSignal(t, h.cancelled)
			if h.hits.Load() != 1 || len(requests()) != 0 {
				t.Fatal("abandonment started further requests")
			}
			h.unblock()
			if _, err := s.Send(context.Background(), testFCMDeviceToken); err != nil {
				t.Fatal(err)
			}
			if h.hits.Load() != 2 || len(requests()) != 1 {
				t.Fatal("sender did not recover after abandonment")
			}
		})
	}
}

func TestFCMSender_RefreshesExpiredToken(t *testing.T) {
	t.Parallel()
	oauth, hits := fakeOAuth(t, 0)
	fcm, requests := fakeFCM(t, http.StatusOK)
	s, err := newFCMSender(testServiceAccountJSON(t, oauth.URL), fcm.URL)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := s.Send(context.Background(), testFCMDeviceToken); err != nil {
			t.Fatal(err)
		}
		if hits.Load() != int32(i+1) {
			t.Fatal("expired token was reused")
		}
		if _, err := s.Send(context.Background(), testFCMDeviceToken); err != nil {
			t.Fatal(err)
		}
		if hits.Load() != int32(i+1) {
			t.Fatal("valid token was not cached")
		}
		s.tokens.mu.Lock()
		s.tokens.cached.Expiry = time.Now().Add(-time.Minute)
		s.tokens.mu.Unlock()
	}
	if len(requests()) != 6 {
		t.Fatal("incorrect FCM count")
	}
}
