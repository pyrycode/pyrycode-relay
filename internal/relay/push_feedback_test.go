package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

type feedbackSender struct {
	calls   atomic.Int32
	started chan struct{}
	reply   chan error
}

func (s *feedbackSender) Send(ctx context.Context, _ string) (string, error) {
	s.calls.Add(1)
	if s.started != nil {
		s.started <- struct{}{}
	}
	select {
	case err := <-s.reply:
		return "", err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

type feedbackPeer struct {
	ctx      context.Context
	cancel   context.CancelFunc
	attempts atomic.Int32
	replies  chan string
	hold     chan struct{}
	fail     bool
}

func newFeedbackPeer(t *testing.T) *feedbackPeer {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &feedbackPeer{ctx: ctx, cancel: cancel, replies: make(chan string, 256)}
}
func (p *feedbackPeer) pushResultContext() context.Context { return p.ctx }
func (p *feedbackPeer) sendPushResult(ctx context.Context, msg []byte) error {
	p.attempts.Add(1)
	if p.hold != nil {
		select {
		case <-p.hold:
		case <-ctx.Done():
			return errors.New("SYNTHETIC-TRANSPORT-SECRET")
		}
	}
	if p.fail {
		return errors.New("SYNTHETIC-TRANSPORT-SECRET")
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	p.replies <- string(msg)
	return nil
}

func feedbackFixture(t *testing.T) (*PushWaker, *feedbackSender) {
	t.Helper()
	s := &feedbackSender{reply: make(chan error, 64), started: make(chan struct{}, 64)}
	w := NewPushWaker(s, discardLogger())
	t.Cleanup(w.Close)
	return w, s
}
func feedbackOrigin(t *testing.T, p *feedbackPeer) *pushWakeOrigin {
	t.Helper()
	return &pushWakeOrigin{ctx: p.ctx, writer: p}
}
func requestFeedback(t *testing.T, w *PushWaker, o *pushWakeOrigin, token, id string) {
	t.Helper()
	raw := json.RawMessage(fmt.Sprintf(`{"platform":"fcm","token":%q,"request_id":%q}`, token, id))
	if err := w.request("daemon-"+token, raw, o); err != nil {
		t.Fatal(err)
	}
}
func feedbackEventually(t *testing.T, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatal("feedback state did not settle")
		}
		time.Sleep(time.Millisecond)
	}
}
func feedbackCount(w *PushWaker) int { w.mu.Lock(); defer w.mu.Unlock(); return w.feedbackCount }
func feedbackReply(t *testing.T, p *feedbackPeer, id string) {
	t.Helper()
	select {
	case got := <-p.replies:
		want := fmt.Sprintf(`{"push_wake_result":{"request_id":%q,"code":"invalid_token"}}`, id)
		if got != want {
			t.Fatalf("result = %s, want %s", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("missing result")
	}
}

func feedbackReplies(t *testing.T, p *feedbackPeer, ids ...string) {
	t.Helper()
	want := make(map[string]bool)
	for _, id := range ids {
		want[fmt.Sprintf(`{"push_wake_result":{"request_id":%q,"code":"invalid_token"}}`, id)] = true
	}
	for range ids {
		select {
		case got := <-p.replies:
			if !want[got] {
				t.Fatalf("unexpected/duplicate result: %s", got)
			}
			delete(want, got)
		case <-time.After(2 * time.Second):
			t.Fatal("missing result")
		}
	}
}
func feedbackComplete(t *testing.T, w *PushWaker, s *feedbackSender, err error) {
	t.Helper()
	s.reply <- err
	feedbackEventually(t, func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		for _, j := range w.jobs {
			if j.active {
				return false
			}
		}
		return true
	})
}

func TestPushFeedback_Syntax(t *testing.T) {
	for _, tc := range []struct{ name, field, id string }{
		{"one", `,"request_id":"A"`, "A"},
		{"64", `,"request_id":"` + strings.Repeat("a", 64) + `"`, strings.Repeat("a", 64)},
		{"alphabet", `,"request_id":"AZaz09_-"`, "AZaz09_-"},
		{"omitted", "", ""}, {"empty", `,"request_id":""`, ""},
		{"65", `,"request_id":"` + strings.Repeat("a", 65) + `"`, ""},
		{"unicode", `,"request_id":"ä"`, ""}, {"space", `,"request_id":"a b"`, ""},
		{"null", `,"request_id":null`, ""}, {"number", `,"request_id":123`, ""},
		{"object", `,"request_id":{}`, ""}, {"array", `,"request_id":[]`, ""}, {"bool", `,"request_id":true`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, s := feedbackFixture(t)
			p := newFeedbackPeer(t)
			o := feedbackOrigin(t, p)
			raw := []byte(`{"push_wake":{"platform":"fcm","token":"SYNTHETIC-DEVICE-SECRET","unknown":true` + tc.field + `}}`)
			env, err := Unmarshal(raw)
			if err != nil {
				t.Fatal(err)
			}
			if err := w.request("daemon", env.PushWake, o); err != nil {
				t.Fatal(err)
			}
			feedbackComplete(t, w, s, ErrFCMUnregistered)
			if tc.id != "" {
				feedbackReply(t, p, tc.id)
			} else if feedbackCount(w) != 0 || p.attempts.Load() != 0 {
				t.Fatal("invalid correlation affected feedback")
			}
			if s.calls.Load() != 1 {
				t.Fatal("ordinary admission changed")
			}
		})
	}
	if _, err := parsePushWake(json.RawMessage(`{"platform":"fcm","token":"x","request_id":`)); !errors.Is(err, ErrMalformedPushWake) {
		t.Fatal("malformed JSON accepted")
	}
}

func TestPushFeedback_Coalescing(t *testing.T) {
	w, s := feedbackFixture(t)
	a, b := newFeedbackPeer(t), newFeedbackPeer(t)
	oa, ob := feedbackOrigin(t, a), feedbackOrigin(t, b)
	requestFeedback(t, w, oa, "SYNTHETIC-DEVICE-SECRET", "same")
	requestFeedback(t, w, oa, "SYNTHETIC-DEVICE-SECRET", "same")
	if err := w.request("different-daemon", json.RawMessage(`{"platform":"fcm","token":"SYNTHETIC-DEVICE-SECRET","request_id":"same"}`), ob); err != nil {
		t.Fatal(err)
	}
	if feedbackCount(w) != 2 {
		t.Fatal("duplicates or connections conflated")
	}
	feedbackComplete(t, w, s, fmt.Errorf("wrapped: %w", ErrFCMUnregistered))
	feedbackReply(t, a, "same")
	feedbackReply(t, b, "same")
	if s.calls.Load() != 1 || a.attempts.Load() != 1 || b.attempts.Load() != 1 {
		t.Fatal("wrong cardinality")
	}
}

func TestPushFeedback_CacheAndOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name          string
		err           error
		invalid, fail bool
	}{
		{"invalid", ErrFCMUnregistered, true, false}, {"failed_write", ErrFCMUnregistered, true, true},
		{"success", nil, false, false}, {"other_4xx", ErrFCMSend, false, false}, {"auth", ErrFCMTokenFetch, false, false},
		{"transient_exhausted", errors.New("sanitised transient"), false, false}, {"cancel", context.Canceled, false, false},
		{"unregistered_cancelled", errors.Join(ErrFCMUnregistered, context.Canceled), false, false},
		{"unregistered_timed_out", errors.Join(ErrFCMUnregistered, context.DeadlineExceeded), false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, s := feedbackFixture(t)
			p := newFeedbackPeer(t)
			p.fail = tc.fail
			o := feedbackOrigin(t, p)
			requestFeedback(t, w, o, "token", "first")
			feedbackComplete(t, w, s, tc.err)
			requestFeedback(t, w, o, "token", "first")
			requestFeedback(t, w, o, "token", "new")
			if tc.invalid {
				feedbackEventually(t, func() bool { return p.attempts.Load() == 2 })
				if !tc.fail {
					feedbackReplies(t, p, "first", "new")
				}
			}
			// Evict without cancellation and join record cleanup, so a broken
			// non-invalid classification cannot be hidden by Close winning scheduling.
			w.mu.Lock()
			w.expireJobsLocked(time.Now().Add(11 * time.Second))
			w.mu.Unlock()
			feedbackEventually(t, func() bool { return feedbackCount(w) == 0 })
			w.Close()
			if s.calls.Load() != 1 {
				t.Fatal("cache invoked provider")
			}
			want := int32(0)
			if tc.invalid {
				want = 2
			}
			if p.attempts.Load() != want {
				t.Fatalf("attempts %d", p.attempts.Load())
			}
		})
	}
	// Refusal creates neither subscriber nor invalid feedback.
	w, _ := feedbackFixture(t)
	p := newFeedbackPeer(t)
	o := feedbackOrigin(t, p)
	w.mu.Lock()
	w.global.tokens = 0
	w.mu.Unlock()
	if err := w.request("daemon", json.RawMessage(`{"platform":"fcm","token":"x","request_id":"refused"}`), o); !errors.Is(err, ErrPushWakeRateLimited) {
		t.Fatal(err)
	}
	if feedbackCount(w) != 0 {
		t.Fatal("refusal retained binding")
	}
}

