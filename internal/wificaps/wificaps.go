// Package wificaps finds what the access point's Wi-Fi is and can do: the
// device facts behind `wifi.capabilities` (Wi-Fi design protocol.md 3.1).
// That is the OpenWrt release and packages that matter to Wi-Fi, the
// hostapd build's features, the regulatory domain, every radio of
// /etc/config/wireless with the channels, widths and modes its phy offers,
// the trunk port towards the gateway, the netifd networks, and which radios
// carry the AP's own uplink (the management path, 3.8).
//
// Everything is read (files, nl80211, `ubus call ... status`, `hostapd
// -v<feature>`); nothing is ever changed, so `perch-apd wifi caps` is safe to
// run from /tmp on a live AP. Capability, not version sniffing: a feature is
// what the build or the kernel says it has.
package wificaps

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/capthndsme/perch-agentkit/openwrt/pkgdb"
	"github.com/capthndsme/perch-agentkit/openwrt/uci"
	"github.com/capthndsme/perch-apd/internal/nl80211"
)

// NL is the part of the nl80211 client the probe uses.
type NL interface {
	Interfaces() ([]nl80211.Interface, error)
	Wiphys() ([]nl80211.Wiphy, error)
	RegDomains() ([]nl80211.RegDomain, error)
}

// Ubus is the part of the ubus client the probe uses.
type Ubus interface {
	Call(ctx context.Context, object, method string, args any, out any) error
	List(ctx context.Context, pattern string) ([]string, error)
}

// Runner runs a command and returns its exit status; err only when it could
// not run at all.
type Runner func(ctx context.Context, name string, args ...string) (exitCode int, err error)

// ExecRunner runs the command for real, output discarded.
func ExecRunner(ctx context.Context, name string, args ...string) (int, error) {
	err := exec.CommandContext(ctx, name, args...).Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	if err != nil {
		return -1, err
	}
	return 0, nil
}

// Prober gathers the facts. Safe for concurrent use.
type Prober struct {
	// Root is where the files are read ("" = /; a fixture tree in tests).
	Root string
	NL   NL   // nil = no nl80211 (no radios)
	Ubus Ubus // nil = no ubus (not OpenWrt)
	Run  Runner

	mu       sync.Mutex
	featKey  string
	features map[string]bool
}

func (p *Prober) path(s string) string {
	if p.Root == "" || p.Root == "/" {
		return s
	}
	return filepath.Join(p.Root, s)
}

