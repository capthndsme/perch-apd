package groups

import (
	"strings"
	"testing"

	"github.com/capthndsme/perch-agentkit/openwrt/uci"
)

// Three layouts, modelled on real APs (names and keys replaced):
//   - filtering: a DSA router whose br-lan already filters VLANs
//     (bridge-vlan 1 untagged on every port, lan on br-lan.1);
//   - untagged: br-lan without VLANs, the uplink ("wan") among its ports;
//   - standalone: the uplink is a port of its own (eth0), no bridge.
const wirelessConf = `
config wifi-device 'radio0'
	option type 'mac80211'
	option band '5g'

config wifi-iface 'default_radio0'
	option device 'radio0'
	option network 'lan'
	option mode 'ap'
	option ssid 'Apartment'
	option encryption 'psk2+ccmp'
	option key 'building-key-1'

config wifi-iface 'wifinet2'
	option device 'radio0'
	option network 'lan'
	option mode 'ap'
	option ssid 'Apartment Guest'
	option encryption 'none'

config wifi-iface 'wifinet3'
	option device 'radio0'
	option network 'lan'
	option mode 'ap'
	option ssid 'Home'
	option encryption 'sae-mixed'
	option key 'home-key-12345'
`

const filteringNet = `
config interface 'loopback'
	option device 'lo'
	option proto 'static'

config device
	option name 'br-lan'
	option type 'bridge'
	list ports 'lan1'
	list ports 'lan2'
	list ports 'wan'

config interface 'lan'
	option device 'br-lan.1'
	option proto 'static'

config bridge-vlan
	option device 'br-lan'
	option vlan '1'
	list ports 'lan1:u*'
	list ports 'lan2:u*'
	list ports 'wan:u*'
`

const untaggedNet = `
config interface 'loopback'
	option device 'lo'
	option proto 'static'

config device
	option name 'br-lan'
	option type 'bridge'
	list ports 'lan2'
	list ports 'wan'

config interface 'lan'
	option device 'br-lan'
	option proto 'static'

config interface 'lan6'
	option device 'br-lan'
	option proto 'none'
`

const standaloneNet = `
config interface 'loopback'
	option device 'lo'
	option proto 'static'

config interface 'lan'
	option device 'eth0'
	option proto 'dhcp'
`

func parse(t *testing.T, name, text string) *uci.Config {
	t.Helper()
	c, err := uci.Parse(name, []byte(text))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func desired() Desired {
	return Desired{Revision: 3, Trunk: "auto", SSIDs: []string{"Apartment"},
		VLANs:    []VLAN{{VID: 102}, {VID: 101}},
		Stations: []Station{{Key: "unit-101-key", VID: 101}, {VID: 102, MACs: []string{"02:00:00:00:00:21"}}}}
}

func section(c *uci.Config, name string) map[string]string {
	s := c.Section(name)
	if s == nil {
		return nil
	}
	out := map[string]string{"~type": s.Type}
	for _, o := range s.Options {
		out[o.Name] = o.Value.Str()
	}
	return out
}

func eq(t *testing.T, what string, got, want map[string]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %v, want %v", what, got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s: %v, want %v", what, got, want)
		}
	}
}

