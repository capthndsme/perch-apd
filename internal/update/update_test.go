package update

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	kit "github.com/capthndsme/perch-agentkit/update"

	"github.com/capthndsme/perch-agentkit/rpc"
	"github.com/capthndsme/perch-apd/internal/applylock"
	"github.com/capthndsme/perch-apd/internal/config"
	"github.com/capthndsme/perch-apd/internal/wifiplane"
)

// The guard is one file with three copies: the package's, the Wi-Fi
// plane's and this one; the self-installed variant is the plane's too.
func TestGuardCopiesMatch(t *testing.T) {
	pkg, err := os.ReadFile("../../openwrt/perch-apd/files/perch-apd-guard.init")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pkg, GuardScript) || !bytes.Equal(pkg, wifiplane.GuardScript) {
		t.Fatal("openwrt/perch-apd/files/perch-apd-guard.init, internal/update's and internal/wifiplane's copies differ")
	}
	if !bytes.Equal(SelfInstalledScript(), wifiplane.SelfInstalledScript()) {
		t.Fatal("the self-installed guard differs from the Wi-Fi plane's")
	}
	if !IsSelfInstalled(SelfInstalledScript()) || IsSelfInstalled(GuardScript) {
		t.Fatal("IsSelfInstalled")
	}
	// The update step runs first, before the plane's config-guard, and
	// the guard starts before the network.
	upd := bytes.Index(pkg, []byte(`"$UPDATE/perch-update.sh" boot "$UPDATE"`))
	plane := bytes.Index(pkg, []byte(`[ -f "$ROLLBACK/pending.json" ] || return 0`))
	if upd < 0 || plane < 0 || upd > plane || !bytes.Contains(pkg, []byte("\nSTART=15\n")) ||
		!bytes.Contains(pkg, []byte("\nUPDATE="+StateDir+"\n")) {
		t.Fatal("the guard's update step is missing or not first")
	}
}

// The updater keeps the package's guard where the device has it, else the
// self-installed one the Wi-Fi plane writes as well.
func TestGuardContent(t *testing.T) {
	root := t.TempDir()
	if !bytes.Equal(guardContent(root), SelfInstalledScript()) {
		t.Fatal("no guard: want the self-installed one")
	}
	os.MkdirAll(filepath.Join(root, "etc/init.d"), 0o755)
	os.WriteFile(filepath.Join(root, GuardInit), GuardScript, 0o755)
	if !bytes.Equal(guardContent(root), GuardScript) {
		t.Fatal("the package's guard: want it kept")
	}
	os.WriteFile(filepath.Join(root, GuardInit), []byte("#!/bin/sh /etc/rc.common\nSTART=15\n"), 0o755)
	if !bytes.Equal(guardContent(root), SelfInstalledScript()) {
		t.Fatal("an older guard: want the self-installed one")
	}
}

// testKey is a signify key made for this test.
type testKey struct {
	priv ed25519.PrivateKey
	num  [8]byte
	pub  string // "RW…"
}

func newTestKey(t *testing.T) testKey {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	k := testKey{priv: priv, num: [8]byte{1, 2, 3, 4, 5, 6, 7, 8}}
	raw := append(append([]byte("Ed"), k.num[:]...), pub...)
	k.pub = base64.StdEncoding.EncodeToString(raw)
	return k
}

type fixture struct {
	root, state, run string
	lock             *applylock.Lock
	u                *Updater
	disp             *rpc.Dispatcher
	key              testKey
}

