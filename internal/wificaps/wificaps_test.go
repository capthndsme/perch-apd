package wificaps

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/capthndsme/perch-apd/internal/nl80211"
)

type fakeNL struct {
	wiphys []nl80211.Wiphy
	ifaces []nl80211.Interface
	regs   []nl80211.RegDomain
}

func (f fakeNL) Wiphys() ([]nl80211.Wiphy, error)                 { return f.wiphys, nil }
func (f fakeNL) Interfaces() ([]nl80211.Interface, error)         { return f.ifaces, nil }
func (f fakeNL) RegDomains() ([]nl80211.RegDomain, error)         { return f.regs, nil }
func (f fakeUbus) List(context.Context, string) ([]string, error) { return f.hostapd, nil }

type fakeUbus struct {
	replies map[string]string
	hostapd []string
}

func (f fakeUbus) Call(_ context.Context, object, method string, _ any, out any) error {
	r, ok := f.replies[object+" "+method]
	if !ok {
		return errors.New("not found")
	}
	return json.Unmarshal([]byte(r), out)
}

type tree struct {
	t    *testing.T
	root string
}

func (tr tree) file(p, s string) {
	tr.t.Helper()
	full := filepath.Join(tr.root, p)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		tr.t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(s), 0o644); err != nil {
		tr.t.Fatal(err)
	}
}

func (tr tree) link(p, target string) {
	tr.t.Helper()
	full := filepath.Join(tr.root, p)
	os.MkdirAll(filepath.Dir(full), 0o755)
	if err := os.Symlink(target, full); err != nil {
		tr.t.Fatal(err)
	}
}

func (tr tree) dir(p string) {
	os.MkdirAll(filepath.Join(tr.root, p), 0o755)
}

