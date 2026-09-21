// Package nl80211 is the small slice of the Linux nl80211 generic-netlink
// API the agent needs: wireless interfaces, associated stations, channel
// survey (noise) and the regulatory domain. The same kernel calls iwinfo
// makes, so the numbers match what prometheus-node-exporter-lua reported.
//
// Parsing is separate from I/O (parse* functions take the attribute bytes of
// one generic-netlink message) so it can be tested without a radio.
package nl80211

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"

	"github.com/mdlayher/genetlink"
	"github.com/mdlayher/netlink"
)

// Commands (linux/nl80211.h).
const (
	cmdGetInterface = 5
	cmdGetStation   = 17
	cmdGetReg       = 31
	cmdGetSurvey    = 50
)

// Top-level attributes.
const (
	attrWiphy           = 1
	attrIfindex         = 3
	attrIfname          = 4
	attrIftype          = 5
	attrMAC             = 6
	attrStaInfo         = 21
	attrRegAlpha2       = 33
	attrWiphyFreq       = 38
	attrSSID            = 52
	attrSurveyInfo      = 84
	attrWiphyTxPowerLvl = 98
	attrChannelWidth    = 159
	attrCenterFreq1     = 160
)

// Station info attributes (enum nl80211_sta_info).
const (
	staInactiveTime       = 1
	staRxBytes            = 2
	staTxBytes            = 3
	staSignal             = 7
	staTxBitrate          = 8
	staRxPackets          = 9
	staTxPackets          = 10
	staTxRetries          = 11
	staTxFailed           = 12
	staSignalAvg          = 13
	staRxBitrate          = 14
	staConnectedTime      = 16
	staFlags              = 17
	staRxBytes64          = 23
	staTxBytes64          = 24
	staExpectedThroughput = 27
)

// Rate info attributes (enum nl80211_rate_info).
const (
	rateBitrate   = 1 // u16, 100 kbit/s
	rateBitrate32 = 5 // u32, 100 kbit/s
)

// Survey info attributes (enum nl80211_survey_info).
const (
	surveyFrequency = 1
	surveyNoise     = 2
	surveyInUse     = 3
	surveyTime      = 4
	surveyTimeBusy  = 5
	surveyTimeRx    = 7
	surveyTimeTx    = 8
)

// Station flags (enum nl80211_sta_flags), bit positions.
const (
	staFlagAuthorized    = 1
	staFlagAuthenticated = 5
	staFlagAssociated    = 7
)

// IfType is enum nl80211_iftype.
type IfType uint32

// Interface types.
const (
	IfTypeUnspecified IfType = 0
	IfTypeAdhoc       IfType = 1
	IfTypeStation     IfType = 2
	IfTypeAP          IfType = 3
	IfTypeAPVLAN      IfType = 4
	IfTypeWDS         IfType = 5
	IfTypeMonitor     IfType = 6
	IfTypeMeshPoint   IfType = 7
	IfTypeP2PClient   IfType = 8
	IfTypeP2PGO       IfType = 9
)

// IwinfoMode is the mode name iwinfo (and so node_exporter-lua) reports.
func (t IfType) IwinfoMode() string {
	switch t {
	case IfTypeAP:
		return "Master"
	case IfTypeAdhoc:
		return "Ad-Hoc"
	case IfTypeStation:
		return "Client"
	case IfTypeMonitor:
		return "Monitor"
	case IfTypeAPVLAN:
		return "Master (VLAN)"
	case IfTypeWDS:
		return "WDS"
	case IfTypeMeshPoint:
		return "Mesh Point"
	case IfTypeP2PClient:
		return "P2P Client"
	case IfTypeP2PGO:
		return "P2P Go"
	}
	return "Unknown"
}

// Interface is one wireless netdev.
type Interface struct {
	Index        int
	Name         string
	Wiphy        int
	Type         IfType
	MAC          net.HardwareAddr
	SSID         string // AP / P2P-GO: the SSID it beacons; station: the SSID it is on
	FrequencyMHz int    // 0 when unknown (interface down)
	ChannelWidth int    // enum nl80211_chan_width, -1 when unknown
	CenterFreq1  int
	TxPowerMBm   int // mBm, 0 when unknown
}

