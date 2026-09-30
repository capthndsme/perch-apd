package wifiplane

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/capthndsme/perch-agentkit/openwrt/plane"
)

// The health monitor (Wi-Fi design protocol.md 7): outside applies, while
// the controller observes or manages this AP, the Wi-Fi's health is looked
// at every MonitorInterval, cheaply (nl80211 first, hostapd only for a BSS
// that does not beacon), and `wifi.health.changed` is sent when a radio or
// an expected BSS went down or came back and stayed so for HealthDebounce.
// Every session gets the state once, as soon as it is steady. Radar checks
// and channel scans are left out (they end on their own; the controller's
// alerts judge how long a BSS was down).

// HealthDebounce is how long a new state must hold before it is sent.
const HealthDebounce = 30 * time.Second

// HealthChanged is the wifi.health.changed notification.
type HealthChanged struct {
	At       string    `json:"at"`
	OK       bool      `json:"ok"`
	Problems []Problem `json:"problems"`
}

type monitorState struct {
	gen       uint64
	have      bool
	candidate string
	since     time.Time
	sent      bool
	reported  string
}

func (s *Service) monitor(ctx context.Context) {
	t := time.NewTicker(s.o.MonitorInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.monitorTick(ctx, time.Now())
		}
	}
}

// lasting are a report's problems that count and do not end on their own.
func lasting(rep Report) ([]Problem, string) {
	out := []Problem{}
	var keys []string
	for _, p := range rep.Problems {
		if p.Preexisting || p.Code == ProblemCACRunning || p.Code == ProblemACSRunning {
			continue
		}
		out = append(out, p)
		keys = append(keys, p.Code+"@"+p.Section)
	}
	sort.Strings(keys)
	return out, strings.Join(keys, ",")
}

// monitorTick looks once; it reports whether it sent a notification.
func (s *Service) monitorTick(ctx context.Context, now time.Time) bool {
	idle := s.Access() != plane.AccessNone && s.p.Mode() != plane.ModeOff &&
		s.p.ApplyState().State == plane.StateIdle && (s.o.Groups == nil || !s.o.Groups.Owned().Pending)
	if !idle {
		// Nothing to watch, or a window is open (its own check runs): start
		// over afterwards.
		s.mu.Lock()
		s.mon.have = false
		s.mu.Unlock()
		return false
	}
	problems, sig := lasting(s.healthNow(ctx, false))
	gen := s.sessionRef(ctx).Gen
	s.mu.Lock()
	m := &s.mon
	if m.gen != gen {
		*m = monitorState{gen: gen}
	}
	if !m.have || sig != m.candidate {
		m.have, m.candidate, m.since = true, sig, now
		s.mu.Unlock()
		return false
	}
	// A second of slack: the ticker's ticks are never exactly an interval
	// apart.
	if (m.sent && sig == m.reported) || now.Sub(m.since) < HealthDebounce-time.Second {
		s.mu.Unlock()
		return false
	}
	s.mu.Unlock()
	if !s.notify(NotifyHealthChanged, HealthChanged{At: now.UTC().Format(time.RFC3339), OK: sig == "", Problems: problems}) {
		return false
	}
	s.mu.Lock()
	if s.mon.gen == gen {
		s.mon.sent, s.mon.reported = true, sig
	}
	s.mu.Unlock()
	return true
}
