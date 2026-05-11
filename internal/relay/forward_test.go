// Manual stress: go test -race -count=20 ./internal/relay/
// (count belongs on the command line, not as a loop in the test;
// see docs/lessons.md "Race-test count is a CI-runner knob".)
package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// fakePhone implements phoneSource and Conn. Tests push frames onto frames;
// closing the channel signals "phone disconnected" — Read returns io.EOF.
// Send captures bytes for the binary-forwarder tests; #25 tests still wrap
// it via registryConn (which discards Send) and never inspect sent.
type fakePhone struct {
	id     string
	frames chan []byte

	mu      sync.Mutex
	sent    [][]byte
	sendErr error
}

func newFakePhone(id string) *fakePhone {
	return &fakePhone{id: id, frames: make(chan []byte, 16)}
}

func (p *fakePhone) ConnID() string { return p.id }

func (p *fakePhone) Read(ctx context.Context) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case frame, ok := <-p.frames:
		if !ok {
			return nil, io.EOF
		}
		return frame, nil
	}
}

func (p *fakePhone) Send(msg []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sendErr != nil {
		return p.sendErr
	}
	cp := make([]byte, len(msg))
	copy(cp, msg)
	p.sent = append(p.sent, cp)
	return nil
}

func (p *fakePhone) Close() {}

func (p *fakePhone) snapshotSent() [][]byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([][]byte, len(p.sent))
	for i, m := range p.sent {
		cp := make([]byte, len(m))
		copy(cp, m)
		out[i] = cp
	}
	return out
}

// waitForPhoneSent polls until want frames have been captured by p.Send,
// or fails the test. Mirrors waitForSent's shape for *fakeBinary.
func waitForPhoneSent(t *testing.T, p *fakePhone, want int, timeout time.Duration) [][]byte {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		got := p.snapshotSent()
		if len(got) >= want {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	got := p.snapshotSent()
	t.Fatalf("waitForPhoneSent: got %d, want %d after %v", len(got), want, timeout)
	return nil
}

// fakeBinarySource implements binarySource. Tests push wire-encoded
// envelopes onto frames; closing the channel signals "binary
// disconnected" — Read returns io.EOF.
type fakeBinarySource struct {
	id     string
	frames chan []byte
}

func newFakeBinarySource(id string) *fakeBinarySource {
	return &fakeBinarySource{id: id, frames: make(chan []byte, 16)}
}

func (b *fakeBinarySource) ConnID() string { return b.id }

func (b *fakeBinarySource) Read(ctx context.Context) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case frame, ok := <-b.frames:
		if !ok {
			return nil, io.EOF
		}
		return frame, nil
	}
}

// fakeBinary implements Conn for use as the registry-side binary. Send is
// called from the forwarder goroutine while the test reads sent from the
// test goroutine, so writes are mu-protected.
type fakeBinary struct {
	id      string
	mu      sync.Mutex
	sent    [][]byte
	sendErr error
}

func (b *fakeBinary) ConnID() string { return b.id }

func (b *fakeBinary) Send(msg []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.sendErr != nil {
		return b.sendErr
	}
	cp := make([]byte, len(msg))
	copy(cp, msg)
	b.sent = append(b.sent, cp)
	return nil
}

func (b *fakeBinary) Close() {}

func (b *fakeBinary) snapshot() [][]byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([][]byte, len(b.sent))
	for i, m := range b.sent {
		cp := make([]byte, len(m))
		copy(cp, m)
		out[i] = cp
	}
	return out
}

