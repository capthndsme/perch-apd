package wifiplane

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/capthndsme/perch-agentkit/openwrt/plane"
	"github.com/capthndsme/perch-agentkit/rpc"
	"github.com/capthndsme/perch-apd/internal/applylock"
	"github.com/capthndsme/perch-apd/internal/groups"
)

func (e *env) base(config string) string {
	return e.svc.Plane().Hashes()[config]
}

// kidsComesUp makes the BSS of newIfaceApply run once "reloaded".
func (e *env) kidsComesUp(status string) {
	e.wifi.set(func(w *fakeWifi) {
		w.radios["radio1"].ifaces["perch_n12_radio1"] = "phy1-ap1"
		w.hostapd["phy1-ap1"] = status
	})
}

// A change is kept when the controller confirms on a fresh session and the
// AP's own check found every expected BSS beaconing.
func TestApplyIsKeptAfterAFreshSessionAndAHealthyCheck(t *testing.T) {
	e := newEnv(t, envOpts{transportOK: true})
	e.configure(plane.ModeManaged)
	e.kidsComesUp(beacon("Kids", 100))

	res, err := e.call("wifi.config.apply", newIfaceApply("a4-000000000001", e.base("wireless")))
	if err != nil {
		t.Fatal(err)
	}
	if res["state"] != plane.StatePendingConfirm || res["reload"] != "wifi" || res["confirmTimeoutSeconds"] != float64(180) {
		t.Fatalf("apply result %v", res)
	}
	if !strings.Contains(e.read("/etc/config/wireless"), "option ssid 'Kids'") ||
		!strings.Contains(e.read("/etc/config/wireless"), "option key 'kids passphrase'") {
		t.Fatalf("not committed:\n%s", e.read("/etc/config/wireless"))
	}
	// The proof is a fresh session: the one the apply came on cannot confirm.
	_, err = e.call("wifi.config.confirm", map[string]string{"applyId": "a4-000000000001"})
	if _, data := rpcData(t, err); data["error"] != plane.CodeNotReconnected {
		t.Fatalf("confirm on the apply's session: %v", data)
	}
	if r := e.waitRedial(); r.State != plane.HealthOK {
		t.Fatalf("health %+v", r)
	}
	res, err = e.call("wifi.config.confirm", map[string]string{"applyId": "a4-000000000001"})
	if err != nil {
		t.Fatal(err)
	}
	h, _ := res["health"].(map[string]any)
	if res["state"] != plane.StateConfirmed || h["ok"] != true || h["pskGuard"] != "skipped" {
		t.Fatalf("confirm %v", res)
	}
	if !strings.Contains(e.read("/etc/config/perch-managed"), "q7m2") {
		t.Fatalf("ledger:\n%s", e.read("/etc/config/perch-managed"))
	}
	// The window is over: the lock is free for the device groups again.
	hold, err := e.lock.TryAcquire(applylock.Groups, "groups-8")
	if err != nil {
		t.Fatalf("lock after the confirm: %v", err)
	}
	hold.Release()
	e.mu.Lock()
	reloads := e.reloads
	e.mu.Unlock()
	if len(reloads) != 1 || strings.Join(reloads[0], ",") != "wireless" {
		t.Fatalf("reloads %v", reloads)
	}
}

