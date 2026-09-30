package wificaps

import (
	"math"
	"path/filepath"
	"sort"
	"strings"

	"github.com/capthndsme/perch-apd/internal/nl80211"
)

// Radio is one wifi-device section of /etc/config/wireless with what its
// phy offers and what it runs.
type Radio struct {
	Section string `json:"section"`
	// Phy is the phy the section resolves to ("" = none: a stale section
	// whose device is gone), by `path` (a suffix of the phy's device
	// path, `+N` = the Nth phy of that device), else `macaddr`, else `phy`,
	// as OpenWrt's wifi scripts resolve it.
	Phy  string `json:"phy,omitempty"`
	Path string `json:"path,omitempty"`
	// Band is the configured band: 2g, 5g, 6g, 60g.
	Band    string `json:"band,omitempty"`
	Present bool   `json:"present"`
	// Up, Disabled, RetrySetupFailed: netifd's view (network.wireless status).
	Up               bool `json:"up"`
	Disabled         bool `json:"disabled"`
	RetrySetupFailed bool `json:"retrySetupFailed"`
	// Country is the section's `country` ("" = unset).
	Country string `json:"country,omitempty"`
	// RegCountry is the domain the kernel applies to this radio: its own for
	// a self-managed phy, else the global one.
	RegCountry     string  `json:"regCountry,omitempty"`
	SelfManagedReg bool    `json:"selfManagedReg,omitempty"`
	TxpowerMaxDbm  float64 `json:"txpowerMaxDbm,omitempty"`
	// MaxBSS is how many AP interfaces the phy can run at once.
	MaxBSS int `json:"maxBss,omitempty"`
	// Widths (MHz), Modes (HT, VHT, HE, EHT) and HTModes (the `htmode`
	// values the band takes, as LuCI and /etc/board.json list them).
	Widths   []int     `json:"widths"`
	Modes    []string  `json:"modes"`
	HTModes  []string  `json:"htmodes"`
	Channels []Channel `json:"channels"`
	// Current is what runs (nil when no interface of the phy is up).
	Current *Current `json:"current"`
}

// Channel is one channel of the radio's band.
type Channel struct {
	Channel  int     `json:"channel"`
	MHz      int     `json:"mhz"`
	MaxDbm   float64 `json:"maxDbm"`
	DFS      bool    `json:"dfs"`
	NoIR     bool    `json:"noIr"`
	Disabled bool    `json:"disabled"`
	// CACSeconds is the radar check before a DFS channel can beacon: the
	// kernel's value, else 60 s (600 s for 5600-5650 MHz under ETSI).
	CACSeconds int `json:"cacSeconds,omitempty"`
	// DFSState of a DFS channel: usable (needs a CAC), available (checked),
	// unavailable (radar seen: closed for the non-occupancy period).
	DFSState string `json:"dfsState,omitempty"`
}

// Current is the radio as it runs.
type Current struct {
	Channel    int     `json:"channel"`
	MHz        int     `json:"mhz"`
	WidthMHz   int     `json:"widthMhz,omitempty"`
	HTMode     string  `json:"htmode,omitempty"` // configured
	TxpowerDbm float64 `json:"txpowerDbm,omitempty"`
}

// uciRadio is a wifi-device section's hardware options.
type uciRadio struct {
	section, path, band, macaddr, phy, country string
	disabled                                   bool
}

func (p *Prober) uciRadios(st map[string]radioStatus) ([]uciRadio, error) {
	var out []uciRadio
	c, err := p.wirelessConfig()
	if err == nil {
		for _, s := range c.OfType("wifi-device") {
			get := func(name string) string {
				if v, ok := s.Get(name); ok {
					return v.Str()
				}
				return ""
			}
			r := uciRadio{section: s.Name, path: get("path"), band: get("band"), macaddr: get("macaddr"),
				phy: get("phy"), country: get("country"), disabled: get("disabled") == "1"}
			if r.band == "" {
				r.band = hwmodeBand(get("hwmode"))
			}
			out = append(out, r)
		}
		return out, nil
	}
	// No readable config: netifd's radios.
	names := make([]string, 0, len(st))
	for name := range st {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		cfg := st[name].Config
		band := string(cfg.Band)
		if band == "" {
			band = hwmodeBand(string(cfg.HWMode))
		}
		out = append(out, uciRadio{section: name, path: string(cfg.Path), band: band, country: string(cfg.Country)})
	}
	return out, err
}

func hwmodeBand(hwmode string) string {
	switch hwmode {
	case "11a":
		return "5g"
	case "11b", "11g":
		return "2g"
	case "11ad":
		return "60g"
	}
	return ""
}