// A tri-radio AP modelled on the RAX3000M (MT7981: phy0 2.4 GHz and phy1
// 5 GHz on one device), with a stale radio section whose path is gone.
func fixture(t *testing.T) (tree, *Prober) {
	tr := tree{t: t, root: t.TempDir()}
	tr.file("etc/config/wireless", `
config wifi-device 'radio0'
	option type 'mac80211'
	option path 'platform/18000000.wifi'
	option band '2g'

config wifi-device 'radio1'
	option type 'mac80211'
	option path 'platform/soc/18000000.wifi+1'
	option band '5g'

config wifi-device 'radio2'
	option type 'mac80211'
	option path 'platform/soc/18000000.wifi'
	option band '2g'
	option country 'PH'

config wifi-iface 'wifinet13'
	option device 'radio1'
	option mode 'ap'
	option ssid 'Example'
	option encryption 'psk2'
	option key 'never-reported-1'
`)
	tr.file("etc/config/network", "\nconfig device\n\toption name 'br-lan'\n\toption type 'bridge'\n\tlist ports 'lan2'\n")
	tr.file("etc/openwrt_release", "DISTRIB_ID='OpenWrt'\nDISTRIB_RELEASE='24.10.0'\nDISTRIB_REVISION='r28427-6df0e3d02a'\nDISTRIB_TARGET='mediatek/filogic'\nDISTRIB_ARCH='aarch64_cortex-a53'\n")
	tr.file("tmp/sysinfo/board_name", "example,ap\n")
	tr.file("usr/lib/opkg/status", "Package: wpad-basic-mbedtls\nVersion: 2024.09.15~5ace39b0-r1\nStatus: install user installed\n\n"+
		"Package: hostapd-common\nVersion: 2024.09.15~5ace39b0-r1\nStatus: install ok installed\n\n"+
		"Package: iw\nVersion: 6.9-r1\nStatus: install ok installed\n\n"+
		"Package: busybox\nVersion: 1.36.1-r2\nStatus: install ok installed\n\n")
	tr.file("usr/sbin/wpad", "binary")
	tr.link("usr/sbin/hostapd", "wpad")
	tr.file("lib/netifd/wireless/mac80211.sh", "#!/bin/sh\n")
	tr.file("proc/self/uid_map", "         0          0 4294967295\n")
	tr.dir("sys/devices/platform/soc/18000000.wifi")
	for i, name := range []string{"phy0", "phy1"} {
		tr.link("sys/class/ieee80211/"+name+"/device", "../../../devices/platform/soc/18000000.wifi")
		tr.file("sys/class/ieee80211/"+name+"/index", []string{"0\n", "1\n"}[i])
		tr.file("sys/class/ieee80211/"+name+"/macaddress", []string{"02:00:00:00:00:a0\n", "02:00:00:00:00:a1\n"}[i])
	}
	// Trunk: the gateway (192.0.2.1, 02:00:00:00:00:01) is learned on lan2.
	tr.file("proc/net/route", "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n"+
		"br-lan.1\t00000000\t010200C0\t0003\t0\t0\t0\t00000000\t0\t0\t0\n")
	tr.file("proc/net/arp", "IP address       HW type     Flags       HW address            Mask     Device\n"+
		"192.0.2.1        0x1         0x2         02:00:00:00:00:01     *        br-lan.1\n")
	e := make([]byte, 16)
	m, _ := net.ParseMAC("02:00:00:00:00:01")
	copy(e, m)
	e[6] = 2
	binary.LittleEndian.PutUint32(e[8:], 100)
	tr.file("sys/class/net/br-lan/brforward", string(e))
	tr.file("sys/class/net/br-lan/brif/lan2/port_no", "0x2\n")
	tr.file("sys/class/net/br-lan/bridge/vlan_filtering", "1\n")
	tr.link("sys/class/net/lan2/master", "../br-lan")

	fiveG := nl80211.WiphyBand{Band: nl80211.Band5GHz, HTCapa: 0x09ef, VHTCapa: 0x339071b6, HasHE: true, HEPhyCap0: 0x0c,
		Freqs: []nl80211.WiphyFreq{
			{MHz: 5180, MaxTxPowerMBm: 2300},
			{MHz: 5500, Radar: true, NoIR: true, MaxTxPowerMBm: 2400, DFSState: nl80211.DFSUsable},
			{MHz: 5600, Radar: true, NoIR: true, MaxTxPowerMBm: 2400, DFSState: nl80211.DFSUnavailable},
			{MHz: 5620, Radar: true, NoIR: true, MaxTxPowerMBm: 2400, CACTimeMs: 600000},
			{MHz: 5845, Disabled: true},
		}}
	twoG := nl80211.WiphyBand{Band: nl80211.Band2GHz, HTCapa: 0x01ef, HasHE: true, HEPhyCap0: 0x02,
		Freqs: []nl80211.WiphyFreq{{MHz: 2412, MaxTxPowerMBm: 2000}, {MHz: 2484, Disabled: true, NoIR: true, MaxTxPowerMBm: 2000}}}
	combo := []nl80211.IfaceCombination{{MaxNum: 16, NumChannels: 1, Limits: []nl80211.IfaceLimit{
		{Max: 1, Types: []nl80211.IfType{nl80211.IfTypeStation}}, {Max: 16, Types: []nl80211.IfType{nl80211.IfTypeAP, nl80211.IfTypeMeshPoint}}}}}
	fnl := fakeNL{
		wiphys: []nl80211.Wiphy{
			{Index: 0, Name: "phy0", Bands: []nl80211.WiphyBand{twoG}, Combinations: combo},
			{Index: 1, Name: "phy1", Bands: []nl80211.WiphyBand{fiveG}, Combinations: combo},
		},
		ifaces: []nl80211.Interface{
			{Index: 7, Name: "phy1-ap0", Wiphy: 1, Type: nl80211.IfTypeAP, FrequencyMHz: 5180, ChannelWidth: 3, TxPowerMBm: 2300},
			{Index: 8, Name: "phy0-ap0", Wiphy: 0, Type: nl80211.IfTypeAP, FrequencyMHz: 2412, ChannelWidth: 1, TxPowerMBm: 2000},
		},
		regs: []nl80211.RegDomain{{Wiphy: -1, Alpha2: "PH", DFSRegion: nl80211.DFSETSI}},
	}
	ub := fakeUbus{hostapd: []string{"hostapd.phy1-ap0"}, replies: map[string]string{
		"network.wireless status": `{
			"radio0": {"up": false, "pending": false, "disabled": false, "retry_setup_failed": true,
				"config": {"path": "platform/18000000.wifi", "channel": "1", "band": "2g", "htmode": "HE20"}, "interfaces": []},
			"radio1": {"up": true, "pending": false, "disabled": false, "retry_setup_failed": false,
				"config": {"path": "platform/soc/18000000.wifi+1", "channel": 36, "band": "5g", "htmode": "HE80", "txpower": 23},
				"interfaces": [{"section": "wifinet13", "ifname": "phy1-ap0", "config": {"mode": "ap", "ssid": "Example"}}]},
			"radio2": {"up": true, "pending": false, "disabled": false, "retry_setup_failed": false,
				"config": {"path": "platform/soc/18000000.wifi", "channel": "1", "band": "2g", "htmode": "HE20", "country": "PH"},
				"interfaces": [{"section": "default_radio2", "ifname": "phy0-ap0", "config": {"mode": "ap"}}]}}`,
		"network.interface dump": `{"interface": [
			{"interface": "wan", "up": false, "proto": "dhcp", "device": "eth1"},
			{"interface": "lan", "up": true, "proto": "static", "device": "br-lan.1", "l3_device": "br-lan.1"},
			{"interface": "loopback", "up": true, "proto": "static", "l3_device": "lo"}]}`,
	}}
	runs := 0
	p := &Prober{Root: tr.root, NL: fnl, Ubus: ub, Run: func(_ context.Context, name string, args ...string) (int, error) {
		runs++
		if name != "/usr/sbin/hostapd" || len(args) != 1 {
			t.Fatalf("ran %s %v", name, args)
		}
		switch args[0] {
		case "-v11r", "-vsae", "-vowe", "-v11ac", "-v11ax", "-vacs":
			return 0, nil
		}
		return 1, nil
	}}
	return tr, p
}