func waitForSent(t *testing.T, b *fakeBinary, want int, timeout time.Duration) [][]byte {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		got := b.snapshot()
		if len(got) >= want {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	got := b.snapshot()
	t.Fatalf("waitForSent: got %d, want %d after %v", len(got), want, timeout)
	return nil
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// runForwarder spawns the forwarder on a goroutine and returns a done chan
// that closes once it returns, plus the cancel for the parent context.
func runForwarder(reg *Registry, serverID string, phone phoneSource) (done chan error, cancel context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	done = make(chan error, 1)
	go func() {
		done <- StartPhoneForwarder(ctx, reg, serverID, phone, discardLogger())
	}()
	return done, cancel
}

func compactJSON(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Compact(&buf, b); err != nil {
		t.Fatalf("compact: %v (input: %s)", err, b)
	}
	return buf.Bytes()
}

func TestStartPhoneForwarder_ForwardsFramesBytewise(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
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

	innerFrames := [][]byte{
		[]byte(`{"type":"hello","nested":{"a":[1,2,3],"b":null}}`),
		[]byte(`{"type":"keystroke","data":"abc","x":42}`),
		[]byte(`{"type":"resize","cols":80,"rows":24,"meta":{"k":"v"}}`),
	}
	for _, f := range innerFrames {
		phone.frames <- f
	}

	got := waitForSent(t, bin, len(innerFrames), 2*time.Second)
	if len(got) != len(innerFrames) {
		t.Fatalf("sent count = %d, want %d", len(got), len(innerFrames))
	}

	for i, wrapped := range got {
		env, err := Unmarshal(wrapped)
		if err != nil {
			t.Fatalf("Unmarshal frame %d: %v", i, err)
		}
		if env.ConnID != phone.ConnID() {
			t.Errorf("frame %d ConnID = %q, want %q", i, env.ConnID, phone.ConnID())
		}
		want := compactJSON(t, innerFrames[i])
		gotInner := compactJSON(t, env.Frame)
		if !bytes.Equal(want, gotInner) {
			t.Errorf("frame %d inner bytes diverged\nwant: %s\n got: %s", i, want, gotInner)
		}
	}

	close(phone.frames)
	select {
	case err := <-done:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("forwarder return = %v, want io.EOF", err)
		}
	case <-time.After(time.Second):
		t.Fatal("forwarder did not return after frames chan closed")
	}
}

func TestStartPhoneForwarder_PhoneDisconnect_LeavesRegistryCleanable(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	bin := &fakeBinary{id: "bin-s1"}
	if err := reg.ClaimServer("s1", bin); err != nil {
		t.Fatalf("ClaimServer: %v", err)
	}
	phone := newFakePhone("client-s1-cc11dd22")
	if err := reg.RegisterPhone("s1", &registryConn{phone}); err != nil {
		t.Fatalf("RegisterPhone: %v", err)
	}

	done, cancel := runForwarder(reg, "s1", phone)
	defer cancel()

	phone.frames <- []byte(`{"type":"k","v":1}`)
	waitForSent(t, bin, 1, time.Second)

	// Mid-stream phone disconnect: closing the chan makes Read return EOF.
	close(phone.frames)
	select {
	case err := <-done:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("forwarder return = %v, want io.EOF", err)
		}
	case <-time.After(time.Second):
		t.Fatal("forwarder did not return after phone disconnect")
	}

	// Mimic the handler-level defer that owns cleanup.
	reg.UnregisterPhone("s1", phone.ConnID())
	if got := reg.PhonesFor("s1"); got != nil {
		t.Fatalf("PhonesFor(s1) = %v, want nil after UnregisterPhone", got)
	}
}

func TestStartPhoneForwarder_NoBinary_ReturnsNil(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	// No ClaimServer, no RegisterPhone — testing the forwarder's own
	// resilience to a missing binary, not the handler's.
	phone := newFakePhone("client-s1-ee22ff33")

	done, cancel := runForwarder(reg, "s1", phone)
	defer cancel()

	phone.frames <- []byte(`{"x":1}`)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("forwarder return = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("forwarder did not return on missing binary")
	}
}

func TestStartPhoneForwarder_ContextCancellation_Returns(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	bin := &fakeBinary{id: "bin-s1"}
	if err := reg.ClaimServer("s1", bin); err != nil {
		t.Fatalf("ClaimServer: %v", err)
	}
	phone := newFakePhone("client-s1-44aa55bb")
	if err := reg.RegisterPhone("s1", &registryConn{phone}); err != nil {
		t.Fatalf("RegisterPhone: %v", err)
	}

	done, cancel := runForwarder(reg, "s1", phone)

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("forwarder return = %v, want context.Canceled", err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("forwarder did not return promptly on ctx cancel")
	}
}

// registryConn adapts a fakePhone to the registry's Conn interface so it can
// be RegisterPhone'd. The forwarder reads through the fakePhone directly via
// phoneSource; the registry needs a Conn (ConnID, Send, Close).
type registryConn struct{ p *fakePhone }

func (c *registryConn) ConnID() string        { return c.p.ConnID() }
func (c *registryConn) Send(msg []byte) error { return nil }
func (c *registryConn) Close()                {}

// runBinaryForwarder spawns StartBinaryForwarder on a goroutine and returns
// a done chan that closes once it returns, plus the cancel for the parent
// context.
func runBinaryForwarder(reg *Registry, serverID string, bin binarySource) (done chan error, cancel context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	done = make(chan error, 1)
	go func() {
		done <- StartBinaryForwarder(ctx, reg, serverID, bin, discardLogger())
	}()
	return done, cancel
}

