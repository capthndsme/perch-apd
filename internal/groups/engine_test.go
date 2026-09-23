package groups

import (
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type recorder struct {
	mu   sync.Mutex
	cmds []string
}

func (r *recorder) run(_ context.Context, name string, args ...string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cmds = append(r.cmds, strings.TrimSpace(name+" "+strings.Join(args, " ")))
	return nil
}

func (r *recorder) list() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.cmds...)
}

type env struct {
	dir  string
	rec  *recorder
	now  time.Time
	fake FS
}

func (v *env) path(name string) string { return filepath.Join(v.dir, "etc/config", name) }

func (v *env) engine(t *testing.T) *Engine {
	t.Helper()
	e, err := New(Options{
		StateDir: filepath.Join(v.dir, "state"), WirelessPath: v.path("wireless"), NetworkPath: v.path("network"),
		StagingDir: filepath.Join(v.dir, "uci"), FS: v.fake, Run: v.rec.run,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return v.now },
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// newEnv writes the configs and a /proc + /sys where the gateway
// (192.0.2.1, 02:00:00:00:00:01) is learned on br-lan's port "wan".
func newEnv(t *testing.T, network string) *env {
	t.Helper()
	dir := t.TempDir()
	v := &env{dir: dir, rec: &recorder{}, now: time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC), fake: FS{Root: filepath.Join(dir, "root")}}
	write := func(p, s string) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(v.path("wireless"), wirelessConf)
	write(v.path("network"), network)
	root := v.fake.Root
	write(filepath.Join(root, "proc/net/route"), "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n"+
		"br-lan.1\t00000000\t010200C0\t0003\t0\t0\t0\t00000000\t0\t0\t0\n"+
		"br-lan.1\t000200C0\t00000000\t0001\t0\t0\t0\t00FFFFFF\t0\t0\t0\n")
	write(filepath.Join(root, "proc/net/arp"), "IP address       HW type     Flags       HW address            Mask     Device\n"+
		"192.0.2.1        0x1         0x2         02:00:00:00:00:01     *        br-lan.1\n")
	var fdb []byte
	entry := func(mac string, port int, local bool) {
		m, _ := net.ParseMAC(mac)
		e := make([]byte, 16)
		copy(e, m)
		e[6] = byte(port)
		if local {
			e[7] = 1
		}
		binary.LittleEndian.PutUint32(e[8:], 100)
		fdb = append(fdb, e...)
	}
	entry("02:00:00:00:00:99", 1, true)
	entry("02:00:00:00:00:31", 1, false)
	entry("02:00:00:00:00:01", 2, false)
	write(filepath.Join(root, "sys/class/net/br-lan/brforward"), string(fdb))
	write(filepath.Join(root, "sys/class/net/br-lan/brif/lan1/port_no"), "0x1\n")
	write(filepath.Join(root, "sys/class/net/br-lan/brif/wan/port_no"), "0x2\n")
	return v
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestDetectTrunk(t *testing.T) {
	v := newEnv(t, filteringNet)
	port, err := DetectTrunk(v.fake)
	if err != nil || port != "wan" {
		t.Fatalf("%q %v", port, err)
	}
	// A default route over a plain port is that port.
	os.WriteFile(filepath.Join(v.fake.Root, "proc/net/route"), []byte("Iface\tDestination\tGateway\n"+
		"eth0\t00000000\t010200C0\t0003\t0\t0\t0\t00000000\t0\t0\t0\n"), 0o644)
	if port, err := DetectTrunk(v.fake); err != nil || port != "eth0" {
		t.Fatalf("%q %v", port, err)
	}
}

func TestApplyConfirm(t *testing.T) {
	v := newEnv(t, filteringNet)
	e := v.engine(t)
	ctx := context.Background()
	d := desired()
	d.ConfirmSeconds = 60
	res, err := e.Apply(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	if res.State != "pending_confirm" || res.TrunkPort != "wan" || res.Deadline != "2026-09-24T12:01:00Z" {
		t.Fatalf("%+v", res)
	}
	if got := v.rec.list(); len(got) != 1 || got[0] != "/etc/init.d/network reload" {
		t.Fatalf("reloads %v", got)
	}
	if !strings.Contains(read(t, v.path("network")), "perch_bv101") || !strings.Contains(read(t, v.path("wireless")), "unit-101-key") {
		t.Fatal("configs not written")
	}
	// The same revision again (a reconnect) answers the same; another is busy.
	if again, err := e.Apply(ctx, d); err != nil || again.State != "pending_confirm" {
		t.Fatalf("%+v %v", again, err)
	}
	other := desired()
	other.Revision = 4
	if _, err := e.Apply(ctx, other); err == nil || err.(*Refusal).Code != "busy" {
		t.Fatalf("busy: %v", err)
	}
	if err := e.Confirm(3); err != nil {
		t.Fatal(err)
	}
	st := e.State(ctx)
	if st.AppliedRevision != 3 || st.Pending != nil || st.TrunkPort != "wan" {
		t.Fatalf("%+v", st)
	}
	// Stations only: the wireless alone reloads.
	next := desired()
	next.Revision = 4
	next.Stations = append(next.Stations, Station{VID: 102, MACs: []string{"02:00:00:00:00:22"}})
	res, err = e.Apply(ctx, next)
	if err != nil || res.State != "pending_confirm" {
		t.Fatalf("%+v %v", res, err)
	}
	if got := v.rec.list(); got[len(got)-1] != "wifi reload" {
		t.Fatalf("reloads %v", got)
	}
	if err := e.Confirm(4); err != nil {
		t.Fatal(err)
	}
	// Nothing new: noop, no reload.
	next.Revision = 5
	n := len(v.rec.list())
	if res, err := e.Apply(ctx, next); err != nil || res.State != "noop" || len(v.rec.list()) != n {
		t.Fatalf("%+v %v", res, err)
	}
	if e.State(ctx).AppliedRevision != 5 {
		t.Fatal("noop revision not kept")
	}
}

func TestApplyRollsBackWithoutConfirm(t *testing.T) {
	v := newEnv(t, untaggedNet)
	e := v.engine(t)
	ctx := context.Background()
	d := desired()
	d.ConfirmSeconds = 30
	if _, err := e.Apply(ctx, d); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(read(t, v.path("network")), "br-lan.1") {
		t.Fatal("bridge not converted")
	}
	// The timer fires: the old configs are back.
	e.mu.Lock()
	e.rollbackLocked(ctx, "no confirm from the controller")
	e.mu.Unlock()
	if read(t, v.path("network")) != untaggedNet || read(t, v.path("wireless")) != wirelessConf {
		t.Fatal("configs not restored byte for byte")
	}
	st := e.State(ctx)
	if st.Pending != nil || st.LastRollback == nil || st.LastRollback.Revision != 3 || st.AppliedRevision != 0 {
		t.Fatalf("%+v", st)
	}
	if err := e.Confirm(3); err == nil {
		t.Fatal("a rolled back revision confirmed")
	}
}

func TestRestartAfterTheWindowRollsBack(t *testing.T) {
	v := newEnv(t, filteringNet)
	e := v.engine(t)
	ctx := context.Background()
	if _, err := e.Apply(ctx, desired()); err != nil {
		t.Fatal(err)
	}
	e.timer.Stop()
	// The AP reboots; the daemon starts after the window.
	v.now = v.now.Add(10 * time.Minute)
	e2 := v.engine(t)
	e2.Start(ctx)
	if read(t, v.path("network")) != filteringNet {
		t.Fatal("not rolled back at start")
	}
	if st := e2.State(ctx); st.LastRollback == nil || st.Pending != nil {
		t.Fatalf("%+v", st)
	}
}

func TestUncommittedChangesRefuse(t *testing.T) {
	v := newEnv(t, filteringNet)
	e := v.engine(t)
	os.MkdirAll(filepath.Join(v.dir, "uci"), 0o755)
	os.WriteFile(filepath.Join(v.dir, "uci", "wireless"), []byte("wireless.x.y='1'\n"), 0o644)
	if _, err := e.Apply(context.Background(), desired()); err == nil || err.(*Refusal).Code != "uncommitted" {
		t.Fatalf("%v", err)
	}
}
