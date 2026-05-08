package relay

import (
	"errors"
	"sync"
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
}

// NewRegistry constructs an empty Registry.
func NewRegistry() *Registry {
	return &Registry{
		binaries: make(map[string]Conn),
		phones:   make(map[string][]Conn),
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

// RegisterPhone appends conn to the phones slice for serverID. Returns
// ErrNoServer if no binary currently holds serverID. The registry does not
// deduplicate by ConnID — the caller is responsible for not registering the
// same phone twice.
func (r *Registry) RegisterPhone(serverID string, conn Conn) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.binaries[serverID]; !ok {
		return ErrNoServer
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
