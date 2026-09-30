package groups

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/capthndsme/perch-agentkit/openwrt/uci"
	"github.com/capthndsme/perch-apd/internal/applylock"
)

// Sections the Wi-Fi config plane creates (wifi README section 9): an SSID
// per radio, and the plumbing of a VLAN-bound SSID on the trunk bridge.
const planeWireless = `
config wifi-iface 'perch_n12_radio0'
	option device 'radio0'
	option mode 'ap'
	option ssid 'Kids'
	option encryption 'sae-mixed'
	option key 'kids-passphrase-1'
	option network 'perch_nv130'
`

const planeNetwork = `
config bridge-vlan 'perch_nbv130'
	option device 'br-lan'
	option vlan '130'
	list ports 'wan:t'

config interface 'perch_nv130'
	option proto 'none'
	option device 'br-lan.130'
`

// A leftover of the plane that looks close to the groups' names.
const planeNear = `
config bridge-vlan 'perch_nbvu'
	option device 'br-lan'
	option vlan '99'
	list ports 'lan2:u*'
`

func sectionsOf(c *uci.Config, names ...string) string {
	var b strings.Builder
	for _, name := range names {
		s := c.Section(name)
		if s == nil {
			b.WriteString(name + ": missing\n")
			continue
		}
		b.WriteString(name + ": " + s.Type)
		for _, o := range s.Options {
			b.WriteString(" " + o.Name + "=" + o.Value.Str())
		}
		b.WriteString("\n")
	}
	return b.String()
}

var planeNames = []string{"perch_nbv130", "perch_nv130"}

func TestOwnedName(t *testing.T) {
	for name, want := range map[string]bool{
		"perch_ws0": true, "perch_ws12": true, "perch_wv101_default_radio0": true, "perch_v101": true,
		"perch_bv101": true, "perch_bvu": true, "perch_dv101": true, "perch_bd101": true,
		// The plane's names, and near misses.
		"perch_n12_radio1": false, "perch_nv130": false, "perch_nbv130": false, "perch_nbvu": false,
		"perch_ws": false, "perch_v": false, "perch_wv101": false, "perch_wv101_": false, "perch_wv_x": false,
		"perch_wv1_a-b": false, "perch_v1a": false, "perch_bvux": false, "lan": false, "wifinet3": false,
	} {
		if OwnedName(name) != want {
			t.Errorf("OwnedName(%q) = %v", name, !want)
		}
	}
}

// Every groups apply used to remove every perch_* section: the plane's
// would have been wiped by the next one. It removes only its own now.
func TestPlanLeavesThePlanesSections(t *testing.T) {
	w := parse(t, "wireless", wirelessConf+planeWireless)
	n := parse(t, "network", filteringNet+planeNetwork+planeNear)
	before := sectionsOf(w, "perch_n12_radio0") + sectionsOf(n, append(planeNames, "perch_nbvu")...)

	for _, prev := range []Ledger{{}, {Owned: []string{}}} { // a ledger without names, and an empty one
		res, err := Plan(w, n, desired(), Facts{TrunkPort: "wan"}, prev)
		if err != nil {
			t.Fatal(err)
		}
		if got := sectionsOf(res.Wireless, "perch_n12_radio0") + sectionsOf(res.Network, append(planeNames, "perch_nbvu")...); got != before {
			t.Fatalf("plane sections changed:\n%s\nwant\n%s", got, before)
		}
		want := []string{"perch_bv101", "perch_bv102", "perch_v101", "perch_v102", "perch_wv101_default_radio0",
			"perch_wv102_default_radio0", "perch_ws0", "perch_ws1"}
		if !reflect.DeepEqual(res.Ledger.Owned, want) {
			t.Fatalf("owned %v", res.Ledger.Owned)
		}

		// Again from the result: the same configs.
		again, err := Plan(res.Wireless, res.Network, desired(), Facts{TrunkPort: "wan"}, res.Ledger)
		if err != nil {
			t.Fatal(err)
		}
		if string(uci.Render(again.Network)) != string(uci.Render(res.Network)) || string(uci.Render(again.Wireless)) != string(uci.Render(res.Wireless)) {
			t.Fatal("not idempotent")
		}
		// No groups any more: back to the start, the plane's sections included.
		back, err := Plan(res.Wireless, res.Network, Desired{Revision: 9}, Facts{}, res.Ledger)
		if err != nil {
			t.Fatal(err)
		}
		if string(uci.Render(back.Network)) != string(uci.Render(n)) || string(uci.Render(back.Wireless)) != string(uci.Render(w)) {
			t.Fatalf("not restored:\n%s", uci.Render(back.Network))
		}
		if back.Ledger.Owned == nil || len(back.Ledger.Owned) != 0 {
			t.Fatalf("owned %#v", back.Ledger.Owned)
		}

		// A state.json from before the names were recorded: the groups'
		// sections go by pattern, the plane's stay.
		legacy := res.Ledger
		legacy.Owned = nil
		back, err = Plan(res.Wireless, res.Network, Desired{Revision: 9}, Facts{}, legacy)
		if err != nil || string(uci.Render(back.Network)) != string(uci.Render(n)) || string(uci.Render(back.Wireless)) != string(uci.Render(w)) {
			t.Fatalf("legacy ledger: %v\n%s", err, uci.Render(back.Network))
		}
	}
}