// claimAndRegister sets up a server slot and registers the given phones
// against it. Returns the bin used for the slot (a stillborn fakeBinary
// satisfies the Conn interface; this forwarder doesn't use BinaryFor).
func claimAndRegister(t *testing.T, reg *Registry, serverID string, phones ...*fakePhone) *fakeBinary {
	t.Helper()
	bin := &fakeBinary{id: "bin-" + serverID}
	if err := reg.ClaimServer(serverID, bin); err != nil {
		t.Fatalf("ClaimServer: %v", err)
	}
	for _, p := range phones {
		if err := reg.RegisterPhone(serverID, p); err != nil {
			t.Fatalf("RegisterPhone(%s): %v", p.ConnID(), err)
		}
	}
	return bin
}

func mustMarshal(t *testing.T, connID string, frame []byte) []byte {
	t.Helper()
	out, err := Marshal(connID, frame)
	if err != nil {
		t.Fatalf("Marshal(%s): %v", connID, err)
	}
	return out
}

func TestStartBinaryForwarder_RoutesToAddressedPhone(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	p1 := newFakePhone("client-s1-aaaa1111")
	p2 := newFakePhone("client-s1-bbbb2222")
	claimAndRegister(t, reg, "s1", p1, p2)

	src := newFakeBinarySource("bin-s1")
	done, cancel := runBinaryForwarder(reg, "s1", src)
	defer cancel()

	inner := []byte(`{"type":"hello","x":[1,2,3]}`)
	src.frames <- mustMarshal(t, p1.ConnID(), inner)

	got := waitForPhoneSent(t, p1, 1, 2*time.Second)
	if !bytes.Equal(compactJSON(t, got[0]), compactJSON(t, inner)) {
		t.Errorf("p1 inner bytes diverged\nwant: %s\n got: %s",
			compactJSON(t, inner), compactJSON(t, got[0]))
	}
	if other := p2.snapshotSent(); len(other) != 0 {
		t.Errorf("p2 sent = %d frames, want 0", len(other))
	}

	close(src.frames)
	select {
	case err := <-done:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("forwarder return = %v, want io.EOF", err)
		}
	case <-time.After(time.Second):
		t.Fatal("forwarder did not return after frames chan closed")
	}
}

func TestStartBinaryForwarder_MultiplePhones(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	p1 := newFakePhone("client-s1-aaaa1111")
	p2 := newFakePhone("client-s1-bbbb2222")
	claimAndRegister(t, reg, "s1", p1, p2)

	src := newFakeBinarySource("bin-s1")
	done, cancel := runBinaryForwarder(reg, "s1", src)
	defer cancel()

	inner1 := []byte(`{"type":"keystroke","data":"abc"}`)
	inner2 := []byte(`{"type":"resize","cols":80,"rows":24}`)
	src.frames <- mustMarshal(t, p1.ConnID(), inner1)
	src.frames <- mustMarshal(t, p2.ConnID(), inner2)

	got1 := waitForPhoneSent(t, p1, 1, 2*time.Second)
	got2 := waitForPhoneSent(t, p2, 1, 2*time.Second)

	if !bytes.Equal(compactJSON(t, got1[0]), compactJSON(t, inner1)) {
		t.Errorf("p1 inner bytes diverged\nwant: %s\n got: %s",
			compactJSON(t, inner1), compactJSON(t, got1[0]))
	}
	if !bytes.Equal(compactJSON(t, got2[0]), compactJSON(t, inner2)) {
		t.Errorf("p2 inner bytes diverged\nwant: %s\n got: %s",
			compactJSON(t, inner2), compactJSON(t, got2[0]))
	}
	if len(p1.snapshotSent()) != 1 {
		t.Errorf("p1 sent = %d, want 1", len(p1.snapshotSent()))
	}
	if len(p2.snapshotSent()) != 1 {
		t.Errorf("p2 sent = %d, want 1", len(p2.snapshotSent()))
	}

	close(src.frames)
	<-done
}

func TestStartBinaryForwarder_UnknownConnID_DropsAndContinues(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	p1 := newFakePhone("client-s1-aaaa1111")
	p2 := newFakePhone("client-s1-bbbb2222")
	claimAndRegister(t, reg, "s1", p1, p2)

	src := newFakeBinarySource("bin-s1")
	done, cancel := runBinaryForwarder(reg, "s1", src)
	defer cancel()

	inner := []byte(`{"type":"after-bogus"}`)
	src.frames <- mustMarshal(t, "client-s1-deadbeef", []byte(`{"dropped":true}`))
	src.frames <- mustMarshal(t, p1.ConnID(), inner)

	got := waitForPhoneSent(t, p1, 1, 2*time.Second)
	if !bytes.Equal(compactJSON(t, got[0]), compactJSON(t, inner)) {
		t.Errorf("p1 inner bytes diverged\nwant: %s\n got: %s",
			compactJSON(t, inner), compactJSON(t, got[0]))
	}
	if other := p2.snapshotSent(); len(other) != 0 {
		t.Errorf("p2 sent = %d frames, want 0", len(other))
	}

	close(src.frames)
	select {
	case err := <-done:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("forwarder return = %v, want io.EOF", err)
		}
	case <-time.After(time.Second):
		t.Fatal("forwarder did not return after frames chan closed")
	}
}

