package collect

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/capthndsme/perch-apd/internal/nl80211"
	"github.com/capthndsme/perch-apd/internal/prom"
	"github.com/capthndsme/perch-apd/internal/sysinfo"
	"github.com/capthndsme/perch-apd/internal/wireless"
	"golang.org/x/sys/unix"
)

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for p, content := range files {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func render(t *testing.T, c Collector) string {
	t.Helper()
	b := prom.NewBuilder()
	if err := c.Collect(context.Background(), b); err != nil {
		t.Fatalf("%s: %v", c.Name(), err)
	}
	return string(b.Bytes())
}

func mustContain(t *testing.T, got string, lines ...string) {
	t.Helper()
	for _, l := range lines {
		if !strings.Contains(got, l+"\n") {
			t.Errorf("missing line %q in:\n%s", l, got)
		}
	}
}

var procFixture = map[string]string{
	"/proc/stat": `cpu  100 0 50 1000 5 0 3 0 0 0
cpu0 60 0 30 500 3 0 2 0 0 0
cpu1 40 0 20 500 2 0 1 0 0 0
intr 123456 0 0 0
ctxt 987654
btime 1789796464
processes 4242
procs_running 1
procs_blocked 0
softirq 1 2 3
`,
	"/proc/loadavg": "0.04 0.03 0.02 1/80 4242\n",
	"/proc/meminfo": `MemTotal:         118768 kB
MemFree:           45024 kB
MemAvailable:      33556 kB
Active(anon):       1000 kB
HugePages_Total:       0
`,
	"/proc/net/dev": `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo:    4113      40    0    0    0     0          0         0     4113      40    0    0    0     0       0          0
phy0-ap0: 18148368942 9006173 0 3 0 0 0 0 465135575 7001002 0 0 0 0 0 0
`,
	"/proc/sys/net/netfilter/nf_conntrack_count": "7\n",
	"/proc/sys/net/netfilter/nf_conntrack_max":   "15360\n",
	"/proc/sys/fs/file-nr":                       "1024\t0\t10000\n",
	"/proc/sys/kernel/random/entropy_avail":      "256\n",
	"/proc/sys/kernel/random/poolsize":           "256\n",
	"/sys/class/net/br-lan/carrier":              "1\n",
	"/sys/class/net/br-lan/mtu":                  "1500\n",
	"/sys/class/net/br-lan/speed":                "-1\n",
	"/sys/class/net/br-lan/flags":                "0x1003\n",
	"/sys/class/net/br-lan/address":              "02:00:00:00:00:10\n",
	"/sys/class/net/br-lan/operstate":            "up\n",
	"/sys/class/net/lan1/speed":                  "1000\n",
	"/sys/class/net/lan1/address":                "02:00:00:00:00:12\n",
	"/etc/openwrt_release":                       "DISTRIB_ID='OpenWrt'\nDISTRIB_RELEASE='25.12.4'\nDISTRIB_REVISION='r1-abc'\nDISTRIB_TARGET='ramips/mt7621'\nDISTRIB_ARCH='mipsel_24kc'\n",
	"/tmp/sysinfo/model":                         "Example AP v1\n",
	"/tmp/sysinfo/board_name":                    "example,ap-v1\n",
	"/proc/cpuinfo":                              "system type\t\t: MediaTek MT7621 ver:1 eco:3\nmachine\t\t\t: Example AP v1\n",
	"/proc/uptime":                               "193172.52 380000.00\n",
}

func TestNodeCollectors(t *testing.T) {
	fs := FS{Root: writeTree(t, procFixture)}

	mustContain(t, render(t, Stat{FS: fs}),
		"# TYPE node_cpu_seconds_total counter",
		`node_cpu_seconds_total{cpu="cpu0",mode="user"} 0.6`,
		`node_cpu_seconds_total{cpu="cpu1",mode="idle"} 5`,
		"node_boot_time_seconds 1789796464",
		"node_context_switches_total 987654",
		"node_intr_total 123456",
		"node_forks_total 4242",
		"node_procs_running_total 1",
		"node_procs_blocked_total 0",
	)
	mustContain(t, render(t, Loadavg{FS: fs}), "node_load1 0.04", "node_load5 0.03", "node_load15 0.02")
	mustContain(t, render(t, Meminfo{FS: fs}),
		"node_memory_MemTotal_bytes 121618432",
		"node_memory_MemAvailable_bytes 34361344",
		"node_memory_Active_anon_bytes 1024000",
		"node_memory_HugePages_Total_bytes 0",
	)
	netdev := render(t, Netdev{FS: fs})
	mustContain(t, netdev,
		"# TYPE node_network_receive_bytes_total counter",
		`node_network_receive_bytes_total{device="lo"} 4113`,
		`node_network_receive_bytes_total{device="phy0-ap0"} 18148368942`,
		`node_network_receive_drop_total{device="phy0-ap0"} 3`,
		`node_network_transmit_bytes_total{device="phy0-ap0"} 465135575`,
		`node_network_transmit_packets_total{device="phy0-ap0"} 7001002`,
	)
	if strings.Contains(netdev, "face") {
		t.Fatal("header parsed as a device")
	}
	netclass := render(t, Netclass{FS: fs})
	mustContain(t, netclass,
		`node_network_carrier{device="br-lan"} 1`,
		`node_network_mtu_bytes{device="br-lan"} 1500`,
		`node_network_flags{device="br-lan"} 4099`,
		`node_network_speed_bytes{device="lan1"} 125000000`,
		`node_network_info{device="br-lan",address="02:00:00:00:00:10",broadcast="",duplex="",operstate="up",ifalias=""} 1`,
	)
	if strings.Contains(netclass, `node_network_speed_bytes{device="br-lan"}`) {
		t.Fatal("negative speed exported")
	}
	mustContain(t, render(t, Conntrack{FS: fs}), "node_nf_conntrack_entries 7", "node_nf_conntrack_entries_limit 15360")
	mustContain(t, render(t, Filefd{FS: fs}), "node_filefd_allocated 1024", "node_filefd_maximum 10000")
	mustContain(t, render(t, Entropy{FS: fs}), "node_entropy_available_bits 256", "node_entropy_pool_size_bits 256")
	mustContain(t, render(t, Time{Now: func() time.Time { return time.Unix(1789989636, 5e8) }}), "node_time_seconds 1789989636")

	un := render(t, Uname{Uname: func(u *unix.Utsname) error {
		copy(u.Sysname[:], "Linux")
		copy(u.Nodename[:], "ap-garage")
		copy(u.Release[:], "6.12.87")
		copy(u.Version[:], "#0 SMP")
		copy(u.Machine[:], "mips")
		copy(u.Domainname[:], "(none)")
		return nil
	}})
	mustContain(t, un, `node_uname_info{domainname="(none)",machine="mips",nodename="ap-garage",release="6.12.87",sysname="Linux",version="#0 SMP"} 1`)

	info := &sysinfo.Info{Root: fs.Root}
	mustContain(t, render(t, OpenWrt{Info: info}),
		`node_openwrt_info{board_name="example,ap-v1",id="OpenWrt",model="Example AP v1",release="25.12.4",revision="r1-abc",system="MediaTek MT7621 ver:1 eco:3",target="ramips/mt7621"} 1`)
}

func TestConntrackAbsentIsNotAnError(t *testing.T) {
	fs := FS{Root: t.TempDir()}
	if got := render(t, Conntrack{FS: fs}); got != "" {
		t.Fatalf("got %q", got)
	}
}

// --- Wi-Fi ---

type fakeNL struct {
	ifaces   []nl80211.Interface
	stations map[int][]nl80211.Station
	survey   map[int][]nl80211.SurveyEntry
	reg      string
}

func (f *fakeNL) Interfaces() ([]nl80211.Interface, error)    { return f.ifaces, nil }
func (f *fakeNL) Stations(i int) ([]nl80211.Station, error)   { return f.stations[i], nil }
func (f *fakeNL) Survey(i int) ([]nl80211.SurveyEntry, error) { return f.survey[i], nil }
func (f *fakeNL) RegDomain() (string, error)                  { return f.reg, nil }

type fakeUbus struct {
	replies map[string]string
	calls   []string
}

func (f *fakeUbus) Call(_ context.Context, object, method string, args any, out any) error {
	f.calls = append(f.calls, object+" "+method)
	r, ok := f.replies[object+" "+method]
	if !ok {
		return errors.New("not found")
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal([]byte(r), out)
}

func mac(s string) net.HardwareAddr { m, _ := net.ParseMAC(s); return m }

const wirelessStatus = `{
  "radio0": {"up": true, "config": {"band": "2g", "channel": "6", "htmode": "HE20", "country": "TW"},
    "interfaces": [{"section": "default_radio0", "ifname": "phy0-ap0", "config": {"mode": "ap", "ssid": "Example"}}]},
  "radio1": {"up": true, "config": {"band": "5g", "channel": "auto", "country": "PH"},
    "interfaces": [
      {"section": "default_radio1", "ifname": "phy1-ap0", "config": {"mode": "ap", "ssid": "Example 5"}},
      {"section": "disabled", "config": {"mode": "ap", "ssid": "Off"}}
    ]}
}`

func wifiSource() (*wireless.Source, *fakeUbus) {
	nl := &fakeNL{
		ifaces: []nl80211.Interface{
			{Index: 10, Name: "phy0-ap0", Wiphy: 0, Type: nl80211.IfTypeAP, MAC: mac("02:00:00:00:00:11"), SSID: "Example", FrequencyMHz: 2437},
			{Index: 11, Name: "phy1-ap0", Wiphy: 1, Type: nl80211.IfTypeAP, MAC: mac("02:00:00:00:00:21"), SSID: "Example 5", FrequencyMHz: 5500},
			{Index: 12, Name: "phy1-ap0.sta1", Wiphy: 1, Type: nl80211.IfTypeAPVLAN, MAC: mac("02:00:00:00:00:22")},
		},
		stations: map[int][]nl80211.Station{
			10: {
				{MAC: mac("02:00:00:00:00:01"), InactiveMs: 10, Signal: -44, HasSignal: true, TxBitrate: 722, HasTxBitrate: true,
					TxBitrate16: 722, HasTxBitrate16: true, RxBitrate: 722, HasRxBitrate: true, ExpectedThroughputKbps: 64960,
					HasExpectedThroughput: true, RxBytes: 18148368942, TxBytes: 465135575, HasBytes: true, RxPackets: 9006173,
					TxPackets: 7001002, HasPackets: true, HasConnected: true, ConnectedSeconds: 3600, HasFlags: true, Authorized: true},
				{MAC: mac("02:00:00:00:00:02"), InactiveMs: 4600, Signal: -45, HasSignal: true, TxBitrate16: 5851, HasTxBitrate16: true,
					TxBitrate: 5851, HasTxBitrate: true},
			},
		},
		survey: map[int][]nl80211.SurveyEntry{
			10: {{FrequencyMHz: 2412, Noise: -95, HasNoise: true}, {FrequencyMHz: 2437, Noise: -91, HasNoise: true, InUse: true}},
		},
		reg: "PH",
	}
	ub := &fakeUbus{replies: map[string]string{"network.wireless status": wirelessStatus}}
	return &wireless.Source{NL: nl, Ubus: ub}, ub
}

func TestWifiCollectorMatchesIwinfo(t *testing.T) {
	src, _ := wifiSource()
	got := render(t, Wifi{Src: src})
	// Two stations: signal (-44*0 + -44)/1 = -44, then (-44*1 + -45)/2 = -44 (truncation).
	// quality floor(100/70 * 66) = 94; bitrate (722 then (722+5851)/2 = 3286) * 100.
	labels0 := `{mode="Master",ifname="phy0-ap0",ssid="Example",channel="6",device="radio0",bssid="02:00:00:00:00:11",country="PH",frequency="2437"}`
	labels1 := `{mode="Master",ifname="phy1-ap0",ssid="Example 5",channel="100",device="radio1",bssid="02:00:00:00:00:21",country="PH",frequency="5500"}`
	mustContain(t, got,
		"# TYPE wifi_network_quality gauge",
		"wifi_network_quality"+labels0+" 94",
		"wifi_network_noise_dbm"+labels0+" -91",
		"wifi_network_bitrate"+labels0+" 328600",
		"wifi_network_signal_dbm"+labels0+" -44",
		"wifi_network_quality"+labels1+" 0",
		"wifi_network_noise_dbm"+labels1+" 0",
		"wifi_network_bitrate"+labels1+" 0",
		"wifi_network_signal_dbm"+labels1+" -255",
	)
	if strings.Contains(got, "sta1") {
		t.Fatal("AP_VLAN interface exported")
	}
}

func TestWifiStationsCollector(t *testing.T) {
	src, _ := wifiSource()
	got := render(t, WifiStations{Src: src})
	l := `{ifname="phy0-ap0",mac="02:00:00:00:00:01"}`
	mustContain(t, got,
		"# TYPE wifi_station_signal_dbm gauge",
		"wifi_station_signal_dbm"+l+" -44",
		"wifi_station_inactive_milliseconds"+l+" 10",
		"wifi_station_expected_throughput_kilobits_per_second"+l+" 64960",
		"wifi_station_transmit_kilobits_per_second"+l+" 72200",
		"wifi_station_receive_kilobits_per_second"+l+" 72200",
		"# TYPE wifi_station_transmit_bytes_total counter",
		"wifi_station_transmit_bytes_total"+l+" 465135575",
		"wifi_station_receive_bytes_total"+l+" 18148368942",
		"wifi_station_transmit_packets_total"+l+" 7001002",
		"wifi_station_receive_packets_total"+l+" 9006173",
		`wifi_stations{ifname="phy0-ap0"} 2`,
		`wifi_stations{ifname="phy1-ap0"} 0`,
	)
	l2 := `{ifname="phy0-ap0",mac="02:00:00:00:00:02"}`
	if strings.Contains(got, "wifi_station_expected_throughput_kilobits_per_second"+l2) ||
		strings.Contains(got, "wifi_station_transmit_bytes_total"+l2) {
		t.Fatal("absent values exported")
	}
}

func TestInventoryFallsBackToNl80211WithoutUbus(t *testing.T) {
	src, _ := wifiSource()
	src.Ubus = nil
	inv, err := src.Inventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Interfaces) != 2 || inv.Interfaces[0].Radio != "phy0" || inv.Interfaces[1].Radio != "phy1" {
		t.Fatalf("%+v", inv.Interfaces)
	}
	if inv.Interfaces[0].Band != "2.4" || inv.Interfaces[1].Band != "5" || inv.Interfaces[1].Channel != 100 {
		t.Fatalf("%+v", inv.Interfaces)
	}
}

func TestInventoryCachesUbusStatus(t *testing.T) {
	src, ub := wifiSource()
	for i := 0; i < 3; i++ {
		if _, err := src.Inventory(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(ub.calls) != 1 {
		t.Fatalf("network.wireless status called %d times", len(ub.calls))
	}
	inv, _ := src.Inventory(context.Background())
	if len(inv.Radios) != 2 || inv.Radios[0].Name != "radio0" || inv.Radios[0].Channel != 6 || inv.Radios[1].Channel != 0 {
		t.Fatalf("radios %+v", inv.Radios)
	}
}

func TestRegistryGatherSelectsAndReportsScrapes(t *testing.T) {
	fs := FS{Root: writeTree(t, procFixture)}
	r := NewRegistry(nil, Loadavg{FS: fs}, Conntrack{FS: fs}, Filefd{FS: FS{Root: t.TempDir()}})
	got := string(r.Gather(context.Background(), []string{"loadavg", "filefd", "nope"}))
	mustContain(t, got,
		"node_load1 0.04",
		`node_scrape_collector_success{collector="loadavg"} 1`,
		`node_scrape_collector_success{collector="filefd"} 0`,
	)
	if strings.Contains(got, "conntrack") {
		t.Fatal("unselected collector ran")
	}
	if strings.Join(r.Names(), ",") != "loadavg,conntrack,filefd" {
		t.Fatal(r.Names())
	}
}

func TestIwinfoQuality(t *testing.T) {
	for sig, want := range map[int]int{-44: 94, -40: 100, -30: 100, -110: 0, -120: 0, -75: 50, 60: 85} {
		if got := iwinfoQualityPercent(sig); got != want {
			t.Errorf("quality(%d) = %d, want %d", sig, got, want)
		}
	}
}
