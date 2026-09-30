package wifiplane

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/capthndsme/perch-agentkit/openwrt/plane"
)

// wifi.health.changed: once per session when steady, then on a change that
// held for HealthDebounce; never while the controller does not watch, and
// radar checks do not count.
func TestHealthMonitor(t *testing.T) {
	e := newEnv(t, envOpts{})
	ctx := context.Background()
	t0 := time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC)
	tick := func(at time.Duration) bool { return e.svc.monitorTick(ctx, t0.Add(at)) }

	if tick(0) || tick(30*time.Second) {
		t.Fatal("sent while the controller does not watch (mode off)")
	}
	e.configure(plane.ModeObserve)
	if tick(60 * time.Second) {
		t.Fatal("sent on the first look")
	}
	if !tick(90 * time.Second) {
		t.Fatal("the steady state was not sent to the new session")
	}
	if tick(120 * time.Second) {
		t.Fatal("sent again without a change")
	}
	// A radar check is no change.
	e.wifi.set(func(w *fakeWifi) { w.hostapd["phy1-ap0"] = inCAC("Example", 100, 40) })
	if tick(150*time.Second) || tick(180*time.Second) {
		t.Fatal("a radar check was reported")
	}
	// A BSS goes down: reported once it held.
	e.wifi.set(func(w *fakeWifi) { w.hostapd["phy1-ap0"] = withStatus("DISABLED", "Example") })
	if tick(210 * time.Second) {
		t.Fatal("sent before the debounce")
	}
	if !tick(240 * time.Second) {
		t.Fatal("the BSS going down was not sent")
	}
	notes := e.notesOf(NotifyHealthChanged)
	var last HealthChanged
	json.Unmarshal(notes[len(notes)-1], &last)
	if len(notes) != 2 || last.OK || len(last.Problems) != 1 || last.Problems[0].Code != ProblemBSSDisabled || last.Problems[0].Section != "default_radio1" {
		t.Fatalf("notes %s", notes)
	}
	// A flap shorter than the debounce is not sent.
	e.wifi.set(func(w *fakeWifi) { w.hostapd["phy1-ap0"] = beacon("Example", 100) })
	tick(270 * time.Second)
	e.wifi.set(func(w *fakeWifi) { w.hostapd["phy1-ap0"] = withStatus("DISABLED", "Example") })
	if tick(300*time.Second) || tick(330*time.Second) {
		t.Fatal("a flap was sent")
	}
	// A new session gets the state again.
	e.mu.Lock()
	e.gen++
	e.mu.Unlock()
	tick(360 * time.Second)
	if !tick(390 * time.Second) {
		t.Fatal("the new session did not get the state")
	}
}
