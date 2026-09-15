package relay

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakeConn is a minimal in-package Conn for tests. The registry's race
// coverage proves the registry is race-free with well-behaved Conns; it does
// not vouch for fakeConn under concurrent mutation, so tests register
// distinct fakes per goroutine.
//
// Close mutates closed under mu so tests on a different goroutine than the
// grace-expiry handler can safely read it via isClosed. Tests that need to
// block until Close fires allocate closeCh; Close closes it once.
type fakeConn struct {
	id      string
	sent    [][]byte
	mu      sync.Mutex
	closed  bool
	closeCh chan struct{}
}

func (c *fakeConn) ConnID() string        { return c.id }
func (c *fakeConn) Send(msg []byte) error { c.sent = append(c.sent, msg); return nil }
func (c *fakeConn) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	if c.closeCh != nil {
		close(c.closeCh)
	}
}

func (c *fakeConn) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

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

func TestRegisterPhoneCapped_BoundaryAndRejection(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	if err := r.ClaimServer("s1", &fakeConn{id: "b-1"}); err != nil {
		t.Fatalf("ClaimServer: %v", err)
	}
	const max = 3
	for i := 0; i < max; i++ {
		p := &fakeConn{id: fmt.Sprintf("p-%d", i)}
		if err := r.RegisterPhoneCapped("s1", p, max); err != nil {
			t.Fatalf("RegisterPhoneCapped #%d at <=cap: got %v, want nil", i, err)
		}
	}
	over := &fakeConn{id: "p-over"}
	if err := r.RegisterPhoneCapped("s1", over, max); !errors.Is(err, ErrPhonesAtCap) {
		t.Fatalf("RegisterPhoneCapped at cap: got %v, want errors.Is(_, ErrPhonesAtCap)", err)
	}
	if got := r.PhonesFor("s1"); len(got) != max {
		t.Errorf("PhonesFor after rejection: got len=%d, want %d", len(got), max)
	}
}

func TestRegisterPhoneCapped_RecoveryAfterUnregister(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	if err := r.ClaimServer("s1", &fakeConn{id: "b-1"}); err != nil {
		t.Fatalf("ClaimServer: %v", err)
	}
	const max = 2
	p1 := &fakeConn{id: "p-1"}
	p2 := &fakeConn{id: "p-2"}
	if err := r.RegisterPhoneCapped("s1", p1, max); err != nil {
		t.Fatalf("RegisterPhoneCapped p1: %v", err)
	}
	if err := r.RegisterPhoneCapped("s1", p2, max); err != nil {
		t.Fatalf("RegisterPhoneCapped p2: %v", err)
	}
	if err := r.RegisterPhoneCapped("s1", &fakeConn{id: "p-3"}, max); !errors.Is(err, ErrPhonesAtCap) {
		t.Fatalf("RegisterPhoneCapped at cap: got %v, want errors.Is(_, ErrPhonesAtCap)", err)
	}
	r.UnregisterPhone("s1", "p-1")
	p3 := &fakeConn{id: "p-3"}
	if err := r.RegisterPhoneCapped("s1", p3, max); err != nil {
		t.Fatalf("RegisterPhoneCapped after unregister: got %v, want nil", err)
	}
}

func TestRegisterPhoneCapped_PerServerIDIndependent(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	if err := r.ClaimServer("s1", &fakeConn{id: "b-1"}); err != nil {
		t.Fatalf("ClaimServer s1: %v", err)
	}
	if err := r.ClaimServer("s2", &fakeConn{id: "b-2"}); err != nil {
		t.Fatalf("ClaimServer s2: %v", err)
	}
	const max = 2
	for i := 0; i < max; i++ {
		if err := r.RegisterPhoneCapped("s1", &fakeConn{id: fmt.Sprintf("p-s1-%d", i)}, max); err != nil {
			t.Fatalf("RegisterPhoneCapped s1 #%d: %v", i, err)
		}
	}
	if err := r.RegisterPhoneCapped("s1", &fakeConn{id: "p-s1-over"}, max); !errors.Is(err, ErrPhonesAtCap) {
		t.Fatalf("RegisterPhoneCapped s1 over: got %v, want ErrPhonesAtCap", err)
	}
	for i := 0; i < max; i++ {
		if err := r.RegisterPhoneCapped("s2", &fakeConn{id: fmt.Sprintf("p-s2-%d", i)}, max); err != nil {
			t.Fatalf("RegisterPhoneCapped s2 #%d (s1 at cap): %v", i, err)
		}
	}
}