func TestStartBinaryForwarder_MalformedEnvelope_DropsAndContinues(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	p1 := newFakePhone("client-s1-aaaa1111")
	claimAndRegister(t, reg, "s1", p1)

	src := newFakeBinarySource("bin-s1")
	done, cancel := runBinaryForwarder(reg, "s1", src)
	defer cancel()

	inner := []byte(`{"type":"after-malformed"}`)
	src.frames <- []byte("not-json")
	src.frames <- mustMarshal(t, p1.ConnID(), inner)

	got := waitForPhoneSent(t, p1, 1, 2*time.Second)
	if !bytes.Equal(compactJSON(t, got[0]), compactJSON(t, inner)) {
		t.Errorf("p1 inner bytes diverged\nwant: %s\n got: %s",
			compactJSON(t, inner), compactJSON(t, got[0]))
	}

	close(src.frames)
	select {
	case err := <-done:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("forwarder return = %v, want io.EOF", err)
		}
	case <-time.After(time.Second):
		t.Fatal("forwarder did not return after frames chan closed")
	}
}

func TestStartBinaryForwarder_PhoneSendError_DropsAndContinues(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	p1 := newFakePhone("client-s1-aaaa1111")
	p1.sendErr = errors.New("phone wedged")
	p2 := newFakePhone("client-s1-bbbb2222")
	claimAndRegister(t, reg, "s1", p1, p2)

	src := newFakeBinarySource("bin-s1")
	done, cancel := runBinaryForwarder(reg, "s1", src)
	defer cancel()

	innerToP1 := []byte(`{"type":"to-p1"}`)
	innerToP2 := []byte(`{"type":"to-p2"}`)
	src.frames <- mustMarshal(t, p1.ConnID(), innerToP1)
	src.frames <- mustMarshal(t, p2.ConnID(), innerToP2)

	got2 := waitForPhoneSent(t, p2, 1, 2*time.Second)
	if !bytes.Equal(compactJSON(t, got2[0]), compactJSON(t, innerToP2)) {
		t.Errorf("p2 inner bytes diverged\nwant: %s\n got: %s",
			compactJSON(t, innerToP2), compactJSON(t, got2[0]))
	}
	// p1.Send returned an error; sendErr-path skips the append so sent stays empty.
	if dropped := p1.snapshotSent(); len(dropped) != 0 {
		t.Errorf("p1 sent = %d, want 0 (Send errored)", len(dropped))
	}

	close(src.frames)
	select {
	case err := <-done:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("forwarder return = %v, want io.EOF", err)
		}
	case <-time.After(time.Second):
		t.Fatal("forwarder did not return after frames chan closed")
	}
}

func TestStartBinaryForwarder_BinaryDisconnect_Returns(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	p1 := newFakePhone("client-s1-aaaa1111")
	claimAndRegister(t, reg, "s1", p1)

	src := newFakeBinarySource("bin-s1")
	done, cancel := runBinaryForwarder(reg, "s1", src)
	defer cancel()

	inner := []byte(`{"type":"first"}`)
	src.frames <- mustMarshal(t, p1.ConnID(), inner)
	waitForPhoneSent(t, p1, 1, 2*time.Second)

	close(src.frames)
	select {
	case err := <-done:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("forwarder return = %v, want io.EOF", err)
		}
	case <-time.After(time.Second):
		t.Fatal("forwarder did not return after binary disconnect")
	}

	// Mimic the handler-level defer: ScheduleReleaseServer with zero
	// grace fires the timer immediately on a runtime goroutine. Poll for
	// the slot to clear — verifies the AC bullet structurally.
	reg.ScheduleReleaseServer("s1", 0)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, ok := reg.BinaryFor("s1"); !ok {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("BinaryFor(s1) still present after ScheduleReleaseServer(0) expired")
}

func TestStartBinaryForwarder_ContextCancellation_Returns(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	p1 := newFakePhone("client-s1-aaaa1111")
	claimAndRegister(t, reg, "s1", p1)

	src := newFakeBinarySource("bin-s1")
	done, cancel := runBinaryForwarder(reg, "s1", src)

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("forwarder return = %v, want context.Canceled", err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("forwarder did not return promptly on ctx cancel")
	}
}
