package wificaps

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/capthndsme/perch-agentkit/openwrt/pkgdb"
	"github.com/capthndsme/perch-apd/internal/nl80211"
)

// Hostapd is the hostapd build: which binary, which package, and what it
// was built with.
type Hostapd struct {
	Binary  string `json:"binary"`            // /usr/sbin/wpad (hostapd is a link to it)
	Variant string `json:"variant,omitempty"` // wpad-basic-mbedtls, wpad-mesh-openssl, hostapd-openssl, ...
	// Ubus: at least one BSS has hostapd's ubus object (hostapd.<ifname>).
	Ubus     bool            `json:"ubus"`
	Features map[string]bool `json:"features"`
}

// FeatureNames are the features asked with `hostapd -v<name>` (exit 0 =
// built in), the probe LuCI's getFeatures uses.
var FeatureNames = []string{"11r", "sae", "owe", "eap", "wps", "mesh", "11ac", "11ax", "11be", "acs", "ocv"}

func (p *Prober) hostapd(ctx context.Context, installed map[string]pkgdb.Package) *Hostapd {
	bin := ""
	for _, name := range []string{"/usr/sbin/hostapd", "/usr/sbin/wpad"} {
		if _, err := os.Lstat(p.path(name)); err == nil {
			bin = name
			break
		}
	}
	if bin == "" {
		return nil
	}
	h := &Hostapd{Binary: bin, Variant: variant(installed), Features: p.hostapdFeatures(ctx, bin)}
	if real, err := filepath.EvalSymlinks(p.path(bin)); err == nil {
		if p.Root != "" && p.Root != "/" {
			real = "/" + strings.TrimPrefix(strings.TrimPrefix(real, filepath.Clean(p.Root)), "/")
		}
		h.Binary = real
	}
	if p.Ubus != nil {
		lctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		names, err := p.Ubus.List(lctx, "hostapd.*")
		cancel()
		h.Ubus = err == nil && len(names) > 0
	}
	return h
}

// variant is the installed package that provides hostapd: a wpad-* one,
// else a hostapd-* one (not hostapd-common or -utils).
func variant(installed map[string]pkgdb.Package) string {
	var wpad, hostapd []string
	for name := range installed {
		switch {
		case strings.HasPrefix(name, "wpad"):
			wpad = append(wpad, name)
		case name == "hostapd" || strings.HasPrefix(name, "hostapd-") && name != "hostapd-common" && name != "hostapd-utils":
			hostapd = append(hostapd, name)
		}
	}
	for _, list := range [][]string{wpad, hostapd} {
		if len(list) > 0 {
			sort.Strings(list)
			return list[0]
		}
	}
	return ""
}

// hostapdFeatures asks the binary once per build (its size and time).
func (p *Prober) hostapdFeatures(ctx context.Context, bin string) map[string]bool {
	key := ""
	if st, err := os.Stat(p.path(bin)); err == nil {
		key = fmt.Sprintf("%s:%d:%d", bin, st.Size(), st.ModTime().UnixNano())
	}
	p.mu.Lock()
	if key != "" && key == p.featKey {
		out := make(map[string]bool, len(p.features))
		for k, v := range p.features {
			out[k] = v
		}
		p.mu.Unlock()
		return out
	}
	p.mu.Unlock()
	out := make(map[string]bool, len(FeatureNames))
	ran := false
	for _, f := range FeatureNames {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		code, err := p.run(cctx, bin, "-v"+f)
		cancel()
		if err == nil {
			ran = true
		}
		out[f] = err == nil && code == 0
	}
	if ran && key != "" {
		p.mu.Lock()
		p.featKey, p.features = key, out
		p.mu.Unlock()
	}
	return out
}

// Regulatory is the regulatory domain.
type Regulatory struct {
	// Global is the kernel's global domain ("PH", "US", "00" = world, ""
	// unknown).
	Global string `json:"global"`
	// DFSRegion of the global domain: FCC, ETSI, JP or "".
	DFSRegion string `json:"dfsRegion,omitempty"`
	// Settable: writing a radio's `country` takes effect. False in an
	// unprivileged container (Reason "user_namespace": setting it fails with
	// EPERM) and without nl80211 ("no_wifi").
	Settable bool    `json:"settable"`
	Reason   *string `json:"reason"`
	// SelfManaged are the phys whose driver sets the domain itself (ath11k):
	// `country` does not change theirs.
	SelfManaged []string `json:"selfManaged"`
}

func (p *Prober) regulatory(wiphys []nl80211.Wiphy, regs []nl80211.RegDomain) Regulatory {
	r := Regulatory{SelfManaged: []string{}, Settable: true}
	for _, d := range regs {
		if d.Wiphy < 0 {
			r.Global, r.DFSRegion = d.Alpha2, nl80211.DFSRegionName(d.DFSRegion)
		}
	}
	for _, w := range wiphys {
		if w.SelfManagedReg {
			r.SelfManaged = append(r.SelfManaged, w.Name)
		}
	}
	reason := func(s string) {
		r.Settable, r.Reason = false, &s
	}
	switch {
	case p.NL == nil:
		reason("no_wifi")
	case !identityUIDMap(p.read("/proc/self/uid_map")):
		reason("user_namespace")
	}
	return r
}

// identityUIDMap: the process sees every uid as itself (not inside a user
// namespace). An unreadable map counts as the identity.
func identityUIDMap(s string) bool {
	if s == "" {
		return true
	}
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) != 1 {
		return false
	}
	f := strings.Fields(lines[0])
	return len(f) == 3 && f[0] == "0" && f[1] == "0" && f[2] == "4294967295"
}