func TestFacts(t *testing.T) {
	_, p := fixture(t)
	f := p.Facts(context.Background())
	if len(f.Problems) != 0 {
		t.Fatalf("problems %v", f.Problems)
	}
	if *f.OpenWrt != (OpenWrt{Release: "24.10.0", Revision: "r28427-6df0e3d02a", Target: "mediatek/filogic", Arch: "aarch64_cortex-a53", Board: "example,ap"}) {
		t.Fatalf("%+v", f.OpenWrt)
	}
	if f.PackageManager != "opkg" || !reflect.DeepEqual(f.Packages, map[string]string{
		"wpad-basic-mbedtls": "2024.09.15~5ace39b0-r1", "hostapd-common": "2024.09.15~5ace39b0-r1", "iw": "6.9-r1"}) {
		t.Fatalf("%s %v", f.PackageManager, f.Packages)
	}
	if f.WifiScripts != "shell" || f.Schema {
		t.Fatal(f.WifiScripts)
	}
	h := f.Hostapd
	if h == nil || h.Binary != "/usr/sbin/wpad" || h.Variant != "wpad-basic-mbedtls" || !h.Ubus ||
		!h.Features["owe"] || !h.Features["11r"] || h.Features["eap"] || h.Features["11be"] || len(h.Features) != len(FeatureNames) {
		t.Fatalf("%+v", h)
	}
	if r := f.Regulatory; r.Global != "PH" || r.DFSRegion != "ETSI" || !r.Settable || r.Reason != nil || len(r.SelfManaged) != 0 {
		t.Fatalf("%+v", r)
	}
	if len(f.Radios) != 3 {
		t.Fatalf("%+v", f.Radios)
	}
	stale, five, two := f.Radios[0], f.Radios[1], f.Radios[2]
	if stale.Section != "radio0" || stale.Present || stale.Phy != "" || !stale.RetrySetupFailed || stale.Up || len(stale.Channels) != 0 {
		t.Fatalf("stale %+v", stale)
	}
	if five.Phy != "phy1" || !five.Present || !five.Up || five.Band != "5g" || five.Country != "" || five.RegCountry != "PH" ||
		five.MaxBSS != 16 || five.TxpowerMaxDbm != 24 {
		t.Fatalf("5g %+v", five)
	}
	if !reflect.DeepEqual(five.Widths, []int{20, 40, 80, 160}) || !reflect.DeepEqual(five.Modes, []string{"HT", "VHT", "HE"}) {
		t.Fatalf("5g %v %v", five.Widths, five.Modes)
	}
	wantCh := []Channel{
		{Channel: 36, MHz: 5180, MaxDbm: 23},
		{Channel: 100, MHz: 5500, MaxDbm: 24, DFS: true, NoIR: true, CACSeconds: 60, DFSState: "usable"},
		{Channel: 120, MHz: 5600, MaxDbm: 24, DFS: true, NoIR: true, CACSeconds: 600, DFSState: "unavailable"}, // ETSI weather radar
		{Channel: 124, MHz: 5620, MaxDbm: 24, DFS: true, NoIR: true, CACSeconds: 600, DFSState: "usable"},      // the kernel's own
		{Channel: 169, MHz: 5845, Disabled: true},
	}
	if !reflect.DeepEqual(five.Channels, wantCh) {
		t.Fatalf("channels %+v", five.Channels)
	}
	if c := five.Current; c == nil || *c != (Current{Channel: 36, MHz: 5180, WidthMHz: 80, HTMode: "HE80", TxpowerDbm: 23}) {
		t.Fatalf("current %+v", c)
	}
	if two.Phy != "phy0" || two.Country != "PH" || !reflect.DeepEqual(two.HTModes, []string{"NOHT", "HT20", "HE20", "HT40", "HE40"}) ||
		two.Current.Channel != 1 || two.TxpowerMaxDbm != 20 {
		t.Fatalf("2g %+v", two)
	}
	if tr := f.Trunk; tr == nil || *tr != (Trunk{Port: "lan2", Bridge: "br-lan", VLANFiltering: true, Source: "auto"}) {
		t.Fatalf("trunk %+v", f.Trunk)
	}
	if !reflect.DeepEqual(f.Networks, []Network{{Name: "lan", Device: "br-lan.1", Proto: "static", Up: true}, {Name: "wan", Device: "eth1", Proto: "dhcp"}}) {
		t.Fatalf("networks %+v", f.Networks)
	}
	// No key ever leaves: the facts hold no wifi-iface option.
	b, _ := json.Marshal(f)
	if strings.Contains(string(b), "never-reported") || strings.Contains(string(b), "Example") {
		t.Fatal("wifi-iface content in the facts")
	}
}

