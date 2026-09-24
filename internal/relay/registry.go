package relay

import (
	"errors"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Sentinel errors returned by Registry methods. Callers branch on these with
// errors.Is. The mapping to WebSocket close codes is informational; the
// registry has no knowledge of WebSockets and the WS upgrade handlers do
// the translation.
var (
	// ErrServerIDConflict is returned by ClaimServer when serverID is
	// already held by another binary. Maps to WS close code 4409.
	ErrServerIDConflict = errors.New("relay: server-id already claimed")

	// ErrNoServer is returned by RegisterPhone when no binary holds the
	// requested serverID. Maps to WS close code 4404.
	ErrNoServer = errors.New("relay: no binary for server-id")

	// ErrPhonesAtCap is returned by RegisterPhoneCapped when
	// len(phones[serverID]) already equals the max-phones policy value
	// passed by the caller. The error is returned BEFORE the slice is
	// mutated. Maps to WS close code 4429.
	ErrPhonesAtCap = errors.New("relay: phones at cap for server-id")
)

// Conn is the registry's view of a WebSocket connection. Real implementations
// land in the /v1/server (#4) and /v1/client (#5) upgrade tickets; tests use
// mocks.
//
// All three methods MUST be safe to call from any goroutine. ConnID and Send
// MUST NOT block on locks the registry itself takes — the registry never
// invokes Send or Close while holding its own lock, but it does call ConnID
// under the lock during UnregisterPhone, so ConnID must be a non-blocking
// getter.
type Conn interface {
	// ConnID returns the relay-assigned connection identifier. Stable for
	// the lifetime of the connection. The registry treats it as an opaque
	// key for slice lookups and calls it under its internal lock, so the
	// implementation must not block.
	ConnID() string

	// Send delivers a fully-formed wire message. Implementations are
	// responsible for serialising writes to the underlying socket. Errors
	// are returned to the caller; the registry does not interpret them.
	Send(msg []byte) error

	// Close terminates the connection. Idempotent. Safe to call
	// concurrently with Send (the implementation must handle the race).
	Close()
}

// Registry maps server-ids to a single binary connection (1:1) and to a list
// of phone connections (1:N). All methods are safe to call concurrently.
//
// The registry holds Conn handles by reference and never inspects their
// internal state. It does not call Send or Close on a Conn while holding its
// own lock — broadcast / close fan-out is the caller's responsibility,
// performed against a PhonesFor snapshot.
type Registry struct {
	mu       sync.RWMutex
	binaries map[string]Conn
	phones   map[string][]Conn
	timers   map[string]*graceEntry

	// onPhoneForwarded is invoked by StartPhoneForwarder after each
	// successful binary.Send. Nil = no-op. Set once at boot via
	// SetForwarderHooks before either listener starts serving; concurrent
	// mutation during serving is undefined. Called outside any registry
	// lock; the hook body MUST NOT acquire r.mu (deadlock risk). The
	// production hook body (metrics_forward.go) is a pure
	// prometheus.CounterVec.WithLabelValues(...).Inc call — atomic per
	// client_golang's contract, no relay-side lock involved.
	onPhoneForwarded func()
	// onBinaryForwarded mirrors onPhoneForwarded for StartBinaryForwarder's
	// per-successful-phone.Send path. Same lifecycle and locking rules.
	onBinaryForwarded func()
	// onGraceExpiry is invoked by handleGraceExpiry after the
	// pointer-identity guard's success branch and the phone-close fan-out.
	// Stale fires do not reach this site. Set once at boot via
	// SetGraceExpiryHook; same locking rules as the forwarder hooks.
	onGraceExpiry func()
	// pushWaker sends the FCM wakes binaries ask for. Nil = push off. Set
	// once at boot via SetPushWaker; same lifecycle rules as the hooks.
	pushWaker *PushWaker
}

// graceEntry wraps a pending grace-period timer. Its pointer identity
// (NOT *time.Timer's) is what the expiry handler compares against the map
// to detect stale fires. Capturing the entry pointer in the AfterFunc
// closure — rather than the *time.Timer assigned after AfterFunc returns —
// avoids the race-detector flag on a self-referential local var.
type graceEntry struct {
	timer *time.Timer
}

// NewRegistry constructs an empty Registry.
func NewRegistry() *Registry {
	return &Registry{
		binaries: make(map[string]Conn),
		phones:   make(map[string][]Conn),
		timers:   make(map[string]*graceEntry),
	}
}

// SetForwarderHooks installs the two per-frame increment hooks. Either (or
// both) may be nil for no-op. Call once at boot before any forwarder
// goroutine runs; concurrent calls during serving are undefined. Hook
// bodies MUST NOT acquire r.mu — deadlock risk.
func (r *Registry) SetForwarderHooks(phone, binary func()) {
	r.onPhoneForwarded = phone
	r.onBinaryForwarded = binary
}

// SetPushWaker installs the waker StartBinaryForwarder hands push_wake
// envelopes to. Nil means push is off. Call once at boot before any
// forwarder goroutine runs; concurrent calls during serving are undefined.
func (r *Registry) SetPushWaker(w *PushWaker) {
	r.pushWaker = w
}

// SetGraceExpiryHook installs the eviction-increment hook. May be nil for
// no-op. Call once at boot before any ScheduleReleaseServer call; concurrent
// calls during serving are undefined. The hook body MUST NOT acquire r.mu.
func (r *Registry) SetGraceExpiryHook(grace func()) {
	r.onGraceExpiry = grace
}

// reclaimCloseCode is the WebSocket close code sent to every phone that
// was registered for a server-id when a binary reclaims that slot during
// its grace window (#127). It is the same 4404 that /v1/client emits when
// no binary holds the slot, and every client already treats it as
// "relay reachable, daemon absent: retry". IANA 1012 Service Restart was
// rejected because no client classifies it.
const reclaimCloseCode = websocket.StatusCode(4404)

// reclaimCloseReason accompanies reclaimCloseCode on the wire.
const reclaimCloseReason = "binary reclaimed server-id"

// ClaimServer registers conn as the binary for serverID. First-claim-wins:
// a second concurrent caller for the same serverID receives
// ErrServerIDConflict and the conflicting caller's conn is left untouched
// (the registry does not Close it).
//
// Use ReleaseServer to free the slot. Outside a grace window ClaimServer
// does NOT inspect or modify the phones slice for serverID; phones
// registered before a plain release remain in the map until they are
// explicitly UnregisterPhone'd by their owner.
//
// During a pending grace window (ScheduleReleaseServer) ClaimServer
// succeeds, replaces the binary Conn, cancels the timer, and evicts every
// phone registered for serverID: the phones are removed from the map under
// the lock, so Counts and PhonesFor never report them after this call
// returns, and each one is closed with reclaimCloseCode outside the lock
// (one goroutine per phone, as Shutdown does, because a close can block on
// the peer's close handshake). Under Mobile Protocol v2 a restarted daemon
// has no cipher state for inherited conns, so handing phones across would
// strand them on a fatal 4421; a 4404 makes them re-dial and re-handshake
// against the live binary. See ADR-0006 (amended for #127).
func (r *Registry) ClaimServer(serverID string, conn Conn) error {
	r.mu.Lock()
	if _, gracing := r.timers[serverID]; !gracing {
		defer r.mu.Unlock()
		if _, ok := r.binaries[serverID]; ok {
			return ErrServerIDConflict
		}
		r.binaries[serverID] = conn
		return nil
	}
	// Grace window in flight: the reclaim path. The expiry handler
	// defends against a concurrent stale fire via pointer-identity on
	// r.timers.
	evicted := r.replaceBinaryLocked(serverID, conn)
	r.mu.Unlock()

	evictPhones(evicted)
	return nil
}

// TakeoverServer hands serverID to conn when incumbent, the binary a
// caller found unresponsive to a liveness probe (#112), still holds it.
// It is identity-scoped: if any other Conn holds the slot by the time the
// lock is taken (a concurrent reclaim, or another prober's takeover), it
// returns ErrServerIDConflict and touches nothing. A nil incumbent means
// the slot was empty when the caller looked, and is refused the same way
// once anyone holds it.
//
// On success the semantics are the grace-window reclaim of ClaimServer:
// any pending grace timer is cancelled, the binary Conn is replaced, and
// every phone registered for serverID is removed under the lock and closed
// with reclaimCloseCode outside it (ADR-0006). The registry does not close
// incumbent; the caller owns that, as it owns the close code.
func (r *Registry) TakeoverServer(serverID string, incumbent, conn Conn) error {
	r.mu.Lock()
	if cur, held := r.binaries[serverID]; held && cur != incumbent {
		r.mu.Unlock()
		return ErrServerIDConflict
	}
	evicted := r.replaceBinaryLocked(serverID, conn)
	r.mu.Unlock()

	evictPhones(evicted)
	return nil
}

// replaceBinaryLocked is the reclaim semantics shared by ClaimServer's
// grace branch and TakeoverServer: cancel any pending release, install
// conn, and take the phones slice out of the registry. The caller holds
// r.mu and passes the returned phones to evictPhones after unlocking.
// Deleting the timer entry here is what makes a stale expiry of the
// replaced binary's timer no-op at the pointer-identity guard in
// handleGraceExpiry.
func (r *Registry) replaceBinaryLocked(serverID string, conn Conn) []Conn {
	if entry, ok := r.timers[serverID]; ok {
		entry.timer.Stop()
		delete(r.timers, serverID)
	}
	r.binaries[serverID] = conn
	evicted := r.phones[serverID]
	delete(r.phones, serverID)
	return evicted
}

// evictPhones closes each phone with reclaimCloseCode, one goroutine per
// phone as Shutdown does, because a close can block on the peer's close
// handshake. Called outside r.mu.
func evictPhones(phones []Conn) {
	for _, p := range phones {
		go closeWithCode(p, reclaimCloseCode, reclaimCloseReason)
	}
}

// closeWithCode closes c with the given application close code when the
// Conn supports it, and falls back to plain Close otherwise. Mirrors the
// gracefulCloser type-assertion seam in shutdown.go: *WSConn takes the
// code-aware path, narrow test fakes take Close.
func closeWithCode(c Conn, code websocket.StatusCode, reason string) {
	if gc, ok := c.(gracefulCloser); ok {
		gc.CloseWithCode(code, reason)
		return
	}
	c.Close()
}

// ReleaseServer removes the binary entry for serverID. Returns true if a
// binary held the slot, false otherwise. Does NOT close the connection;
// the caller owns conn lifecycle. Does NOT touch the phones slice — orphan
// phones survive a release until their owner unregisters them, so the grace
// period (#8) can keep phones reachable across a binary disconnect window.
func (r *Registry) ReleaseServer(serverID string) (released bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.binaries[serverID]; !ok {
		return false
	}
	delete(r.binaries, serverID)
	return true
}

// ScheduleReleaseServer arms (or replaces) a deferred release of serverID
// after d. The grace window is the reclaim path: until the timer fires,
//
//   - BinaryFor(serverID) continues to return the (now-closed) binary Conn —
//     callers that Send through it observe whatever error the underlying
//     impl returns.
//   - RegisterPhone(serverID, conn) continues to succeed.
//   - ClaimServer(serverID, conn) replaces the binary atomically, cancels
//     the pending timer (returns nil, NOT ErrServerIDConflict), and evicts
//     every phone registered for serverID with close code 4404 (#127).
//
// If a timer is already pending for serverID, it is stopped and replaced
// (last call wins). Calling ScheduleReleaseServer for a serverID with no
// binary holding it arms the timer anyway; on expiry it removes only
// whatever state happens to be present (defensive, matches the no-op
// semantics of ReleaseServer on an unheld id).
//
// On expiry (no reclaim within d): the binary entry for serverID is
// removed, every phone currently registered for serverID is removed from
// the registry and Close() is invoked on it. Close calls happen OUTSIDE
// the registry lock (snapshot-and-iterate, same shape as PhonesFor).
//
// This method does not block on d; the timer fires on Go's runtime
// goroutine for time.AfterFunc.
func (r *Registry) ScheduleReleaseServer(serverID string, d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.armGraceLocked(serverID, d)
}

// ScheduleReleaseServerIfHeld is ScheduleReleaseServer scoped to conn: it
// arms the grace timer only while conn holds serverID, and returns whether
// it did. A binary displaced by TakeoverServer (#112) runs its handler's
// disconnect path after the new binary holds the slot; scoping the release
// to its own Conn stops it arming a timer whose expiry would delete the new
// binary. Expiry needs no scoping of its own: every replacement of the
// binary deletes the pending timer under the same lock (replaceBinaryLocked).
func (r *Registry) ScheduleReleaseServerIfHeld(serverID string, conn Conn, d time.Duration) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cur, held := r.binaries[serverID]; !held || cur != conn {
		return false
	}
	r.armGraceLocked(serverID, d)
	return true
}