// newFixture is an unowned AP (/usr/bin/perch-apd, no package record) on
// OpenWrt 24.10 in a temp tree.
func newFixture(t *testing.T, cfg *config.Config, ver string) *fixture {
	t.Helper()
	f := &fixture{root: t.TempDir(), lock: applylock.New(), key: newTestKey(t)}
	f.state = filepath.Join(f.root, "etc/perch-apd/update")
	f.run = filepath.Join(f.root, "tmp/perch-update/perch-apd")
	os.MkdirAll(filepath.Join(f.root, "etc"), 0o755)
	os.MkdirAll(filepath.Join(f.root, "usr/bin"), 0o755)
	os.MkdirAll(filepath.Join(f.root, "proc"), 0o755)
	os.WriteFile(filepath.Join(f.root, "etc/openwrt_release"), []byte("DISTRIB_ID='OpenWrt'\nDISTRIB_RELEASE='24.10.2'\nDISTRIB_ARCH='x86_64'\n"), 0o644)
	os.WriteFile(filepath.Join(f.root, "usr/bin/perch-apd"), []byte("old binary"), 0o755)
	os.WriteFile(filepath.Join(f.root, "proc/meminfo"), []byte("MemTotal: 262144 kB\nMemAvailable: 131072 kB\n"), 0o644)
	os.WriteFile(filepath.Join(f.root, "proc/uptime"), []byte("1000.00 900.00\n"), 0o644)
	os.WriteFile(filepath.Join(f.root, "proc/mounts"), []byte("/dev/root / ext4 rw 0 0\ntmpfs /tmp tmpfs rw 0 0\n"), 0o644)
	os.MkdirAll(filepath.Join(f.root, "tmp"), 0o755)
	if cfg == nil {
		cfg = &config.Config{Controller: "http://127.0.0.1:9", SelfUpdate: true, UpdateKeys: []string{f.key.pub}}
	}
	u, err := New(Options{Config: cfg, Lock: f.lock, Log: nil, poll: 10 * time.Millisecond,
		target: func(tg kit.Target) kit.Target {
			tg.Root, tg.StateDir, tg.RunDir = f.root, f.state, f.run
			tg.Executable = "/usr/bin/perch-apd"
			tg.Guard.Content = guardContent(f.root)
			if ver != "" {
				tg.Version = ver
			}
			tg.Arch = "amd64"
			return tg
		}})
	if err != nil {
		t.Fatal(err)
	}
	f.u = u
	f.disp = rpc.NewDispatcher()
	u.Register(f.disp)
	return f
}

func (f *fixture) call(t *testing.T, method string, params any) (json.RawMessage, *rpc.Error) {
	t.Helper()
	msg, err := rpc.Request(1, method, params)
	if err != nil {
		t.Fatal(err)
	}
	m, perr := rpc.Decode(msg)
	if perr != nil {
		t.Fatal("decode")
	}
	r := f.disp.Serve(context.Background(), m)
	return r.Result, r.Error
}

func dataOf(e *rpc.Error) map[string]string {
	out := map[string]string{}
	b, _ := json.Marshal(e.Data)
	json.Unmarshal(b, &out)
	return out
}

func TestStatusRefusalsAndCapability(t *testing.T) {
	off := newFixture(t, &config.Config{SelfUpdate: false}, "1.1.0")
	st := off.u.Status(context.Background())
	if st.Refusal == nil || *st.Refusal != kit.CodeSelfUpdateOff || st.Enabled || off.u.Capability() != "" {
		t.Fatalf("self_update '0': %+v", st)
	}
	nokey := newFixture(t, &config.Config{SelfUpdate: true}, "1.1.0")
	if st := nokey.u.Status(context.Background()); st.Refusal == nil || *st.Refusal != kit.CodeNoTrustedKeys {
		t.Fatalf("no key: %+v", st)
	}
	f := newFixture(t, nil, "1.1.0")
	st = f.u.Status(context.Background())
	if st.Refusal != nil || f.u.Capability() != kit.Capability || len(st.KeyIDs) != 1 || st.KeyIDs[0] != "0102030405060708" {
		t.Fatalf("with a UCI key: %+v", st)
	}
	if st.InstallKind != kit.InstallUnowned || len(st.Methods) != 1 || st.Methods[0] != kit.MethodBinary ||
		st.BinaryPath != "/usr/bin/perch-apd" || st.Guard != kit.GuardMissing || st.OpenWrt == nil || st.OpenWrt.Series != "24.10" {
		t.Fatalf("status %+v", st)
	}
	sum := sha256.Sum256([]byte("old binary"))
	if st.BinarySHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("binary hash %s", st.BinarySHA256)
	}
	// agent.update.status is the same block.
	raw, e := f.call(t, "agent.update.status", map[string]any{})
	var got kit.Status
	if e != nil || json.Unmarshal(raw, &got) != nil || got.InstallKind != kit.InstallUnowned {
		t.Fatalf("agent.update.status %s %v", raw, e)
	}
}

