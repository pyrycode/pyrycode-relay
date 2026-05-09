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

// fakePhone implements phoneSource. Tests push frames onto frames; closing
// the channel signals "phone disconnected" — Read returns io.EOF.
type fakePhone struct {
	id     string
	frames chan []byte
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