// armGraceLocked arms (or replaces) the grace timer for serverID. The
// caller holds r.mu.
func (r *Registry) armGraceLocked(serverID string, d time.Duration) {
	if existing, ok := r.timers[serverID]; ok {
		existing.timer.Stop()
		delete(r.timers, serverID)
	}

	entry := &graceEntry{}
	r.timers[serverID] = entry
	entry.timer = time.AfterFunc(d, func() { r.handleGraceExpiry(serverID, entry) })
}

// handleGraceExpiry runs on the time.AfterFunc goroutine when the grace
// timer for serverID fires. The pointer-identity check on r.timers[serverID]
// closes the one race the lock cannot: a timer whose body has already
// started executing cannot be Stop()'d, but is queued behind us on the
// lock. By the time it acquires the lock, ClaimServer or a later
// ScheduleReleaseServer may have replaced the entry — in which case self
// no longer matches and we no-op.
func (r *Registry) handleGraceExpiry(serverID string, self *graceEntry) {
	r.mu.Lock()
	if r.timers[serverID] != self {
		r.mu.Unlock()
		return
	}
	delete(r.timers, serverID)
	delete(r.binaries, serverID)
	snapshot := r.phones[serverID]
	delete(r.phones, serverID)
	r.mu.Unlock()

	for _, p := range snapshot {
		p.Close()
	}

	if h := r.onGraceExpiry; h != nil {
		h()
	}
}

