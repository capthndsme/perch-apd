package main

import "testing"

func TestGroupVLANID(t *testing.T) {
	for _, c := range []struct {
		name string
		vid  int
		ok   bool
	}{
		{"phy0-ap0-g10", 10, true},
		{"wlan1-g4094", 4094, true},
		{"wlan1-g0", 0, true},
		{"a-g1-g23", 23, true},
		{"wlan1-g12345", 0, false}, // more than four digits
		{"wlan1-g", 0, false},
		{"wlan1-g1x", 0, false},
		{"wlan1-g-1", 0, false},
		{"wlan1.10", 0, false},
		{"phy0-ap0", 0, false},
	} {
		vid, ok := groupVLANID(c.name)
		if vid != c.vid || ok != c.ok {
			t.Errorf("groupVLANID(%q) = %d, %v; want %d, %v", c.name, vid, ok, c.vid, c.ok)
		}
	}
}
