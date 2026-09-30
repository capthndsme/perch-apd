package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"github.com/capthndsme/perch-agentkit/link"
)

// What the Wi-Fi config plane needs from the session loop (Wi-Fi design
// protocol.md 3.4): every session has a number and a signing challenge of
// its own, an apply drops the session and dials a fresh one at once (the
// controller confirms on that one, which proves the AP still reaches it),
// and while a confirm is due, or right after a rollback, a lost session is
// redialed every FastRedial instead of backing off.

// FastRedial is the pause between dials while the plane wants a session.
const FastRedial = 2 * time.Second

type sessionInfo struct {
	gen       uint64
	challenge string
}

// newChallenge is a session's signing challenge: 32 hex digits.
func newChallenge() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(b[:])
}

// beginSession numbers the session about to be dialed.
func (a *Agent) beginSession() {
	a.mu.Lock()
	a.gen++
	a.cur = sessionInfo{gen: a.gen, challenge: newChallenge()}
	a.redialNow = false
	a.mu.Unlock()
}

// endSession forgets the session and tells the plane.
func (a *Agent) endSession() {
	a.mu.Lock()
	a.cur = sessionInfo{}
	a.mu.Unlock()
	if a.onEnd != nil {
		a.onEnd()
	}
}

// SessionRef names the session a request came in on: its number in this
// process (from 1) and its signing challenge. A request's context is its
// session's, which ends with it; an ended one is 0 and "".
func (a *Agent) SessionRef(ctx context.Context) (gen uint64, challenge string) {
	if ctx != nil && ctx.Err() != nil {
		return 0, ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cur.gen, a.cur.challenge
}

// Send sends a notification on the current session; false when there is
// none or it could not be written.
func (a *Agent) Send(method string, params any) bool {
	a.mu.Lock()
	s := a.sess
	a.mu.Unlock()
	if s == nil {
		return false
	}
	if err := s.Notify(method, params); err != nil {
		a.log.Debug("notification not sent", "method", method, "err", err)
		return false
	}
	return true
}

// Reconnect closes the current session (1000 with reason) and dials a new
// one at once. Without a session it does nothing: the loop is dialing.
func (a *Agent) Reconnect(reason string) {
	if len(reason) > 120 {
		reason = reason[:120] // a close reason is at most 123 bytes
	}
	a.mu.Lock()
	s := a.sess
	if s != nil {
		a.redialNow = true
	}
	a.mu.Unlock()
	if s == nil {
		return
	}
	a.log.Info("closing the controller session to dial a fresh one", "reason", reason)
	// Close waits for the controller's close frame; the caller goes on.
	go s.Close(websocket.StatusNormalClosure, reason)
}

// redialDelay shortens the wait after a session ended: none after
// Reconnect, at most FastRedial while the plane wants a session. A refusal
// the backoff does not decide (401, 404, a Retry-After) keeps its delay.
func (a *Agent) redialDelay(err error, wait time.Duration) time.Duration {
	a.mu.Lock()
	now := a.redialNow
	a.redialNow = false
	a.mu.Unlock()
	var se *link.StatusError
	if errors.As(err, &se) && (se.Status < http.StatusInternalServerError || se.RetryAfter > 0) {
		return wait
	}
	if now {
		return 0
	}
	if a.redialFast != nil && a.redialFast() && wait > FastRedial {
		return FastRedial
	}
	return wait
}