// RegisterPhone appends conn to the phones slice for serverID. Returns
// ErrNoServer if no binary currently holds serverID. The registry does not
// deduplicate by ConnID — the caller is responsible for not registering the
// same phone twice.
//
// Equivalent to RegisterPhoneCapped(serverID, conn, 0): no cap. The dedicated
// no-cap entry point exists for test fixtures and callers that explicitly do
// not want the cap-aware contract.
func (r *Registry) RegisterPhone(serverID string, conn Conn) error {
	return r.RegisterPhoneCapped(serverID, conn, 0)
}

// RegisterPhoneCapped appends conn to the phones slice for serverID,
// returning ErrPhonesAtCap when len(phones[serverID]) already equals max.
// The cap check and the slice append run under one write lock, so two
// concurrent callers at max-1 cannot both succeed (race shape mirrors
// ClaimServer's first-claim-wins).
//
// max <= 0 disables the cap and the method behaves as the legacy
// RegisterPhone. ErrNoServer takes precedence over ErrPhonesAtCap when both
// would apply — the no-server check runs first.
//
// The mapping ErrPhonesAtCap → WS close code 4429 is informational; the
// registry does not interpret close codes. /v1/client emits 4429 in the
// stillborn-WSConn window.
func (r *Registry) RegisterPhoneCapped(serverID string, conn Conn, max int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.binaries[serverID]; !ok {
		return ErrNoServer
	}
	if max > 0 && len(r.phones[serverID]) >= max {
		return ErrPhonesAtCap
	}
	r.phones[serverID] = append(r.phones[serverID], conn)
	return nil
}