func TestPushFeedback_OriginCancellation(t *testing.T) {
	w, s := feedbackFixture(t)
	a, b := newFeedbackPeer(t), newFeedbackPeer(t)
	oa, ob := feedbackOrigin(t, a), feedbackOrigin(t, b)
	requestFeedback(t, w, oa, "token", "old")
	requestFeedback(t, w, ob, "token", "live")
	a.cancel()
	feedbackEventually(t, func() bool { return feedbackCount(w) == 1 })
	replacement := newFeedbackPeer(t)
	feedbackComplete(t, w, s, ErrFCMUnregistered)
	feedbackReply(t, b, "live")
	if a.attempts.Load() != 0 || replacement.attempts.Load() != 0 {
		t.Fatal("stale result")
	}
	requestFeedback(t, w, feedbackOrigin(t, replacement), "token", "fresh")
	feedbackReply(t, replacement, "fresh")
}

func TestPushFeedback_Eviction(t *testing.T) {
	for _, late := range []bool{false, true} {
		t.Run(fmt.Sprint(late), func(t *testing.T) {
			w, s := feedbackFixture(t)
			p := newFeedbackPeer(t)
			p.hold = make(chan struct{})
			o := feedbackOrigin(t, p)
			base := time.Now()
			now := base
			w.mu.Lock()
			w.now = func() time.Time { return now }
			w.mu.Unlock()
			requestFeedback(t, w, o, "token", "first")
			if late {
				w.mu.Lock()
				now = base.Add(10 * time.Second)
				w.mu.Unlock()
				requestFeedback(t, w, o, "token", "late")
				if s.calls.Load() > 1 {
					t.Fatal("active duplicate restarted job")
				}
			}
			feedbackComplete(t, w, s, ErrFCMUnregistered)
			feedbackEventually(t, func() bool { return p.attempts.Load() >= 1 })
			if !late {
				w.mu.Lock()
				now = base.Add(10*time.Second - time.Nanosecond)
				w.mu.Unlock()
				requestFeedback(t, w, o, "token", "cached")
				if s.calls.Load() != 1 {
					t.Fatal("pre-boundary cache invoked provider")
				}
			}
			w.mu.Lock()
			now = base.Add(10 * time.Second)
			w.expireJobsLocked(now)
			jobs := len(w.jobs)
			w.mu.Unlock()
			if jobs != 0 || feedbackCount(w) == 0 {
				t.Fatal("queued state lost on eviction")
			}
			close(p.hold)
			if late {
				feedbackReplies(t, p, "first", "late")
			} else {
				feedbackReplies(t, p, "first", "cached")
			}
			feedbackEventually(t, func() bool { return feedbackCount(w) == 0 })
			requestFeedback(t, w, o, "token", "first")
			feedbackComplete(t, w, s, ErrFCMUnregistered)
			feedbackReply(t, p, "first")
			if s.calls.Load() != 2 {
				t.Fatal("fresh job did not start after exact window")
			}
		})
	}
}