// Station is one associated peer (a client on an AP interface).
type Station struct {
	MAC                    net.HardwareAddr
	InactiveMs             uint32
	ConnectedSeconds       uint32
	HasConnected           bool
	RxBytes                uint64 // received by this interface from the peer
	TxBytes                uint64 // sent by this interface to the peer
	HasBytes               bool
	RxPackets, TxPackets   uint32
	HasPackets             bool
	TxRetries, TxFailed    uint32
	Signal                 int8
	HasSignal              bool
	SignalAvg              int8
	HasSignalAvg           bool
	TxBitrate              uint32 // 100 kbit/s units
	HasTxBitrate           bool
	RxBitrate              uint32 // 100 kbit/s units
	HasRxBitrate           bool
	TxBitrate16            uint16 // the legacy u16 value iwinfo averages
	HasTxBitrate16         bool
	ExpectedThroughputKbps uint32
	HasExpectedThroughput  bool
	Authorized             bool
	Authenticated          bool
	Associated             bool
	HasFlags               bool
}

// SurveyEntry is one channel of the survey dump.
type SurveyEntry struct {
	FrequencyMHz int
	Noise        int8
	HasNoise     bool
	InUse        bool
	TimeMs       uint64
	BusyMs       uint64
	RxMs         uint64
	TxMs         uint64
}

// ErrUnavailable is returned when the kernel has no nl80211 (no cfg80211,
// no radio): callers treat it as "no Wi-Fi", not as a failure.
var ErrUnavailable = errors.New("nl80211 not available")

// Client talks to nl80211. Safe for concurrent use.
type Client struct {
	mu     sync.Mutex
	conn   *genetlink.Conn
	family genetlink.Family
}

// Dial opens a generic netlink socket and resolves the nl80211 family.
func Dial() (*Client, error) {
	conn, err := genetlink.Dial(nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	family, err := conn.GetFamily("nl80211")
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return &Client{conn: conn, family: family}, nil
}

// Close releases the socket.
func (c *Client) Close() error { return c.conn.Close() }

func (c *Client) dump(cmd uint8, attrs func(ae *netlink.AttributeEncoder)) ([]genetlink.Message, error) {
	var data []byte
	if attrs != nil {
		ae := netlink.NewAttributeEncoder()
		attrs(ae)
		b, err := ae.Encode()
		if err != nil {
			return nil, err
		}
		data = b
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn.Execute(genetlink.Message{
		Header: genetlink.Header{Command: cmd, Version: c.family.Version},
		Data:   data,
	}, c.family.ID, netlink.Request|netlink.Dump)
}

// Interfaces dumps every wireless interface.
func (c *Client) Interfaces() ([]Interface, error) {
	msgs, err := c.dump(cmdGetInterface, nil)
	if err != nil {
		return nil, err
	}
	out := make([]Interface, 0, len(msgs))
	for _, m := range msgs {
		ifi, err := parseInterface(m.Data)
		if err != nil {
			return nil, err
		}
		if ifi.Index == 0 {
			continue // wdev without a netdev (P2P device)
		}
		out = append(out, ifi)
	}
	return out, nil
}

// Stations dumps the peers of one interface.
func (c *Client) Stations(ifindex int) ([]Station, error) {
	msgs, err := c.dump(cmdGetStation, func(ae *netlink.AttributeEncoder) {
		ae.Uint32(attrIfindex, uint32(ifindex))
	})
	if err != nil {
		return nil, err
	}
	out := make([]Station, 0, len(msgs))
	for _, m := range msgs {
		st, err := parseStation(m.Data)
		if err != nil {
			return nil, err
		}
		if st.MAC == nil {
			continue
		}
		out = append(out, st)
	}
	return out, nil
}

// Survey dumps the channel survey of one interface.
func (c *Client) Survey(ifindex int) ([]SurveyEntry, error) {
	msgs, err := c.dump(cmdGetSurvey, func(ae *netlink.AttributeEncoder) {
		ae.Uint32(attrIfindex, uint32(ifindex))
	})
	if err != nil {
		return nil, err
	}
	out := make([]SurveyEntry, 0, len(msgs))
	for _, m := range msgs {
		e, ok, err := parseSurvey(m.Data)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, e)
		}
	}
	return out, nil
}

