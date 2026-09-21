package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/capthndsme/perch-agentkit/rpc"
	"github.com/capthndsme/perch-apd/internal/leds"
	"github.com/capthndsme/perch-apd/internal/nl80211"
	"github.com/capthndsme/perch-apd/internal/sysinfo"
	"github.com/capthndsme/perch-apd/internal/ubus"
	"github.com/capthndsme/perch-apd/internal/wireless"
)

type fakeNL struct {
	stations map[int][]nl80211.Station
}

func mac(s string) net.HardwareAddr { m, _ := net.ParseMAC(s); return m }

func (f *fakeNL) Interfaces() ([]nl80211.Interface, error) {
	return []nl80211.Interface{
		{Index: 10, Name: "phy0-ap0", Type: nl80211.IfTypeAP, MAC: mac("02:00:00:00:00:11"), SSID: "Example", FrequencyMHz: 2437},
		{Index: 11, Name: "phy1-ap0", Wiphy: 1, Type: nl80211.IfTypeAP, MAC: mac("02:00:00:00:00:21"), SSID: "Example", FrequencyMHz: 5180},
	}, nil
}
func (f *fakeNL) Stations(i int) ([]nl80211.Station, error) { return f.stations[i], nil }
func (f *fakeNL) Survey(int) ([]nl80211.SurveyEntry, error) { return nil, nil }
func (f *fakeNL) RegDomain() (string, error)                { return "PH", nil }

type call struct {
	object, method string
	args           map[string]any
}

type fakeUbus struct {
	calls     []call
	hostapd   []string
	delErr    error
	available bool
}

func (f *fakeUbus) Available() bool { return f.available }
func (f *fakeUbus) List(_ context.Context, pattern string) ([]string, error) {
	return f.hostapd, nil
}
func (f *fakeUbus) Call(_ context.Context, object, method string, args any, out any) error {
	c := call{object: object, method: method}
	if args != nil {
		b, _ := json.Marshal(args)
		_ = json.Unmarshal(b, &c.args)
	}
	f.calls = append(f.calls, c)
	if method == "del_client" {
		return f.delErr
	}
	return errors.New("no such call in fake")
}

func newDeps(t *testing.T) (*Deps, *fakeUbus, *rpc.Dispatcher) {
	t.Helper()
	nl := &fakeNL{stations: map[int][]nl80211.Station{
		11: {{MAC: mac("02:00:00:00:00:01"), InactiveMs: 5, Signal: -50, HasSignal: true}},
	}}
	ub := &fakeUbus{available: true, hostapd: []string{"hostapd.phy0-ap0", "hostapd.phy1-ap0"}}
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "openwrt_release"), nil, 0o644)
	ledDir := filepath.Join(root, "leds")
	os.MkdirAll(filepath.Join(ledDir, "green:power"), 0o755)
	os.WriteFile(filepath.Join(ledDir, "green:power", "trigger"), []byte("[none] timer\n"), 0o644)
	os.WriteFile(filepath.Join(ledDir, "green:power", "brightness"), []byte("1\n"), 0o644)
	etc := filepath.Join(root, "etc")
	os.MkdirAll(etc, 0o755)
	os.WriteFile(filepath.Join(etc, "openwrt_release"), []byte("DISTRIB_ID='OpenWrt'\nDISTRIB_RELEASE='25.12.4'\n"), 0o644)
	d := &Deps{
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Wireless: &wireless.Source{NL: nl},
		Ubus:     ub,
		Locator:  &leds.Locator{Dir: ledDir},
		Info:     &sysinfo.Info{Root: root},
	}
	disp := rpc.NewDispatcher()
	Register(disp, d)
	return d, ub, disp
}

func invoke(t *testing.T, disp *rpc.Dispatcher, method, params string) *rpc.Message {
	t.Helper()
	frame := `{"jsonrpc":"2.0","id":1,"method":"` + method + `"`
	if params != "" {
		frame += `,"params":` + params
	}
	frame += "}"
	m, errResp := rpc.Decode([]byte(frame))
	if errResp != nil {
		t.Fatal(errResp.Error)
	}
	return disp.Serve(context.Background(), m)
}

func TestKickFindsClientAndCallsHostapd(t *testing.T) {
	_, ub, disp := newDeps(t)
	// Hint points at the wrong interface: the agent must find phy1-ap0 itself.
	resp := invoke(t, disp, "client.kick", `{"mac":"02:00:00:00:00:01","ifname":"phy0-ap0","banTimeMs":5000,"reason":1}`)
	if resp.Error != nil {
		t.Fatal(resp.Error)
	}
	if string(resp.Result) != `{"mac":"02:00:00:00:00:01","ifname":"phy1-ap0","banTimeMs":5000}` {
		t.Fatal(string(resp.Result))
	}
	if len(ub.calls) != 1 {
		t.Fatalf("calls %+v", ub.calls)
	}
	c := ub.calls[0]
	if c.object != "hostapd.phy1-ap0" || c.method != "del_client" {
		t.Fatalf("%+v", c)
	}
	if c.args["addr"] != "02:00:00:00:00:01" || c.args["reason"] != float64(1) || c.args["deauth"] != true || c.args["ban_time"] != float64(5000) {
		t.Fatalf("args %+v", c.args)
	}
}