// A radar check extends the health wait (its seconds left plus a margin),
// the confirm is answered health_pending meanwhile, and the change is kept
// once the BSS beacons.
func TestARadarCheckExtendsTheHealthWait(t *testing.T) {
	e := newEnv(t, envOpts{transportOK: true})
	e.configure(plane.ModeManaged)
	e.kidsComesUp(inCAC("Kids", 100, 58))
	if _, err := e.call("wifi.config.apply", newIfaceApply("a4-000000000002", e.base("wireless"))); err != nil {
		t.Fatal(err)
	}
	r := e.waitRedial()
	if r.State != plane.HealthPending || r.Extend != 58*time.Second+cacMargin {
		t.Fatalf("first check %+v", r)
	}
	rep := r.Report.(Report)
	if !rep.Pending || rep.OK || len(rep.Problems) != 1 || rep.Problems[0].Code != ProblemCACRunning || rep.Problems[0].Section != "radio1" {
		t.Fatalf("report %+v", rep)
	}
	_, err := e.call("wifi.config.confirm", map[string]string{"applyId": "a4-000000000002"})
	if _, data := rpcData(t, err); data["error"] != plane.CodeHealthPending || data["health"] == nil {
		t.Fatalf("confirm during the radar check: %v", data)
	}
	// Past the 45 s health wait: still waiting, the radar check is not over.
	e.clock.Advance(50 * time.Second)
	if st := e.svc.Plane().ApplyState(); st.State != plane.StatePendingConfirm || st.Health != plane.HealthPending {
		t.Fatalf("after the health wait: %+v", st)
	}
	e.kidsComesUp(beacon("Kids", 100))
	e.clock.Advance(3 * time.Second)
	if st := e.svc.Plane().ApplyState(); st.Health != plane.HealthOK {
		t.Fatalf("after the radar check: %+v", st)
	}
	if res, err := e.call("wifi.config.confirm", map[string]string{"applyId": "a4-000000000002"}); err != nil || res["state"] != plane.StateConfirmed {
		t.Fatalf("confirm %v %v", res, err)
	}
}

// A BSS that never comes up rolls the change back when the wait is over,
// not at the confirm deadline; the outcome goes to the controller with the
// last report.
func TestABSSThatStaysDownRollsBack(t *testing.T) {
	e := newEnv(t, envOpts{transportOK: true})
	e.configure(plane.ModeManaged)
	before := e.read("/etc/config/wireless")
	e.kidsComesUp(withStatus("DISABLED", "Kids"))
	if _, err := e.call("wifi.config.apply", newIfaceApply("a4-000000000003", e.base("wireless"))); err != nil {
		t.Fatal(err)
	}
	if r := e.waitRedial(); r.State != plane.HealthPending {
		t.Fatalf("first check %+v", r)
	}
	e.clock.Advance(46 * time.Second)
	if got := e.read("/etc/config/wireless"); got != before {
		t.Fatalf("not restored:\n%s", got)
	}
	results := e.notesOf("wifi.config.result")
	if len(results) != 1 {
		t.Fatalf("results %d", len(results))
	}
	var res struct {
		ApplyID, Outcome, Reason string
		Health                   Report
	}
	json.Unmarshal(results[0], &res)
	if res.ApplyID != "a4-000000000003" || res.Outcome != plane.OutcomeRolledBack || res.Reason != plane.ReasonHealthFailed ||
		len(res.Health.Problems) == 0 || res.Health.Problems[0].Code != ProblemBSSDisabled {
		t.Fatalf("result %s", results[0])
	}
	_, err := e.call("wifi.config.confirm", map[string]string{"applyId": "a4-000000000003"})
	if _, data := rpcData(t, err); data["error"] != plane.CodeUnhealthy || data["rolledBack"] != true {
		t.Fatalf("confirm after the rollback: %v", data)
	}
	if !e.svc.RedialFast() {
		t.Fatal("no fast redial after a rollback")
	}
	if hold, err := e.lock.TryAcquire(applylock.Update, "u1"); err != nil {
		t.Fatalf("lock after the rollback: %v", err)
	} else {
		hold.Release()
	}
}