func TestPlanFilteringBridge(t *testing.T) {
	res, err := Plan(parse(t, "wireless", wirelessConf), parse(t, "network", filteringNet), desired(), Facts{TrunkPort: "wan"}, Ledger{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Converted || res.Bridge != "br-lan" || res.TrunkPort != "wan" {
		t.Fatalf("%+v", res)
	}
	eq(t, "bridge-vlan 101", section(res.Network, "perch_bv101"),
		map[string]string{"~type": "bridge-vlan", "device": "br-lan", "vlan": "101", "ports": "wan:t"})
	eq(t, "interface 101", section(res.Network, "perch_v101"),
		map[string]string{"~type": "interface", "proto": "none", "device": "br-lan.101"})
	if got := section(res.Network, "lan")["device"]; got != "br-lan.1" {
		t.Fatalf("lan moved: %s", got)
	}
	// Only the PSK interface on the SSID is managed: the open one is not
	// on the list, the other SSID is not listed.
	if strings.Join(res.Managed, ",") != "default_radio0" {
		t.Fatalf("managed %v", res.Managed)
	}
	if section(res.Wireless, "default_radio0")["dynamic_vlan"] != "1" {
		t.Fatal("dynamic_vlan not set")
	}
	if res.Ledger.DynamicVLAN["default_radio0"] != "" || len(res.Ledger.DynamicVLAN) != 1 {
		t.Fatalf("ledger %+v", res.Ledger)
	}
	eq(t, "wifi-vlan", section(res.Wireless, "perch_wv101_default_radio0"),
		map[string]string{"~type": "wifi-vlan", "iface": "default_radio0", "name": "g101", "vid": "101", "network": "perch_v101"})
	eq(t, "group key", section(res.Wireless, "perch_ws0"),
		map[string]string{"~type": "wifi-station", "iface": "default_radio0", "key": "unit-101-key", "vid": "101"})
	// A binding keeps the SSID's own passphrase.
	eq(t, "binding", section(res.Wireless, "perch_ws1"),
		map[string]string{"~type": "wifi-station", "iface": "default_radio0", "key": "building-key-1", "vid": "102", "mac": "02:00:00:00:00:21"})
	if section(res.Wireless, "wifinet3")["dynamic_vlan"] != "" {
		t.Fatal("an unmanaged interface was touched")
	}
}

func TestPlanUntaggedBridgeIsConvertedAndUndone(t *testing.T) {
	w, n := parse(t, "wireless", wirelessConf), parse(t, "network", untaggedNet)
	res, err := Plan(w, n, desired(), Facts{TrunkPort: "wan"}, Ledger{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Converted {
		t.Fatal("not converted")
	}
	eq(t, "untagged vlan", section(res.Network, "perch_bvu"),
		map[string]string{"~type": "bridge-vlan", "device": "br-lan", "vlan": "1", "ports": "lan2:u* wan:u*"})
	for _, iface := range []string{"lan", "lan6"} {
		if got := section(res.Network, iface)["device"]; got != "br-lan.1" {
			t.Fatalf("%s: %s", iface, got)
		}
	}
	c := res.Ledger.Converted
	if c == nil || c.Bridge != "br-lan" || c.UntaggedVLAN != 1 || c.Moved["lan"] != "br-lan" || c.Moved["lan6"] != "br-lan" {
		t.Fatalf("ledger %+v", res.Ledger.Converted)
	}

	// The same desired state again from the result: nothing moves.
	again, err := Plan(res.Wireless, res.Network, desired(), Facts{TrunkPort: "wan"}, res.Ledger)
	if err != nil {
		t.Fatal(err)
	}
	if string(uci.Render(again.Network)) != string(uci.Render(res.Network)) ||
		string(uci.Render(again.Wireless)) != string(uci.Render(res.Wireless)) {
		t.Fatalf("not idempotent:\n%s\n---\n%s", uci.Render(again.Network), uci.Render(res.Network))
	}

	// No VLANs any more: every Perch section and change is gone.
	none := Desired{Revision: 4, SSIDs: []string{"Apartment"}}
	back, err := Plan(res.Wireless, res.Network, none, Facts{TrunkPort: "wan"}, res.Ledger)
	if err != nil {
		t.Fatal(err)
	}
	if string(uci.Render(back.Network)) != string(uci.Render(n)) {
		t.Fatalf("network not restored:\n%s", uci.Render(back.Network))
	}
	if string(uci.Render(back.Wireless)) != string(uci.Render(w)) {
		t.Fatalf("wireless not restored:\n%s", uci.Render(back.Wireless))
	}
}

func TestPlanUntaggedVLANAvoidsGroupVLANs(t *testing.T) {
	d := desired()
	d.VLANs = []VLAN{{VID: 1}, {VID: 2}, {VID: 101}, {VID: 102}}
	res, err := Plan(parse(t, "wireless", wirelessConf), parse(t, "network", untaggedNet), d, Facts{TrunkPort: "wan"}, Ledger{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Ledger.Converted.UntaggedVLAN != 3 || section(res.Network, "lan")["device"] != "br-lan.3" {
		t.Fatalf("%+v", res.Ledger.Converted)
	}
}

func TestPlanStandalonePort(t *testing.T) {
	res, err := Plan(parse(t, "wireless", wirelessConf), parse(t, "network", standaloneNet), desired(), Facts{TrunkPort: "eth0"}, Ledger{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Bridge != "" || res.Converted {
		t.Fatalf("%+v", res)
	}
	eq(t, "8021q", section(res.Network, "perch_dv101"),
		map[string]string{"~type": "device", "type": "8021q", "ifname": "eth0", "vid": "101", "name": "eth0.101"})
	eq(t, "bridge", section(res.Network, "perch_bd101"),
		map[string]string{"~type": "device", "type": "bridge", "name": "br-pv101", "ports": "eth0.101"})
	eq(t, "interface", section(res.Network, "perch_v101"),
		map[string]string{"~type": "interface", "proto": "none", "device": "br-pv101"})
}

func TestPlanRefusals(t *testing.T) {
	w, n := parse(t, "wireless", wirelessConf), parse(t, "network", filteringNet)
	code := func(err error) string {
		if r, ok := err.(*Refusal); ok {
			return r.Code
		}
		return ""
	}
	d := desired()
	d.SSIDs = []string{"Apartment Guest"} // open: no passphrase to key
	if _, err := Plan(w, n, d, Facts{TrunkPort: "wan"}, Ledger{}); code(err) != "no_managed_iface" {
		t.Fatalf("open SSID: %v", err)
	}
	if _, err := Plan(w, n, desired(), Facts{}, Ledger{}); code(err) != "trunk_unknown" {
		t.Fatalf("no trunk: %v", err)
	}
	d = desired()
	d.Stations = append(d.Stations, Station{Key: "short", VID: 101})
	if _, err := Plan(w, n, d, Facts{TrunkPort: "wan"}, Ledger{}); code(err) != "bad_params" {
		t.Fatalf("short key: %v", err)
	}
	d = desired()
	d.Stations = append(d.Stations, Station{Key: "unlisted-vlan", VID: 300})
	if _, err := Plan(w, n, d, Facts{TrunkPort: "wan"}, Ledger{}); code(err) != "bad_params" {
		t.Fatalf("unlisted VLAN: %v", err)
	}
	d = desired()
	d.Trunk = "wan; reboot"
	if _, err := Plan(w, n, d, Facts{TrunkPort: "wan"}, Ledger{}); code(err) != "bad_params" {
		t.Fatalf("bad trunk: %v", err)
	}
}

func TestPlanKeepsAnExistingDynamicVLAN(t *testing.T) {
	w := parse(t, "wireless", strings.Replace(wirelessConf, "option key 'building-key-1'", "option key 'building-key-1'\n\toption dynamic_vlan '2'", 1))
	res, err := Plan(w, parse(t, "network", filteringNet), desired(), Facts{TrunkPort: "wan"}, Ledger{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Ledger.DynamicVLAN["default_radio0"] != "2" {
		t.Fatalf("%+v", res.Ledger)
	}
	back, err := Plan(res.Wireless, res.Network, Desired{Revision: 9}, Facts{}, res.Ledger)
	if err != nil {
		t.Fatal(err)
	}
	if section(back.Wireless, "default_radio0")["dynamic_vlan"] != "2" {
		t.Fatal("dynamic_vlan not put back")
	}
}
