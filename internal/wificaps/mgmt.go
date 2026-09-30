package wificaps

import (
	"context"
	"path/filepath"
	"sort"
	"strings"

	"github.com/capthndsme/perch-apd/internal/nl80211"
)

// ManagementRadios lists the radio sections that carry the AP's own uplink
// when its route to the controller leaves over dev (Wi-Fi design
// protocol.md 3.8): dev itself when it is a wireless netdev, and every
// station, mesh-point or WDS netdev below it (bridge ports, a VLAN's
// parent, batman-adv's hard interfaces: the `lower_*` links in sysfs). An
// AP with a wired uplink has none: `[]`. The Wi-Fi config plane protects
// those radios and their non-AP interfaces from its jobs.
func (p *Prober) ManagementRadios(ctx context.Context, dev string) []string {
	out := []string{}
	if dev == "" {
		return out
	}
	nlByName := map[string]nl80211.Interface{}
	if p.NL != nil {
		if list, err := p.NL.Interfaces(); err == nil {
			for _, ifi := range list {
				nlByName[ifi.Name] = ifi
			}
		}
	}
	st, _ := p.wirelessStatus(ctx)
	radioOfIf, modeOfIf := map[string]string{}, map[string]string{}
	for radio, rs := range st {
		for _, i := range rs.Interfaces {
			if i.Ifname != "" {
				radioOfIf[i.Ifname] = radio
				modeOfIf[i.Ifname] = string(i.Config.Mode)
			}
		}
	}
	var phyRadio map[string]string // phy → radio section, on demand
	radioOf := func(ifname string) string {
		if r := radioOfIf[ifname]; r != "" {
			return r
		}
		phy := filepath.Base(p.linkTarget("/sys/class/net/" + ifname + "/phy80211"))
		if phy == "." || phy == "" {
			return ""
		}
		if phyRadio == nil {
			phyRadio = map[string]string{}
			list, _ := p.uciRadios(st)
			for section, ph := range p.radioPhys(list) {
				if ph != "" {
					phyRadio[ph] = section
				}
			}
		}
		return phyRadio[phy]
	}
	wireless := func(ifname string) bool {
		_, ok := nlByName[ifname]
		return ok || p.exists("/sys/class/net/"+ifname+"/phy80211")
	}
	uplink := func(ifname string) bool {
		if ifi, ok := nlByName[ifname]; ok {
			switch ifi.Type {
			case nl80211.IfTypeStation, nl80211.IfTypeMeshPoint, nl80211.IfTypeWDS:
				return true
			}
			return false
		}
		switch modeOfIf[ifname] {
		case "sta", "mesh", "wds":
			return true
		}
		return false
	}
	found := map[string]bool{}
	seen := map[string]bool{}
	queue := []string{dev}
	for depth := 0; len(queue) > 0 && depth < 6; depth++ {
		var next []string
		for _, d := range queue {
			if seen[d] {
				continue
			}
			seen[d] = true
			if wireless(d) {
				if d == dev || uplink(d) {
					if r := radioOf(d); r != "" {
						found[r] = true
					}
				}
				continue
			}
			for _, e := range p.list("/sys/class/net/" + d) {
				if name, ok := strings.CutPrefix(e, "lower_"); ok {
					next = append(next, name)
				}
			}
			next = append(next, p.list("/sys/class/net/"+d+"/brif")...)
		}
		queue = next
	}
	for r := range found {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

func (p *Prober) linkTarget(s string) string {
	real, err := filepath.EvalSymlinks(p.path(s))
	if err != nil {
		return ""
	}
	return real
}