func TestPushFeedback_Expiry(t *testing.T) {
	w, _ := feedbackFixture(t)
	p := newFeedbackPeer(t)
	o := feedbackOrigin(t, p)
	base := time.Now()
	now := base
	w.mu.Lock()
	w.jobNow = func() time.Time { return now }
	w.mu.Unlock()
	requestFeedback(t, w, o, "token", "id")
	w.mu.Lock()
	var r *pushWakeFeedback
	for _, j := range w.jobs {
		for _, v := range j.feedback {
			r = v
		}
	}
	expiry := r.expires
	now = base.Add(59 * time.Second)
	w.mu.Unlock()
	requestFeedback(t, w, o, "token", "id")
	if !expiry.Equal(base.Add(60*time.Second)) || !r.expires.Equal(expiry) {
		t.Fatal("renewed expiry")
	}
	w.mu.Lock()
	now = expiry
	w.expireFeedbackLocked(now)
	w.mu.Unlock()
	feedbackEventually(t, func() bool { return feedbackCount(w) == 0 })
	if p.attempts.Load() != 0 {
		t.Fatal("expired feedback wrote")
	}
}

func TestPushFeedback_ExpiryBoundary(t *testing.T) {
	for _, age := range []time.Duration{60*time.Second - time.Nanosecond, 60 * time.Second} {
		t.Run(age.String(), func(t *testing.T) {
			w, s := feedbackFixture(t)
			p := newFeedbackPeer(t)
			o := feedbackOrigin(t, p)
			base := time.Now()
			now := base
			w.mu.Lock()
			w.jobNow = func() time.Time { return now }
			w.mu.Unlock()
			requestFeedback(t, w, o, "token", "boundary")
			w.mu.Lock()
			now = base.Add(age)
			w.mu.Unlock()
			feedbackComplete(t, w, s, ErrFCMUnregistered)
			if age < 60*time.Second {
				feedbackReply(t, p, "boundary")
			}
			w.mu.Lock()
			w.expireJobsLocked(time.Now().Add(11 * time.Second))
			w.mu.Unlock()
			feedbackEventually(t, func() bool { return feedbackCount(w) == 0 })
			want := int32(0)
			if age < 60*time.Second {
				want = 1
			}
			if p.attempts.Load() != want {
				t.Fatal("expiry boundary ignored")
			}
		})
	}
}

