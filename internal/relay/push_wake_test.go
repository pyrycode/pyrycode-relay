package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// testWakeToken is the device token used by the wake tests. Every log
// assertion checks it never appears.
const testWakeToken = "wake-token-SECRET-4f1a"

// fakePushSender records each Send. When block is non-nil, Send waits until
// block is closed or ctx is cancelled. started receives once per Send entry.
type fakePushSender struct {
	mu      sync.Mutex
	tokens  []string
	err     error
	block   chan struct{}
	started chan struct{}
}

func newFakePushSender() *fakePushSender {
	return &fakePushSender{started: make(chan struct{}, 64)}
}

func (s *fakePushSender) Send(ctx context.Context, deviceToken string) error {
	s.mu.Lock()
	s.tokens = append(s.tokens, deviceToken)
	block, err := s.block, s.err
	s.mu.Unlock()
	s.started <- struct{}{}
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}

func (s *fakePushSender) sentTokens() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.tokens...)
}

func (s *fakePushSender) waitStarted(t *testing.T, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case <-s.started:
		case <-time.After(2 * time.Second):
			t.Fatalf("waitStarted: only %d of %d sends started", i, n)
		}
	}
}

// lockedBuffer is a goroutine-safe log sink: send goroutines log while the
// test reads.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func captureLogger() (*slog.Logger, *lockedBuffer) {
	buf := &lockedBuffer{}
	return slog.New(slog.NewTextHandler(buf, nil)), buf
}

func wakeJSON(platform, token string) json.RawMessage {
	out, _ := json.Marshal(map[string]string{"platform": platform, "token": token})
	return out
}

func TestPushWaker_NilIsPushOff(t *testing.T) {
	t.Parallel()
	var w *PushWaker
	if err := w.Request("s1", wakeJSON("fcm", testWakeToken)); !errors.Is(err, ErrPushOff) {
		t.Fatalf("nil waker Request = %v, want ErrPushOff", err)
	}
}

func TestPushWaker_SendsOnceToToken(t *testing.T) {
	t.Parallel()
	sender := newFakePushSender()
	w := NewPushWaker(sender, discardLogger())
	defer w.Close()

	if err := w.Request("s1", wakeJSON("fcm", testWakeToken)); err != nil {
		t.Fatalf("Request: %v", err)
	}
	sender.waitStarted(t, 1)
	if got := sender.sentTokens(); len(got) != 1 || got[0] != testWakeToken {
		t.Fatalf("sent tokens = %q, want exactly [%q]", got, testWakeToken)
	}
}

func TestPushWaker_RejectsInvalidWake(t *testing.T) {
	t.Parallel()
	sender := newFakePushSender()
	w := NewPushWaker(sender, discardLogger())
	defer w.Close()

	for _, tc := range []struct {
		name string
		raw  json.RawMessage
		want error
	}{
		{"apns", wakeJSON("apns", testWakeToken), ErrUnsupportedPushPlatform},
		{"unknown platform", wakeJSON("webpush", testWakeToken), ErrUnsupportedPushPlatform},
		{"missing platform", json.RawMessage(`{"token":"` + testWakeToken + `"}`), ErrUnsupportedPushPlatform},
		{"empty token", wakeJSON("fcm", ""), ErrEmptyPushToken},
		{"number", json.RawMessage(`5`), ErrMalformedPushWake},
		{"string", json.RawMessage(`"` + testWakeToken + `"`), ErrMalformedPushWake},
		{"token not string", json.RawMessage(`{"platform":"fcm","token":7}`), ErrMalformedPushWake},
	} {
		err := w.Request("s1", tc.raw)
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: Request = %v, want %v", tc.name, err, tc.want)
			continue
		}
		if strings.Contains(err.Error(), testWakeToken) {
			t.Errorf("%s: error %q contains the token", tc.name, err)
		}
	}
	if got := sender.sentTokens(); len(got) != 0 {
		t.Fatalf("invalid wakes reached the sender: %q", got)
	}
}