// RegDomain returns the global regulatory domain ("PH", "US", "00").
func (c *Client) RegDomain() (string, error) {
	c.mu.Lock()
	msgs, err := c.conn.Execute(genetlink.Message{
		Header: genetlink.Header{Command: cmdGetReg, Version: c.family.Version},
	}, c.family.ID, netlink.Request)
	c.mu.Unlock()
	if err != nil {
		return "", err
	}
	for _, m := range msgs {
		ad, err := netlink.NewAttributeDecoder(m.Data)
		if err != nil {
			return "", err
		}
		for ad.Next() {
			if ad.Type() == attrRegAlpha2 {
				return strings.TrimRight(string(ad.Bytes()), "\x00"), nil
			}
		}
	}
	return "", nil
}

func parseInterface(b []byte) (Interface, error) {
	ifi := Interface{ChannelWidth: -1}
	ad, err := netlink.NewAttributeDecoder(b)
	if err != nil {
		return ifi, err
	}
	for ad.Next() {
		v := ad.Bytes()
		switch ad.Type() {
		case attrIfindex:
			ifi.Index = int(u32(v))
		case attrIfname:
			ifi.Name = cstring(v)
		case attrWiphy:
			ifi.Wiphy = int(u32(v))
		case attrIftype:
			ifi.Type = IfType(u32(v))
		case attrMAC:
			if len(v) == 6 {
				ifi.MAC = net.HardwareAddr(append([]byte(nil), v...))
			}
		case attrSSID:
			ifi.SSID = string(v)
		case attrWiphyFreq:
			ifi.FrequencyMHz = int(u32(v))
		case attrChannelWidth:
			ifi.ChannelWidth = int(u32(v))
		case attrCenterFreq1:
			ifi.CenterFreq1 = int(u32(v))
		case attrWiphyTxPowerLvl:
			ifi.TxPowerMBm = int(int32(u32(v)))
		}
	}
	return ifi, ad.Err()
}

func parseStation(b []byte) (Station, error) {
	var st Station
	ad, err := netlink.NewAttributeDecoder(b)
	if err != nil {
		return st, err
	}
	for ad.Next() {
		v := ad.Bytes()
		switch ad.Type() {
		case attrMAC:
			if len(v) == 6 {
				st.MAC = net.HardwareAddr(append([]byte(nil), v...))
			}
		case attrStaInfo:
			if err := parseStaInfo(v, &st); err != nil {
				return st, err
			}
		}
	}
	return st, ad.Err()
}

func parseStaInfo(b []byte, st *Station) error {
	ad, err := netlink.NewAttributeDecoder(b)
	if err != nil {
		return err
	}
	var rx32, tx32 uint64
	var have32 bool
	for ad.Next() {
		v := ad.Bytes()
		switch ad.Type() {
		case staInactiveTime:
			st.InactiveMs = u32(v)
		case staConnectedTime:
			st.ConnectedSeconds, st.HasConnected = u32(v), true
		case staRxBytes:
			rx32, have32 = uint64(u32(v)), true
		case staTxBytes:
			tx32, have32 = uint64(u32(v)), true
		case staRxBytes64:
			st.RxBytes, st.HasBytes = u64(v), true
		case staTxBytes64:
			st.TxBytes, st.HasBytes = u64(v), true
		case staRxPackets:
			st.RxPackets, st.HasPackets = u32(v), true
		case staTxPackets:
			st.TxPackets, st.HasPackets = u32(v), true
		case staTxRetries:
			st.TxRetries = u32(v)
		case staTxFailed:
			st.TxFailed = u32(v)
		case staSignal:
			if len(v) >= 1 {
				st.Signal, st.HasSignal = int8(v[0]), true
			}
		case staSignalAvg:
			if len(v) >= 1 {
				st.SignalAvg, st.HasSignalAvg = int8(v[0]), true
			}
		case staTxBitrate:
			r32, r16, has32, has16 := parseRate(v)
			if has32 {
				st.TxBitrate, st.HasTxBitrate = r32, true
			} else if has16 {
				st.TxBitrate, st.HasTxBitrate = uint32(r16), true
			}
			st.TxBitrate16, st.HasTxBitrate16 = r16, has16
		case staRxBitrate:
			r32, r16, has32, has16 := parseRate(v)
			if has32 {
				st.RxBitrate, st.HasRxBitrate = r32, true
			} else if has16 {
				st.RxBitrate, st.HasRxBitrate = uint32(r16), true
			}
		case staExpectedThroughput:
			st.ExpectedThroughputKbps, st.HasExpectedThroughput = u32(v), true
		case staFlags:
			// struct nl80211_sta_flag_update { __u32 mask; __u32 set; }
			if len(v) >= 8 {
				mask := binary.NativeEndian.Uint32(v[0:4])
				set := binary.NativeEndian.Uint32(v[4:8])
				flag := func(bit uint) bool { return mask&(1<<bit) != 0 && set&(1<<bit) != 0 }
				st.Authorized = flag(staFlagAuthorized)
				st.Authenticated = flag(staFlagAuthenticated)
				st.Associated = flag(staFlagAssociated)
				st.HasFlags = true
			}
		}
	}
	if !st.HasBytes && have32 {
		st.RxBytes, st.TxBytes, st.HasBytes = rx32, tx32, true
	}
	return ad.Err()
}

