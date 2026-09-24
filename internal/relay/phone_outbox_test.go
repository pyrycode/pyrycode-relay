package relay

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

var errStallClosed = errors.New("stallPhone: closed")

// stallPhone is a Conn whose Send blocks until release is closed (the phone
// starts reading again) or the conn is closed (the write is aborted, as
// WSConn.Close does via closeCtx). events records sends and the first close
// in the order they took effect, so tests can assert frame-then-close order.
type stallPhone struct {
	id      string
	entered chan struct{} // one signal per Send call, before it blocks
	release chan struct{}
	closed  chan struct{}

	closeOnce sync.Once
	mu        sync.Mutex
	events    []string
}

func newStallPhone(id string) *stallPhone {
	return &stallPhone{
		id:      id,
		entered: make(chan struct{}, 64),
		release: make(chan struct{}),
		closed:  make(chan struct{}),
	}
}

func (p *stallPhone) ConnID() string { return p.id }

func (p *stallPhone) Send(msg []byte) error {
	p.entered <- struct{}{}
	select {
	case <-p.release:
		p.record("send:" + string(msg))
		return nil
	case <-p.closed:
		return errStallClosed
	}
}

func (p *stallPhone) Close() { p.CloseWithCode(websocket.StatusNormalClosure, "") }

func (p *stallPhone) CloseWithCode(code websocket.StatusCode, _ string) {
	p.closeOnce.Do(func() {
		p.record(fmt.Sprintf("close:%d", code))
		close(p.closed)
	})
}

func (p *stallPhone) record(e string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, e)
}

func (p *stallPhone) snapshotEvents() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.events...)
}

func (p *stallPhone) waitEntered(t *testing.T) {
	t.Helper()
	select {
	case <-p.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("stallPhone: Send never entered")
	}
}

func (p *stallPhone) waitClosed(t *testing.T) {
	t.Helper()
	select {
	case <-p.closed:
	case <-time.After(2 * time.Second):
		t.Fatalf("stallPhone: not closed; events %v", p.snapshotEvents())
	}
}

// startOutbox wraps conn in a phoneOutbox and runs its drain loop the way
// ClientHandler does. The returned channel closes when run returns. Cleanup
// mirrors the handler's teardown: close the conn, stop, wait for run.
func startOutbox(t *testing.T, conn Conn, depth int, onWritten func()) (*phoneOutbox, <-chan struct{}) {
	t.Helper()
	o := newPhoneOutbox(conn, "s1", depth, onWritten, discardLogger())
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		o.run()
	}()
	t.Cleanup(func() {
		conn.Close()
		o.stop()
		<-exited
	})
	return o, exited
}

func waitExited(t *testing.T, exited <-chan struct{}) {
	t.Helper()
	select {
	case <-exited:
	case <-time.After(2 * time.Second):
		t.Fatal("outbox run did not return")
	}
}

// AC1: a phone whose write blocks must not delay frames to its siblings, and
// the forwarder must keep reading the binary meanwhile.
func TestStartBinaryForwarder_StalledPhone_DoesNotBlockOthers(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	claimAndRegister(t, reg, "s1")
	stalled := newStallPhone("client-s1-stalled0")
	healthy := newFakePhone("client-s1-healthy0")
	stalledBox, _ := startOutbox(t, stalled, phoneOutboxDepth, nil)
	healthyBox, _ := startOutbox(t, healthy, phoneOutboxDepth, nil)
	for _, c := range []Conn{stalledBox, healthyBox} {
		if err := reg.RegisterPhone("s1", c); err != nil {
			t.Fatalf("RegisterPhone: %v", err)
		}
	}

	src := newFakeBinarySource("bin-s1")
	_, cancel := runBinaryForwarder(reg, "s1", src)
	defer cancel()

	src.frames <- mustMarshal(t, stalled.ConnID(), []byte(`{"n":0}`))
	stalled.waitEntered(t)

	const n = 5
	for i := 1; i <= n; i++ {
		src.frames <- mustMarshal(t, healthy.ConnID(), []byte(fmt.Sprintf(`{"n":%d}`, i)))
	}
	got := waitForPhoneSent(t, healthy, n, 2*time.Second)
	for i, f := range got {
		if want := fmt.Sprintf(`{"n":%d}`, i+1); string(f) != want {
			t.Errorf("healthy frame %d = %s, want %s", i, f, want)
		}
	}
	if ev := stalled.snapshotEvents(); len(ev) != 0 {
		t.Fatalf("stalled phone events = %v, want its write still blocked", ev)
	}
	if len(src.frames) != 0 {
		t.Fatalf("forwarder left %d binary frames unread", len(src.frames))
	}
}