func bandIndex(band string) int {
	switch band {
	case "2g":
		return nl80211.Band2GHz
	case "5g":
		return nl80211.Band5GHz
	case "6g":
		return nl80211.Band6GHz
	case "60g":
		return nl80211.Band60GHz
	}
	return -1
}

// phyInfo is a phy in /sys/class/ieee80211.
type phyInfo struct {
	name, devpath, mac string
	index              int
}

func (p *Prober) phys() []phyInfo {
	var out []phyInfo
	for _, name := range p.list("/sys/class/ieee80211") {
		dir := "/sys/class/ieee80211/" + name
		ph := phyInfo{name: name, index: atoi(p.read(dir + "/index")), mac: strings.ToLower(p.read(dir + "/macaddress"))}
		if real, err := filepath.EvalSymlinks(p.path(dir + "/device")); err == nil {
			ph.devpath = real
		}
		out = append(out, ph)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].index < out[j].index })
	return out
}

// findPhy resolves a radio section to its phy the way OpenWrt's wifi
// scripts do (ucode wifi/utils.uc find_phy): by path, the phys whose device
// path ends in the part before "+", ordered by index, "+N" picking the
// Nth; else by macaddr; else by the phy name.
func findPhy(phys []phyInfo, r uciRadio) string {
	if r.path != "" {
		base, ofs, _ := strings.Cut(r.path, "+")
		n := atoi(ofs)
		var matches []string
		for _, ph := range phys {
			if ph.devpath != "" && strings.HasSuffix(ph.devpath, base) {
				matches = append(matches, ph.name)
			}
		}
		if n >= 0 && n < len(matches) {
			return matches[n]
		}
		return ""
	}
	if r.macaddr != "" {
		for _, ph := range phys {
			if ph.mac == strings.ToLower(r.macaddr) {
				return ph.name
			}
		}
	}
	if r.phy != "" {
		for _, ph := range phys {
			if ph.name == r.phy {
				return ph.name
			}
		}
	}
	return ""
}

// radioPhys maps each radio section to its phy ("" = none).
func (p *Prober) radioPhys(radios []uciRadio) map[string]string {
	phys := p.phys()
	out := make(map[string]string, len(radios))
	for _, r := range radios {
		out[r.section] = findPhy(phys, r)
	}
	return out
}

func (p *Prober) radios(st map[string]radioStatus, wiphys []nl80211.Wiphy, ifaces []nl80211.Interface,
	reg Regulatory, regs []nl80211.RegDomain) ([]Radio, error) {
	list, err := p.uciRadios(st)
	phyOf := p.radioPhys(list)
	byName := map[string]nl80211.Wiphy{}
	for _, w := range wiphys {
		byName[w.Name] = w
	}
	selfReg := map[int]string{}
	for _, d := range regs {
		if d.Wiphy >= 0 {
			selfReg[d.Wiphy] = d.Alpha2
		}
	}
	region := reg.DFSRegion
	out := make([]Radio, 0, len(list))
	for _, u := range list {
		r := Radio{Section: u.section, Path: u.path, Band: u.band, Country: u.country, Disabled: u.disabled,
			Widths: []int{}, Modes: []string{}, HTModes: []string{}, Channels: []Channel{}}
		s, hasStatus := st[u.section]
		if hasStatus {
			r.Up, r.RetrySetupFailed = s.Up, s.RetrySetupFailed
			r.Disabled = r.Disabled || s.Disabled
		}
		r.Phy = phyOf[u.section]
		w, known := byName[r.Phy]
		r.Present = r.Phy != "" && (known || len(wiphys) == 0)
		if !known {
			out = append(out, r)
			continue
		}
		r.SelfManagedReg = w.SelfManagedReg
		r.RegCountry = reg.Global
		if c, ok := selfReg[w.Index]; ok && w.SelfManagedReg {
			r.RegCountry = c
		}
		r.MaxBSS = w.MaxInterfaces(nl80211.IfTypeAP)
		bi := bandIndex(u.band)
		if bi < 0 && len(w.Bands) == 1 {
			bi = w.Bands[0].Band
		}
		if b, ok := w.Band(bi); ok {
			r.Widths, r.Modes, r.HTModes = bandModes(b)
			for _, f := range b.Freqs {
				ch := Channel{Channel: freqChannel(f.MHz), MHz: f.MHz, MaxDbm: dbm(f.MaxTxPowerMBm),
					DFS: f.Radar, NoIR: f.NoIR, Disabled: f.Disabled}
				if f.Radar {
					ch.CACSeconds = cacSeconds(f, region)
					ch.DFSState = dfsState(f.DFSState)
				}
				if !f.Disabled && ch.MaxDbm > r.TxpowerMaxDbm {
					r.TxpowerMaxDbm = ch.MaxDbm
				}
				r.Channels = append(r.Channels, ch)
			}
		}
		r.Current = current(w, ifaces)
		if r.Current != nil && hasStatus {
			r.Current.HTMode = string(s.Config.HTMode)
		}
		out = append(out, r)
	}
	return out, err
}

