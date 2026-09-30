package nl80211

import (
	"sort"

	"github.com/mdlayher/genetlink"
	"github.com/mdlayher/netlink"
)

// What a radio can do: the wiphy dump (NL80211_CMD_GET_WIPHY, split) and
// the regulatory domains (NL80211_CMD_GET_REG). Read-only; the same data
// OpenWrt's wifi-detect.uc turns into /etc/board.json and LuCI into its
// channel and width lists.

// Commands.
const cmdGetWiphy = 1

// Wiphy attributes (enum nl80211_attrs).
const (
	attrWiphyName              = 2
	attrWiphyBands             = 22
	attrSupportedIftypes       = 32
	attrWiphyAntennaAvailTx    = 113
	attrWiphyAntennaAvailRx    = 114
	attrInterfaceCombinations  = 120
	attrDFSRegion              = 146
	attrSplitWiphyDump         = 174
	attrWiphySelfManagedReg    = 216
	bandAttrFreqs              = 1
	bandAttrHTCapa             = 4
	bandAttrVHTCapa            = 8
	bandAttrIftypeData         = 9
	bandIftypeAttrIftypes      = 1
	bandIftypeAttrHECapPhy     = 3
	bandIftypeAttrEHTCapPhy    = 9
	freqAttrFreq               = 1
	freqAttrDisabled           = 2
	freqAttrNoIR               = 3
	freqAttrRadar              = 5
	freqAttrMaxTxPower         = 6
	freqAttrDFSState           = 7
	freqAttrNoHT40Minus        = 9
	freqAttrNoHT40Plus         = 10
	freqAttrNo80MHz            = 11
	freqAttrNo160MHz           = 12
	freqAttrDFSCACTime         = 13
	freqAttrIndoorOnly         = 14
	freqAttrNoHE               = 19
	freqAttrNo320MHz           = 26
	freqAttrNoEHT              = 27
	ifaceCombLimits            = 1
	ifaceCombMaxNum            = 2
	ifaceCombNumChannels       = 4
	ifaceCombRadarDetectWidths = 5
	ifaceLimitMax              = 1
	ifaceLimitTypes            = 2
)

// Bands (enum nl80211_band).
const (
	Band2GHz  = 0
	Band5GHz  = 1
	Band60GHz = 2
	Band6GHz  = 3
)

// DFS regions (enum nl80211_dfs_regions).
const (
	DFSUnset = 0
	DFSFCC   = 1
	DFSETSI  = 2
	DFSJP    = 3
)

// DFS states of a channel (enum nl80211_dfs_state).
const (
	DFSUsable      = 0
	DFSUnavailable = 1
	DFSAvailable   = 2
)

// Wiphy is one radio as the kernel describes it.
type Wiphy struct {
	Index int
	Name  string // "phy0"
	// Bands, ordered by band number.
	Bands []WiphyBand
	// IfTypes the wiphy supports.
	IfTypes      []IfType
	Combinations []IfaceCombination
	// SelfManagedReg: the driver sets the regulatory domain itself (ath11k
	// and others); a `country` in the config does not change it.
	SelfManagedReg                 bool
	AntennaAvailTx, AntennaAvailRx uint32
}

// WiphyBand is one band of a wiphy.
type WiphyBand struct {
	Band  int // Band2GHz, Band5GHz, Band60GHz, Band6GHz
	Freqs []WiphyFreq
	// HTCapa and VHTCapa are the HT and VHT capability fields (0 = none).
	HTCapa  uint16
	VHTCapa uint32
	// HEPhyCap0 and EHTPhyCap0 are the first byte of the HE and EHT PHY
	// capabilities (channel width bits), OR-ed over the interface types;
	// HasHE / HasEHT say whether any interface type has them.
	HEPhyCap0, EHTPhyCap0 byte
	HasHE, HasEHT         bool
}

// WiphyFreq is one channel of a band.
type WiphyFreq struct {
	MHz           int
	Disabled      bool
	NoIR          bool // no initiating radiation: no AP here (passive only)
	Radar         bool // DFS channel
	IndoorOnly    bool
	MaxTxPowerMBm int // 0 = not reported
	DFSState      int // DFSUsable, DFSUnavailable, DFSAvailable (radar channels)
	CACTimeMs     int // 0 = not reported
	NoHT40Minus   bool
	NoHT40Plus    bool
	No80MHz       bool
	No160MHz      bool
	No320MHz      bool
	NoHE          bool
	NoEHT         bool
}