func TestPushFeedback_RetryTerminal(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		t.Run(fmt.Sprint(invalid), func(t *testing.T) {
			w, s := feedbackFixture(t)
			p := newFeedbackPeer(t)
			o := feedbackOrigin(t, p)
			// Existing retry tests prove the timed schedule; this barrier-free wait
			// isolates feedback selection after the same three charged attempts.
			w.wait = func(ctx context.Context, _ time.Duration) bool { return ctx.Err() == nil }
			for i := 0; i < 2; i++ {
				s.reply <- &fcmRetryError{err: ErrFCMSend, retry: true}
			}
			terminal := ErrFCMSend
			if invalid {
				terminal = ErrFCMUnregistered
			}
			s.reply <- terminal
			requestFeedback(t, w, o, "token", "id")
			feedbackEventually(t, func() bool {
				w.mu.Lock()
				defer w.mu.Unlock()
				for _, j := range w.jobs {
					if j.active {
						return false
					}
				}
				return true
			})
			if invalid {
				feedbackReply(t, p, "id")
			}
			w.mu.Lock()
			w.expireJobsLocked(time.Now().Add(11 * time.Second))
			w.mu.Unlock()
			feedbackEventually(t, func() bool { return feedbackCount(w) == 0 })
			want := int32(0)
			if invalid {
				want = 1
			}
			if s.calls.Load() != 3 || p.attempts.Load() != want {
				t.Fatal("terminal feedback/attempt budget changed")
			}
		})
	}
}

