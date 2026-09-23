package groups

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParsePSKLine(t *testing.T) {
	cases := []struct {
		line string
		want pskEntry
		ok   bool
	}{
		// 24.10's hostapd.sh writes two spaces after the VLAN.
		{"vlanid=131  00:00:00:00:00:00 building-key-1", pskEntry{MAC: wildcardMAC, Key: "building-key-1", VLAN: "131"}, true},
		{"vlanid=130 02:00:00:00:01:00 a key with spaces", pskEntry{MAC: "02:00:00:00:01:00", Key: "a key with spaces", VLAN: "130"}, true},
		{"keyid=x wps=1 02:00:00:00:01:AA secret-123", pskEntry{MAC: "02:00:00:00:01:aa", Key: "secret-123"}, true},
		{"# comment", pskEntry{}, false},
		{"02:00:00:00:01:00", pskEntry{}, false},
		{"", pskEntry{}, false},
	}
	for _, c := range cases {
		got, ok := parsePSKLine(c.line)
		if ok != c.ok || got != c.want {
			t.Fatalf("%q: %+v %v, want %+v %v", c.line, got, ok, c.want, c.ok)
		}
	}
}

// A binding that lost its MAC on the way to hostapd fails the confirm and
// the AP rolls back.
func TestConfirmRefusesABindingWithoutItsMAC(t *testing.T) {
	v := newEnv(t, filteringNet)
	e := v.engine(t)
	ctx := context.Background()
	if _, err := e.Apply(ctx, desired()); err != nil {
		t.Fatal(err)
	}
	psk := filepath.Join(v.fake.Root, "var/run/hostapd-phy0-ap0.psk")
	os.MkdirAll(filepath.Dir(psk), 0o755)
	os.WriteFile(psk, []byte("vlanid=101  00:00:00:00:00:00 unit-101-key\nvlanid=102  00:00:00:00:00:00 building-key-1\n"), 0o600)
	err := e.Confirm(ctx, 3)
	if err == nil || err.(*Refusal).Code != "unsafe_binding" || !strings.Contains(err.Error(), "VLAN 102") {
		t.Fatalf("%v", err)
	}
	if read(t, v.path("wireless")) != wirelessConf || read(t, v.path("network")) != filteringNet {
		t.Fatal("not rolled back")
	}
	if st := e.State(ctx); st.Pending != nil || st.LastRollback == nil || st.AppliedRevision != 0 {
		t.Fatalf("%+v", st)
	}

	// The same revision with the MAC in place is kept.
	if _, err := e.Apply(ctx, desired()); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(psk, []byte("vlanid=101  00:00:00:00:00:00 unit-101-key\nvlanid=102  02:00:00:00:00:21 building-key-1\n"), 0o600)
	if err := e.Confirm(ctx, 3); err != nil {
		t.Fatal(err)
	}
	if e.State(ctx).AppliedRevision != 3 {
		t.Fatal("not applied")
	}
}
