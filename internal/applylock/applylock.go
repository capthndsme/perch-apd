// Package applylock is the access point's one write lock (Wi-Fi design
// protocol.md 3.4 and 6, decision D11).
//
// Three writers change the AP with a window that ends in a confirm or a
// rollback: the device-groups engine (groups.apply until groups.confirm or
// its deadline), the Wi-Fi config plane (wifi.config.apply until
// wifi.config.confirm, a rollback or its deadline) and an agent update (the
// install until the new binary is confirmed). Each of them restores only
// what it snapshotted itself, so their windows must never overlap: a writer
// takes the lock before it snapshots and holds it until its window closes;
// another writer asking in between is refused `busy` with the holder's
// reason (groups_pending, plane_pending, update_pending), and the controller
// retries later.
//
// The lock lives in memory, one per process (the daemon creates it once and
// hands it to every writer). A window that survives a restart is re-armed
// from the writer's own persisted state, and that writer takes the lock
// again at start, before any controller session exists.
package applylock

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// Holder names a writer.
type Holder string

// The writers.
const (
	// Groups is the device-groups engine (internal/groups).
	Groups Holder = "groups"
	// Plane is the Wi-Fi config plane (wifi.config.*).
	Plane Holder = "plane"
	// Update is an agent update (agent.update.install until its confirm).
	Update Holder = "update"
)

// Reason is the busy reason another writer reports while h holds the lock:
// "groups_pending", "plane_pending" or "update_pending".
func (h Holder) Reason() string { return string(h) + "_pending" }

// Info describes a hold.
type Info struct {
	Holder Holder `json:"holder"`
	// ID is the holder's own name for its window: "groups-<revision>", an
	// applyId, an update job id.
	ID    string    `json:"id"`
	Since time.Time `json:"since"`
}

// ErrBusy matches every BusyError (errors.Is).
var ErrBusy = errors.New("applylock: another write is pending")

// BusyError is TryAcquire's refusal: someone else holds the lock.
type BusyError struct {
	Info
}

func (e *BusyError) Error() string {
	return fmt.Sprintf("busy: %s %s holds the write lock since %s", e.Holder, e.ID,
		e.Since.UTC().Format(time.RFC3339))
}

// Reason is the protocol's busy reason for this refusal.
func (e *BusyError) Reason() string { return e.Holder.Reason() }

// Is makes errors.Is(err, ErrBusy) true.
func (e *BusyError) Is(target error) bool { return target == ErrBusy }

// Reason returns the busy reason err carries ("" when err is no BusyError).
func Reason(err error) string {
	var b *BusyError
	if errors.As(err, &b) {
		return b.Reason()
	}
	return ""
}

// Lock is the write lock. The zero value is not usable: call New.
type Lock struct {
	mu  sync.Mutex
	cur *Hold
	now func() time.Time
}

// New returns a free lock.
func New() *Lock { return &Lock{now: time.Now} }

// NewWithClock is New with a clock for Since (tests).
func NewWithClock(now func() time.Time) *Lock { return &Lock{now: now} }

// TryAcquire takes the lock for holder's window id, or returns a
// *BusyError naming the current holder. It never waits. A holder that
// already holds the lock is refused as well: one window per writer.
func (l *Lock) TryAcquire(holder Holder, id string) (*Hold, error) {
	if holder == "" {
		return nil, errors.New("applylock: empty holder")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cur != nil {
		return nil, &BusyError{Info: l.cur.info}
	}
	h := &Hold{l: l, info: Info{Holder: holder, ID: id, Since: l.now()}}
	l.cur = h
	return h, nil
}

// Current reports the hold, if any.
func (l *Lock) Current() (Info, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cur == nil {
		return Info{}, false
	}
	return l.cur.info, true
}

// Busy is "" while the lock is free, else the holder's busy reason. It is
// the check for a writer that must not start while a window is open (the
// updater's busy hook).
func (l *Lock) Busy() string {
	if info, ok := l.Current(); ok {
		return info.Holder.Reason()
	}
	return ""
}

// Hold is one writer's hold on the lock.
type Hold struct {
	l    *Lock
	info Info
}

// Info describes the hold.
func (h *Hold) Info() Info { return h.info }

// Active reports whether the hold still holds the lock.
func (h *Hold) Active() bool {
	if h == nil {
		return false
	}
	h.l.mu.Lock()
	defer h.l.mu.Unlock()
	return h.l.cur == h
}

// Release frees the lock. Safe to call more than once and on nil; a hold
// that no longer holds the lock releases nothing.
func (h *Hold) Release() {
	if h == nil {
		return
	}
	h.l.mu.Lock()
	defer h.l.mu.Unlock()
	if h.l.cur == h {
		h.l.cur = nil
	}
}