func TestPushFeedback_Caps(t *testing.T) {
	w, s := feedbackFixture(t)
	p := newFeedbackPeer(t)
	o := feedbackOrigin(t, p)
	for job := 0; job < 2; job++ {
		for i := 0; i < 65; i++ {
			requestFeedback(t, w, o, fmt.Sprint(job), fmt.Sprintf("j%d_%d", job, i))
		}
		if feedbackCount(w) != (job+1)*64 {
			t.Fatal("job cap")
		}
	}
	requestFeedback(t, w, o, "third", "overflow")
	if feedbackCount(w) != 128 {
		t.Fatal("origin cap")
	}
	// Fill the remaining 29 retained jobs with independent origins.
	for job := 3; job < 32; job++ {
		peer := newFeedbackPeer(t)
		origin := feedbackOrigin(t, peer)
		for i := 0; i < 64; i++ {
			requestFeedback(t, w, origin, fmt.Sprint(job), fmt.Sprintf("id%d", i))
		}
	}
	// The third job was admitted without feedback. Fill its 64 records too.
	last := newFeedbackPeer(t)
	lastOrigin := feedbackOrigin(t, last)
	for i := 0; i < 64; i++ {
		requestFeedback(t, w, lastOrigin, "third", fmt.Sprintf("id%d", i))
	}
	if feedbackCount(w) != 2048 {
		t.Fatalf("global count=%d", feedbackCount(w))
	}
	// Hold every delivery so eviction cannot create an accounting gap.
	w.mu.Lock()
	hold := make(chan struct{})
	for origin := range w.feedbackOrigins {
		origin.writer.(*feedbackPeer).hold = hold
	}
	w.mu.Unlock()
	for i := 0; i < 32; i++ {
		s.reply <- ErrFCMUnregistered
	}
	feedbackEventually(t, func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		for _, j := range w.jobs {
			if j.active {
				return false
			}
		}
		return true
	})
	feedbackEventually(t, func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		total := int32(0)
		for origin := range w.feedbackOrigins {
			total += origin.writer.(*feedbackPeer).attempts.Load()
		}
		return total == 2048
	})
	w.mu.Lock()
	w.expireJobsLocked(time.Now().Add(11 * time.Second))
	w.global.tokens = 32
	w.mu.Unlock()
	newPeer := newFeedbackPeer(t)
	requestFeedback(t, w, feedbackOrigin(t, newPeer), "new-job", "global-overflow")
	if feedbackCount(w) != 2048 {
		t.Fatal("queued global accounting gap")
	}
	w.Close()
	if feedbackCount(w) != 0 {
		t.Fatal("close retained records")
	}
}

func TestPushFeedback_Close(t *testing.T) {
	for _, queued := range []bool{false, true} {
		t.Run(fmt.Sprint(queued), func(t *testing.T) {
			w, s := feedbackFixture(t)
			p := newFeedbackPeer(t)
			p.hold = make(chan struct{})
			o := feedbackOrigin(t, p)
			requestFeedback(t, w, o, "token", "id")
			if queued {
				feedbackComplete(t, w, s, ErrFCMUnregistered)
				feedbackEventually(t, func() bool { return p.attempts.Load() == 1 })
			}
			var wg sync.WaitGroup
			for i := 0; i < 4; i++ {
				wg.Add(1)
				go func() { defer wg.Done(); w.Close() }()
			}
			wg.Wait()
			if feedbackCount(w) != 0 {
				t.Fatal("close leaked")
			}
			before := p.attempts.Load()
			if err := w.request("daemon", json.RawMessage(`{"platform":"fcm","token":"later","request_id":"id"}`), o); !errors.Is(err, ErrPushOff) {
				t.Fatal(err)
			}
			if p.attempts.Load() != before {
				t.Fatal("write after close")
			}
		})
	}
}

