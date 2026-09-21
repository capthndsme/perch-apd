// Package wireless builds the agent's picture of the Wi-Fi side of the
// device: which interfaces exist, which radio (UCI `wifi-device`) each
// belongs to, what it beacons, and who is associated. It merges
// `ubus call network.wireless status` (names and config, as iwinfo's users
// see them) with nl80211 (live kernel state).
package wireless

import (
	"context"
	"errors"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/capthndsme/perch-apd/internal/nl80211"
	"github.com/capthndsme/perch-apd/internal/ubus"
)

// Radio is one UCI wifi-device as netifd reports it.
type Radio struct {
	Name    string `json:"name"`
	Band    string `json:"band,omitempty"` // UCI: 2g, 5g, 6g, 60g
	Channel int    `json:"channel,omitempty"`
	HTMode  string `json:"htmode,omitempty"`
	Country string `json:"country,omitempty"`
	Up      bool   `json:"up"`
}

// Iface is one wireless interface.
type Iface struct {
	Ifname       string         `json:"ifname"`
	Radio        string         `json:"radio"`
	Mode         string         `json:"mode"` // UCI mode: ap, sta, mesh, adhoc, monitor
	SSID         string         `json:"ssid"`
	BSSID        string         `json:"bssid"`
	FrequencyMHz int            `json:"frequencyMhz,omitempty"`
	Channel      int            `json:"channel,omitempty"`
	Band         string         `json:"band,omitempty"` // server convention: 2.4, 5, 6, 60
	Stations     int            `json:"stations"`
	Index        int            `json:"-"`
	Type         nl80211.IfType `json:"-"`
	Country      string         `json:"-"`
}

// Inventory is a point-in-time view.
type Inventory struct {
	Radios     []Radio
	Interfaces []Iface
}

// NL is the part of the nl80211 client this package uses.
type NL interface {
	Interfaces() ([]nl80211.Interface, error)
	Stations(ifindex int) ([]nl80211.Station, error)
	Survey(ifindex int) ([]nl80211.SurveyEntry, error)
	RegDomain() (string, error)
}

// Ubus is the part of the ubus client this package uses.
type Ubus interface {
	Call(ctx context.Context, object, method string, args any, out any) error
}

// Source caches the slow parts (the ubus fork) and reads nl80211 live.
type Source struct {
	NL   NL   // nil = no Wi-Fi
	Ubus Ubus // nil = not OpenWrt: fall back to nl80211 only

	StatusTTL time.Duration
	RegTTL    time.Duration
	Now       func() time.Time

	mu        sync.Mutex
	status    map[string]radioStatus
	statusAt  time.Time
	statusErr error
	reg       string
	regAt     time.Time
}

type radioStatus struct {
	Up         bool `json:"up"`
	Disabled   bool `json:"disabled"`
	Config     radioConfig
	Interfaces []struct {
		Section string `json:"section"`
		Ifname  string `json:"ifname"`
		Config  struct {
			Mode string `json:"mode"`
			SSID string `json:"ssid"`
		} `json:"config"`
	} `json:"interfaces"`
}

type radioConfig struct {
	Band    string      `json:"band"`
	HWMode  string      `json:"hwmode"`
	Channel stringOrInt `json:"channel"`
	HTMode  string      `json:"htmode"`
	Country string      `json:"country"`
}

// stringOrInt accepts "6", 6 and "auto".
type stringOrInt int

func (s *stringOrInt) UnmarshalJSON(b []byte) error {
	str := strings.Trim(string(b), `"`)
	n, err := strconv.Atoi(str)
	if err != nil {
		*s = 0
		return nil
	}
	*s = stringOrInt(n)
	return nil
}

func (s *Source) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Invalidate forces the next Inventory to re-read netifd's status.
func (s *Source) Invalidate() {
	s.mu.Lock()
	s.statusAt = time.Time{}
	s.mu.Unlock()
}

func (s *Source) radioStatus(ctx context.Context) (map[string]radioStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ttl := s.StatusTTL
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	if !s.statusAt.IsZero() && s.now().Sub(s.statusAt) < ttl {
		return s.status, s.statusErr
	}
	if s.Ubus == nil {
		s.status, s.statusErr, s.statusAt = nil, ubus.ErrUnavailable, s.now()
		return nil, s.statusErr
	}
	var st map[string]radioStatus
	err := s.Ubus.Call(ctx, "network.wireless", "status", nil, &st)
	s.status, s.statusErr, s.statusAt = st, err, s.now()
	return st, err
}

// RegDomain returns the global regulatory domain, cached.
func (s *Source) RegDomain() string {
	if s.NL == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ttl := s.RegTTL
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	if !s.regAt.IsZero() && s.now().Sub(s.regAt) < ttl {
		return s.reg
	}
	reg, err := s.NL.RegDomain()
	if err != nil {
		reg = ""
	}
	s.reg, s.regAt = reg, s.now()
	return reg
}

// ErrNoWifi: this device has no nl80211 (no radios).
var ErrNoWifi = errors.New("no wireless interfaces (nl80211 unavailable)")