func (p *Prober) read(s string) string {
	b, err := os.ReadFile(p.path(s))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func (p *Prober) exists(s string) bool {
	_, err := os.Stat(p.path(s))
	return err == nil
}

func (p *Prober) list(dir string) []string {
	entries, err := os.ReadDir(p.path(dir))
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func (p *Prober) run(ctx context.Context, name string, args ...string) (int, error) {
	if p.Run != nil {
		return p.Run(ctx, name, args...)
	}
	return ExecRunner(ctx, name, args...)
}

// Facts are the device facts of wifi.capabilities. The Wi-Fi config plane
// adds its own fields (access, transport, hashes, apply state, groups) next
// to them.
type Facts struct {
	OpenWrt *OpenWrt `json:"openwrt,omitempty"`
	// PackageManager is "opkg", "apk" or "" (no database).
	PackageManager string `json:"packageManager,omitempty"`
	// Packages: name → version of the installed packages Wi-Fi depends on
	// (WatchPackages).
	Packages map[string]string `json:"packages"`
	// WifiScripts is "ucode" (netifd's Wi-Fi handling in ucode, 25.12),
	// "shell" (mac80211.sh) or "" (none).
	WifiScripts string `json:"wifiScripts"`
	// Schema: /usr/share/schema/wireless.wifi-iface.json exists (25.12).
	Schema     bool       `json:"schema"`
	Hostapd    *Hostapd   `json:"hostapd"` // nil = no hostapd/wpad
	Regulatory Regulatory `json:"regulatory"`
	Radios     []Radio    `json:"radios"`
	Trunk      *Trunk     `json:"trunk"`
	Networks   []Network  `json:"networks"`
	// Problems are what could not be read (the rest is still reported).
	Problems []string `json:"problems,omitempty"`
}

// OpenWrt is /etc/openwrt_release and the board name.
type OpenWrt struct {
	Release  string `json:"release"`
	Revision string `json:"revision,omitempty"`
	Target   string `json:"target,omitempty"`
	Arch     string `json:"arch,omitempty"`
	Board    string `json:"board,omitempty"`
}

// Network is one netifd interface.
type Network struct {
	Name   string `json:"name"`
	Device string `json:"device,omitempty"` // the L3 device
	Proto  string `json:"proto,omitempty"`
	Up     bool   `json:"up"`
}

// WatchPackages are the packages reported in Facts.Packages; a trailing
// "*" is a prefix.
var WatchPackages = []string{"wpad*", "hostapd*", "wifi-scripts", "iw", "iw-full", "iwinfo", "usteer", "dawn", "umdns", "luci-base"}

// Facts gathers everything. It never fails: what cannot be read is left
// out and named in Problems.
func (p *Prober) Facts(ctx context.Context) Facts {
	f := Facts{Packages: map[string]string{}, Radios: []Radio{}, Networks: []Network{}}
	problem := func(what string, err error) {
		f.Problems = append(f.Problems, what+": "+err.Error())
	}
	f.OpenWrt = p.openwrt()

	db := pkgdb.DB{Root: p.Root}
	f.PackageManager = db.Manager()
	installed, err := db.Installed()
	if err != nil && !errors.Is(err, pkgdb.ErrNoDatabase) {
		problem("packages", err)
	}
	f.Packages = watched(installed)

	switch {
	case p.exists("/usr/share/ucode/wifi"):
		f.WifiScripts = "ucode"
	case p.exists("/lib/netifd/wireless/mac80211.uc"):
		f.WifiScripts = "ucode"
	case p.exists("/lib/netifd/wireless/mac80211.sh"):
		f.WifiScripts = "shell"
	}
	f.Schema = p.exists("/usr/share/schema/wireless.wifi-iface.json")
	f.Hostapd = p.hostapd(ctx, installed)

	var wiphys []nl80211.Wiphy
	var ifaces []nl80211.Interface
	var regs []nl80211.RegDomain
	if p.NL != nil {
		if wiphys, err = p.NL.Wiphys(); err != nil {
			problem("nl80211 wiphy", err)
		}
		if ifaces, err = p.NL.Interfaces(); err != nil {
			problem("nl80211 interfaces", err)
		}
		if regs, err = p.NL.RegDomains(); err != nil {
			problem("nl80211 regulatory", err)
		}
	}
	f.Regulatory = p.regulatory(wiphys, regs)
	st, err := p.wirelessStatus(ctx)
	if err != nil && p.Ubus != nil {
		problem("network.wireless status", err)
	}
	radios, err := p.radios(st, wiphys, ifaces, f.Regulatory, regs)
	if err != nil && !errors.Is(err, uci.ErrNoConfig) {
		problem("wireless config", err)
	}
	f.Radios = radios
	f.Trunk = p.trunk()
	if nets, err := p.networks(ctx); err == nil {
		f.Networks = nets
	} else if p.Ubus != nil {
		problem("network.interface dump", err)
	}
	return f
}

func (p *Prober) openwrt() *OpenWrt {
	raw := p.read("/etc/openwrt_release")
	if raw == "" {
		return nil
	}
	o := &OpenWrt{}
	for _, line := range strings.Split(raw, "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `'"`)
		switch strings.TrimSpace(k) {
		case "DISTRIB_RELEASE":
			o.Release = v
		case "DISTRIB_REVISION":
			o.Revision = v
		case "DISTRIB_TARGET":
			o.Target = v
		case "DISTRIB_ARCH":
			o.Arch = v
		}
	}
	o.Board = p.read("/tmp/sysinfo/board_name")
	return o
}

func watched(installed map[string]pkgdb.Package) map[string]string {
	out := map[string]string{}
	for name, pkg := range installed {
		for _, w := range WatchPackages {
			if prefix, ok := strings.CutSuffix(w, "*"); ok && strings.HasPrefix(name, prefix) || name == w {
				out[name] = pkg.Version
				break
			}
		}
	}
	return out
}

// flexString decodes a JSON string, number or bool as its text (netifd
// reports `channel` as "36" or 36, `txpower` as a number).
type flexString string

func (s *flexString) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var v string
		if err := json.Unmarshal(b, &v); err != nil {
			return err
		}
		*s = flexString(v)
		return nil
	}
	if string(b) == "null" {
		*s = ""
		return nil
	}
	*s = flexString(b)
	return nil
}

// radioStatus is one radio of `ubus call network.wireless status`.
type radioStatus struct {
	Up               bool `json:"up"`
	Pending          bool `json:"pending"`
	Disabled         bool `json:"disabled"`
	RetrySetupFailed bool `json:"retry_setup_failed"`
	Config           struct {
		Path    flexString `json:"path"`
		Band    flexString `json:"band"`
		HWMode  flexString `json:"hwmode"`
		Channel flexString `json:"channel"`
		HTMode  flexString `json:"htmode"`
		Country flexString `json:"country"`
		TxPower flexString `json:"txpower"`
	} `json:"config"`
	Interfaces []struct {
		Section string `json:"section"`
		Ifname  string `json:"ifname"`
		Config  struct {
			Mode flexString `json:"mode"`
		} `json:"config"`
	} `json:"interfaces"`
}

func (p *Prober) wirelessStatus(ctx context.Context) (map[string]radioStatus, error) {
	if p.Ubus == nil {
		return nil, errors.New("no ubus")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var st map[string]radioStatus
	if err := p.Ubus.Call(ctx, "network.wireless", "status", nil, &st); err != nil {
		return nil, err
	}
	return st, nil
}

func (p *Prober) networks(ctx context.Context) ([]Network, error) {
	if p.Ubus == nil {
		return nil, errors.New("no ubus")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var dump struct {
		Interface []struct {
			Interface string `json:"interface"`
			Up        bool   `json:"up"`
			Proto     string `json:"proto"`
			Device    string `json:"device"`
			L3Device  string `json:"l3_device"`
		} `json:"interface"`
	}
	if err := p.Ubus.Call(ctx, "network.interface", "dump", nil, &dump); err != nil {
		return nil, err
	}
	out := []Network{}
	for _, i := range dump.Interface {
		if i.Interface == "" || i.Interface == "loopback" {
			continue
		}
		dev := i.L3Device
		if dev == "" {
			dev = i.Device
		}
		out = append(out, Network{Name: i.Interface, Device: dev, Proto: i.Proto, Up: i.Up})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out, nil
}

// wirelessConfig reads /etc/config/wireless (never reported: only the
// wifi-device sections' hardware options are used).
func (p *Prober) wirelessConfig() (*uci.Config, error) {
	l, err := uci.Files{Dir: p.path(uci.DefaultDir)}.Load("wireless")
	if err != nil {
		return nil, err
	}
	return l.Config, nil
}

func atoi(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}