// signedStage is a dry-run agent.update.stage of a release signed with the
// fixture's key.
func (f *fixture) signedStage(t *testing.T, dry bool) (json.RawMessage, *rpc.Error) {
	t.Helper()
	bin := []byte("new binary")
	sum := sha256.Sum256(bin)
	m := map[string]any{"schema": kit.Schema, "product": kit.ProductAPD, "version": "1.2.0", "channel": "local",
		"minVersion": nil, "minFromVersion": nil, "minControllerVersion": nil,
		"artefacts": []any{map[string]any{"file": "perch-apd-linux-amd64", "kind": "binary", "arch": "amd64",
			"size": len(bin), "gzipSize": len(bin), "sha256": hex.EncodeToString(sum[:])}}}
	manifest, _ := json.Marshal(m)
	sig := kit.SignDetached(f.key.priv, f.key.num, "verify with a test key", manifest)
	return f.call(t, "agent.update.stage", map[string]any{"updateId": "u-00000000000000a1", "dryRun": dry,
		"source": "release", "method": "binary", "manifest": base64.StdEncoding.EncodeToString(manifest),
		"signature": string(sig),
		"artefacts": []any{map[string]any{"file": "perch-apd-linux-amd64",
			"path": "/api/v1/agent-updates/files/1/perch-apd-linux-amd64?d=ap-1&exp=1&sig=00"}}})
}

// A groups or Wi-Fi change waiting for its confirm makes an update wait:
// the preflight names it (busy_pending_apply), a real stage is refused.
func TestBusyHookFollowsTheWriteLock(t *testing.T) {
	f := newFixture(t, nil, "1.1.0")
	if f.u.busy(context.Background()) != "" {
		t.Fatal("busy with a free lock")
	}
	raw, e := f.signedStage(t, true)
	var res kit.StageResult
	if e != nil || json.Unmarshal(raw, &res) != nil || !res.Preflight.OK || res.Preflight.Busy != nil {
		t.Fatalf("dry run, lock free: %s %v", raw, e)
	}
	for _, h := range []applylock.Holder{applylock.Groups, applylock.Plane} {
		hold, err := f.lock.TryAcquire(h, "x-1")
		if err != nil {
			t.Fatal(err)
		}
		raw, e := f.signedStage(t, true)
		res = kit.StageResult{}
		if e != nil || json.Unmarshal(raw, &res) != nil || res.Preflight.OK || res.Preflight.Busy == nil ||
			*res.Preflight.Busy != h.Reason() || res.Preflight.Problems[0].Code != kit.CodeBusyPendingApply {
			t.Fatalf("%s: %s %v", h, raw, e)
		}
		if _, e := f.signedStage(t, false); e == nil || dataOf(e)["error"] != kit.CodeBusyPendingApply {
			t.Fatalf("%s: a real stage went ahead: %v", h, e)
		}
		hold.Release()
	}
	// Our own hold is no reason (the kit refuses a second update itself).
	hold, _ := f.lock.TryAcquire(applylock.Update, "u-1")
	if f.u.busy(context.Background()) != "" {
		t.Fatal("an update's own hold reported as busy")
	}
	hold.Release()
}

// agent.update.install takes the write lock first: refused while groups or
// the Wi-Fi plane hold it, and freed again when the install fails.
func TestInstallTakesTheWriteLock(t *testing.T) {
	f := newFixture(t, nil, "1.1.0")
	for holder, want := range map[applylock.Holder]string{applylock.Plane: kit.CodeBusyPendingApply, applylock.Groups: kit.CodeBusyPendingApply, applylock.Update: kit.CodeBusy} {
		hold, _ := f.lock.TryAcquire(holder, "x-1")
		_, e := f.call(t, "agent.update.install", map[string]any{"updateId": "u-00000000000000a1"})
		if e == nil || dataOf(e)["error"] != want || dataOf(e)["reason"] != holder.Reason() {
			t.Fatalf("%s holds: %+v", holder, e)
		}
		hold.Release()
	}
	_, e := f.call(t, "agent.update.install", map[string]any{"updateId": "u-00000000000000a1"})
	if e == nil || dataOf(e)["error"] != kit.CodeNotStaged {
		t.Fatalf("nothing staged: %+v", e)
	}
	if _, held := f.lock.Current(); held {
		t.Fatal("a failed install kept the write lock")
	}
}

