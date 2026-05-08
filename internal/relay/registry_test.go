package relay

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

// fakeConn is a minimal in-package Conn for tests. The registry's race
// coverage proves the registry is race-free with well-behaved Conns; it does
// not vouch for fakeConn under concurrent mutation, so tests register
// distinct fakes per goroutine.
type fakeConn struct {
	id     string
	sent   [][]byte
	closed bool
}

func (c *fakeConn) ConnID() string        { return c.id }
func (c *fakeConn) Send(msg []byte) error { c.sent = append(c.sent, msg); return nil }
func (c *fakeConn) Close()                { c.closed = true }

func TestClaimServer_FirstWinsSecondConflicts(t *testing.T) {
	t.Parallel()
	r := NewRegistry()

	first := &fakeConn{id: "b-1"}
	second := &fakeConn{id: "b-2"}

	if err := r.ClaimServer("s1", first); err != nil {
		t.Fatalf("first ClaimServer: unexpected err: %v", err)
	}
	if err := r.ClaimServer("s1", second); !errors.Is(err, ErrServerIDConflict) {
		t.Fatalf("second ClaimServer: got %v, want errors.Is(_, ErrServerIDConflict)", err)
	}
	got, ok := r.BinaryFor("s1")
	if !ok {
		t.Fatal("BinaryFor: missing entry after conflict")
	}
	if got.ConnID() != "b-1" {
		t.Errorf("BinaryFor: got conn id %q, want %q (original)", got.ConnID(), "b-1")
	}
}

func TestReleaseServer_ReclaimSucceeds(t *testing.T) {
	t.Parallel()
	r := NewRegistry()

	first := &fakeConn{id: "b-1"}
	second := &fakeConn{id: "b-2"}

	if err := r.ClaimServer("s1", first); err != nil {
		t.Fatalf("ClaimServer: %v", err)
	}
	if released := r.ReleaseServer("s1"); !released {
		t.Fatal("ReleaseServer: got false, want true")
	}
	if err := r.ClaimServer("s1", second); err != nil {
		t.Fatalf("re-ClaimServer: %v", err)
	}
	got, ok := r.BinaryFor("s1")
	if !ok || got.ConnID() != "b-2" {
		t.Errorf("BinaryFor after reclaim: got (%v, %v), want (b-2, true)", got, ok)
	}
	if released := r.ReleaseServer("unknown"); released {
		t.Error("ReleaseServer of unheld id: got true, want false")
	}
}

func TestRegisterPhone_RequiresBinary(t *testing.T) {
	t.Parallel()
	r := NewRegistry()

	phone := &fakeConn{id: "p-1"}
	if err := r.RegisterPhone("s1", phone); !errors.Is(err, ErrNoServer) {
		t.Fatalf("RegisterPhone without binary: got %v, want errors.Is(_, ErrNoServer)", err)
	}

	if err := r.ClaimServer("s1", &fakeConn{id: "b-1"}); err != nil {
		t.Fatalf("ClaimServer: %v", err)
	}
	if err := r.RegisterPhone("s1", phone); err != nil {
		t.Fatalf("RegisterPhone after binary: unexpected err: %v", err)
	}
}

func TestUnregisterPhone_RemovesByConnID(t *testing.T) {
	t.Parallel()
	r := NewRegistry()

	if err := r.ClaimServer("s1", &fakeConn{id: "b-1"}); err != nil {
		t.Fatalf("ClaimServer: %v", err)
	}
	p1 := &fakeConn{id: "p-1"}
	p2 := &fakeConn{id: "p-2"}
	p3 := &fakeConn{id: "p-3"}
	for _, p := range []*fakeConn{p1, p2, p3} {
		if err := r.RegisterPhone("s1", p); err != nil {
			t.Fatalf("RegisterPhone %q: %v", p.id, err)
		}
	}

	r.UnregisterPhone("s1", "p-2")

	got := r.PhonesFor("s1")
	if len(got) != 2 {
		t.Fatalf("PhonesFor: got len=%d, want 2", len(got))
	}
	ids := map[string]bool{}
	for _, c := range got {
		ids[c.ConnID()] = true
	}
	if !ids["p-1"] || !ids["p-3"] || ids["p-2"] {
		t.Errorf("PhonesFor: got ids=%v, want {p-1, p-3}", ids)
	}

	r.UnregisterPhone("s1", "does-not-exist")
	if got := r.PhonesFor("s1"); len(got) != 2 {
		t.Errorf("PhonesFor after no-op unregister: got len=%d, want 2", len(got))
	}
	r.UnregisterPhone("unknown-server", "p-1")
}