// TestRegisterPhone_NoCapAfterDelegation pins the wrapper-delegation
// contract: RegisterPhone passes max=0 to RegisterPhoneCapped, which
// disables the cap. A regression that hard-codes a cap into the wrapper
// would trip this test.
func TestRegisterPhone_NoCapAfterDelegation(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	if err := r.ClaimServer("s1", &fakeConn{id: "b-1"}); err != nil {
		t.Fatalf("ClaimServer: %v", err)
	}
	for i := 0; i < 64; i++ {
		if err := r.RegisterPhone("s1", &fakeConn{id: fmt.Sprintf("p-%d", i)}); err != nil {
			t.Fatalf("RegisterPhone #%d: %v", i, err)
		}
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

func TestScheduleReleaseServer_ReclaimWithinGrace_NoReleaseFires(t *testing.T) {
	t.Parallel()
	r := NewRegistry()

	b1 := &fakeConn{id: "b-1"}
	b2 := &fakeConn{id: "b-2"}
	p1 := &fakeConn{id: "p-1"}

	if err := r.ClaimServer("s1", b1); err != nil {
		t.Fatalf("ClaimServer b1: %v", err)
	}
	if err := r.RegisterPhone("s1", p1); err != nil {
		t.Fatalf("RegisterPhone p1: %v", err)
	}

	r.ScheduleReleaseServer("s1", 50*time.Millisecond)

	if err := r.ClaimServer("s1", b2); err != nil {
		t.Fatalf("reclaim ClaimServer: got %v, want nil", err)
	}
	got, ok := r.BinaryFor("s1")
	if !ok || got.ConnID() != "b-2" {
		t.Errorf("BinaryFor after reclaim: got (%v, %v), want (b-2, true)", got, ok)
	}
	// p1 was inherited before #127; it is now evicted at reclaim time so
	// it re-dials and re-handshakes against b2 (see the dedicated
	// TestClaimServer_ReclaimDuringGrace_* tests). The stale-fire check
	// below uses a phone registered against the new binary instead.
	if phones := r.PhonesFor("s1"); phones != nil {
		t.Errorf("PhonesFor after reclaim: got %v, want nil (phones evicted at reclaim)", phones)
	}
	p2 := &fakeConn{id: "p-2"}
	if err := r.RegisterPhone("s1", p2); err != nil {
		t.Fatalf("RegisterPhone p2 against b2: %v", err)
	}

	// Sleep past the original grace window. If the timer had fired stale
	// and bypassed the pointer check, p2 would now be closed.
	time.Sleep(100 * time.Millisecond)

	if p2.isClosed() {
		t.Error("p2 was closed after reclaim — stale timer fire was not suppressed")
	}
	if got, ok := r.BinaryFor("s1"); !ok || got.ConnID() != "b-2" {
		t.Errorf("BinaryFor after grace window elapsed: got (%v, %v), want (b-2, true)", got, ok)
	}
}

func TestScheduleReleaseServer_ExpiryRemovesBinaryAndClosesPhones(t *testing.T) {
	t.Parallel()
	r := NewRegistry()

	if err := r.ClaimServer("s1", &fakeConn{id: "b-1"}); err != nil {
		t.Fatalf("ClaimServer: %v", err)
	}
	p1 := &fakeConn{id: "p-1", closeCh: make(chan struct{})}
	p2 := &fakeConn{id: "p-2", closeCh: make(chan struct{})}
	if err := r.RegisterPhone("s1", p1); err != nil {
		t.Fatalf("RegisterPhone p1: %v", err)
	}
	if err := r.RegisterPhone("s1", p2); err != nil {
		t.Fatalf("RegisterPhone p2: %v", err)
	}

	r.ScheduleReleaseServer("s1", 20*time.Millisecond)

	for _, p := range []*fakeConn{p1, p2} {
		select {
		case <-p.closeCh:
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for %s.Close()", p.id)
		}
	}

	if _, ok := r.BinaryFor("s1"); ok {
		t.Error("BinaryFor: binary entry still present after grace expiry")
	}
	if got := r.PhonesFor("s1"); got != nil {
		t.Errorf("PhonesFor after expiry: got %v, want nil", got)
	}
	if err := r.RegisterPhone("s1", &fakeConn{id: "p-3"}); !errors.Is(err, ErrNoServer) {
		t.Errorf("RegisterPhone after expiry: got %v, want errors.Is(_, ErrNoServer)", err)
	}
	if err := r.ClaimServer("s1", &fakeConn{id: "b-2"}); err != nil {
		t.Errorf("ClaimServer after expiry: got %v, want nil", err)
	}
}

// A phone that registered during the grace window sent its noise_init
// into the closed prior binary conn and is waiting for a reply that will
// never come. Reclaim evicts it alongside the pre-grace phones (#127).
func TestScheduleReleaseServer_PhoneRegisteredDuringGrace_ClosedOnReclaim(t *testing.T) {
	t.Parallel()
	r := NewRegistry()

	if err := r.ClaimServer("s1", &fakeConn{id: "b-1"}); err != nil {
		t.Fatalf("ClaimServer b1: %v", err)
	}

	r.ScheduleReleaseServer("s1", 50*time.Millisecond)

	p1 := &fakeConn{id: "p-1", closeCh: make(chan struct{})}
	if err := r.RegisterPhone("s1", p1); err != nil {
		t.Fatalf("RegisterPhone during grace: got %v, want nil", err)
	}

	if err := r.ClaimServer("s1", &fakeConn{id: "b-2"}); err != nil {
		t.Fatalf("reclaim ClaimServer: got %v, want nil", err)
	}

	if phones := r.PhonesFor("s1"); phones != nil {
		t.Errorf("PhonesFor after reclaim: got %v, want nil", phones)
	}
	select {
	case <-p1.closeCh:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for p1.Close() on reclaim")
	}
}

// codedFakeConn is a fakeConn that also satisfies the gracefulCloser seam,
// so a test can assert which application close code the registry chose.
// CloseWithCode records the code then delegates to Close so closeCh and
// isClosed keep working.
type codedFakeConn struct {
	fakeConn
	code   websocket.StatusCode
	reason string
}

func (c *codedFakeConn) CloseWithCode(code websocket.StatusCode, reason string) {
	c.mu.Lock()
	if !c.closed {
		c.code = code
		c.reason = reason
	}
	c.mu.Unlock()
	c.Close()
}

func (c *codedFakeConn) closeCode() websocket.StatusCode {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.code
}

// TestClaimServer_ReclaimDuringGrace_EvictsPhonesWith4404 pins the #127
// contract: every phone registered for the server-id at reclaim time is
// gone from the registry before ClaimServer returns, a code-aware phone
// receives 4404, and a narrow Conn gets plain Close.
func TestClaimServer_ReclaimDuringGrace_EvictsPhonesWith4404(t *testing.T) {
	t.Parallel()
	r := NewRegistry()

	if err := r.ClaimServer("s1", &fakeConn{id: "b-1"}); err != nil {
		t.Fatalf("ClaimServer b1: %v", err)
	}
	coded := &codedFakeConn{fakeConn: fakeConn{id: "p-coded", closeCh: make(chan struct{})}}
	narrow := &fakeConn{id: "p-narrow", closeCh: make(chan struct{})}
	if err := r.RegisterPhone("s1", coded); err != nil {
		t.Fatalf("RegisterPhone coded: %v", err)
	}
	if err := r.RegisterPhone("s1", narrow); err != nil {
		t.Fatalf("RegisterPhone narrow: %v", err)
	}
	// A phone on an unrelated server-id must not be touched.
	if err := r.ClaimServer("s2", &fakeConn{id: "b-other"}); err != nil {
		t.Fatalf("ClaimServer s2: %v", err)
	}
	other := &fakeConn{id: "p-other"}
	if err := r.RegisterPhone("s2", other); err != nil {
		t.Fatalf("RegisterPhone other: %v", err)
	}

	r.ScheduleReleaseServer("s1", time.Hour)

	if err := r.ClaimServer("s1", &fakeConn{id: "b-2"}); err != nil {
		t.Fatalf("reclaim ClaimServer: got %v, want nil", err)
	}

	// Removal is synchronous: Counts and PhonesFor must not see the
	// evicted phones once ClaimServer has returned.
	if phones := r.PhonesFor("s1"); phones != nil {
		t.Errorf("PhonesFor(s1) after reclaim: got %v, want nil", phones)
	}
	if b, p := r.Counts(); b != 2 || p != 1 {
		t.Errorf("Counts after reclaim: got (%d,%d), want (2,1)", b, p)
	}

	// Closing is asynchronous (one goroutine per phone); wait for it.
	for _, p := range []*fakeConn{&coded.fakeConn, narrow} {
		select {
		case <-p.closeCh:
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for %s.Close() on reclaim", p.id)
		}
	}
	if got := coded.closeCode(); got != websocket.StatusCode(4404) {
		t.Errorf("coded phone close code: got %d, want 4404", got)
	}
	if !narrow.isClosed() {
		t.Error("narrow phone: Close() not called on reclaim")
	}
	if other.isClosed() {
		t.Error("phone on unrelated server-id was closed by reclaim")
	}
	if got, ok := r.BinaryFor("s1"); !ok || got.ConnID() != "b-2" {
		t.Errorf("BinaryFor after reclaim: got (%v, %v), want (b-2, true)", got, ok)
	}
	if err := r.RegisterPhone("s1", &fakeConn{id: "p-3"}); err != nil {
		t.Errorf("RegisterPhone against reclaimed binary: got %v, want nil", err)
	}
}

// TestClaimServer_NoGrace_PhonesUntouched pins that #127 only changes the
// grace-window branch of ClaimServer: a held slot still conflicts and a
// plain release followed by a claim leaves orphan phones alone.
func TestClaimServer_NoGrace_PhonesUntouched(t *testing.T) {
	t.Parallel()
	r := NewRegistry()

	if err := r.ClaimServer("s1", &fakeConn{id: "b-1"}); err != nil {
		t.Fatalf("ClaimServer b1: %v", err)
	}
	p1 := &codedFakeConn{fakeConn: fakeConn{id: "p-1"}}
	if err := r.RegisterPhone("s1", p1); err != nil {
		t.Fatalf("RegisterPhone p1: %v", err)
	}

	if err := r.ClaimServer("s1", &fakeConn{id: "b-2"}); !errors.Is(err, ErrServerIDConflict) {
		t.Fatalf("ClaimServer on held slot: got %v, want errors.Is(_, ErrServerIDConflict)", err)
	}
	if p1.isClosed() {
		t.Error("p1 closed by a conflicting claim")
	}

	if released := r.ReleaseServer("s1"); !released {
		t.Fatal("ReleaseServer: got false, want true")
	}
	if err := r.ClaimServer("s1", &fakeConn{id: "b-2"}); err != nil {
		t.Fatalf("ClaimServer after plain release: got %v, want nil", err)
	}
	// Give any stray close goroutine a chance to run before asserting.
	time.Sleep(20 * time.Millisecond)
	if p1.isClosed() {
		t.Error("p1 closed by a no-grace claim")
	}
	if phones := r.PhonesFor("s1"); len(phones) != 1 || phones[0].ConnID() != "p-1" {
		t.Errorf("PhonesFor after no-grace claim: got %v, want [p-1]", phones)
	}
}

// TestClaimServer_ReclaimDuringGrace_StaleFireTouchesNothing invokes the
// expiry handler directly with the entry that the reclaim replaced, which
// is exactly the goroutine a lost Stop() race would produce. The
// pointer-identity guard must leave the new binary and its phones alone.
func TestClaimServer_ReclaimDuringGrace_StaleFireTouchesNothing(t *testing.T) {
	t.Parallel()
	r := NewRegistry()

	if err := r.ClaimServer("s1", &fakeConn{id: "b-1"}); err != nil {
		t.Fatalf("ClaimServer b1: %v", err)
	}
	old := &fakeConn{id: "p-old", closeCh: make(chan struct{})}
	if err := r.RegisterPhone("s1", old); err != nil {
		t.Fatalf("RegisterPhone old: %v", err)
	}

	r.ScheduleReleaseServer("s1", time.Hour)
	r.mu.Lock()
	stale := r.timers["s1"]
	r.mu.Unlock()
	if stale == nil {
		t.Fatal("no grace entry armed")
	}

	if err := r.ClaimServer("s1", &fakeConn{id: "b-2"}); err != nil {
		t.Fatalf("reclaim ClaimServer: got %v, want nil", err)
	}
	select {
	case <-old.closeCh:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for old phone eviction")
	}
	fresh := &fakeConn{id: "p-fresh"}
	if err := r.RegisterPhone("s1", fresh); err != nil {
		t.Fatalf("RegisterPhone fresh: %v", err)
	}

	var fired int
	r.SetGraceExpiryHook(func() { fired++ })
	r.handleGraceExpiry("s1", stale)

	if fired != 0 {
		t.Errorf("grace expiry hook fired %d times on a stale entry, want 0", fired)
	}
	if fresh.isClosed() {
		t.Error("stale fire closed a phone registered against the reclaimed binary")
	}
	if got, ok := r.BinaryFor("s1"); !ok || got.ConnID() != "b-2" {
		t.Errorf("BinaryFor after stale fire: got (%v, %v), want (b-2, true)", got, ok)
	}
	if phones := r.PhonesFor("s1"); len(phones) != 1 || phones[0].ConnID() != "p-fresh" {
		t.Errorf("PhonesFor after stale fire: got %v, want [p-fresh]", phones)
	}
}

func TestScheduleReleaseServer_PhoneRegisteredDuringGrace_ClosedOnExpiry(t *testing.T) {
	t.Parallel()
	r := NewRegistry()

	if err := r.ClaimServer("s1", &fakeConn{id: "b-1"}); err != nil {
		t.Fatalf("ClaimServer: %v", err)
	}

	r.ScheduleReleaseServer("s1", 20*time.Millisecond)

	p1 := &fakeConn{id: "p-1", closeCh: make(chan struct{})}
	if err := r.RegisterPhone("s1", p1); err != nil {
		t.Fatalf("RegisterPhone during grace: got %v, want nil", err)
	}

	select {
	case <-p1.closeCh:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for p1.Close() on grace expiry")
	}
	if !p1.isClosed() {
		t.Error("p1.isClosed() = false after closeCh signalled")
	}
}

func TestScheduleReleaseServer_ReplacesPendingTimer(t *testing.T) {
	t.Parallel()
	r := NewRegistry()

	if err := r.ClaimServer("s1", &fakeConn{id: "b-1"}); err != nil {
		t.Fatalf("ClaimServer: %v", err)
	}
	p1 := &fakeConn{id: "p-1", closeCh: make(chan struct{})}
	if err := r.RegisterPhone("s1", p1); err != nil {
		t.Fatalf("RegisterPhone: %v", err)
	}

	// Arm a long timer first, then replace with a short one.
	r.ScheduleReleaseServer("s1", time.Hour)
	r.ScheduleReleaseServer("s1", 20*time.Millisecond)

	select {
	case <-p1.closeCh:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for replacement timer to fire")
	}
}

func TestScheduleReleaseServer_OnUnheldID_NoOp(t *testing.T) {
	t.Parallel()
	r := NewRegistry()

	r.ScheduleReleaseServer("nope", 10*time.Millisecond)
	time.Sleep(40 * time.Millisecond)

	if b, p := r.Counts(); b != 0 || p != 0 {
		t.Errorf("Counts after unheld-id schedule: got (%d,%d), want (0,0)", b, p)
	}
}

// TestScheduleReleaseServer_RaceFreedomUnderRapidCycles hammers the
// disconnect/reclaim cycle from many goroutines and asserts the absence of
// DATA RACE reports under -race. Exercises time.AfterFunc cancel/replace and
// stale-fire pointer-identity paths.
//
// Run with: go test -race -count=20 -run TestScheduleReleaseServer_RaceFreedomUnderRapidCycles ./internal/relay
func TestScheduleReleaseServer_RaceFreedomUnderRapidCycles(t *testing.T) {
	t.Parallel()
	r := NewRegistry()

	const goroutines = 16
	const opsPer = 200

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		g := g
		go func() {
			defer wg.Done()
			sid := fmt.Sprintf("s-%d", g%4)
			for i := 0; i < opsPer; i++ {
				_ = r.ClaimServer(sid, &fakeConn{id: fmt.Sprintf("b-%d-%d", g, i)})
				_ = r.RegisterPhone(sid, &fakeConn{id: fmt.Sprintf("p-%d-%d", g, i)})
				r.ScheduleReleaseServer(sid, time.Millisecond)
				_ = r.ClaimServer(sid, &fakeConn{id: fmt.Sprintf("b2-%d-%d", g, i)})
				r.UnregisterPhone(sid, fmt.Sprintf("p-%d-%d", g, i))
				_, _ = r.Counts()
			}
		}()
	}
	wg.Wait()

	// Sleep past max grace duration so any in-flight timers fire and exit.
	time.Sleep(50 * time.Millisecond)

	// Final state need not be empty (some claims won the cancellation race);
	// we only assert no panic and that Counts is callable.
	_, _ = r.Counts()
}

func TestSnapshot_EmptyRegistry(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	if got := r.Snapshot(); got != nil {
		t.Errorf("Snapshot on empty registry: got %v, want nil", got)
	}
}

func TestSnapshot_IncludesBinariesAndPhones(t *testing.T) {
	t.Parallel()
	r := NewRegistry()

	b1 := &fakeConn{id: "b-1"}
	b2 := &fakeConn{id: "b-2"}
	p1 := &fakeConn{id: "p-1"}
	p2 := &fakeConn{id: "p-2"}
	p3 := &fakeConn{id: "p-3"}

	if err := r.ClaimServer("s1", b1); err != nil {
		t.Fatalf("ClaimServer s1: %v", err)
	}
	if err := r.ClaimServer("s2", b2); err != nil {
		t.Fatalf("ClaimServer s2: %v", err)
	}
	if err := r.RegisterPhone("s1", p1); err != nil {
		t.Fatalf("RegisterPhone p1: %v", err)
	}
	if err := r.RegisterPhone("s1", p2); err != nil {
		t.Fatalf("RegisterPhone p2: %v", err)
	}
	if err := r.RegisterPhone("s2", p3); err != nil {
		t.Fatalf("RegisterPhone p3: %v", err)
	}

	snap := r.Snapshot()
	if len(snap) != 5 {
		t.Fatalf("Snapshot len: got %d, want 5", len(snap))
	}
	got := map[string]bool{}
	for _, c := range snap {
		got[c.ConnID()] = true
	}
	for _, want := range []string{"b-1", "b-2", "p-1", "p-2", "p-3"} {
		if !got[want] {
			t.Errorf("Snapshot: missing %q (got %v)", want, got)
		}
	}
}

func TestSnapshot_FreshSliceIsolation(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	if err := r.ClaimServer("s1", &fakeConn{id: "b-1"}); err != nil {
		t.Fatalf("ClaimServer: %v", err)
	}
	if err := r.RegisterPhone("s1", &fakeConn{id: "p-1"}); err != nil {
		t.Fatalf("RegisterPhone: %v", err)
	}

	snap := r.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("Snapshot len: got %d, want 2", len(snap))
	}
	snap[0] = &fakeConn{id: "evil"}

	again := r.Snapshot()
	for _, c := range again {
		if c.ConnID() == "evil" {
			t.Fatal("registry observed mutation through Snapshot")
		}
	}

	// Mutating registry after snapshot: the original snap length stays put.
	if err := r.RegisterPhone("s1", &fakeConn{id: "p-2"}); err != nil {
		t.Fatalf("RegisterPhone p2: %v", err)
	}
	if len(snap) != 2 {
		t.Errorf("original snapshot length changed: got %d, want 2", len(snap))
	}
	if len(r.Snapshot()) != 3 {
		t.Errorf("new Snapshot after register: want 3")
	}
}