func writePlan(t *testing.T, f *fixture, phase, to string) {
	t.Helper()
	os.MkdirAll(f.state, 0o700)
	err := kit.WritePlan(f.state, &kit.Plan{UpdateID: "u-00000000000000b2", Product: kit.ProductAPD, Service: "perch-apd",
		Program: "perch-apd", From: "1.1.0", To: to, Method: kit.MethodBinary, Store: kit.StoreFlash,
		Stage: f.run + "/staged", Rollback: f.state + "/rollback", Run: f.run, Binary: "/usr/bin/perch-apd",
		FlashPath: "/", Probation: 180, CrashRestarts: 3, Phase: phase})
	if err != nil {
		t.Fatal(err)
	}
}

// A process that starts while a plan is in progress (the new version in
// its check) holds the write lock until the plan is over: groups and the
// Wi-Fi plane are refused update_pending meanwhile.
func TestStartupHoldsTheLockWhileAnUpdateIsChecked(t *testing.T) {
	f := newFixture(t, nil, "1.2.0")
	writePlan(t, f, kit.PhaseProbation, "1.2.0")
	os.MkdirAll(f.run, 0o700)
	os.WriteFile(filepath.Join(f.run, "armed"), nil, 0o600)
	os.WriteFile(filepath.Join(f.run, "watch.pid"), []byte("1\n"), 0o600) // not the watchdog: resume is tried
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.u.Startup(ctx)
	info, held := f.lock.Current()
	if !held || info.Holder != applylock.Update || info.ID != "u-00000000000000b2" || !f.u.Holding() {
		t.Fatalf("lock %+v %v", info, held)
	}
	if _, err := f.lock.TryAcquire(applylock.Plane, "a4-1"); applylock.Reason(err) != "update_pending" {
		t.Fatalf("the plane got in: %v", err)
	}
	if err := kit.SetPlanPhase(f.state, kit.PhaseConfirmed); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for f.u.Holding() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if _, held := f.lock.Current(); held {
		t.Fatal("the lock outlived the plan")
	}
}

// No plan, or a finished one: nothing is held.
func TestStartupWithoutAPlanHoldsNothing(t *testing.T) {
	f := newFixture(t, nil, "1.2.0")
	f.u.Startup(context.Background())
	writePlan(t, f, kit.PhaseRolledBack, "1.2.0")
	f.u.Startup(context.Background())
	if _, held := f.lock.Current(); held {
		t.Fatal("held without a plan in progress")
	}
}

// The new version dials once the watchdog has set probation; anything else
// dials at once.
func TestWaitProbation(t *testing.T) {
	f := newFixture(t, nil, "1.2.0")
	start := time.Now()
	f.u.WaitProbation(context.Background(), time.Second)
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("waited without a plan")
	}
	writePlan(t, f, kit.PhaseInstalling, "1.2.0")
	go func() {
		time.Sleep(300 * time.Millisecond)
		kit.SetPlanPhase(f.state, kit.PhaseProbation)
	}()
	start = time.Now()
	f.u.WaitProbation(context.Background(), 5*time.Second)
	if d := time.Since(start); d < 250*time.Millisecond || d > 3*time.Second {
		t.Fatalf("waited %s", d)
	}
	// The old version (not the plan's target) never waits.
	old := newFixture(t, nil, "1.1.0")
	writePlan(t, old, kit.PhaseInstalling, "1.2.0")
	start = time.Now()
	old.u.WaitProbation(context.Background(), time.Second)
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("the old version waited")
	}
	// A plan stuck in installing: at most max.
	stuck := newFixture(t, nil, "1.2.0")
	writePlan(t, stuck, kit.PhaseInstalling, "1.2.0")
	start = time.Now()
	stuck.u.WaitProbation(context.Background(), 300*time.Millisecond)
	if d := time.Since(start); d < 250*time.Millisecond || d > 2*time.Second {
		t.Fatalf("stuck plan: waited %s", d)
	}
}

// The updater never writes the daemon's own configuration.
func TestNeverWritesTheConfig(t *testing.T) {
	f := newFixture(t, nil, "1.1.0")
	f.signedStage(t, true)
	f.call(t, "agent.update.install", map[string]any{"updateId": "u-00000000000000a1"})
	if _, err := os.Stat(filepath.Join(f.root, "etc/config/perch-apd")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("/etc/config/perch-apd written")
	}
}