// IfaceCombination is one of the wiphy's valid interface combinations.
type IfaceCombination struct {
	Limits            []IfaceLimit
	MaxNum            int
	NumChannels       int
	RadarDetectWidths uint32
}

// IfaceLimit is "at most Max interfaces of these types".
type IfaceLimit struct {
	Max   int
	Types []IfType
}

// MaxInterfaces is how many interfaces of type t the wiphy can run at
// once: the best of its combinations (each bounded by its total), 1 when it
// supports t without any combination, 0 when it does not support t.
func (w Wiphy) MaxInterfaces(t IfType) int {
	best := 0
	for _, c := range w.Combinations {
		n := 0
		for _, l := range c.Limits {
			for _, lt := range l.Types {
				if lt == t {
					n += l.Max
					break
				}
			}
		}
		if c.MaxNum > 0 && n > c.MaxNum {
			n = c.MaxNum
		}
		if n > best {
			best = n
		}
	}
	if best == 0 {
		for _, s := range w.IfTypes {
			if s == t {
				return 1
			}
		}
	}
	return best
}

// Band returns the band b, if the wiphy has it.
func (w Wiphy) Band(b int) (WiphyBand, bool) {
	for _, x := range w.Bands {
		if x.Band == b {
			return x, true
		}
	}
	return WiphyBand{}, false
}

// Wiphys dumps every radio (split dump: a wiphy spans several messages).
func (c *Client) Wiphys() ([]Wiphy, error) {
	msgs, err := c.dump(cmdGetWiphy, func(ae *netlink.AttributeEncoder) {
		ae.Flag(attrSplitWiphyDump, true)
	})
	if err != nil {
		return nil, err
	}
	raw := make([][]byte, len(msgs))
	for i, m := range msgs {
		raw[i] = m.Data
	}
	return parseWiphys(raw)
}

// parseWiphys merges the messages of a (split) wiphy dump.
func parseWiphys(msgs [][]byte) ([]Wiphy, error) {
	byIndex := map[int]*Wiphy{}
	var order []int
	bands := map[int]map[int]*WiphyBand{}
	for _, b := range msgs {
		ad, err := netlink.NewAttributeDecoder(b)
		if err != nil {
			return nil, err
		}
		// The index comes first in every message; collect the rest and
		// apply it once the index is known.
		var w *Wiphy
		type pending struct {
			typ uint16
			v   []byte
		}
		var rest []pending
		for ad.Next() {
			if ad.Type() == attrWiphy {
				idx := int(u32(ad.Bytes()))
				if byIndex[idx] == nil {
					byIndex[idx] = &Wiphy{Index: idx}
					bands[idx] = map[int]*WiphyBand{}
					order = append(order, idx)
				}
				w = byIndex[idx]
				continue
			}
			rest = append(rest, pending{ad.Type(), append([]byte(nil), ad.Bytes()...)})
		}
		if err := ad.Err(); err != nil {
			return nil, err
		}
		if w == nil {
			continue
		}
		for _, a := range rest {
			switch a.typ {
			case attrWiphyName:
				w.Name = cstring(a.v)
			case attrSupportedIftypes:
				w.IfTypes = append(w.IfTypes, parseIftypeFlags(a.v)...)
			case attrWiphySelfManagedReg:
				w.SelfManagedReg = true
			case attrWiphyAntennaAvailTx:
				w.AntennaAvailTx = u32(a.v)
			case attrWiphyAntennaAvailRx:
				w.AntennaAvailRx = u32(a.v)
			case attrInterfaceCombinations:
				combs, err := parseCombinations(a.v)
				if err != nil {
					return nil, err
				}
				w.Combinations = append(w.Combinations, combs...)
			case attrWiphyBands:
				if err := parseBands(a.v, bands[w.Index]); err != nil {
					return nil, err
				}
			}
		}
	}
	out := make([]Wiphy, 0, len(order))
	sort.Ints(order)
	for _, idx := range order {
		w := byIndex[idx]
		for _, b := range bands[idx] {
			w.Bands = append(w.Bands, *b)
		}
		sort.Slice(w.Bands, func(i, j int) bool { return w.Bands[i].Band < w.Bands[j].Band })
		out = append(out, *w)
	}
	return out, nil
}