// What was broken before the apply (a radio netifd gave up on) is reported
// and never a reason to roll back.
func TestBrokenBeforeIsNotARollbackReason(t *testing.T) {
	e := newEnv(t, envOpts{transportOK: true})
	e.configure(plane.ModeManaged)
	e.wifi.set(func(w *fakeWifi) {
		w.radios["radio0"] = &fakeRadio{retry: true, ifaces: map[string]string{}}
	})
	e.kidsComesUp(beacon("Kids", 100))
	if _, err := e.call("wifi.config.apply", newIfaceApply("a4-000000000004", e.base("wireless"))); err != nil {
		t.Fatal(err)
	}
	r := e.waitRedial()
	rep := r.Report.(Report)
	if r.State != plane.HealthOK || !rep.OK || len(rep.Problems) != 2 {
		t.Fatalf("check %+v", rep)
	}
	for _, p := range rep.Problems {
		if !p.Preexisting {
			t.Fatalf("problem not marked preexisting: %+v", p)
		}
	}
}

// Without a confirm the AP restores by itself at the deadline.
func TestNoConfirmRestoresAtTheDeadline(t *testing.T) {
	e := newEnv(t, envOpts{transportOK: true})
	e.configure(plane.ModeManaged)
	before := e.read("/etc/config/wireless")
	e.kidsComesUp(beacon("Kids", 100))
	if _, err := e.call("wifi.config.apply", newIfaceApply("a4-000000000005", e.base("wireless"))); err != nil {
		t.Fatal(err)
	}
	e.waitRedial()
	e.clock.Advance(181 * time.Second)
	if e.read("/etc/config/wireless") != before {
		t.Fatal("not restored at the deadline")
	}
	var res struct{ Reason string }
	notes := e.notesOf("wifi.config.result")
	if len(notes) != 1 || json.Unmarshal(notes[0], &res) != nil || res.Reason != plane.ReasonConfirmTimeout {
		t.Fatalf("results %s", notes)
	}
}

// The device groups own their sections: reads mark them, and the plane
// never adopts, writes or creates one, signed or not.
func TestDeviceGroupSectionsAreMarkedAndNeverTouched(t *testing.T) {
	g := &fakeGroups{owned: groups.OwnedView{Sections: []string{"perch_ws0"}, DynamicVLAN: []string{"default_radio0"}}}
	e := newEnv(t, envOpts{transportOK: true, groups: g})
	e.configure(plane.ModeManaged)

	res, err := e.call("wifi.config.read", nil)
	if err != nil {
		t.Fatal(err)
	}
	var read struct {
		Configs []struct {
			Name     string
			Sections []struct {
				Name    string
				Owner   string
				Secrets map[string]string
				Options map[string]any
			}
		}
		GroupsOwned struct{ DynamicVLAN []string } `json:"groupsOwned"`
	}
	b, _ := json.Marshal(res)
	json.Unmarshal(b, &read)
	seen := map[string]string{}
	for _, c := range read.Configs {
		for _, s := range c.Sections {
			if c.Name == "wireless" {
				seen[s.Name] = s.Owner
				if s.Name == "default_radio0" && s.Secrets["key"] != "hmac:ac349a3eb980336c" {
					t.Fatalf("fleet fingerprint %v", s.Secrets)
				}
				if _, ok := s.Options["key"]; ok {
					t.Fatalf("a key left the AP: %v", s.Options)
				}
			}
		}
	}
	if seen["perch_ws0"] != "groups" || seen["default_radio0"] != "" || strings.Join(read.GroupsOwned.DynamicVLAN, ",") != "default_radio0" {
		t.Fatalf("owners %v, groupsOwned %v", seen, read.GroupsOwned)
	}
	if strings.Contains(string(b), "correct horse") || strings.Contains(string(b), "group key") {
		t.Fatal("a passphrase in the read")
	}

	adopt := map[string]any{"applyId": "a4-000000000006", "kind": "adopt", "base": map[string]string{"wireless": e.base("wireless")},
		"ops":    []map[string]any{{"op": "adopt", "config": "wireless", "section": "perch_ws0", "perchId": "x1"}},
		"ledger": map[string]any{}}
	_, err = e.call("wifi.config.apply", adopt)
	if code, data := rpcData(t, err); code != rpc.CodeCommandFailed || data["error"] != plane.CodeNotOwned || data["owner"] != "groups" {
		t.Fatalf("adopting a groups section: %d %v", code, data)
	}
	create := newIfaceApply("a4-000000000007", e.base("wireless"))
	create["ops"].([]map[string]any)[0]["section"] = "perch_ws9"
	_, err = e.call("wifi.config.apply", create)
	if _, data := rpcData(t, err); data["error"] != plane.CodeNotOwned {
		t.Fatalf("creating a groups name: %v", data)
	}
	payload, _ := json.Marshal(adopt)
	signed := map[string]any{"payload": string(payload), "sig": map[string]any{"v": 1, "ts": 1, "nonce": "0123456789abcdef", "challenge": "x", "mac": "00"}}
	_, err = e.call("wifi.config.apply", signed)
	if _, data := rpcData(t, err); data["error"] != plane.CodeNotOwned {
		t.Fatalf("a signed adopt of a groups section: %v", data)
	}
}

