package wifiplane

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/capthndsme/perch-agentkit/openwrt/plane"
	"github.com/capthndsme/perch-agentkit/openwrt/uci"
)

func mustParse(t *testing.T, text string) *uci.Config {
	t.Helper()
	c, err := uci.Parse("wireless", []byte(text))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Expected: enabled AP interfaces on enabled radios, plus the controller's
// list (the union); stations, mesh points and interfaces on a missing or
// disabled radio are not BSSes to wait for.
func TestExpectations(t *testing.T) {
	cfg := mustParse(t, tWireless+`
config wifi-device 'radio2'
	option band '2g'
	option disabled '1'

config wifi-iface 'on_disabled_radio'
	option device 'radio2'
	option mode 'ap'
	option ssid 'Off'

config wifi-iface 'on_missing_radio'
	option device 'radio3'
	option mode 'ap'
	option ssid 'Gone'

config wifi-iface 'uplink'
	option device 'radio1'
	option mode 'sta'
	option ssid 'Upstream'

config wifi-iface 'switched_off'
	option device 'radio0'
	option ssid 'Paused'
	option disabled '1'
`)
	e := expectFrom(cfg, Expect{})
	if got := strings.Join(sortedKeys(boolKeys(e.bss)), ","); got != "default_radio0,default_radio1" {
		t.Fatalf("bss %s", got)
	}
	if got := strings.Join(sortedKeys(e.radios), ","); got != "radio0,radio1" {
		t.Fatalf("radios %s", got)
	}
	if !e.want()["switched_off"] || e.want()["uplink"] {
		t.Fatalf("want %v", e.want())
	}
	// The controller's list wins: a BSS it expects must run.
	e = expectFrom(cfg, Expect{BSS: []string{"switched_off", "perch_n1_radio1"}, Radios: []string{"radio2"}})
	if _, ok := e.bss["switched_off"]; !ok || !e.radios["radio2"] || e.bss["switched_off"].ssid != "Paused" {
		t.Fatalf("union %+v", e)
	}
	if _, ok := e.bss["perch_n1_radio1"]; !ok {
		t.Fatal("a BSS only the controller names")
	}
}

func boolKeys(m map[string]expBSS) map[string]bool {
	out := map[string]bool{}
	for k := range m {
		out[k] = true
	}
	return out
}

func TestEvaluate(t *testing.T) {
	cfg := mustParse(t, tWireless)
	e := expectFrom(cfg, Expect{})
	run := func(radios wirelessStatus, bss map[string]*bssRun) (Report, bool, time.Duration) {
		return evaluate(time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC), runState{radios: radios, bss: bss}, e, before{})
	}
	up := func(ifaces ...string) wirelessStatus {
		st := wirelessStatus{}
		for _, r := range []string{"radio0", "radio1"} {
			v := st[r]
			v.Up = true
			st[r] = v
		}
		return st
	}
	ok := func(radio, ifname, ssid string) *bssRun {
		return &bssRun{radio: radio, ifname: ifname, reachable: true, st: hostapdStatus{Status: "ENABLED", SSID: ssid, Channel: 1}}
	}
	rep, good, _ := run(up(), map[string]*bssRun{"default_radio0": ok("radio0", "phy0-ap0", "Example"), "default_radio1": ok("radio1", "phy1-ap0", "Example")})
	if !good || !rep.OK || rep.Pending || len(rep.Problems) != 0 || len(rep.BSS) != 2 || len(rep.Radios) != 2 {
		t.Fatalf("healthy %+v", rep)
	}
	// The wrong SSID, a BSS missing, a radio down.
	st := up()
	r1 := st["radio1"]
	r1.Up = false
	st["radio1"] = r1
	rep, good, _ = run(st, map[string]*bssRun{"default_radio0": ok("radio0", "phy0-ap0", "Old name")})
	codes := []string{}
	for _, p := range rep.Problems {
		codes = append(codes, p.Code+"@"+p.Section)
	}
	if good || rep.OK || rep.Pending || strings.Join(codes, ",") != "radio_down@radio1,ssid_mismatch@default_radio0,bss_missing@default_radio1" {
		t.Fatalf("broken %v %+v", codes, rep)
	}
	// A channel scan and a starting BSS are pending; hostapd not answering
	// is not.
	rep, good, extend := run(up(), map[string]*bssRun{
		"default_radio0": {radio: "radio0", ifname: "phy0-ap0", reachable: true, st: hostapdStatus{Status: "ACS"}},
		"default_radio1": {radio: "radio1", ifname: "phy1-ap0", reachable: true, st: hostapdStatus{Status: "COUNTRY_UPDATE"}},
	})
	if good || !rep.Pending || extend != 0 {
		t.Fatalf("scanning %+v", rep)
	}
	rep, _, _ = run(up(), map[string]*bssRun{
		"default_radio0": ok("radio0", "phy0-ap0", "Example"),
		"default_radio1": {radio: "radio1", ifname: "phy1-ap0"},
	})
	if rep.Pending || rep.Problems[0].Code != ProblemHostapdUnreachable {
		t.Fatalf("unreachable %+v", rep)
	}
	// Status unreadable: nothing can be judged.
	rep, good, _ = evaluate(time.Now(), runState{err: context.DeadlineExceeded}, e, before{})
	if good || rep.Problems[0].Code != ProblemStatusUnreadable {
		t.Fatalf("no status %+v", rep)
	}
}

