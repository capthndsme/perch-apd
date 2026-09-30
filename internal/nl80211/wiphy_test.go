package nl80211

import (
	"reflect"
	"testing"

	"github.com/mdlayher/netlink"
)

func flags(ae *netlink.AttributeEncoder, typ uint16, types ...IfType) {
	ae.Nested(typ, func(n *netlink.AttributeEncoder) error {
		for _, t := range types {
			n.Flag(uint16(t), true)
		}
		return nil
	})
}

func freqs(list ...func(*netlink.AttributeEncoder)) func(*netlink.AttributeEncoder) error {
	return func(n *netlink.AttributeEncoder) error {
		for i, f := range list {
			n.Nested(uint16(i), func(fe *netlink.AttributeEncoder) error { f(fe); return nil })
		}
		return nil
	}
}

// A split dump as a DBDC MT7915 sends it: phy1's 5 GHz channels arrive in
// two messages, its capabilities and combinations in others.
func TestParseWiphysSplitDump(t *testing.T) {
	var msgs [][]byte
	msgs = append(msgs, encode(t, func(ae *netlink.AttributeEncoder) {
		ae.Uint32(attrWiphy, 1)
		ae.String(attrWiphyName, "phy1")
		ae.Flag(attrWiphySelfManagedReg, true)
		ae.Uint32(attrWiphyAntennaAvailTx, 3)
		flags(ae, attrSupportedIftypes, IfTypeStation, IfTypeAP, IfTypeAPVLAN, IfTypeMeshPoint)
	}))
	msgs = append(msgs, encode(t, func(ae *netlink.AttributeEncoder) {
		ae.Uint32(attrWiphy, 1)
		ae.Nested(attrWiphyBands, func(b *netlink.AttributeEncoder) error {
			b.Nested(Band5GHz, func(n *netlink.AttributeEncoder) error {
				n.Uint16(bandAttrHTCapa, 0x09ef)
				n.Uint32(bandAttrVHTCapa, 0x339071b2)
				n.Nested(bandAttrIftypeData, func(d *netlink.AttributeEncoder) error {
					d.Nested(0, func(e *netlink.AttributeEncoder) error {
						flags(e, bandIftypeAttrIftypes, IfTypeAP)
						e.Bytes(bandIftypeAttrHECapPhy, []byte{0x06, 0x20, 0, 0, 0, 0, 0, 0, 0, 0, 0})
						return nil
					})
					return nil
				})
				n.Nested(bandAttrFreqs, freqs(
					func(f *netlink.AttributeEncoder) {
						f.Uint32(freqAttrFreq, 5180)
						f.Uint32(freqAttrMaxTxPower, 2300)
					},
				))
				return nil
			})
			return nil
		})
	}))
	msgs = append(msgs, encode(t, func(ae *netlink.AttributeEncoder) {
		ae.Uint32(attrWiphy, 1)
		ae.Nested(attrWiphyBands, func(b *netlink.AttributeEncoder) error {
			b.Nested(Band5GHz, func(n *netlink.AttributeEncoder) error {
				n.Nested(bandAttrFreqs, freqs(
					func(f *netlink.AttributeEncoder) {
						f.Uint32(freqAttrFreq, 5500)
						f.Flag(freqAttrNoIR, true)
						f.Flag(freqAttrRadar, true)
						f.Uint32(freqAttrMaxTxPower, 2300)
						f.Uint32(freqAttrDFSState, DFSUsable)
						f.Uint32(freqAttrDFSCACTime, 60000)
						f.Flag(freqAttrNo160MHz, true)
					},
					func(f *netlink.AttributeEncoder) {
						f.Uint32(freqAttrFreq, 5845)
						f.Flag(freqAttrDisabled, true)
					},
				))
				return nil
			})
			return nil
		})
	}))
	msgs = append(msgs, encode(t, func(ae *netlink.AttributeEncoder) {
		ae.Uint32(attrWiphy, 1)
		ae.Nested(attrInterfaceCombinations, func(c *netlink.AttributeEncoder) error {
			c.Nested(1, func(n *netlink.AttributeEncoder) error {
				n.Nested(ifaceCombLimits, func(l *netlink.AttributeEncoder) error {
					l.Nested(1, func(x *netlink.AttributeEncoder) error {
						x.Uint32(ifaceLimitMax, 1)
						flags(x, ifaceLimitTypes, IfTypeStation)
						return nil
					})
					l.Nested(2, func(x *netlink.AttributeEncoder) error {
						x.Uint32(ifaceLimitMax, 16)
						flags(x, ifaceLimitTypes, IfTypeAP, IfTypeMeshPoint)
						return nil
					})
					return nil
				})
				n.Uint32(ifaceCombMaxNum, 16)
				n.Uint32(ifaceCombNumChannels, 1)
				n.Uint32(ifaceCombRadarDetectWidths, 0x3f)
				return nil
			})
			return nil
		})
	}))
	// Another wiphy in between, with a band of its own.
	msgs = append(msgs, encode(t, func(ae *netlink.AttributeEncoder) {
		ae.Uint32(attrWiphy, 0)
		ae.String(attrWiphyName, "phy0")
		flags(ae, attrSupportedIftypes, IfTypeAP)
		ae.Nested(attrWiphyBands, func(b *netlink.AttributeEncoder) error {
			b.Nested(Band2GHz, func(n *netlink.AttributeEncoder) error {
				n.Uint16(bandAttrHTCapa, 0x01ef)
				n.Nested(bandAttrFreqs, freqs(func(f *netlink.AttributeEncoder) { f.Uint32(freqAttrFreq, 2412) }))
				return nil
			})
			return nil
		})
	}))
	got, err := parseWiphys(msgs)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "phy0" || got[1].Name != "phy1" {
		t.Fatalf("%+v", got)
	}
	p0, p1 := got[0], got[1]
	if p0.MaxInterfaces(IfTypeAP) != 1 || p0.SelfManagedReg {
		t.Fatalf("phy0 %+v", p0)
	}
	if !p1.SelfManagedReg || p1.AntennaAvailTx != 3 || p1.MaxInterfaces(IfTypeAP) != 16 ||
		p1.MaxInterfaces(IfTypeStation) != 1 || p1.MaxInterfaces(IfTypeAdhoc) != 0 {
		t.Fatalf("phy1 %+v", p1)
	}
	b, ok := p1.Band(Band5GHz)
	if !ok || b.HTCapa != 0x09ef || b.VHTCapa != 0x339071b2 || !b.HasHE || b.HEPhyCap0 != 0x06 || b.HasEHT {
		t.Fatalf("band %+v", b)
	}
	want := []WiphyFreq{
		{MHz: 5180, MaxTxPowerMBm: 2300},
		{MHz: 5500, NoIR: true, Radar: true, MaxTxPowerMBm: 2300, DFSState: DFSUsable, CACTimeMs: 60000, No160MHz: true},
		{MHz: 5845, Disabled: true},
	}
	if !reflect.DeepEqual(b.Freqs, want) {
		t.Fatalf("freqs %+v", b.Freqs)
	}
	c := p1.Combinations[0]
	if c.MaxNum != 16 || c.NumChannels != 1 || c.RadarDetectWidths != 0x3f || len(c.Limits) != 2 {
		t.Fatalf("%+v", c)
	}
	if _, ok := p1.Band(Band2GHz); ok {
		t.Fatal("phy0's band on phy1")
	}
}

func TestParseRegDomains(t *testing.T) {
	msgs := [][]byte{
		encode(t, func(ae *netlink.AttributeEncoder) {
			ae.String(attrRegAlpha2, "PH")
			ae.Uint8(attrDFSRegion, DFSFCC)
		}),
		encode(t, func(ae *netlink.AttributeEncoder) {
			ae.Uint32(attrWiphy, 0)
			ae.Flag(attrWiphySelfManagedReg, true)
			ae.String(attrRegAlpha2, "US")
			ae.Uint8(attrDFSRegion, DFSFCC)
		}),
	}
	got, err := parseRegDomains(msgs)
	if err != nil {
		t.Fatal(err)
	}
	want := []RegDomain{{Wiphy: -1, Alpha2: "PH", DFSRegion: DFSFCC}, {Wiphy: 0, Alpha2: "US", DFSRegion: DFSFCC}}
	if !reflect.DeepEqual(got, want) || DFSRegionName(DFSETSI) != "ETSI" {
		t.Fatalf("%+v", got)
	}
	if ChannelWidthMHz(3) != 80 || ChannelWidthMHz(5) != 160 || ChannelWidthMHz(99) != 0 {
		t.Fatal("widths")
	}
}