// A VLAN the plane already carries on the bridge is used, not duplicated.
func TestPlanUsesAVLANThePlaneCarries(t *testing.T) {
	d := desired()
	d.VLANs = append(d.VLANs, VLAN{VID: 130})
	d.Stations = append(d.Stations, Station{Key: "unit-130-key", VID: 130})
	n := parse(t, "network", filteringNet+planeNetwork)
	res, err := Plan(parse(t, "wireless", wirelessConf), n, d, Facts{TrunkPort: "wan"}, Ledger{Owned: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Network.Section("perch_bv130") != nil {
		t.Fatal("VLAN 130 duplicated")
	}
	if !strings.Contains(strings.Join(res.Issues, "\n"), "VLAN 130 is already on br-lan") {
		t.Fatalf("issues %v", res.Issues)
	}
	// The plane's plumbing without the trunk port tagged is flagged.
	n = parse(t, "network", filteringNet+strings.Replace(planeNetwork, "list ports 'wan:t'", "list ports 'lan1:t'\n\tlist ports 'wan:u'", 1))
	res, err = Plan(parse(t, "wireless", wirelessConf), n, d, Facts{TrunkPort: "wan"}, Ledger{Owned: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(res.Issues, "\n"), "does not carry wan tagged") {
		t.Fatalf("issues %v", res.Issues)
	}
}

// A converted bridge stays converted while another VLAN (the plane's) is
// on it; it is undone once only the groups' were.
func TestConversionKeptWhileOtherVLANsRemain(t *testing.T) {
	w, n := parse(t, "wireless", wirelessConf), parse(t, "network", untaggedNet)
	res, err := Plan(w, n, desired(), Facts{TrunkPort: "wan"}, Ledger{})
	if err != nil || !res.Converted {
		t.Fatalf("%v %+v", err, res)
	}
	// The plane adds a VLAN of its own on the (now filtering) bridge.
	withPlane := parse(t, "network", string(uci.Render(res.Network))+planeNetwork)

	back, err := Plan(res.Wireless, withPlane, Desired{Revision: 4}, Facts{}, res.Ledger)
	if err != nil {
		t.Fatal(err)
	}
	if !back.Converted || back.Ledger.Converted == nil || !reflect.DeepEqual(back.Ledger.Owned, []string{"perch_bvu"}) {
		t.Fatalf("conversion not kept: %+v", back.Ledger)
	}
	if got := sectionsOf(back.Network, "perch_bvu", "lan", "lan6", "perch_nbv130", "perch_bv101"); got !=
		"perch_bvu: bridge-vlan device=br-lan vlan=1 ports=lan2:u* wan:u*\n"+
			"lan: interface device=br-lan.1 proto=static\n"+
			"lan6: interface device=br-lan.1 proto=none\n"+
			"perch_nbv130: bridge-vlan device=br-lan vlan=130 ports=wan:t\n"+
			"perch_bv101: missing\n" {
		t.Fatalf("%s", got)
	}
	if string(uci.Render(back.Wireless)) != string(uci.Render(w)) {
		t.Fatal("wireless not restored")
	}
	// The groups come back: the kept conversion is reused, not redone.
	again, err := Plan(back.Wireless, back.Network, desired(), Facts{TrunkPort: "wan"}, back.Ledger)
	if err != nil || !again.Converted || again.Ledger.Converted.UntaggedVLAN != 1 {
		t.Fatalf("%v %+v", err, again)
	}
	if strings.Count(string(uci.Render(again.Network)), "'perch_bvu'") != 1 {
		t.Fatal("perch_bvu duplicated")
	}
	// A group on the kept untagged VLAN is refused.
	d := desired()
	d.VLANs = append(d.VLANs, VLAN{VID: 1})
	if _, err := Plan(back.Wireless, back.Network, d, Facts{TrunkPort: "wan"}, back.Ledger); code(err) != "untagged_vlan_conflict" {
		t.Fatalf("%v", err)
	}
	// The plane's VLAN goes: now the conversion is undone as well.
	noPlane := back.Network.Clone()
	var kept []*uci.Section
	for _, s := range noPlane.Sections {
		if s.Name != "perch_nbv130" && s.Name != "perch_nv130" {
			kept = append(kept, s)
		}
	}
	noPlane.Sections = kept
	noPlane.Reindex()
	final, err := Plan(back.Wireless, noPlane, Desired{Revision: 5}, Facts{}, back.Ledger)
	if err != nil {
		t.Fatal(err)
	}
	if string(uci.Render(final.Network)) != string(uci.Render(n)) || final.Converted || final.Ledger.Converted != nil {
		t.Fatalf("not undone:\n%s", uci.Render(final.Network))
	}
}

func code(err error) string {
	var r *Refusal
	if errors.As(err, &r) {
		return r.Code
	}
	return ""
}

// A section with one of the groups' names that the engine did not create
// is never merged into (libuci would merge two sections of one name).
func TestNameTakenRefuses(t *testing.T) {
	foreign := "\nconfig wifi-station 'perch_ws0'\n\toption iface 'default_radio0'\n\toption key 'someone-elses'\n"
	w := parse(t, "wireless", wirelessConf+foreign)
	if _, err := Plan(w, parse(t, "network", filteringNet), desired(), Facts{TrunkPort: "wan"}, Ledger{Owned: []string{}}); code(err) != "name_taken" {
		t.Fatalf("%v", err)
	}
	// Listed as the engine's own, it is replaced.
	res, err := Plan(w, parse(t, "network", filteringNet), desired(), Facts{TrunkPort: "wan"}, Ledger{Owned: []string{"perch_ws0"}})
	if err != nil || section(res.Wireless, "perch_ws0")["key"] != "unit-101-key" {
		t.Fatalf("%v", err)
	}
}

func TestLedgerOwnedSurvivesJSON(t *testing.T) {
	for _, l := range []Ledger{{}, {Owned: []string{}}, {Owned: []string{"perch_ws0"}}} {
		b, _ := json.Marshal(l)
		var back Ledger
		if err := json.Unmarshal(b, &back); err != nil {
			t.Fatal(err)
		}
		if (back.Owned == nil) != (l.Owned == nil) || len(back.Owned) != len(l.Owned) {
			t.Fatalf("%s: %#v", b, back.Owned)
		}
	}
	// state.json from 1.1.0-pre.1: no "owned" key.
	var old state
	if err := json.Unmarshal([]byte(`{"appliedRevision":3,"ledger":{"dynamicVlan":{"default_radio0":""}}}`), &old); err != nil || old.Ledger.Owned != nil {
		t.Fatalf("%v %#v", err, old.Ledger.Owned)
	}
}

// The engine against real files: the plane's sections survive a groups
// apply, its confirm, and the removal of every group.
func TestEngineKeepsThePlanesSections(t *testing.T) {
	v := newEnv(t, filteringNet+planeNetwork)
	os.WriteFile(v.path("wireless"), []byte(wirelessConf+planeWireless), 0o644)
	e := v.engine(t)
	ctx := context.Background()
	if _, err := e.Apply(ctx, desired()); err != nil {
		t.Fatal(err)
	}
	if err := e.Confirm(ctx, 3); err != nil {
		t.Fatal(err)
	}
	for _, name := range append(planeNames, "perch_bv101", "perch_ws0") {
		if !strings.Contains(read(t, v.path("network"))+read(t, v.path("wireless")), "'"+name+"'") {
			t.Fatalf("%s missing", name)
		}
	}
	if _, err := e.Apply(ctx, Desired{Revision: 4, SSIDs: []string{"Apartment"}}); err != nil {
		t.Fatal(err)
	}
	if err := e.Confirm(ctx, 4); err != nil {
		t.Fatal(err)
	}
	if read(t, v.path("network")) != string(uci.Render(parse(t, "network", filteringNet+planeNetwork))) ||
		read(t, v.path("wireless")) != string(uci.Render(parse(t, "wireless", wirelessConf+planeWireless))) {
		t.Fatalf("plane sections not intact:\n%s", read(t, v.path("network")))
	}
}

func busyReason(err error) string {
	var r *Refusal
	if errors.As(err, &r) && r.Code == "busy" {
		return r.Reason
	}
	return ""
}

// The shared write lock: a groups apply waits for any other writer's
// window, and holds the lock through its own.
func TestEngineWriteLock(t *testing.T) {
	v := newEnv(t, filteringNet)
	lock := applylock.New()
	e := v.engine(t)
	e.o.Lock = lock
	ctx := context.Background()

	plane, err := lock.TryAcquire(applylock.Plane, "a4-000000000001")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Apply(ctx, desired()); busyReason(err) != "plane_pending" {
		t.Fatalf("%v", err)
	}
	if read(t, v.path("network")) != filteringNet || len(v.rec.list()) != 0 {
		t.Fatal("written while the plane held the lock")
	}
	plane.Release()

	// Its own window holds the lock until the confirm.
	if _, err := e.Apply(ctx, desired()); err != nil {
		t.Fatal(err)
	}
	if info, ok := lock.Current(); !ok || info.Holder != applylock.Groups || info.ID != "groups-3" {
		t.Fatalf("%+v", info)
	}
	if _, err := lock.TryAcquire(applylock.Update, "u1"); applylock.Reason(err) != "groups_pending" {
		t.Fatalf("%v", err)
	}
	other := desired()
	other.Revision = 4
	if _, err := e.Apply(ctx, other); busyReason(err) != "groups_pending" {
		t.Fatalf("%v", err)
	}
	if err := e.Confirm(ctx, 3); err != nil {
		t.Fatal(err)
	}
	if lock.Busy() != "" {
		t.Fatal("confirm kept the lock")
	}

	// A rollback releases it too; a noop never holds it.
	next := desired()
	next.Revision = 5
	next.Stations = next.Stations[:1]
	if _, err := e.Apply(ctx, next); err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	e.rollbackLocked(ctx, "no confirm from the controller")
	e.mu.Unlock()
	if lock.Busy() != "" {
		t.Fatal("rollback kept the lock")
	}
	same := desired()
	same.Revision = 6
	if res, err := e.Apply(ctx, same); err != nil || res.State != "noop" || lock.Busy() != "" {
		t.Fatalf("%+v %v %q", res, err, lock.Busy())
	}

	// A plan refusal releases it.
	bad := desired()
	bad.Revision = 7
	bad.SSIDs = []string{"Nowhere"}
	if _, err := e.Apply(ctx, bad); code(err) != "no_managed_iface" || lock.Busy() != "" {
		t.Fatalf("%v %q", err, lock.Busy())
	}
}

// A window still open at start takes the lock again.
func TestStartRetakesTheLock(t *testing.T) {
	v := newEnv(t, filteringNet)
	e := v.engine(t)
	ctx := context.Background()
	if _, err := e.Apply(ctx, desired()); err != nil {
		t.Fatal(err)
	}
	e.timer.Stop()
	// The daemon restarts inside the window.
	lock := applylock.New()
	e2 := v.engine(t)
	e2.o.Lock = lock
	e2.Start(ctx)
	defer e2.timer.Stop()
	if lock.Busy() != "groups_pending" {
		t.Fatal("pending window without the lock")
	}
	if err := e2.Confirm(ctx, 3); err != nil || lock.Busy() != "" {
		t.Fatalf("%v %q", err, lock.Busy())
	}
}

func TestLuCIStateRefuses(t *testing.T) {
	v := newEnv(t, filteringNet)
	e := v.engine(t)
	e.o.RpcdDir = filepath.Join(v.dir, "rpcd")
	ctx := context.Background()
	snap := filepath.Join(e.o.RpcdDir, "snapshot-files")
	os.MkdirAll(snap, 0o755)
	os.WriteFile(filepath.Join(snap, "wireless"), []byte("x"), 0o600)
	if _, err := e.Apply(ctx, desired()); busyReason(err) != "luci_pending" {
		t.Fatalf("%v", err)
	}
	os.RemoveAll(snap)
	// Changes staged in a LuCI session (rpcd), not only the uci CLI's.
	sess := filepath.Join(e.o.RpcdDir, "uci-0123456789abcdef")
	os.MkdirAll(sess, 0o700)
	os.WriteFile(filepath.Join(sess, "network"), []byte("network.lan.ipaddr='192.168.1.2'\n"), 0o600)
	if _, err := e.Apply(ctx, desired()); code(err) != "uncommitted" {
		t.Fatalf("%v", err)
	}
	os.WriteFile(filepath.Join(sess, "network"), nil, 0o600) // reverted: libuci leaves an empty file
	if _, err := e.Apply(ctx, desired()); err != nil {
		t.Fatal(err)
	}
}

// Every write is reported with its revision and the written files' hashes
// (the plane's change log names the groups engine as the author).
func TestWroteHookAndOwnedView(t *testing.T) {
	v := newEnv(t, untaggedNet)
	e := v.engine(t)
	var calls []string
	e.o.Wrote = func(id string, hashes map[string]string) {
		for _, name := range []string{"wireless", "network"} {
			if h, ok := hashes[name]; ok {
				if h != uci.FileHash([]byte(read(t, v.path(name)))) {
					t.Errorf("%s hash does not match the file", name)
				}
				calls = append(calls, id+":"+name)
			}
		}
	}
	ctx := context.Background()
	if _, err := e.Apply(ctx, desired()); err != nil {
		t.Fatal(err)
	}
	ov := e.Owned()
	if !ov.Pending || ov.Converted != "br-lan" || !reflect.DeepEqual(ov.DynamicVLAN, []string{"default_radio0"}) ||
		!reflect.DeepEqual(ov.Sections, []string{"perch_bv101", "perch_bv102", "perch_bvu", "perch_v101", "perch_v102",
			"perch_ws0", "perch_ws1", "perch_wv101_default_radio0", "perch_wv102_default_radio0"}) {
		t.Fatalf("%+v", ov)
	}
	e.mu.Lock()
	e.rollbackLocked(ctx, "test")
	e.mu.Unlock()
	if strings.Join(calls, ",") != "groups-3:wireless,groups-3:network,groups-3:wireless,groups-3:network" {
		t.Fatalf("%v", calls)
	}
	if ov := e.Owned(); ov.Pending || len(ov.Sections) != 0 || ov.Converted != "" {
		t.Fatalf("%+v", ov)
	}
}
