package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/capthndsme/perch-agentkit/hoststat"
	"github.com/capthndsme/perch-apd/internal/handlers"
	"github.com/capthndsme/perch-apd/internal/sysinfo"
)

// `perch-apd ports` reads /sys/class/net and board.json and nothing else: no
// configuration (this one would not even load), no ubus or nl80211, no
// controller. It is what runs from /tmp on a live AP.
func TestPortsCommandReadsOnlySysfs(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"etc/board.json": `{"network":{"lan":{"ports":["lan1","lan2"]},"wan":{"device":"wan"}}}`,
		// the DSA conduit: not a port
		"sys/class/net/eth0/type": "1", "sys/class/net/eth0/ifindex": "2", "sys/class/net/eth0/iflink": "2",
		"sys/class/net/eth0/dsa/tagging": "mtk", "sys/class/net/eth0/device/uevent": "DRIVER=example",
		// DSA user ports
		"sys/class/net/lan1/type": "1", "sys/class/net/lan1/ifindex": "4", "sys/class/net/lan1/iflink": "2",
		"sys/class/net/lan1/uevent": "DEVTYPE=dsa", "sys/class/net/lan1/flags": "0x1003", "sys/class/net/lan1/carrier": "1",
		"sys/class/net/lan1/operstate": "up", "sys/class/net/lan1/speed": "1000", "sys/class/net/lan1/duplex": "full",
		"sys/class/net/lan1/address": "02:00:00:00:00:10", "sys/class/net/lan1/of_node/label": "lan1\x00",
		"sys/class/net/lan2/type": "1", "sys/class/net/lan2/ifindex": "5", "sys/class/net/lan2/iflink": "2",
		"sys/class/net/lan2/uevent": "DEVTYPE=dsa", "sys/class/net/lan2/flags": "0x1003", "sys/class/net/lan2/carrier": "0",
		"sys/class/net/lan2/operstate": "lowerlayerdown", "sys/class/net/lan2/speed": "-1", "sys/class/net/lan2/duplex": "unknown",
		// a second MAC as WAN, bridged into LAN
		"sys/class/net/wan/type": "1", "sys/class/net/wan/ifindex": "3", "sys/class/net/wan/iflink": "3",
		"sys/class/net/wan/brport/state": "3", "sys/class/net/wan/device/uevent": "DRIVER=example",
		"sys/class/net/wan/flags": "0x1103", "sys/class/net/wan/carrier": "0", "sys/class/net/wan/operstate": "down",
		// the bridge and a wireless interface: not ports
		"sys/class/net/br-lan/type": "1", "sys/class/net/br-lan/ifindex": "8", "sys/class/net/br-lan/iflink": "8",
		"sys/class/net/br-lan/uevent": "DEVTYPE=bridge", "sys/class/net/br-lan/bridge/forward_delay": "1500",
		"sys/class/net/phy0-ap0/type": "1", "sys/class/net/phy0-ap0/ifindex": "10", "sys/class/net/phy0-ap0/iflink": "10",
		"sys/class/net/phy0-ap0/uevent": "DEVTYPE=wlan", "sys/class/net/phy0-ap0/phy80211/index": "0",
		"sys/class/net/phy0-ap0/device/uevent": "DRIVER=example-wifi",
	}
	for p, v := range files {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(v+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	badConfig := filepath.Join(t.TempDir(), "perch-apd")
	os.WriteFile(badConfig, []byte("config agent 'main'\n\toption controller 'ftp://x'\n"), 0o600)

	var out bytes.Buffer
	oldOut, oldFS, oldDevice := stdout, sysFS, newDevice
	defer func() { stdout, sysFS, newDevice = oldOut, oldFS, oldDevice }()
	stdout, sysFS = &out, hoststat.FS{Root: root}
	newDevice = func(*slog.Logger) *device {
		t.Fatal("ports opened the device (ubus, nl80211)")
		return nil
	}
	if code := run([]string{"ports", "--config", badConfig}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	var ports []hoststat.Port
	if err := json.Unmarshal(out.Bytes(), &ports); err != nil {
		t.Fatalf("%v: %s", err, out.Bytes())
	}
	var got []string
	for _, p := range ports {
		got = append(got, p.Name+":"+p.Role)
	}
	if strings.Join(got, ",") != "wan:wan,lan1:lan,lan2:lan" || !strings.Contains(out.String(), "\n  {\n    \"name\": \"wan\"") {
		t.Fatalf("ports %v\n%s", got, out.String())
	}
	if !*ports[1].Carrier || *ports[1].SpeedMbps != 1000 || ports[2].SpeedMbps != nil || ports[0].Medium != "copper" {
		t.Fatalf("link state %+v", ports)
	}

	// Without /sys/class/net: an error, not an empty list.
	out.Reset()
	sysFS = hoststat.FS{Root: t.TempDir()}
	if code := run([]string{"ports"}); code != 1 || out.Len() != 0 {
		t.Fatalf("exit %d, output %q", code, out.String())
	}

	out.Reset()
	if run([]string{"help"}); !strings.Contains(out.String(), "perch-apd ports") {
		t.Fatalf("usage without the ports command:\n%s", out.String())
	}
}

// `perch-apd info` shows the capabilities the daemon would announce:
// "ports" only while the configuration leaves port reporting on.
func TestInfoFollowsTheConfiguredPorts(t *testing.T) {
	root := t.TempDir()
	for p, v := range map[string]string{
		"sys/class/net/lan1/type": "1", "sys/class/net/lan1/ifindex": "2", "sys/class/net/lan1/iflink": "2",
		"sys/class/net/lan1/device/uevent": "DRIVER=example",
	} {
		os.MkdirAll(filepath.Join(root, filepath.Dir(p)), 0o755)
		os.WriteFile(filepath.Join(root, p), []byte(v+"\n"), 0o644)
	}
	var out bytes.Buffer
	oldOut, oldDevice := stdout, newDevice
	defer func() { stdout, newDevice = oldOut, oldDevice }()
	stdout = &out
	newDevice = func(*slog.Logger) *device {
		d := &device{info: &sysinfo.Info{Root: root}, ports: &hoststat.PortReader{FS: hoststat.FS{Root: root}}}
		d.deps = &handlers.Deps{Info: d.info, Ports: d.ports}
		return d
	}
	dir := t.TempDir()
	for _, tc := range []struct{ conf, want string }{
		{"", "metrics,ports"}, // no file: the defaults
		{"config agent 'main'\n\toption ports '1'\n", "metrics,ports"},
		{"config agent 'main'\n\toption ports '0'\n", "metrics"},
	} {
		path := filepath.Join(dir, "perch-apd")
		os.Remove(path)
		if tc.conf != "" {
			os.WriteFile(path, []byte(tc.conf), 0o600)
		}
		out.Reset()
		if code := run([]string{"info", "--config", path}); code != 0 {
			t.Fatalf("exit %d", code)
		}
		var info struct{ Capabilities []string }
		if err := json.Unmarshal(out.Bytes(), &info); err != nil || strings.Join(info.Capabilities, ",") != tc.want {
			t.Fatalf("config %q: capabilities %v (%v)", tc.conf, info.Capabilities, err)
		}
	}
}

func TestOneProcOnlyOn32BitWithoutAnExplicitGOMAXPROCS(t *testing.T) {
	unset := func(string) string { return "" }
	set := func(k string) string {
		if k == "GOMAXPROCS" {
			return "2"
		}
		return ""
	}
	for _, tc := range []struct {
		intSize int
		getenv  func(string) string
		want    bool
	}{
		{32, unset, true},
		{32, set, false},
		{64, unset, false},
		{64, set, false},
	} {
		if got := oneProc(tc.intSize, tc.getenv); got != tc.want {
			t.Errorf("oneProc(%d, GOMAXPROCS=%q) = %v", tc.intSize, tc.getenv("GOMAXPROCS"), got)
		}
	}
}
