package wifiplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/capthndsme/perch-agentkit/openwrt/plane"
	"github.com/capthndsme/perch-agentkit/openwrt/ubus"
	"github.com/capthndsme/perch-agentkit/rpc"
	"github.com/capthndsme/perch-apd/internal/applylock"
	"github.com/capthndsme/perch-apd/internal/groups"
	"github.com/capthndsme/perch-apd/internal/wificaps"
)

// The test harness: an AP's file tree (placeholders only), a fake clock
// for the confirm and health timers, the kit's file backend, and a fake
// runner that answers `ip route get` and the ubus calls the plane makes
// (network.wireless status, hostapd get_status, network.interface dump).

const tWireless = `
config wifi-device 'radio0'
	option type 'mac80211'
	option path 'platform/soc/18000000.wifi'
	option band '2g'
	option channel '1'
	option htmode 'HE20'

config wifi-device 'radio1'
	option type 'mac80211'
	option path 'platform/soc/18000000.wifi+1'
	option band '5g'
	option channel '100'
	option htmode 'HE80'

config wifi-iface 'default_radio0'
	option device 'radio0'
	option mode 'ap'
	option ssid 'Example'
	option encryption 'psk2'
	option key 'correct horse battery'
	option network 'lan'

config wifi-iface 'default_radio1'
	option device 'radio1'
	option mode 'ap'
	option ssid 'Example'
	option encryption 'psk2'
	option key 'correct horse battery'
	option network 'lan'

config wifi-station 'perch_ws0'
	option iface 'default_radio0'
	option key 'group key'
	option vid '130'
`

const tNetwork = `
config interface 'loopback'
	option device 'lo'
	option proto 'static'
	option ipaddr '127.0.0.1'

config device
	option name 'br-lan'
	option type 'bridge'
	list ports 'lan1'

config interface 'lan'
	option device 'br-lan'
	option proto 'static'
	option ipaddr '192.168.1.2'
	option netmask '255.255.255.0'
`

// fakeClock fires timers when the test advances it.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	at             time.Time
	f              func()
	stopped, fired bool
}

func newClock() *fakeClock { return &fakeClock{now: time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) AfterFunc(d time.Duration, f func()) func() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{at: c.now.Add(d), f: f}
	c.timers = append(c.timers, t)
	return func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		was := !t.stopped && !t.fired
		t.stopped = true
		return was
	}
}

// active counts the timers that have neither fired nor been stopped.
func (c *fakeClock) active() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, t := range c.timers {
		if !t.stopped && !t.fired {
			n++
		}
	}
	return n
}

// Advance moves the clock and runs every timer that is due, in order.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
	for {
		c.mu.Lock()
		var due *fakeTimer
		for _, t := range c.timers {
			if !t.stopped && !t.fired && !t.at.After(c.now) && (due == nil || t.at.Before(due.at)) {
				due = t
			}
		}
		if due != nil {
			due.fired = true
		}
		c.mu.Unlock()
		if due == nil {
			return
		}
		due.f()
	}
}

// radioRun and bssRun of the fake Wi-Fi.
type fakeRadio struct {
	up, pending, retry bool
	ifaces             map[string]string // section → ifname
}

type fakeWifi struct {
	mu      sync.Mutex
	radios  map[string]*fakeRadio
	hostapd map[string]string // ifname → get_status JSON ("" = no object)
	calls   []string
}

func newFakeWifi() *fakeWifi {
	return &fakeWifi{
		radios: map[string]*fakeRadio{
			"radio0": {up: true, ifaces: map[string]string{"default_radio0": "phy0-ap0"}},
			"radio1": {up: true, ifaces: map[string]string{"default_radio1": "phy1-ap0"}},
		},
		hostapd: map[string]string{
			"phy0-ap0": beacon("Example", 1),
			"phy1-ap0": beacon("Example", 100),
		},
	}
}

func beacon(ssid string, channel int) string {
	return fmt.Sprintf(`{"driver":"nl80211","status":"ENABLED","bssid":"02:00:00:00:00:%02x","ssid":%q,"channel":%d,"dfs":{"cac_seconds":0,"cac_active":false,"cac_seconds_left":0}}`, channel%256, ssid, channel)
}

func inCAC(ssid string, channel, left int) string {
	return fmt.Sprintf(`{"driver":"nl80211","status":"DFS","bssid":"02:00:00:00:00:%02x","ssid":%q,"channel":%d,"dfs":{"cac_seconds":60,"cac_active":true,"cac_seconds_left":%d}}`, channel%256, ssid, channel, left)
}

func withStatus(status, ssid string) string {
	return fmt.Sprintf(`{"driver":"nl80211","status":%q,"ssid":%q,"channel":36}`, status, ssid)
}

func (w *fakeWifi) set(f func(w *fakeWifi)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	f(w)
}