func TestPhonesFor_SnapshotIsolation(t *testing.T) {
	t.Parallel()
	r := NewRegistry()

	if err := r.ClaimServer("s1", &fakeConn{id: "b-1"}); err != nil {
		t.Fatalf("ClaimServer: %v", err)
	}
	p1 := &fakeConn{id: "p-1"}
	p2 := &fakeConn{id: "p-2"}
	if err := r.RegisterPhone("s1", p1); err != nil {
		t.Fatalf("RegisterPhone p1: %v", err)
	}
	if err := r.RegisterPhone("s1", p2); err != nil {
		t.Fatalf("RegisterPhone p2: %v", err)
	}

	snap := r.PhonesFor("s1")
	if len(snap) != 2 {
		t.Fatalf("snap len: got %d, want 2", len(snap))
	}
	snap[0] = &fakeConn{id: "evil"}

	again := r.PhonesFor("s1")
	if len(again) != 2 {
		t.Fatalf("again len: got %d, want 2", len(again))
	}
	for _, c := range again {
		if c.ConnID() == "evil" {
			t.Fatal("registry observed mutation through snapshot")
		}
	}

	if err := r.RegisterPhone("s1", &fakeConn{id: "p-3"}); err != nil {
		t.Fatalf("RegisterPhone p3: %v", err)
	}
	if len(snap) != 2 {
		t.Errorf("original snapshot length changed: got %d, want 2", len(snap))
	}
}

func TestCounts_AcrossLifecycle(t *testing.T) {
	t.Parallel()
	r := NewRegistry()

	assert := func(stage string, wantB, wantP int) {
		t.Helper()
		b, p := r.Counts()
		if b != wantB || p != wantP {
			t.Errorf("%s: Counts=(%d,%d), want (%d,%d)", stage, b, p, wantB, wantP)
		}
	}

	assert("initial", 0, 0)
	if err := r.ClaimServer("s1", &fakeConn{id: "b-1"}); err != nil {
		t.Fatalf("ClaimServer: %v", err)
	}
	assert("after claim", 1, 0)

	if err := r.RegisterPhone("s1", &fakeConn{id: "p-1"}); err != nil {
		t.Fatalf("RegisterPhone p1: %v", err)
	}
	if err := r.RegisterPhone("s1", &fakeConn{id: "p-2"}); err != nil {
		t.Fatalf("RegisterPhone p2: %v", err)
	}
	assert("after 2 phones", 1, 2)

	r.UnregisterPhone("s1", "p-1")
	assert("after unregister 1", 1, 1)

	if released := r.ReleaseServer("s1"); !released {
		t.Fatal("ReleaseServer: got false, want true")
	}
	// Orphan phones survive ReleaseServer by design (see open question 1).
	assert("after release (orphan retained)", 0, 1)

	r.UnregisterPhone("s1", "p-2")
	assert("after orphan unregister", 0, 0)
}

func TestUnregisterPhone_RemovesEmptySlice(t *testing.T) {
	t.Parallel()
	r := NewRegistry()

	if err := r.ClaimServer("s1", &fakeConn{id: "b-1"}); err != nil {
		t.Fatalf("ClaimServer: %v", err)
	}
	if err := r.RegisterPhone("s1", &fakeConn{id: "p-1"}); err != nil {
		t.Fatalf("RegisterPhone: %v", err)
	}
	r.UnregisterPhone("s1", "p-1")

	if got := r.PhonesFor("s1"); got != nil {
		t.Errorf("PhonesFor after empty: got %v, want nil", got)
	}
	if _, p := r.Counts(); p != 0 {
		t.Errorf("phones count after empty: got %d, want 0", p)
	}
}

// TestRegistry_RaceFreedom hammers the public API from many goroutines and
// asserts the absence of DATA RACE reports under -race.
//
// Run with: go test -race -count=20 -run TestRegistry_RaceFreedom ./internal/relay
func TestRegistry_RaceFreedom(t *testing.T) {
	t.Parallel()
	r := NewRegistry()

	const goroutines = 32
	const opsPer = 200

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		g := g
		go func() {
			defer wg.Done()
			sid := fmt.Sprintf("s-%d", g%4)
			bin := &fakeConn{id: fmt.Sprintf("b-%d", g)}
			for i := 0; i < opsPer; i++ {
				_ = r.ClaimServer(sid, bin)
				_, _ = r.BinaryFor(sid)
				_ = r.RegisterPhone(sid, &fakeConn{id: fmt.Sprintf("p-%d-%d", g, i)})
				_ = r.PhonesFor(sid)
				r.UnregisterPhone(sid, fmt.Sprintf("p-%d-%d", g, i))
				_, _ = r.Counts()
				_ = r.ReleaseServer(sid)
			}
		}()
	}
	wg.Wait()
}