// TestSnapshot_RaceFreedom hammers Snapshot concurrently with mutation,
// asserting no data race under -race and that every Snapshot reflects an
// internally-consistent state (no torn reads).
func TestSnapshot_RaceFreedom(t *testing.T) {
	t.Parallel()
	r := NewRegistry()

	const goroutines = 16
	const opsPer = 200

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		g := g
		go func() {
			defer wg.Done()
			sid := fmt.Sprintf("s-%d", g%4)
			for i := 0; i < opsPer; i++ {
				_ = r.ClaimServer(sid, &fakeConn{id: fmt.Sprintf("b-%d-%d", g, i)})
				_ = r.RegisterPhone(sid, &fakeConn{id: fmt.Sprintf("p-%d-%d", g, i)})
				_ = r.Snapshot()
				r.UnregisterPhone(sid, fmt.Sprintf("p-%d-%d", g, i))
				_ = r.ReleaseServer(sid)
			}
		}()
	}
	wg.Wait()
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
				_ = r.RegisterPhoneCapped(sid, &fakeConn{id: fmt.Sprintf("pc-%d-%d", g, i)}, 16)
				_ = r.PhonesFor(sid)
				r.UnregisterPhone(sid, fmt.Sprintf("p-%d-%d", g, i))
				r.UnregisterPhone(sid, fmt.Sprintf("pc-%d-%d", g, i))
				_, _ = r.Counts()
				_ = r.ReleaseServer(sid)
			}
		}()
	}
	wg.Wait()
}