// UnregisterPhone removes the phone whose ConnID equals connID from the
// slice for serverID. No-op if no matching phone is present (including when
// no slice exists for serverID). Does NOT close the connection; the caller
// owns conn lifecycle.
//
// When the slice becomes empty as a result, the entry is deleted from the
// phones map so Counts and PhonesFor see no orphaned empty slices.
func (r *Registry) UnregisterPhone(serverID string, connID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	slice, ok := r.phones[serverID]
	if !ok {
		return
	}
	for i, c := range slice {
		if c.ConnID() != connID {
			continue
		}
		slice[i] = slice[len(slice)-1]
		slice[len(slice)-1] = nil
		slice = slice[:len(slice)-1]
		if len(slice) == 0 {
			delete(r.phones, serverID)
		} else {
			r.phones[serverID] = slice
		}
		return
	}
}

// BinaryFor returns the binary holding serverID, if any.
func (r *Registry) BinaryFor(serverID string) (Conn, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c, ok := r.binaries[serverID]
	return c, ok
}

// PhonesFor returns a snapshot of the phones registered for serverID. The
// returned slice is freshly allocated; the caller may iterate, append, or
// otherwise mutate it without affecting the registry's internal state or
// holding any registry lock. Returns nil for an unknown serverID or when no
// phones are registered.
//
// The Conn handles inside the slice are the same references the registry
// holds; calling Send or Close on them affects the live connection.
func (r *Registry) PhonesFor(serverID string) []Conn {
	r.mu.RLock()
	defer r.mu.RUnlock()
	src := r.phones[serverID]
	if len(src) == 0 {
		return nil
	}
	out := make([]Conn, len(src))
	copy(out, src)
	return out
}