// One write lock per AP: the device groups' window refuses the plane, and
// the plane's window refuses the groups (and an update).
func TestTheWriteLockIsShared(t *testing.T) {
	e := newEnv(t, envOpts{transportOK: true})
	e.configure(plane.ModeManaged)
	hold, err := e.lock.TryAcquire(applylock.Groups, "groups-7")
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.call("wifi.config.apply", newIfaceApply("a4-000000000008", e.base("wireless")))
	if _, data := rpcData(t, err); data["error"] != plane.CodeBusy || data["reason"] != "groups_pending" {
		t.Fatalf("apply during a groups window: %v", data)
	}
	hold.Release()
	e.kidsComesUp(beacon("Kids", 100))
	if _, err := e.call("wifi.config.apply", newIfaceApply("a4-000000000009", e.base("wireless"))); err != nil {
		t.Fatal(err)
	}
	if _, err := e.lock.TryAcquire(applylock.Groups, "groups-8"); applylock.Reason(err) != "plane_pending" {
		t.Fatalf("groups during a plane window: %v", err)
	}
	e.waitRedial()
}

// access none (or a device that is not OpenWrt): every wifi.* is -32001
// wifi_config_off; the hello still says so.
func TestAccessNoneIsOff(t *testing.T) {
	no := false
	for _, eo := range []envOpts{{access: plane.AccessNone}, {access: plane.AccessWrite, openwrt: &no}} {
		e := newEnv(t, eo)
		for _, m := range e.svc.Methods() {
			_, err := e.call(m, map[string]any{})
			if code, data := rpcData(t, err); code != rpc.CodeUnsupported || data["error"] != ErrOff {
				t.Fatalf("%s: %d %v", m, code, data)
			}
		}
		b, _ := json.Marshal(e.svc.Hello(context.Background()))
		var h map[string]any
		json.Unmarshal(b, &h)
		if h["access"] != "none" || h["protocol"] != float64(1) || h["hashes"] != nil {
			t.Fatalf("hello %s", b)
		}
	}
}

