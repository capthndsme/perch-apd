// Package sysinfo answers "what device is this": board, OpenWrt release,
// kernel, uptime and interface MACs.
package sysinfo

import (
	"bufio"
	"context"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// Board is `ubus call system board`, flattened.
type Board struct {
	Hostname     string
	Model        string
	BoardName    string
	System       string
	Kernel       string
	Distribution string // "OpenWrt"
	Release      string // "25.12.4"
	Revision     string
	Target       string // "ramips/mt7621"
	Description  string
}

// Ubus is the part of the ubus client this package uses.
type Ubus interface {
	Call(ctx context.Context, object, method string, args any, out any) error
}

// Info gathers device facts; board info is cached (it only changes with a
// hostname edit or a firmware upgrade).
type Info struct {
	Root string // "" = /
	Ubus Ubus   // nil = files only
	TTL  time.Duration

	mu      sync.Mutex
	board   Board
	boardAt time.Time
}

func (i *Info) path(p string) string {
	if i.Root == "" || i.Root == "/" {
		return p
	}
	return filepath.Join(i.Root, p)
}

// Board returns board and release information.
func (i *Info) Board(ctx context.Context) Board {
	i.mu.Lock()
	defer i.mu.Unlock()
	ttl := i.TTL
	if ttl <= 0 {
		ttl = time.Minute
	}
	if !i.boardAt.IsZero() && time.Since(i.boardAt) < ttl {
		return i.board
	}
	b := i.readBoard(ctx)
	i.board, i.boardAt = b, time.Now()
	return b
}

func (i *Info) readBoard(ctx context.Context) Board {
	var b Board
	if i.Ubus != nil {
		var r struct {
			Kernel    string `json:"kernel"`
			Hostname  string `json:"hostname"`
			System    string `json:"system"`
			Model     string `json:"model"`
			BoardName string `json:"board_name"`
			Release   struct {
				Distribution string `json:"distribution"`
				Version      string `json:"version"`
				Revision     string `json:"revision"`
				Target       string `json:"target"`
				Description  string `json:"description"`
			} `json:"release"`
		}
		if err := i.Ubus.Call(ctx, "system", "board", nil, &r); err == nil {
			b = Board{
				Hostname: r.Hostname, Model: r.Model, BoardName: r.BoardName, System: r.System,
				Kernel: r.Kernel, Distribution: r.Release.Distribution, Release: r.Release.Version,
				Revision: r.Release.Revision, Target: r.Release.Target, Description: r.Release.Description,
			}
		}
	}
	// Fill the gaps from files (non-OpenWrt hosts, or ubus down).
	rel := parseShellVars(i.read("/etc/openwrt_release"))
	osrel := parseShellVars(i.read("/etc/os-release"))
	fill := func(dst *string, vals ...string) {
		if *dst != "" {
			return
		}
		for _, v := range vals {
			if v != "" {
				*dst = v
				return
			}
		}
	}
	fill(&b.Distribution, rel["DISTRIB_ID"], osrel["NAME"])
	fill(&b.Release, rel["DISTRIB_RELEASE"], osrel["VERSION_ID"])
	fill(&b.Revision, rel["DISTRIB_REVISION"])
	fill(&b.Target, rel["DISTRIB_TARGET"])
	fill(&b.Description, rel["DISTRIB_DESCRIPTION"], osrel["PRETTY_NAME"])
	fill(&b.Model, firstLine(i.read("/tmp/sysinfo/model")), firstLine(i.read("/sys/firmware/devicetree/base/model")), firstLine(i.read("/sys/class/dmi/id/product_name")))
	fill(&b.BoardName, firstLine(i.read("/tmp/sysinfo/board_name")))
	fill(&b.System, cpuSystem(i.read("/proc/cpuinfo")))
	var u unix.Utsname
	if unix.Uname(&u) == nil {
		fill(&b.Hostname, unix.ByteSliceToString(u.Nodename[:]))
		fill(&b.Kernel, unix.ByteSliceToString(u.Release[:]))
	}
	return b
}

func (i *Info) read(p string) string {
	data, err := os.ReadFile(i.path(p))
	if err != nil {
		return ""
	}
	return string(data)
}

// Arch is the OpenWrt package architecture (DISTRIB_ARCH, e.g.
// mipsel_24kc), "" off OpenWrt.
func (i *Info) Arch() string {
	return parseShellVars(i.read("/etc/openwrt_release"))["DISTRIB_ARCH"]
}

// IsOpenWrt reports whether /etc/openwrt_release exists.
func (i *Info) IsOpenWrt() bool {
	_, err := os.Stat(i.path("/etc/openwrt_release"))
	return err == nil
}

// Uptime is seconds since boot.
func (i *Info) Uptime() (float64, error) {
	data, err := os.ReadFile(i.path("/proc/uptime"))
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0, os.ErrInvalid
	}
	return strconv.ParseFloat(fields[0], 64)
}

// MACs lists the lowercase hardware addresses of every non-loopback
// interface, sorted and de-duplicated (bridges repeat their ports' MACs).
func MACs() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagLoopback != 0 || len(ifi.HardwareAddr) != 6 {
			continue
		}
		mac := strings.ToLower(ifi.HardwareAddr.String())
		if mac == "00:00:00:00:00:00" || seen[mac] {
			continue
		}
		seen[mac] = true
		out = append(out, mac)
	}
	sort.Strings(out)
	if len(out) > 64 {
		out = out[:64]
	}
	return out
}

// parseShellVars reads KEY='value' / KEY="value" / KEY=value lines.
func parseShellVars(s string) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '\'' && v[len(v)-1] == '\'' || v[0] == '"' && v[len(v)-1] == '"') {
			v = v[1 : len(v)-1]
		}
		out[strings.TrimSpace(k)] = v
	}
	return out
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	return strings.TrimSpace(strings.TrimRight(s, "\x00"))
}

// cpuSystem mimics OpenWrt's `system` field: "system type" (MIPS),
// "Hardware" / "model name" / "Processor" elsewhere.
func cpuSystem(cpuinfo string) string {
	keys := []string{"system type", "Hardware", "model name", "Processor", "cpu model"}
	found := map[string]string{}
	for _, line := range strings.Split(cpuinfo, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		if _, dup := found[k]; !dup {
			found[k] = strings.TrimSpace(v)
		}
	}
	for _, k := range keys {
		if v := found[k]; v != "" {
			return v
		}
	}
	return ""
}