func parseIftypeFlags(b []byte) []IfType {
	ad, err := netlink.NewAttributeDecoder(b)
	if err != nil {
		return nil
	}
	var out []IfType
	for ad.Next() {
		out = append(out, IfType(ad.Type()))
	}
	return out
}

func parseCombinations(b []byte) ([]IfaceCombination, error) {
	ad, err := netlink.NewAttributeDecoder(b)
	if err != nil {
		return nil, err
	}
	var out []IfaceCombination
	for ad.Next() {
		cd, err := netlink.NewAttributeDecoder(ad.Bytes())
		if err != nil {
			return nil, err
		}
		var c IfaceCombination
		for cd.Next() {
			switch cd.Type() {
			case ifaceCombLimits:
				ld, err := netlink.NewAttributeDecoder(cd.Bytes())
				if err != nil {
					return nil, err
				}
				for ld.Next() {
					var l IfaceLimit
					nd, err := netlink.NewAttributeDecoder(ld.Bytes())
					if err != nil {
						return nil, err
					}
					for nd.Next() {
						switch nd.Type() {
						case ifaceLimitMax:
							l.Max = int(u32(nd.Bytes()))
						case ifaceLimitTypes:
							l.Types = parseIftypeFlags(nd.Bytes())
						}
					}
					if err := nd.Err(); err != nil {
						return nil, err
					}
					c.Limits = append(c.Limits, l)
				}
				if err := ld.Err(); err != nil {
					return nil, err
				}
			case ifaceCombMaxNum:
				c.MaxNum = int(u32(cd.Bytes()))
			case ifaceCombNumChannels:
				c.NumChannels = int(u32(cd.Bytes()))
			case ifaceCombRadarDetectWidths:
				c.RadarDetectWidths = u32(cd.Bytes())
			}
		}
		if err := cd.Err(); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, ad.Err()
}

// parseBands adds one message's bands to acc (a split dump sends a band's
// channels over several messages).
func parseBands(b []byte, acc map[int]*WiphyBand) error {
	ad, err := netlink.NewAttributeDecoder(b)
	if err != nil {
		return err
	}
	for ad.Next() {
		idx := int(ad.Type())
		band := acc[idx]
		if band == nil {
			band = &WiphyBand{Band: idx}
			acc[idx] = band
		}
		bd, err := netlink.NewAttributeDecoder(ad.Bytes())
		if err != nil {
			return err
		}
		for bd.Next() {
			v := bd.Bytes()
			switch bd.Type() {
			case bandAttrFreqs:
				freqs, err := parseFreqs(v)
				if err != nil {
					return err
				}
				band.Freqs = append(band.Freqs, freqs...)
			case bandAttrHTCapa:
				band.HTCapa = uint16(u32(v))
			case bandAttrVHTCapa:
				band.VHTCapa = u32(v)
			case bandAttrIftypeData:
				if err := parseIftypeData(v, band); err != nil {
					return err
				}
			}
		}
		if err := bd.Err(); err != nil {
			return err
		}
	}
	return ad.Err()
}

func parseIftypeData(b []byte, band *WiphyBand) error {
	ad, err := netlink.NewAttributeDecoder(b)
	if err != nil {
		return err
	}
	for ad.Next() {
		nd, err := netlink.NewAttributeDecoder(ad.Bytes())
		if err != nil {
			return err
		}
		for nd.Next() {
			v := nd.Bytes()
			switch nd.Type() {
			case bandIftypeAttrHECapPhy:
				if len(v) > 0 {
					band.HasHE = true
					band.HEPhyCap0 |= v[0]
				}
			case bandIftypeAttrEHTCapPhy:
				if len(v) > 0 {
					band.HasEHT = true
					band.EHTPhyCap0 |= v[0]
				}
			}
		}
		if err := nd.Err(); err != nil {
			return err
		}
	}
	return ad.Err()
}

func parseFreqs(b []byte) ([]WiphyFreq, error) {
	ad, err := netlink.NewAttributeDecoder(b)
	if err != nil {
		return nil, err
	}
	var out []WiphyFreq
	for ad.Next() {
		fd, err := netlink.NewAttributeDecoder(ad.Bytes())
		if err != nil {
			return nil, err
		}
		var f WiphyFreq
		for fd.Next() {
			v := fd.Bytes()
			switch fd.Type() {
			case freqAttrFreq:
				f.MHz = int(u32(v))
			case freqAttrDisabled:
				f.Disabled = true
			case freqAttrNoIR:
				f.NoIR = true
			case freqAttrRadar:
				f.Radar = true
			case freqAttrMaxTxPower:
				f.MaxTxPowerMBm = int(u32(v))
			case freqAttrDFSState:
				f.DFSState = int(u32(v))
			case freqAttrDFSCACTime:
				f.CACTimeMs = int(u32(v))
			case freqAttrIndoorOnly:
				f.IndoorOnly = true
			case freqAttrNoHT40Minus:
				f.NoHT40Minus = true
			case freqAttrNoHT40Plus:
				f.NoHT40Plus = true
			case freqAttrNo80MHz:
				f.No80MHz = true
			case freqAttrNo160MHz:
				f.No160MHz = true
			case freqAttrNo320MHz:
				f.No320MHz = true
			case freqAttrNoHE:
				f.NoHE = true
			case freqAttrNoEHT:
				f.NoEHT = true
			}
		}
		if err := fd.Err(); err != nil {
			return nil, err
		}
		if f.MHz > 0 {
			out = append(out, f)
		}
	}
	return out, ad.Err()
}

// RegDomain is one regulatory domain: the global one (Wiphy -1) or a
// self-managed wiphy's own.
type RegDomain struct {
	Wiphy     int    // -1 = global
	Alpha2    string // "PH", "US", "00"
	DFSRegion int    // DFSUnset, DFSFCC, DFSETSI, DFSJP
}

// RegDomains returns the global regulatory domain and, where the kernel
// reports them, the self-managed wiphys' own (a GET_REG dump; kernels
// without the dump answer the global one alone).
func (c *Client) RegDomains() ([]RegDomain, error) {
	c.mu.Lock()
	msgs, err := c.conn.Execute(genetlink.Message{
		Header: genetlink.Header{Command: cmdGetReg, Version: c.family.Version},
	}, c.family.ID, netlink.Request|netlink.Dump)
	if err != nil {
		msgs, err = c.conn.Execute(genetlink.Message{
			Header: genetlink.Header{Command: cmdGetReg, Version: c.family.Version},
		}, c.family.ID, netlink.Request)
	}
	c.mu.Unlock()
	if err != nil {
		return nil, err
	}
	raw := make([][]byte, len(msgs))
	for i, m := range msgs {
		raw[i] = m.Data
	}
	return parseRegDomains(raw)
}

func parseRegDomains(msgs [][]byte) ([]RegDomain, error) {
	var out []RegDomain
	for _, b := range msgs {
		ad, err := netlink.NewAttributeDecoder(b)
		if err != nil {
			return nil, err
		}
		r := RegDomain{Wiphy: -1}
		for ad.Next() {
			v := ad.Bytes()
			switch ad.Type() {
			case attrWiphy:
				r.Wiphy = int(u32(v))
			case attrRegAlpha2:
				r.Alpha2 = cstring(v)
			case attrDFSRegion:
				if len(v) > 0 {
					r.DFSRegion = int(v[0])
				}
			}
		}
		if err := ad.Err(); err != nil {
			return nil, err
		}
		if r.Alpha2 != "" {
			out = append(out, r)
		}
	}
	return out, nil
}

// DFSRegionName is "FCC", "ETSI", "JP" or "".
func DFSRegionName(r int) string {
	switch r {
	case DFSFCC:
		return "FCC"
	case DFSETSI:
		return "ETSI"
	case DFSJP:
		return "JP"
	}
	return ""
}

// ChannelWidthMHz converts enum nl80211_chan_width (Interface.ChannelWidth)
// to MHz (80+80 counts as 160); 0 when unknown.
func ChannelWidthMHz(w int) int {
	switch w {
	case 0, 1: // 20 MHz without HT, 20 MHz
		return 20
	case 2:
		return 40
	case 3:
		return 80
	case 4, 5: // 80+80, 160
		return 160
	case 6:
		return 5
	case 7:
		return 10
	case 13:
		return 320
	}
	return 0
}