func dbm(mbm int) float64 { return math.Round(float64(mbm)) / 100 }

func freqChannel(mhz int) int {
	if mhz == 5935 { // 6 GHz channel 2
		return 2
	}
	return nl80211.FreqToChannel(mhz)
}

func cacSeconds(f nl80211.WiphyFreq, region string) int {
	if f.CACTimeMs > 0 {
		return (f.CACTimeMs + 999) / 1000
	}
	if region == "ETSI" && f.MHz >= 5600 && f.MHz <= 5650 {
		return 600 // weather radar
	}
	return 60
}

func dfsState(s int) string {
	switch s {
	case nl80211.DFSUnavailable:
		return "unavailable"
	case nl80211.DFSAvailable:
		return "available"
	}
	return "usable"
}

// bandModes works out the band's widths, modes and htmode values the way
// OpenWrt's wifi-detect.uc does for /etc/board.json.
func bandModes(b nl80211.WiphyBand) (widths []int, modes []string, htmodes []string) {
	ht, vht, he, eht := b.HTCapa > 0, b.VHTCapa > 0, b.HasHE, b.HasEHT
	hecap, ehtcap := b.HEPhyCap0, b.EHTPhyCap0
	modes = []string{}
	for _, m := range []struct {
		on   bool
		name string
	}{{ht, "HT"}, {vht, "VHT"}, {he, "HE"}, {eht, "EHT"}} {
		if m.on {
			modes = append(modes, m.name)
		}
	}
	seen := map[string]bool{}
	add := func(on bool, name string) {
		if on && !seen[name] {
			seen[name] = true
			htmodes = append(htmodes, name)
		}
	}
	add(true, "NOHT")
	add(ht, "HT20")
	add(vht, "VHT20")
	add(he, "HE20")
	add(eht, "EHT20")
	if b.HTCapa&0x2 != 0 {
		add(true, "HT40")
		add(vht, "VHT40")
	}
	add(hecap&0x2 != 0, "HE40")
	add(eht && hecap&0x2 != 0, "EHT40")
	if b.Band != nl80211.Band2GHz {
		add(hecap&0x4 != 0, "HE40")
		add(eht && hecap&0x4 != 0, "EHT40")
		add(vht, "VHT80")
		add(hecap&0x4 != 0, "HE80")
		add(eht && hecap&0x4 != 0, "EHT80")
		add((b.VHTCapa>>2)&0x3 != 0, "VHT160")
		add(hecap&0x18 != 0, "HE160")
		add(eht && hecap&0x18 != 0, "EHT160")
		add(ehtcap&0x2 != 0, "EHT320")
	}
	if b.Band == nl80211.Band60GHz {
		htmodes = []string{}
	}
	ws := map[int]bool{20: true}
	for _, m := range htmodes {
		if i := strings.IndexAny(m, "0123456789"); i > 0 {
			ws[atoi(m[i:])] = true
		}
	}
	for w := range ws {
		widths = append(widths, w)
	}
	sort.Ints(widths)
	if htmodes == nil {
		htmodes = []string{}
	}
	return widths, modes, htmodes
}

// current is the phy's running channel: its first interface with a
// frequency, an AP one if any.
func current(w nl80211.Wiphy, ifaces []nl80211.Interface) *Current {
	var pick *nl80211.Interface
	for i := range ifaces {
		ifi := &ifaces[i]
		if ifi.Wiphy != w.Index || ifi.FrequencyMHz <= 0 {
			continue
		}
		if pick == nil || ifi.Type == nl80211.IfTypeAP && pick.Type != nl80211.IfTypeAP {
			pick = ifi
		}
	}
	if pick == nil {
		return nil
	}
	c := &Current{Channel: freqChannel(pick.FrequencyMHz), MHz: pick.FrequencyMHz,
		WidthMHz: nl80211.ChannelWidthMHz(pick.ChannelWidth)}
	if pick.TxPowerMBm > 0 {
		c.TxpowerDbm = dbm(pick.TxPowerMBm)
	}
	return c
}