// Inventory lists radios and interfaces. Interfaces come from netifd when
// it is reachable (so a radio is named radio0, like LuCI and the lua
// exporter call it) and from nl80211 otherwise.
func (s *Source) Inventory(ctx context.Context) (Inventory, error) {
	var inv Inventory
	if s.NL == nil {
		return inv, ErrNoWifi
	}
	nlIfaces, err := s.NL.Interfaces()
	if err != nil {
		return inv, err
	}
	byName := make(map[string]nl80211.Interface, len(nlIfaces))
	for _, ifi := range nlIfaces {
		byName[ifi.Name] = ifi
	}

	status, statusErr := s.radioStatus(ctx)
	// A newly created interface that the cached status does not know yet:
	// refresh once so it gets its radio name.
	if statusErr == nil {
		known := map[string]bool{}
		for _, r := range status {
			for _, i := range r.Interfaces {
				known[i.Ifname] = true
			}
		}
		for _, ifi := range nlIfaces {
			if (ifi.Type == nl80211.IfTypeAP || ifi.Type == nl80211.IfTypeStation || ifi.Type == nl80211.IfTypeMeshPoint) && !known[ifi.Name] {
				s.Invalidate()
				status, statusErr = s.radioStatus(ctx)
				break
			}
		}
	}

	if statusErr == nil && status != nil {
		names := make([]string, 0, len(status))
		for name := range status {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			r := status[name]
			inv.Radios = append(inv.Radios, Radio{
				Name:    name,
				Band:    uciBand(r.Config),
				Channel: int(r.Config.Channel),
				HTMode:  r.Config.HTMode,
				Country: r.Config.Country,
				Up:      r.Up,
			})
			for _, ri := range r.Interfaces {
				if ri.Ifname == "" {
					continue
				}
				ifi, ok := byName[ri.Ifname]
				if !ok {
					continue
				}
				inv.Interfaces = append(inv.Interfaces, makeIface(ifi, name, ri.Config.Mode, ri.Config.SSID, r.Config.Country))
			}
		}
		return inv, nil
	}

	// No netifd: every nl80211 interface that carries traffic.
	sort.Slice(nlIfaces, func(i, j int) bool { return nlIfaces[i].Name < nlIfaces[j].Name })
	radios := map[string]bool{}
	for _, ifi := range nlIfaces {
		switch ifi.Type {
		case nl80211.IfTypeAP, nl80211.IfTypeStation, nl80211.IfTypeMeshPoint, nl80211.IfTypeAdhoc:
		default:
			continue
		}
		radio := "phy" + strconv.Itoa(ifi.Wiphy)
		if !radios[radio] {
			radios[radio] = true
			inv.Radios = append(inv.Radios, Radio{Name: radio, Up: true, Channel: nl80211.FreqToChannel(ifi.FrequencyMHz)})
		}
		inv.Interfaces = append(inv.Interfaces, makeIface(ifi, radio, uciMode(ifi.Type), "", ""))
	}
	return inv, nil
}

func makeIface(ifi nl80211.Interface, radio, mode, cfgSSID, country string) Iface {
	ssid := ifi.SSID
	if ssid == "" {
		ssid = cfgSSID
	}
	if mode == "" {
		mode = uciMode(ifi.Type)
	}
	return Iface{
		Ifname:       ifi.Name,
		Radio:        radio,
		Mode:         mode,
		SSID:         ssid,
		BSSID:        strings.ToLower(ifi.MAC.String()),
		FrequencyMHz: ifi.FrequencyMHz,
		Channel:      nl80211.FreqToChannel(ifi.FrequencyMHz),
		Band:         nl80211.Band(ifi.FrequencyMHz),
		Index:        ifi.Index,
		Type:         ifi.Type,
		Country:      country,
	}
}

func uciMode(t nl80211.IfType) string {
	switch t {
	case nl80211.IfTypeAP:
		return "ap"
	case nl80211.IfTypeStation:
		return "sta"
	case nl80211.IfTypeMeshPoint:
		return "mesh"
	case nl80211.IfTypeAdhoc:
		return "adhoc"
	case nl80211.IfTypeMonitor:
		return "monitor"
	}
	return "unknown"
}

func uciBand(c radioConfig) string {
	if c.Band != "" {
		return c.Band
	}
	switch c.HWMode { // pre-21.02 configs
	case "11a":
		return "5g"
	case "11b", "11g":
		return "2g"
	}
	return ""
}

// StationsOf dumps the stations of one interface.
func (s *Source) StationsOf(ifi Iface) ([]nl80211.Station, error) {
	if s.NL == nil {
		return nil, ErrNoWifi
	}
	return s.NL.Stations(ifi.Index)
}