func (w *fakeWifi) status() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := map[string]any{}
	for name, r := range w.radios {
		var ifs []map[string]any
		for sec, ifname := range r.ifaces {
			ifs = append(ifs, map[string]any{"section": sec, "ifname": ifname, "config": map[string]any{"mode": "ap"}})
		}
		out[name] = map[string]any{"up": r.up, "pending": r.pending, "autostart": true, "disabled": false,
			"retry_setup_failed": r.retry, "config": map[string]any{}, "interfaces": ifs}
	}
	b, _ := json.Marshal(out)
	return string(b)
}

// run is the fake runner: ip and ubus.
func (w *fakeWifi) run(_ context.Context, name string, args ...string) ([]byte, []byte, int, error) {
	if name == "ip" {
		return []byte("192.168.1.10 dev br-lan src 192.168.1.2 uid 0\n    cache\n"), nil, 0, nil
	}
	if len(args) >= 2 && args[0] == "-t" {
		args = args[2:]
	}
	w.mu.Lock()
	w.calls = append(w.calls, strings.Join(args, " "))
	w.mu.Unlock()
	if len(args) >= 3 && args[0] == "call" {
		switch {
		case args[1] == "network.wireless" && args[2] == "status":
			return []byte(w.status()), nil, 0, nil
		case args[1] == "network.interface" && args[2] == "dump":
			return []byte(`{"interface":[{"interface":"lan","up":true,"pending":false,"l3_device":"br-lan","device":"br-lan","proto":"static"}]}`), nil, 0, nil
		case strings.HasPrefix(args[1], "hostapd.") && args[2] == "get_status":
			w.mu.Lock()
			st := w.hostapd[strings.TrimPrefix(args[1], "hostapd.")]
			w.mu.Unlock()
			if st == "" {
				return nil, []byte("Command failed: Not found"), 4, nil
			}
			return []byte(st), nil, 0, nil
		case args[1] == "session" && args[2] == "list":
			return nil, nil, 0, nil
		}
	}
	if len(args) >= 2 && args[0] == "list" {
		return nil, []byte("Command failed: Not found"), 4, nil
	}
	return nil, []byte("Command failed: Not found"), 4, nil
}

// env is one AP under test.
type env struct {
	t       *testing.T
	root    string
	clock   *fakeClock
	wifi    *fakeWifi
	svc     *Service
	lock    *applylock.Lock
	gen     uint64
	mu      sync.Mutex
	notes   []note
	reloads [][]string
	redial  chan string
	checks  chan plane.HealthReport
}

type note struct {
	method string
	params json.RawMessage
}

type envOpts struct {
	access      string
	transportOK bool
	groups      Groups
	noGuard     bool
	openwrt     *bool
}

func newEnv(t *testing.T, eo envOpts) *env {
	t.Helper()
	root := t.TempDir()
	e := &env{t: t, root: root, clock: newClock(), wifi: newFakeWifi(), lock: applylock.New(), gen: 1,
		redial: make(chan string, 8), checks: make(chan plane.HealthReport, 64)}
	e.write("/etc/config/wireless", tWireless)
	e.write("/etc/config/network", tNetwork)
	e.write("/etc/openwrt_release", "DISTRIB_ID='OpenWrt'\nDISTRIB_RELEASE='24.10.2'\nDISTRIB_TARGET='mediatek/filogic'\nDISTRIB_ARCH='aarch64_cortex-a53'\n")
	if !eo.noGuard {
		if _, _, err := EnsureGuard(root); err != nil {
			t.Fatal(err)
		}
	}
	if eo.access == "" {
		eo.access = plane.AccessWrite
	}
	ub := &ubus.Client{Bin: "ubus", Run: e.wifi.run, LookPath: func(string) (string, error) { return "/bin/ubus", nil }}
	yes := true
	if eo.openwrt == nil {
		eo.openwrt = &yes
	}
	svc, err := New(Options{
		Access: eo.access, Allow: []string{"wireless", "network", "dhcp"}, ConfirmMax: 900,
		TransportOK: eo.transportOK, ServerURL: "https://192.168.1.10",
		Lock: e.lock, Groups: eo.groups, GroupsEnabled: eo.groups != nil,
		Prober: &wificaps.Prober{Root: root, Ubus: ub}, Ubus: ub,
		Log:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Root: root, OpenWrt: eo.openwrt,
		Backend: &plane.FileBackend{Dir: filepath.Join(root, "etc/config"), OnReload: func(c []string) {
			e.mu.Lock()
			e.reloads = append(e.reloads, c)
			e.mu.Unlock()
		}},
		Clock: e.clock, Run: e.wifi.run,
		SettleMin: time.Millisecond, SettleMax: 50 * time.Millisecond, HealthInterval: 3 * time.Second,
		MonitorInterval: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	e.svc = svc
	svc.checked = func(r plane.HealthReport) { e.checks <- r }
	svc.Session(func(ctx context.Context) (uint64, string) {
		e.mu.Lock()
		defer e.mu.Unlock()
		return e.gen, fmt.Sprintf("challenge-%d", e.gen)
	}, func(method string, params any) bool {
		b, _ := json.Marshal(params)
		e.mu.Lock()
		e.notes = append(e.notes, note{method, b})
		e.mu.Unlock()
		return true
	}, func(reason string) {
		e.mu.Lock()
		e.gen++
		e.mu.Unlock()
		e.redial <- reason
	})
	return e
}

func (e *env) write(p, content string) {
	e.t.Helper()
	full := filepath.Join(e.root, p)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(strings.TrimLeft(content, "\n")), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) read(p string) string {
	b, _ := os.ReadFile(filepath.Join(e.root, p))
	return string(b)
}

// configure puts the AP in a mode, as agent.configure does.
func (e *env) configure(mode string) {
	e.svc.Configure(json.RawMessage(fmt.Sprintf(`{"metricsIntervalSeconds":5,"wifiConfig":{"mode":%q,"fingerprintKey":"%s","healthWaitSeconds":45}}`,
		mode, strings.Repeat("44", 32))))
}

// call runs a wifi.* method and returns the result as JSON, or the error.
func (e *env) call(method string, params any) (map[string]any, error) {
	e.t.Helper()
	var raw json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			e.t.Fatal(err)
		}
		raw = b
	}
	res, err := e.svc.Serve(context.Background(), method, raw)
	if err != nil {
		return nil, err
	}
	b, _ := json.Marshal(res)
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		e.t.Fatalf("%s result %s: %v", method, b, err)
	}
	return out, nil
}