// Only wireless and network are ever allowed, whatever the list says; the
// hello carries the plane's state and this session's signing challenge.
func TestHelloAndCapabilities(t *testing.T) {
	e := newEnv(t, envOpts{})
	b, _ := json.Marshal(e.svc.Hello(context.Background()))
	var h struct {
		Protocol       int
		Access         string
		TransportOK    bool     `json:"transportOk"`
		AllowInsecure  *bool    `json:"allowInsecure"`
		AllowedConfigs []string `json:"allowedConfigs"`
		Hashes         map[string]string
		Apply          struct{ State string }
		Signing        struct {
			Required  bool
			Challenge string
		}
		Management struct {
			Network *string
			Device  string
			Radios  []string
		}
		Groups struct {
			Engine  bool
			Enabled bool
			State   string
		}
	}
	if err := json.Unmarshal(b, &h); err != nil {
		t.Fatal(err)
	}
	if h.Access != "write" || h.TransportOK || h.AllowInsecure == nil || *h.AllowInsecure ||
		strings.Join(h.AllowedConfigs, ",") != "network,wireless" || h.Hashes["wireless"] == "" || h.Apply.State != "idle" ||
		!h.Signing.Required || h.Signing.Challenge != "challenge-1" || h.Management.Device != "br-lan" ||
		h.Management.Network == nil || *h.Management.Network != "lan" || len(h.Management.Radios) != 0 ||
		!h.Groups.Engine || h.Groups.Enabled || h.Groups.State != "idle" {
		t.Fatalf("hello %s", b)
	}
	// Over plain HTTP without the opt-in, writes are refused before anything.
	e.configure(plane.ModeManaged)
	_, err := e.call("wifi.config.apply", newIfaceApply("a4-000000000010", e.base("wireless")))
	if _, data := rpcData(t, err); data["error"] != "insecure_transport" {
		t.Fatalf("apply over plain HTTP: %v", data)
	}
	caps, err := e.call("wifi.capabilities", nil)
	if err != nil {
		t.Fatal(err)
	}
	ow, _ := caps["openwrt"].(map[string]any)
	radios, _ := caps["radios"].([]any)
	g, _ := caps["groups"].(map[string]any)
	if caps["guard"] != GuardSelfInstalled || ow["release"] != "24.10.2" || caps["confirmMaxSeconds"] != float64(900) ||
		len(radios) != 2 || g["owned"] == nil || caps["access"] != "write" {
		t.Fatalf("capabilities %v", caps)
	}
}

// Without the boot guard an apply is refused (a reboot in the window would
// not be undone before the network starts); a dry run is fine.
func TestAnApplyNeedsTheBootGuard(t *testing.T) {
	e := newEnv(t, envOpts{transportOK: true, noGuard: true})
	e.configure(plane.ModeManaged)
	a := newIfaceApply("a4-000000000011", e.base("wireless"))
	_, err := e.call("wifi.config.apply", a)
	if _, data := rpcData(t, err); data["error"] != ErrGuardMissing {
		t.Fatalf("apply without the guard: %v", data)
	}
	a["dryRun"] = true
	res, err := e.call("wifi.config.apply", a)
	if err != nil || res["state"] != plane.StateDryRun {
		t.Fatalf("dry run %v %v", res, err)
	}
	if strings.Contains(string(mustJSON(res)), "kids passphrase") {
		t.Fatal("a secret in the dry run")
	}
}

// Perch never creates or removes radios and never reorders the existing
// interfaces; a PSK guard it cannot run is refused rather than skipped.
func TestTheAPsOwnRules(t *testing.T) {
	e := newEnv(t, envOpts{transportOK: true})
	e.configure(plane.ModeManaged)
	radio := map[string]any{"applyId": "a4-000000000012", "kind": "apply", "base": map[string]string{"wireless": e.base("wireless")},
		"ops": []map[string]any{{"op": "put", "config": "wireless", "section": "radio9", "type": "wifi-device",
			"options": map[string]any{"type": "mac80211", "band": "6g"}}},
		"ledger": map[string]any{"set": []map[string]string{{"perchId": "r9", "config": "wireless", "section": "radio9"}}}}
	_, err := e.call("wifi.config.apply", radio)
	if _, data := rpcData(t, err); data["error"] != plane.CodeInvalidConfig {
		t.Fatalf("a new radio: %v", data)
	}
	order := map[string]any{"applyId": "a4-000000000013", "kind": "apply", "base": map[string]string{"wireless": e.base("wireless")},
		"ops": []map[string]any{
			{"op": "adopt", "config": "wireless", "section": "default_radio0", "perchId": "i0"},
			{"op": "adopt", "config": "wireless", "section": "default_radio1", "perchId": "i1"},
			{"op": "order", "config": "wireless", "type": "wifi-iface", "sections": []string{"default_radio1", "default_radio0"}}},
		"ledger": map[string]any{}}
	_, err = e.call("wifi.config.apply", order)
	if _, data := rpcData(t, err); data["error"] != plane.CodeInvalidConfig || !strings.Contains(stringOf(data["error"])+"", "invalid") {
		t.Fatalf("a reorder: %v", data)
	}
	psk := newIfaceApply("a4-000000000014", e.base("wireless"))
	psk["guards"] = map[string]any{"pskWildcardDigests": []string{strings.Repeat("ab", 32)}}
	_, err = e.call("wifi.config.apply", psk)
	if _, data := rpcData(t, err); data["error"] != plane.CodeBadParams {
		t.Fatalf("a PSK guard: %v", data)
	}
}