func TestHostapdFeaturesAreAskedOncePerBuild(t *testing.T) {
	_, p := fixture(t)
	n := 0
	inner := p.Run
	p.Run = func(ctx context.Context, name string, args ...string) (int, error) {
		n++
		return inner(ctx, name, args...)
	}
	p.Facts(context.Background())
	p.Facts(context.Background())
	if n != len(FeatureNames) {
		t.Fatalf("%d runs", n)
	}
}

func TestRegulatoryVariants(t *testing.T) {
	tr, p := fixture(t)
	// An unprivileged container (the lab AP): country cannot be set.
	tr.file("proc/self/uid_map", "         0     100000      65536\n")
	nl := p.NL.(fakeNL)
	nl.wiphys[1].SelfManagedReg = true
	nl.regs = append(nl.regs, nl80211.RegDomain{Wiphy: 1, Alpha2: "US", DFSRegion: nl80211.DFSFCC})
	p.NL = nl
	f := p.Facts(context.Background())
	r := f.Regulatory
	if r.Settable || r.Reason == nil || *r.Reason != "user_namespace" || !reflect.DeepEqual(r.SelfManaged, []string{"phy1"}) {
		t.Fatalf("%+v", r)
	}
	if f.Radios[1].RegCountry != "US" || !f.Radios[1].SelfManagedReg || f.Radios[2].RegCountry != "PH" {
		t.Fatalf("%+v", f.Radios)
	}
	p.NL = nil
	if r := p.Facts(context.Background()).Regulatory; r.Settable || *r.Reason != "no_wifi" {
		t.Fatalf("%+v", r)
	}
}

func TestFindPhy(t *testing.T) {
	phys := []phyInfo{
		{name: "phy0", index: 0, devpath: "/sys/devices/platform/1e140000.pcie/pci0000:00/0000:00:01.0/0000:02:00.0", mac: "02:00:00:00:00:a0"},
		{name: "phy1", index: 1, devpath: "/sys/devices/platform/1e140000.pcie/pci0000:00/0000:00:01.0/0000:02:00.0", mac: "02:00:00:00:00:a1"},
		{name: "phy2", index: 2, devpath: "/sys/devices/platform/soc@0/c000000.wifi"},
	}
	for _, c := range []struct {
		r    uciRadio
		want string
	}{
		{uciRadio{path: "1e140000.pcie/pci0000:00/0000:00:01.0/0000:02:00.0"}, "phy0"},
		{uciRadio{path: "1e140000.pcie/pci0000:00/0000:00:01.0/0000:02:00.0+1"}, "phy1"},
		{uciRadio{path: "1e140000.pcie/pci0000:00/0000:00:01.0/0000:02:00.0+2"}, ""},
		{uciRadio{path: "platform/soc@0/c000000.wifi"}, "phy2"},
		{uciRadio{path: "platform/c000000.wifi"}, ""}, // stale: the device moved
		{uciRadio{macaddr: "02:00:00:00:00:A1"}, "phy1"},
		{uciRadio{phy: "phy2"}, "phy2"},
		{uciRadio{phy: "phy9"}, ""},
	} {
		if got := findPhy(phys, c.r); got != c.want {
			t.Errorf("%+v: %q, want %q", c.r, got, c.want)
		}
	}
}

