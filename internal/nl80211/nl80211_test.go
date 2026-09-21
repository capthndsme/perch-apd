package nl80211

import (
	"encoding/binary"
	"testing"

	"github.com/mdlayher/netlink"
)

func encode(t *testing.T, fn func(ae *netlink.AttributeEncoder)) []byte {
	t.Helper()
	ae := netlink.NewAttributeEncoder()
	fn(ae)
	b, err := ae.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseInterface(t *testing.T) {
	b := encode(t, func(ae *netlink.AttributeEncoder) {
		ae.Uint32(attrIfindex, 12)
		ae.String(attrIfname, "phy0-ap0")
		ae.Uint32(attrWiphy, 0)
		ae.Uint32(attrIftype, uint32(IfTypeAP))
		ae.Bytes(attrMAC, []byte{0x02, 0, 0, 0, 0, 0x11})
		ae.Bytes(attrSSID, []byte("Example"))
		ae.Uint32(attrWiphyFreq, 2437)
		ae.Uint32(attrChannelWidth, 1)
		ae.Uint32(attrCenterFreq1, 2437)
		ae.Uint32(attrWiphyTxPowerLvl, 2000)
		ae.Uint64(999, 7) // unknown attributes are skipped
	})
	ifi, err := parseInterface(b)
	if err != nil {
		t.Fatal(err)
	}
	if ifi.Index != 12 || ifi.Name != "phy0-ap0" || ifi.Type != IfTypeAP || ifi.SSID != "Example" ||
		ifi.MAC.String() != "02:00:00:00:00:11" || ifi.FrequencyMHz != 2437 || ifi.ChannelWidth != 1 ||
		ifi.TxPowerMBm != 2000 {
		t.Fatalf("%+v", ifi)
	}
	if ifi.Type.IwinfoMode() != "Master" {
		t.Fatal(ifi.Type.IwinfoMode())
	}
}

func TestParseStation(t *testing.T) {
	flags := make([]byte, 8)
	binary.NativeEndian.PutUint32(flags[0:], 1<<staFlagAuthorized|1<<staFlagAssociated|1<<staFlagAuthenticated)
	binary.NativeEndian.PutUint32(flags[4:], 1<<staFlagAuthorized|1<<staFlagAssociated)
	b := encode(t, func(ae *netlink.AttributeEncoder) {
		ae.Bytes(attrMAC, []byte{0x02, 0, 0, 0, 0, 0x01})
		ae.Uint32(attrIfindex, 12)
		ae.Nested(attrStaInfo, func(n *netlink.AttributeEncoder) error {
			n.Uint32(staInactiveTime, 10)
			n.Uint32(staConnectedTime, 3600)
			n.Uint32(staRxBytes, 1) // superseded by the 64-bit counters
			n.Uint32(staTxBytes, 2)
			n.Uint64(staRxBytes64, 7366132107)
			n.Uint64(staTxBytes64, 465135575)
			n.Uint32(staRxPackets, 9054069)
			n.Uint32(staTxPackets, 7037518)
			n.Uint8(staSignal, uint8(0xd7)) // -41
			n.Uint8(staSignalAvg, uint8(0xd6))
			n.Nested(staTxBitrate, func(r *netlink.AttributeEncoder) error {
				r.Uint16(rateBitrate, 722)
				r.Uint32(rateBitrate32, 722)
				return nil
			})
			n.Nested(staRxBitrate, func(r *netlink.AttributeEncoder) error {
				r.Uint32(rateBitrate32, 9607) // > u16 range handled by BITRATE32 alone
				return nil
			})
			n.Uint32(staExpectedThroughput, 64960)
			n.Bytes(staFlags, flags)
			return nil
		})
	})
	st, err := parseStation(b)
	if err != nil {
		t.Fatal(err)
	}
	if st.MAC.String() != "02:00:00:00:00:01" || st.InactiveMs != 10 || st.ConnectedSeconds != 3600 {
		t.Fatalf("%+v", st)
	}
	if st.RxBytes != 7366132107 || st.TxBytes != 465135575 || !st.HasBytes {
		t.Fatalf("bytes %+v", st)
	}
	if st.Signal != -41 || !st.HasSignal || st.SignalAvg != -42 {
		t.Fatalf("signal %+v", st)
	}
	if st.TxBitrate != 722 || st.TxBitrate16 != 722 || !st.HasTxBitrate16 || st.RxBitrate != 9607 || st.HasTxBitrate != true {
		t.Fatalf("rates %+v", st)
	}
	if st.ExpectedThroughputKbps != 64960 || !st.Authorized || !st.Associated || st.Authenticated {
		t.Fatalf("misc %+v", st)
	}
}

func TestParseStation32BitBytesOnly(t *testing.T) {
	b := encode(t, func(ae *netlink.AttributeEncoder) {
		ae.Bytes(attrMAC, []byte{0x02, 0, 0, 0, 0, 0x02})
		ae.Nested(attrStaInfo, func(n *netlink.AttributeEncoder) error {
			n.Uint32(staRxBytes, 100)
			n.Uint32(staTxBytes, 200)
			return nil
		})
	})
	st, err := parseStation(b)
	if err != nil {
		t.Fatal(err)
	}
	if !st.HasBytes || st.RxBytes != 100 || st.TxBytes != 200 || st.HasSignal || st.HasTxBitrate {
		t.Fatalf("%+v", st)
	}
}

func TestParseSurvey(t *testing.T) {
	b := encode(t, func(ae *netlink.AttributeEncoder) {
		ae.Uint32(attrIfindex, 12)
		ae.Nested(attrSurveyInfo, func(n *netlink.AttributeEncoder) error {
			n.Uint32(surveyFrequency, 2437)
			n.Uint8(surveyNoise, uint8(0xa5)) // -91
			n.Flag(surveyInUse, true)
			n.Uint64(surveyTime, 1000)
			n.Uint64(surveyTimeBusy, 250)
			n.Uint64(surveyTimeRx, 100)
			n.Uint64(surveyTimeTx, 50)
			return nil
		})
	})
	e, ok, err := parseSurvey(b)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if e.FrequencyMHz != 2437 || e.Noise != -91 || !e.HasNoise || !e.InUse || e.TimeMs != 1000 || e.BusyMs != 250 {
		t.Fatalf("%+v", e)
	}
	_, ok, _ = parseSurvey(encode(t, func(ae *netlink.AttributeEncoder) { ae.Uint32(attrIfindex, 1) }))
	if ok {
		t.Fatal("entry without survey info reported")
	}
}

func TestFreqToChannelAndBand(t *testing.T) {
	cases := []struct {
		freq    int
		channel int
		band    string
	}{
		{2412, 1, "2.4"}, {2437, 6, "2.4"}, {2472, 13, "2.4"}, {2484, 14, "2.4"},
		{5180, 36, "5"}, {5500, 100, "5"}, {5745, 149, "5"}, {5885, 177, "5"},
		{5955, 1, "6"}, {6415, 93, "6"}, {58320, 1, "60"}, {0, 0, ""},
	}
	for _, c := range cases {
		if got := FreqToChannel(c.freq); got != c.channel {
			t.Errorf("FreqToChannel(%d) = %d, want %d", c.freq, got, c.channel)
		}
		if got := Band(c.freq); got != c.band {
			t.Errorf("Band(%d) = %q, want %q", c.freq, got, c.band)
		}
	}
}