func TestPushFeedback_WSContention(t *testing.T) {
	for _, end := range []string{"deadline", "shutdown", "disconnect"} {
		t.Run(end, func(t *testing.T) {
			w, s := feedbackFixture(t)
			reg, url, cleanup := startServer(t, time.Millisecond)
			defer cleanup()
			reg.SetPushWaker(w)
			c, _, err := dialWith(t, url, validHeaders("origin"))
			if err != nil {
				t.Fatal(err)
			}
			defer c.CloseNow()
			var ws *WSConn
			feedbackEventually(t, func() bool {
				conn, ok := reg.BinaryFor("origin")
				if ok {
					ws = conn.(*WSConn)
				}
				return ok
			})
			phone := newFakePhone("phone")
			if err := reg.RegisterPhone("origin", phone); err != nil {
				t.Fatal(err)
			}
			ws.writeMu.Lock()
			var once sync.Once
			unlock := func() { once.Do(ws.writeMu.Unlock) }
			defer unlock()
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			if err := c.Write(ctx, websocket.MessageText, []byte(`{"push_wake":{"platform":"fcm","token":"SYNTHETIC-DEVICE-SECRET","request_id":"wake"}}`)); err != nil {
				t.Fatal(err)
			}
			select {
			case <-s.started:
			case <-ctx.Done():
				t.Fatal("provider not invoked")
			}
			feedbackComplete(t, w, s, ErrFCMUnregistered)
			// A phone-addressed envelope with push fields remains ordinary opaque
			// routing even while result delivery is waiting for the writer.
			if err := c.Write(ctx, websocket.MessageText, []byte(`{"conn_id":"phone","frame":{"opaque":"SYNTHETIC-INNER-SECRET"},"push_wake":{"platform":"fcm","token":"ignored","request_id":"ignored"}}`)); err != nil {
				t.Fatal(err)
			}
			feedbackEventually(t, func() bool { phone.mu.Lock(); defer phone.mu.Unlock(); return len(phone.sent) == 1 })
			phone.mu.Lock()
			got := string(phone.sent[0])
			phone.mu.Unlock()
			if got != `{"opaque":"SYNTHETIC-INNER-SECRET"}` {
				t.Fatal("phone frame changed")
			}
			w.mu.Lock()
			w.expireJobsLocked(time.Now().Add(11 * time.Second))
			w.mu.Unlock()
			start := time.Now()
			switch end {
			case "shutdown":
				w.Close()
			case "disconnect":
				c.CloseNow()
			}
			limit := time.Second
			if end == "deadline" {
				limit = 6 * time.Second
			}
			for feedbackCount(w) != 0 {
				if time.Since(start) > limit {
					t.Fatal("result delivery exceeded cancellation/budget")
				}
				time.Sleep(time.Millisecond)
			}
			if s.calls.Load() != 1 {
				t.Fatal("phone envelope caused provider send")
			}
			unlock()
		})
	}
}