// AC2: frames reach a phone in order, and a close directive closes the phone
// only after its own frame and every earlier one has been written.
func TestStartBinaryForwarder_CloseDirective_WaitsForEarlierFrames(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	claimAndRegister(t, reg, "s1")
	phone := newStallPhone("client-s1-ordered0")
	box, exited := startOutbox(t, phone, phoneOutboxDepth, nil)
	if err := reg.RegisterPhone("s1", box); err != nil {
		t.Fatalf("RegisterPhone: %v", err)
	}

	src := newFakeBinarySource("bin-s1")
	_, cancel := runBinaryForwarder(reg, "s1", src)
	defer cancel()

	src.frames <- mustMarshal(t, phone.ConnID(), []byte(`{"n":1}`))
	src.frames <- mustMarshal(t, phone.ConnID(), []byte(`{"n":2}`))
	src.frames <- closeEnvelopeJSON(t, phone.ConnID(), json.RawMessage(`{"n":3}`), 4401)
	src.frames <- mustMarshal(t, phone.ConnID(), []byte(`{"n":4}`))
	phone.waitEntered(t)

	// Let the forwarder consume every envelope while the first write is held.
	deadline := time.Now().Add(2 * time.Second)
	for len(src.frames) != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	if ev := phone.snapshotEvents(); len(ev) != 0 {
		t.Fatalf("events before release = %v, want none (close must wait)", ev)
	}

	close(phone.release)
	waitExited(t, exited)
	want := []string{`send:{"n":1}`, `send:{"n":2}`, `send:{"n":3}`, "close:4401"}
	if got := phone.snapshotEvents(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

// AC3: a phone whose backlog hits the bound is closed with 1011 and its
// outbox refuses further frames.
func TestPhoneOutbox_BacklogFull_Closes1011(t *testing.T) {
	t.Parallel()

	phone := newStallPhone("client-s1-backlog0")
	box, exited := startOutbox(t, phone, 2, nil)

	if err := box.Enqueue([]byte(`{"n":0}`), 0); err != nil {
		t.Fatalf("Enqueue 0: %v", err)
	}
	phone.waitEntered(t) // item 0 is in flight; the queue is empty again
	for i := 1; i <= 2; i++ {
		if err := box.Enqueue([]byte(`{}`), 0); err != nil {
			t.Fatalf("Enqueue %d: %v", i, err)
		}
	}
	if err := box.Enqueue([]byte(`{}`), 0); !errors.Is(err, ErrPhoneBacklogFull) {
		t.Fatalf("Enqueue over bound = %v, want ErrPhoneBacklogFull", err)
	}

	phone.waitClosed(t)
	waitExited(t, exited)
	if got := phone.snapshotEvents(); fmt.Sprint(got) != "[close:1011]" {
		t.Fatalf("events = %v, want [close:1011] (pending frames dropped)", got)
	}
	if err := box.Enqueue([]byte(`{}`), 0); !errors.Is(err, ErrPhoneOutboxClosed) {
		t.Fatalf("Enqueue after overflow = %v, want ErrPhoneOutboxClosed", err)
	}
}

// AC3: a failed or timed-out write closes the phone with 1011, drops what
// is pending, and counts nothing as forwarded.
func TestPhoneOutbox_WriteFailure_Closes1011(t *testing.T) {
	t.Parallel()

	phone := newFakePhone("client-s1-failing0")
	phone.sendErr = errors.New("write timeout")
	var written atomic.Int32
	box, exited := startOutbox(t, phone, phoneOutboxDepth, func() { written.Add(1) })

	if err := box.Enqueue([]byte(`{"n":1}`), 0); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if code := waitForPhoneClose(t, phone, 2*time.Second); code != 1011 {
		t.Fatalf("close code = %d, want 1011", code)
	}
	waitExited(t, exited)
	if n := written.Load(); n != 0 {
		t.Fatalf("onWritten called %d times, want 0", n)
	}
	if err := box.Enqueue([]byte(`{}`), 0); !errors.Is(err, ErrPhoneOutboxClosed) {
		t.Fatalf("Enqueue after write failure = %v, want ErrPhoneOutboxClosed", err)
	}
}

// onWritten counts exactly the frames written to the phone, including a
// close directive's final frame.
func TestPhoneOutbox_CountsWrittenFrames(t *testing.T) {
	t.Parallel()

	phone := newFakePhone("client-s1-counted0")
	var written atomic.Int32
	box, exited := startOutbox(t, phone, phoneOutboxDepth, func() { written.Add(1) })

	for i := 0; i < 3; i++ {
		if err := box.Enqueue([]byte(`{}`), 0); err != nil {
			t.Fatalf("Enqueue %d: %v", i, err)
		}
	}
	if err := box.Enqueue(nil, 4400); err != nil {
		t.Fatalf("Enqueue close: %v", err)
	}
	waitExited(t, exited)
	if n := written.Load(); n != 3 {
		t.Fatalf("onWritten called %d times, want 3", n)
	}
	if code, _ := phone.snapshotClose(); code != 4400 {
		t.Fatalf("close code = %d, want 4400", code)
	}
}

// AC4: stop releases an idle drain loop, and a stopped outbox refuses
// frames without blocking.
func TestPhoneOutbox_Stop_ReleasesRun(t *testing.T) {
	t.Parallel()

	phone := newFakePhone("client-s1-stopped0")
	box := newPhoneOutbox(phone, "s1", 1, nil, discardLogger())
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		box.run()
	}()

	box.stop()
	waitExited(t, exited)
	for i := 0; i < 3; i++ {
		if err := box.Enqueue([]byte(`{}`), 0); !errors.Is(err, ErrPhoneOutboxClosed) {
			t.Fatalf("Enqueue %d after stop = %v, want ErrPhoneOutboxClosed", i, err)
		}
	}
	if sent := phone.snapshotSent(); len(sent) != 0 {
		t.Fatalf("stopped outbox wrote %d frames", len(sent))
	}
}

// AC4: a drain loop blocked in a write returns once the conn is closed, as
// the handler's teardown does.
func TestPhoneOutbox_CloseAbortsBlockedWrite(t *testing.T) {
	t.Parallel()

	phone := newStallPhone("client-s1-blocked0")
	box := newPhoneOutbox(phone, "s1", phoneOutboxDepth, nil, discardLogger())
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		box.run()
	}()
	if err := box.Enqueue([]byte(`{}`), 0); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	phone.waitEntered(t)

	box.Close()
	box.stop()
	waitExited(t, exited)
}

// The wrapper keeps code-aware close, so eviction (4404) and shutdown
// (1001) reach the phone with their codes rather than a plain close.
func TestPhoneOutbox_CloseWithCode_Delegates(t *testing.T) {
	t.Parallel()

	phone := newFakePhone("client-s1-coded000")
	box := newPhoneOutbox(phone, "s1", 1, nil, discardLogger())
	closeWithCode(box, reclaimCloseCode, reclaimCloseReason)
	if code, _ := phone.snapshotClose(); code != uint16(reclaimCloseCode) {
		t.Fatalf("close code = %d, want %d", code, reclaimCloseCode)
	}
	if box.ConnID() != phone.ConnID() {
		t.Fatalf("ConnID = %q, want %q", box.ConnID(), phone.ConnID())
	}
}