func TestKickDefaultsAndErrors(t *testing.T) {
	_, ub, disp := newDeps(t)
	resp := invoke(t, disp, "client.kick", `{"mac":"02-00-00-00-00-01"}`)
	if resp.Error != nil {
		t.Fatal(resp.Error)
	}
	if a := ub.calls[0].args; a["reason"] != float64(5) || a["ban_time"] != float64(0) || a["deauth"] != true {
		t.Fatalf("defaults %+v", a)
	}

	cases := map[string]int{
		`{"mac":"nope"}`: rpc.CodeInvalidParams,
		`{"mac":"02:00:00:00:00:01","banTimeMs":-1}`: rpc.CodeInvalidParams,
		`{"mac":"02:00:00:00:00:01","reason":0}`:     rpc.CodeInvalidParams,
		`{"mac":"02:00:00:00:00:99"}`:                rpc.CodeNotFound,
		`{"mac":"02:00:00:00:00:01","deauth":"x"}`:   rpc.CodeInvalidParams,
	}
	for params, code := range cases {
		r := invoke(t, disp, "client.kick", params)
		if r.Error == nil || r.Error.Code != code {
			t.Errorf("%s: %+v", params, r.Error)
		}
	}

	ub.delErr = ubus.ErrNotFound
	if r := invoke(t, disp, "client.kick", `{"mac":"02:00:00:00:00:01"}`); r.Error == nil || r.Error.Code != rpc.CodeUnsupported {
		t.Fatalf("missing hostapd object: %+v", r.Error)
	}
	ub.delErr = errors.New("ubus call hostapd.phy1-ap0 del_client: Invalid argument")
	if r := invoke(t, disp, "client.kick", `{"mac":"02:00:00:00:00:01"}`); r.Error == nil || r.Error.Code != rpc.CodeCommandFailed ||
		!strings.Contains(r.Error.Message, "Invalid argument") {
		t.Fatalf("command failure: %+v", r.Error)
	}
	ub.available = false
	if r := invoke(t, disp, "client.kick", `{"mac":"02:00:00:00:00:01"}`); r.Error == nil || r.Error.Code != rpc.CodeUnsupported {
		t.Fatalf("no ubus: %+v", r.Error)
	}
}

func TestLocate(t *testing.T) {
	_, _, disp := newDeps(t)
	r := invoke(t, disp, "locate.start", `{"durationSeconds":60}`)
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	var res map[string]any
	json.Unmarshal(r.Result, &res)
	if res["active"] != true || res["durationSeconds"] != float64(60) || res["leds"] != float64(1) {
		t.Fatalf("%v", res)
	}
	if r := invoke(t, disp, "locate.stop", ""); string(r.Result) != `{"active":false}` {
		t.Fatalf("%s", r.Result)
	}
	if r := invoke(t, disp, "locate.start", `{"durationSeconds":601}`); r.Error == nil || r.Error.Code != rpc.CodeInvalidParams {
		t.Fatalf("%+v", r.Error)
	}
}

func TestRebootAnswersThenReboots(t *testing.T) {
	d, _, disp := newDeps(t)
	var rebooted atomic.Bool
	d.Reboot = func() error { rebooted.Store(true); return nil }
	r := invoke(t, disp, "system.reboot", `{"delaySeconds":0}`)
	if r.Error != nil || string(r.Result) != `{"delaySeconds":0,"scheduled":true}` {
		t.Fatalf("%s %+v", r.Result, r.Error)
	}
	if rebooted.Load() {
		t.Fatal("rebooted before answering")
	}
	deadline := time.Now().Add(2 * time.Second)
	for !rebooted.Load() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !rebooted.Load() {
		t.Fatal("never rebooted")
	}
	if r := invoke(t, disp, "system.reboot", `{"delaySeconds":61}`); r.Error == nil {
		t.Fatal("delay 61 accepted")
	}
}

func TestSystemInfoAndCapabilities(t *testing.T) {
	d, ub, disp := newDeps(t)
	r := invoke(t, disp, "system.info", "")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	var info SystemInfo
	if err := json.Unmarshal(r.Result, &info); err != nil {
		t.Fatal(err)
	}
	if info.Protocol != 1 || info.Release != "25.12.4" || strings.Join(info.Capabilities, ",") != "metrics,clients,kick,locate,reboot" {
		t.Fatalf("%+v", info)
	}
	if len(info.Interfaces) != 2 || info.Interfaces[1].Stations != 1 || info.Interfaces[1].Band != "5" || info.Interfaces[0].Radio != "phy0" {
		t.Fatalf("interfaces %+v", info.Interfaces)
	}
	ub.hostapd = nil
	if caps := strings.Join(d.Capabilities(context.Background()), ","); caps != "metrics,clients,locate,reboot" {
		t.Fatalf("caps without hostapd: %s", caps)
	}
}

func TestPingClientsAndNoPullMethod(t *testing.T) {
	_, _, disp := newDeps(t)
	r := invoke(t, disp, "ping", "")
	if r.Error != nil || !strings.Contains(string(r.Result), `"pong":true`) {
		t.Fatalf("%s", r.Result)
	}
	r = invoke(t, disp, "clients.list", `{"ifname":"phy1-ap0"}`)
	if r.Error != nil || !strings.Contains(string(r.Result), `"mac":"02:00:00:00:00:01"`) || !strings.Contains(string(r.Result), `"signalDbm":-50`) {
		t.Fatalf("%s %+v", r.Result, r.Error)
	}
	// Metrics are pushed (metrics.push); there is no method to pull them.
	if r := invoke(t, disp, "metrics.collect", ""); r.Error == nil || r.Error.Code != rpc.CodeMethodNotFound {
		t.Fatalf("metrics.collect still served: %+v", r)
	}
}

func TestValidMAC(t *testing.T) {
	for s, want := range map[string]bool{
		"02:00:00:00:00:01": true, "02-00-00-AA-bb-01": true, "02:00:00:00:00:0": false,
		"02:00:00:00:00:0g": false, "02:00:00:00:00:011": false, "02.00.00.00.00.01": false, "": false,
	} {
		if validMAC(s) != want {
			t.Errorf("validMAC(%q) != %v", s, want)
		}
	}
}