// Noise returns the interface's noise floor the way iwinfo does: the survey
// entry of the channel in use, else the first entry that has one; 0 when the
// driver does not report noise.
func (s *Source) Noise(ifi Iface) int {
	if s.NL == nil {
		return 0
	}
	entries, err := s.NL.Survey(ifi.Index)
	if err != nil {
		return 0
	}
	var noise int8
	for _, e := range entries {
		if !e.HasNoise {
			continue
		}
		if noise == 0 || e.InUse {
			noise = e.Noise
		}
	}
	return int(noise)
}

// Client is one associated station, as clients.list reports it.
type Client struct {
	MAC                    string  `json:"mac"`
	Ifname                 string  `json:"ifname"`
	SSID                   string  `json:"ssid"`
	Radio                  string  `json:"radio"`
	Band                   string  `json:"band,omitempty"`
	FrequencyMHz           int     `json:"frequencyMhz,omitempty"`
	SignalDbm              *int    `json:"signalDbm,omitempty"`
	InactiveMs             uint32  `json:"inactiveMs"`
	ConnectedSeconds       *uint32 `json:"connectedSeconds,omitempty"`
	RxRateKbps             *uint64 `json:"rxRateKbps,omitempty"`
	TxRateKbps             *uint64 `json:"txRateKbps,omitempty"`
	ExpectedThroughputKbps *uint32 `json:"expectedThroughputKbps,omitempty"`
	RxBytes                *uint64 `json:"rxBytes,omitempty"`
	TxBytes                *uint64 `json:"txBytes,omitempty"`
	RxPackets              *uint32 `json:"rxPackets,omitempty"`
	TxPackets              *uint32 `json:"txPackets,omitempty"`
	Authorized             *bool   `json:"authorized,omitempty"`
}

// Clients lists associated stations of every interface (or one ifname).
func (s *Source) Clients(ctx context.Context, onlyIfname string) ([]Client, error) {
	inv, err := s.Inventory(ctx)
	if err != nil {
		return nil, err
	}
	out := []Client{}
	for _, ifi := range inv.Interfaces {
		if onlyIfname != "" && ifi.Ifname != onlyIfname {
			continue
		}
		stations, err := s.StationsOf(ifi)
		if err != nil {
			continue // interface went away between the two dumps
		}
		for _, st := range stations {
			out = append(out, clientFrom(ifi, st))
		}
	}
	return out, nil
}

// FindClient returns the interface a MAC is associated with, trying the
// hint first.
func (s *Source) FindClient(ctx context.Context, mac net.HardwareAddr, hint string) (Iface, bool, error) {
	inv, err := s.Inventory(ctx)
	if err != nil {
		return Iface{}, false, err
	}
	ifaces := inv.Interfaces
	sort.SliceStable(ifaces, func(i, j int) bool { return ifaces[i].Ifname == hint && ifaces[j].Ifname != hint })
	for _, ifi := range ifaces {
		stations, err := s.StationsOf(ifi)
		if err != nil {
			continue
		}
		for _, st := range stations {
			if strings.EqualFold(st.MAC.String(), mac.String()) {
				return ifi, true, nil
			}
		}
	}
	return Iface{}, false, nil
}

func clientFrom(ifi Iface, st nl80211.Station) Client {
	c := Client{
		MAC:          strings.ToLower(st.MAC.String()),
		Ifname:       ifi.Ifname,
		SSID:         ifi.SSID,
		Radio:        ifi.Radio,
		Band:         ifi.Band,
		FrequencyMHz: ifi.FrequencyMHz,
		InactiveMs:   st.InactiveMs,
	}
	if sig, ok := stationSignal(st); ok {
		v := int(sig)
		c.SignalDbm = &v
	}
	if st.HasConnected {
		v := st.ConnectedSeconds
		c.ConnectedSeconds = &v
	}
	if st.HasRxBitrate && st.RxBitrate > 0 {
		v := uint64(st.RxBitrate) * 100
		c.RxRateKbps = &v
	}
	if st.HasTxBitrate && st.TxBitrate > 0 {
		v := uint64(st.TxBitrate) * 100
		c.TxRateKbps = &v
	}
	if st.HasExpectedThroughput && st.ExpectedThroughputKbps > 0 {
		v := st.ExpectedThroughputKbps
		c.ExpectedThroughputKbps = &v
	}
	if st.HasBytes {
		rx, tx := st.RxBytes, st.TxBytes
		c.RxBytes, c.TxBytes = &rx, &tx
	}
	if st.HasPackets {
		rx, tx := st.RxPackets, st.TxPackets
		c.RxPackets, c.TxPackets = &rx, &tx
	}
	if st.HasFlags {
		v := st.Authorized
		c.Authorized = &v
	}
	return c
}

// stationSignal is iwinfo's per-station signal: the last frame's signal,
// the average when the driver only reports that.
func stationSignal(st nl80211.Station) (int8, bool) {
	if st.HasSignal && st.Signal != 0 {
		return st.Signal, true
	}
	if st.HasSignalAvg && st.SignalAvg != 0 {
		return st.SignalAvg, true
	}
	return 0, false
}

// StationSignal is exported for the metrics collector.
func StationSignal(st nl80211.Station) (int8, bool) { return stationSignal(st) }
