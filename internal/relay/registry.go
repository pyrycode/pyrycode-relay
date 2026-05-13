package relay

import (
	"errors"
	"sync"
	"time"
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

// ClaimServer registers conn as the binary for serverID. First-claim-wins:
// a second concurrent caller for the same serverID receives
// ErrServerIDConflict and the conflicting caller's conn is left untouched
// (the registry does not Close it).
//
// Use ReleaseServer to free the slot. ClaimServer does NOT inspect or modify
// the phones slice for serverID; phones registered before a release remain
// in the map until they are explicitly UnregisterPhone'd by their owner, or
// removed by a higher-layer cleanup (#8 grace period).
func (r *Registry) ClaimServer(serverID string, conn Conn) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if entry, gracing := r.timers[serverID]; gracing {
		// Grace window in flight: cancel the pending release and atomically
		// replace the binary Conn. Phones registered during the grace window
		// stay; the new binary inherits them. The expiry handler defends
		// against a concurrent stale fire via pointer-identity on r.timers.
		entry.timer.Stop()
		delete(r.timers, serverID)
		r.binaries[serverID] = conn
		return nil
	}
	if _, ok := r.binaries[serverID]; ok {
		return ErrServerIDConflict
	}
	r.binaries[serverID] = conn
	return nil
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
//   - ClaimServer(serverID, conn) replaces the binary atomically and
//     cancels the pending timer (returns nil, NOT ErrServerIDConflict).
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

// Counts returns the number of binaries currently claimed and the total
// number of phone connections summed across all server-ids. For the health
// endpoint (#10). One call is internally consistent; two concurrent calls
// may observe different values.
func (r *Registry) Counts() (binaries, phones int) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	b := len(r.binaries)
	p := 0
	for _, s := range r.phones {
		p += len(s)
	}
	return b, p
}