// The controller's mode gates applies; a session's end turns it off.
func TestModeFromTheController(t *testing.T) {
	e := newEnv(t, envOpts{transportOK: true})
	e.configure(plane.ModeObserve)
	_, err := e.call("wifi.config.apply", newIfaceApply("a4-000000000015", e.base("wireless")))
	if _, data := rpcData(t, err); data["error"] != "not_managed" {
		t.Fatalf("apply in observe: %v", data)
	}
	e.configure(plane.ModeManaged)
	if e.svc.Plane().Mode() != plane.ModeManaged {
		t.Fatal("mode")
	}
	e.svc.EndSession()
	if e.svc.Plane().Mode() != plane.ModeOff {
		t.Fatal("mode after the session")
	}
	e.svc.Configure(json.RawMessage(`{"metricsIntervalSeconds":5}`)) // an older controller
	if e.svc.Plane().Mode() != plane.ModeOff {
		t.Fatal("mode without a block")
	}
}

// A reboot inside the window: the guard restores before the network
// starts, and the next daemon reports the outcome in its hello.
func TestTheBootGuardRestoresAfterAReboot(t *testing.T) {
	e := newEnv(t, envOpts{transportOK: true})
	e.configure(plane.ModeManaged)
	before := e.read("/etc/config/wireless")
	e.kidsComesUp(beacon("Kids", 100))
	if _, err := e.call("wifi.config.apply", newIfaceApply("a4-000000000016", e.base("wireless"))); err != nil {
		t.Fatal(err)
	}
	e.waitRedial()
	// The reboot: tmpfs is gone.
	if err := os.RemoveAll(filepath.Join(e.root, RunDir)); err != nil {
		t.Fatal(err)
	}
	res, err := RunGuard(e.root, time.Now())
	if err != nil || res == nil || res.Reason != plane.ReasonReboot || res.Outcome != plane.OutcomeRolledBack {
		t.Fatalf("guard %+v %v", res, err)
	}
	if e.read("/etc/config/wireless") != before {
		t.Fatal("not restored by the guard")
	}
	if res, err := RunGuard(e.root, time.Now()); res != nil || err != nil {
		t.Fatalf("a second guard run %+v %v", res, err)
	}
	e2 := newEnv(t, envOpts{transportOK: true})
	e2.root = e.root
	svc, err := New(Options{Access: "write", Allow: []string{"wireless", "network"}, Root: e.root, OpenWrt: boolPtr(true),
		Ubus: e2.svc.ubus, Prober: e2.svc.o.Prober, Clock: e2.clock, Log: e2.svc.log})
	if err != nil {
		t.Fatal(err)
	}
	svc.Start()
	b, _ := json.Marshal(svc.Hello(context.Background()))
	if !strings.Contains(string(b), `"applyId":"a4-000000000016"`) || !strings.Contains(string(b), `"reason":"reboot"`) {
		t.Fatalf("hello after the reboot %s", b)
	}
}

func boolPtr(b bool) *bool { return &b }

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func stringOf(v any) string {
	s, _ := v.(string)
	return s
}