func TestPushFeedback_WireAndHygiene(t *testing.T) {
	w, s := feedbackFixture(t)
	logger, logs := captureLogger()
	w.logger = logger
	metrics := newPushMetricsBundle(w)
	reg := NewRegistry()
	reg.SetPushWaker(w)
	srv := httptest.NewServer(ServerHandler(reg, logger, time.Millisecond, 256*1024, nil))
	defer srv.Close()
	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	a, _, err := dialWith(t, url, validHeaders("A"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.CloseNow()
	b, _, err := dialWith(t, url, validHeaders("B"))
	if err != nil {
		t.Fatal(err)
	}
	defer b.CloseNow()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	raw := []byte(`{"push_wake":{"platform":"fcm","token":"SYNTHETIC-DEVICE-SECRET","request_id":"same","unknown":"SYNTHETIC-UNKNOWN-SECRET"}}`)
	for _, c := range []*websocket.Conn{a, b, a} {
		if err := c.Write(ctx, websocket.MessageText, raw); err != nil {
			t.Fatal(err)
		}
	}
	feedbackEventually(t, func() bool { return feedbackCount(w) == 2 })
	feedbackComplete(t, w, s, ErrFCMUnregistered)
	for _, c := range []*websocket.Conn{a, b} {
		_, data, err := c.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != `{"push_wake_result":{"request_id":"same","code":"invalid_token"}}` {
			t.Fatalf("noncanonical reply: %s", data)
		}
	}
	w.Close()
	assertPushWakeOutcomes(t, metrics, map[string]int{"unregistered": 1})
	for _, secret := range []string{"SYNTHETIC-DEVICE-SECRET", "SYNTHETIC-UNKNOWN-SECRET", "SYNTHETIC-TRANSPORT-SECRET"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatal("secret in diagnostics")
		}
	}
	if s.calls.Load() != 1 {
		t.Fatal("cross-daemon coalescing changed")
	}
}

func TestPushFeedback_WriteErrorSanitised(t *testing.T) {
	ws := startScriptedPeer(t, func(ctx context.Context, c *websocket.Conn) { <-ctx.Done() })
	ws.writeMu.Lock()
	defer ws.writeMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	err := ws.sendPushResult(ctx, []byte("SYNTHETIC-TRANSPORT-SECRET"))
	if !errors.Is(err, ErrPushResultFailed) || err.Error() != ErrPushResultFailed.Error() {
		t.Fatalf("unsanitised failure: %v", err)
	}
}

func TestPushFeedback_RegistryReplacement(t *testing.T) {
	for _, queued := range []bool{false, true} {
		t.Run(fmt.Sprint(queued), func(t *testing.T) {
			w, s := feedbackFixture(t)
			reg, url, cleanup := startServer(t, time.Millisecond)
			defer cleanup()
			reg.SetPushWaker(w)
			c, _, err := dialWith(t, url, validHeaders("reused"))
			if err != nil {
				t.Fatal(err)
			}
			defer c.CloseNow()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			var ws *WSConn
			feedbackEventually(t, func() bool {
				old, ok := reg.BinaryFor("reused")
				if ok {
					ws = old.(*WSConn)
				}
				return ok
			})
			ws.writeMu.Lock()
			var once sync.Once
			unlock := func() { once.Do(ws.writeMu.Unlock) }
			defer unlock()
			if err := c.Write(ctx, websocket.MessageText, []byte(`{"push_wake":{"platform":"fcm","token":"token","request_id":"old"}}`)); err != nil {
				t.Fatal(err)
			}
			feedbackEventually(t, func() bool { return feedbackCount(w) == 1 })
			if queued {
				feedbackComplete(t, w, s, ErrFCMUnregistered)
			}
			replacement := newFakePhone("replacement")
			// Takeover deliberately does not close the old transport. Feedback must
			// stop at replacement itself, before the handler's asynchronous close.
			if err := reg.TakeoverServer("reused", ws, replacement); err != nil {
				t.Fatal(err)
			}
			feedbackEventually(t, func() bool { return feedbackCount(w) == 0 })
			if !queued {
				feedbackComplete(t, w, s, ErrFCMUnregistered)
			}
			if ws.closeCtx.Err() != nil {
				t.Fatal("feedback cancellation closed ordinary transport")
			}
			unlock()
			if err := ws.Send([]byte(`{"ordinary":"still-open"}`)); err != nil {
				t.Fatal(err)
			}
			_, data, err := c.Read(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != `{"ordinary":"still-open"}` {
				t.Fatal("stale result on replaced connection")
			}
			replacement.mu.Lock()
			sent := len(replacement.sent)
			replacement.mu.Unlock()
			if sent != 0 {
				t.Fatal("result redirected to replacement")
			}
		})
	}
}