// wifi.health outside an apply: the committed config against what runs,
// through the service (hostapd asked for each AP interface only).
func TestHealthNow(t *testing.T) {
	e := newEnv(t, envOpts{})
	e.wifi.set(func(w *fakeWifi) {
		w.radios["radio1"].ifaces["uplink"] = "phy1-sta0" // not an AP: never asked
		w.hostapd["phy1-ap0"] = inCAC("Example", 100, 12)
	})
	res, err := e.call(MethodHealth, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(res)
	var rep Report
	json.Unmarshal(b, &rep)
	if rep.OK || !rep.Pending || len(rep.Problems) != 1 || rep.Problems[0].Code != ProblemCACRunning ||
		!strings.Contains(rep.Problems[0].Message, "12 s left") {
		t.Fatalf("health %s", b)
	}
	var radio1 RadioHealth
	for _, r := range rep.Radios {
		if r.Section == "radio1" {
			radio1 = r
		}
	}
	if radio1.DFS == nil || !radio1.DFS.CACActive || radio1.DFS.CACSecondsLeft != 12 || radio1.Channel != 100 {
		t.Fatalf("radio1 %+v", radio1)
	}
	e.wifi.mu.Lock()
	defer e.wifi.mu.Unlock()
	for _, c := range e.wifi.calls {
		if strings.Contains(c, "phy1-sta0") {
			t.Fatalf("asked hostapd about a station: %s", c)
		}
	}
}

// The settle waits while netifd still sets a radio up.
func TestSettleWaitsForPendingRadios(t *testing.T) {
	e := newEnv(t, envOpts{})
	e.svc.o.SettleMax = 2 * time.Second
	e.wifi.set(func(w *fakeWifi) { w.radios["radio1"].pending = true })
	go func() {
		time.Sleep(300 * time.Millisecond)
		e.wifi.set(func(w *fakeWifi) { w.radios["radio1"].pending = false })
	}()
	start := time.Now()
	if err := e.svc.settle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < 250*time.Millisecond || d > 1500*time.Millisecond {
		t.Fatalf("settled after %v", d)
	}
}

func TestReloadKind(t *testing.T) {
	for _, tc := range []struct {
		configs []string
		want    string
	}{{[]string{"wireless"}, "wifi"}, {[]string{"network", "wireless"}, "network"}, {[]string{"perch-managed"}, "none"}} {
		if got := reloadKind(tc.configs); got != tc.want {
			t.Errorf("%v: %s", tc.configs, got)
		}
	}
	_ = plane.StateIdle
}