func parseRate(b []byte) (r32 uint32, r16 uint16, has32, has16 bool) {
	ad, err := netlink.NewAttributeDecoder(b)
	if err != nil {
		return
	}
	for ad.Next() {
		v := ad.Bytes()
		switch ad.Type() {
		case rateBitrate32:
			r32, has32 = u32(v), true
		case rateBitrate:
			if len(v) >= 2 {
				r16, has16 = binary.NativeEndian.Uint16(v), true
			}
		}
	}
	return
}

func parseSurvey(b []byte) (SurveyEntry, bool, error) {
	var e SurveyEntry
	found := false
	ad, err := netlink.NewAttributeDecoder(b)
	if err != nil {
		return e, false, err
	}
	for ad.Next() {
		if ad.Type() != attrSurveyInfo {
			continue
		}
		found = true
		nad, err := netlink.NewAttributeDecoder(ad.Bytes())
		if err != nil {
			return e, false, err
		}
		for nad.Next() {
			v := nad.Bytes()
			switch nad.Type() {
			case surveyFrequency:
				e.FrequencyMHz = int(u32(v))
			case surveyNoise:
				if len(v) >= 1 {
					e.Noise, e.HasNoise = int8(v[0]), true
				}
			case surveyInUse:
				e.InUse = true
			case surveyTime:
				e.TimeMs = u64(v)
			case surveyTimeBusy:
				e.BusyMs = u64(v)
			case surveyTimeRx:
				e.RxMs = u64(v)
			case surveyTimeTx:
				e.TxMs = u64(v)
			}
		}
		if err := nad.Err(); err != nil {
			return e, false, err
		}
	}
	return e, found, ad.Err()
}

// u32/u64 decode host-endian integers, tolerating the other width.
func u32(b []byte) uint32 {
	switch {
	case len(b) >= 4:
		return binary.NativeEndian.Uint32(b)
	case len(b) >= 2:
		return uint32(binary.NativeEndian.Uint16(b))
	case len(b) == 1:
		return uint32(b[0])
	}
	return 0
}

func u64(b []byte) uint64 {
	if len(b) >= 8 {
		return binary.NativeEndian.Uint64(b)
	}
	return uint64(u32(b))
}

func cstring(b []byte) string { return strings.TrimRight(string(b), "\x00") }

// FreqToChannel converts a centre frequency to a channel number the way
// iwinfo does (so labels match node_exporter-lua).
func FreqToChannel(freq int) int {
	switch {
	case freq <= 0:
		return 0
	case freq == 2484:
		return 14
	case freq < 2484:
		return (freq - 2407) / 5
	case freq >= 4910 && freq <= 4980:
		return (freq - 4000) / 5
	case freq < 5950:
		return (freq - 5000) / 5
	case freq <= 45000:
		return (freq - 5950) / 5
	case freq >= 58320 && freq <= 70200:
		return (freq - 56160) / 2160
	}
	return 0
}

// Band names a frequency the way the Perch Network Controller does: "2.4", "5",
// "6", "60" ("" when unknown).
func Band(freq int) string {
	switch {
	case freq <= 0:
		return ""
	case freq < 3000:
		return "2.4"
	case freq < 5900:
		return "5"
	case freq < 58000:
		return "6"
	}
	return "60"
}