// waitRedial waits for the plane to drop the session after an apply, and
// for the health check that starts right after.
func (e *env) waitRedial() plane.HealthReport {
	e.t.Helper()
	select {
	case <-e.redial:
	case <-time.After(5 * time.Second):
		e.t.Fatal("the plane never reconnected after the apply")
	}
	return e.nextCheck()
}

// nextCheck waits for the next health check's answer, and for the kit to
// have acted on it: a pending check leaves the deadline and the next check
// armed, any other only the deadline.
func (e *env) nextCheck() plane.HealthReport {
	e.t.Helper()
	var r plane.HealthReport
	select {
	case r = <-e.checks:
	case <-time.After(5 * time.Second):
		e.t.Fatal("no health check")
	}
	want := 1
	if r.State == plane.HealthPending {
		want = 2
	}
	deadline := time.Now().Add(5 * time.Second)
	for e.clock.active() != want || e.svc.Plane().ApplyState().Health != r.State {
		if time.Now().After(deadline) {
			e.t.Fatalf("the kit did not take the check: %d timers, %+v", e.clock.active(), e.svc.Plane().ApplyState())
		}
		time.Sleep(2 * time.Millisecond)
	}
	return r
}

func (e *env) notesOf(method string) []json.RawMessage {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []json.RawMessage
	for _, n := range e.notes {
		if n.method == method {
			out = append(out, n.params)
		}
	}
	return out
}

// rpcData is the code and data of a refusal.
func rpcData(t *testing.T, err error) (int, map[string]any) {
	t.Helper()
	var re *rpc.Error
	if !errors.As(err, &re) {
		t.Fatalf("not an rpc error: %v", err)
	}
	b, _ := json.Marshal(re.Data)
	var d map[string]any
	json.Unmarshal(b, &d)
	return re.Code, d
}

// newIfaceApply is an apply that adds one BSS on radio1.
func newIfaceApply(id, base string) map[string]any {
	return map[string]any{
		"applyId": id, "kind": "apply", "confirmTimeoutSeconds": 180,
		"base": map[string]string{"wireless": base},
		"ops": []map[string]any{{"op": "put", "config": "wireless", "section": "perch_n12_radio1", "type": "wifi-iface",
			"options": map[string]any{"device": "radio1", "mode": "ap", "ssid": "Kids", "encryption": "sae-mixed",
				"key": map[string]string{"$secret": "s1"}, "network": "lan"}}},
		"ledger":  map[string]any{"set": []map[string]string{{"perchId": "q7m2", "config": "wireless", "section": "perch_n12_radio1", "domain": "wifi_ifaces"}}},
		"secrets": map[string]string{"s1": "kids passphrase"},
		"expect":  map[string]any{"bss": []string{"default_radio1", "perch_n12_radio1"}, "radios": []string{"radio1"}},
	}
}

// fakeGroups is the device-groups engine as the plane sees it.
type fakeGroups struct {
	owned groups.OwnedView
}

func (g *fakeGroups) Owned() groups.OwnedView { return g.owned }
func (g *fakeGroups) State(context.Context) groups.StateResult {
	return groups.StateResult{AppliedRevision: 7}
}