// The htmode lists match what OpenWrt's wifi-detect.uc wrote into the
// three live APs' /etc/board.json.
func TestBandModesMatchBoardJSON(t *testing.T) {
	for _, c := range []struct {
		name   string
		band   nl80211.WiphyBand
		widths []int
		want   string
	}{
		{"2g HE", nl80211.WiphyBand{Band: nl80211.Band2GHz, HTCapa: 0x01ef, HasHE: true, HEPhyCap0: 0x02},
			[]int{20, 40}, "NOHT HT20 HE20 HT40 HE40"},
		{"5g HE80 (MT7915 DBDC)", nl80211.WiphyBand{Band: nl80211.Band5GHz, HTCapa: 0x09ef, VHTCapa: 0x339071b2, HasHE: true, HEPhyCap0: 0x04},
			[]int{20, 40, 80}, "NOHT HT20 VHT20 HE20 HT40 VHT40 HE40 VHT80 HE80"},
		{"5g HE160", nl80211.WiphyBand{Band: nl80211.Band5GHz, HTCapa: 0x09ef, VHTCapa: 0x339071b6, HasHE: true, HEPhyCap0: 0x0c},
			[]int{20, 40, 80, 160}, "NOHT HT20 VHT20 HE20 HT40 VHT40 HE40 VHT80 HE80 VHT160 HE160"},
		{"6g EHT320", nl80211.WiphyBand{Band: nl80211.Band6GHz, HasHE: true, HEPhyCap0: 0x0c, HasEHT: true, EHTPhyCap0: 0x02},
			[]int{20, 40, 80, 160, 320}, "NOHT HE20 EHT20 HE40 EHT40 HE80 EHT80 HE160 EHT160 EHT320"},
		{"legacy 2g", nl80211.WiphyBand{Band: nl80211.Band2GHz}, []int{20}, "NOHT"},
	} {
		widths, _, htmodes := bandModes(c.band)
		if !reflect.DeepEqual(widths, c.widths) || strings.Join(htmodes, " ") != c.want {
			t.Errorf("%s: %v %v", c.name, widths, htmodes)
		}
	}
}

func TestManagementRadios(t *testing.T) {
	tr, p := fixture(t)
	ctx := context.Background()
	// A wired AP: br-lan.1 over br-lan over lan2 and two AP interfaces.
	tr.link("sys/class/net/br-lan.1/lower_br-lan", "../br-lan")
	for _, m := range []string{"lan2", "phy1-ap0", "phy0-ap0"} {
		tr.link("sys/class/net/br-lan/lower_"+m, "../"+m)
		tr.dir("sys/class/net/" + m)
	}
	tr.link("sys/class/net/phy1-ap0/phy80211", "../../ieee80211/phy1")
	tr.link("sys/class/net/phy0-ap0/phy80211", "../../ieee80211/phy0")
	if got := p.ManagementRadios(ctx, "br-lan.1"); len(got) != 0 || got == nil {
		t.Fatalf("wired: %#v", got)
	}
	// A mesh backhaul on the 5 GHz radio, bridged through batman-adv: that
	// radio carries the uplink. netifd does not list the mesh interface
	// here, so it resolves through its phy.
	tr.link("sys/class/net/br-lan/lower_bat0", "../bat0")
	tr.link("sys/class/net/bat0/lower_phy1-mesh0", "../phy1-mesh0")
	tr.link("sys/class/net/phy1-mesh0/phy80211", "../../ieee80211/phy1")
	nl := p.NL.(fakeNL)
	nl.ifaces = append(nl.ifaces, nl80211.Interface{Index: 9, Name: "phy1-mesh0", Wiphy: 1, Type: nl80211.IfTypeMeshPoint})
	p.NL = nl
	if got := p.ManagementRadios(ctx, "br-lan.1"); !reflect.DeepEqual(got, []string{"radio1"}) {
		t.Fatalf("mesh: %v", got)
	}
	// A routed station uplink: the route's own device names its radio.
	tr.dir("sys/class/net/phy0-sta0")
	tr.link("sys/class/net/phy0-sta0/phy80211", "../../ieee80211/phy0")
	if got := p.ManagementRadios(ctx, "phy0-sta0"); !reflect.DeepEqual(got, []string{"radio2"}) {
		t.Fatalf("sta: %v", got)
	}
	if got := p.ManagementRadios(ctx, ""); got == nil || len(got) != 0 {
		t.Fatal("no device")
	}
}

func TestIdentityUIDMap(t *testing.T) {
	for s, want := range map[string]bool{
		"         0          0 4294967295\n": true,
		"":                                   true,
		"0 100000 65536":                     false,
		"0 0 4294967295\n1 1 1":              false,
	} {
		if identityUIDMap(s) != want {
			t.Errorf("%q", s)
		}
	}
}