// Snapshot returns a freshly-allocated slice of every Conn currently
// registered as a binary or a phone. The caller may iterate, append, or
// otherwise mutate the slice without affecting the registry's internal
// state or holding any registry lock. Returns nil when no conns are
// registered. Used by the graceful-shutdown path (#31) to fan close
// frames out without exposing internal maps.
//
// Order is unspecified (map-iteration order); callers don't care. The
// Conn handles inside the slice are the same references the registry
// holds; calling Close on them affects the live connection.
func (r *Registry) Snapshot() []Conn {
	r.mu.RLock()
	defer r.mu.RUnlock()
	total := len(r.binaries)
	for _, s := range r.phones {
		total += len(s)
	}
	if total == 0 {
		return nil
	}
	out := make([]Conn, 0, total)
	for _, c := range r.binaries {
		out = append(out, c)
	}
	for _, s := range r.phones {
		out = append(out, s...)
	}
	return out
}

// Counts returns the number of binaries currently connected and the total
// number of phone connections summed across all server-ids. For the health
// endpoint (#10) and the connection gauges. A binary in its grace window
// (a pending ScheduleReleaseServer timer) still holds its slot but is not
// connected, so it is left out (#115); its phones are still counted. One
// call is internally consistent; two concurrent calls may observe
// different values.
func (r *Registry) Counts() (binaries, phones int) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	b := len(r.binaries)
	for id := range r.timers {
		if _, held := r.binaries[id]; held {
			b--
		}
	}
	p := 0
	for _, s := range r.phones {
		p += len(s)
	}
	return b, p
}