func TestPushWaker_RateLimitedPerServerID(t *testing.T) {
	t.Parallel()
	sender := newFakePushSender()
	w := newPushWaker(sender, time.Hour, 2, 16, discardLogger())
	defer w.Close()

	for i := 0; i < 2; i++ {
		if err := w.Request("s1", wakeJSON("fcm", testWakeToken)); err != nil {
			t.Fatalf("wake %d: %v", i, err)
		}
	}
	if err := w.Request("s1", wakeJSON("fcm", testWakeToken)); !errors.Is(err, ErrPushWakeRateLimited) {
		t.Fatalf("over-burst wake = %v, want ErrPushWakeRateLimited", err)
	}
	if err := w.Request("s2", wakeJSON("fcm", testWakeToken)); err != nil {
		t.Fatalf("other server-id wake = %v, want nil", err)
	}
	sender.waitStarted(t, 3)
}

func TestPushWaker_InFlightCap(t *testing.T) {
	t.Parallel()
	sender := newFakePushSender()
	release := make(chan struct{})
	sender.block = release
	w := newPushWaker(sender, time.Millisecond, 100, 2, discardLogger())
	defer w.Close()

	// Distinct server-ids so only the in-flight cap can refuse.
	for _, id := range []string{"s1", "s2"} {
		if err := w.Request(id, wakeJSON("fcm", testWakeToken)); err != nil {
			t.Fatalf("wake %s: %v", id, err)
		}
	}
	sender.waitStarted(t, 2)
	if err := w.Request("s3", wakeJSON("fcm", testWakeToken)); !errors.Is(err, ErrPushWakeInFlightCap) {
		t.Fatalf("wake over cap = %v, want ErrPushWakeInFlightCap", err)
	}

	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for {
		err := w.Request("s4", wakeJSON("fcm", testWakeToken))
		if err == nil {
			break
		}
		if !errors.Is(err, ErrPushWakeInFlightCap) || time.Now().After(deadline) {
			t.Fatalf("wake after release = %v, want nil", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestPushWaker_CloseCancelsBlockedSend(t *testing.T) {
	t.Parallel()
	sender := newFakePushSender()
	sender.block = make(chan struct{})
	w := NewPushWaker(sender, discardLogger())

	if err := w.Request("s1", wakeJSON("fcm", testWakeToken)); err != nil {
		t.Fatalf("Request: %v", err)
	}
	sender.waitStarted(t, 1)

	closed := make(chan struct{})
	go func() {
		w.Close()
		w.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return while a send was blocked")
	}
	if err := w.Request("s1", wakeJSON("fcm", testWakeToken)); !errors.Is(err, ErrPushOff) {
		t.Fatalf("Request after Close = %v, want ErrPushOff", err)
	}
}

func TestPushWaker_SendFailureLoggedWithoutToken(t *testing.T) {
	t.Parallel()
	sender := newFakePushSender()
	sender.err = errors.New("relay: fcm send failed: status 404")
	logger, logs := captureLogger()
	w := NewPushWaker(sender, logger)

	if err := w.Request("s1", wakeJSON("fcm", testWakeToken)); err != nil {
		t.Fatalf("Request: %v", err)
	}
	w.Close() // waits for the send goroutine, so its log line is written

	out := logs.String()
	if !strings.Contains(out, "push_wake_send_failed") || !strings.Contains(out, "status 404") {
		t.Fatalf("send failure not logged by status: %q", out)
	}
	if strings.Contains(out, testWakeToken) {
		t.Fatalf("log contains the token: %q", out)
	}
}

func TestNewPushWakerFromEnv(t *testing.T) {
	t.Parallel()
	lookup := func(env map[string]string) func(string) (string, bool) {
		return func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	}

	w, err := NewPushWakerFromEnv(lookup(nil), discardLogger())
	if err != nil || w != nil {
		t.Fatalf("unset: got (%v, %v), want (nil, nil)", w, err)
	}

	creds := string(testServiceAccountJSON(t, "https://oauth2.example.invalid/token"))
	w, err = NewPushWakerFromEnv(lookup(map[string]string{envFCMCredentials: creds}), discardLogger())
	if err != nil || w == nil {
		t.Fatalf("set: got (%v, %v), want a waker", w, err)
	}
	w.Close()

	_, err = NewPushWakerFromEnv(lookup(map[string]string{envFCMCredentials: "{}"}), discardLogger())
	if !errors.Is(err, ErrFCMCredentials) {
		t.Fatalf("bad key: err = %v, want ErrFCMCredentials", err)
	}
}
