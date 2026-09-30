// Package update wires the kit's self-updater (perch-agentkit/update) into
// perch-apd (agent-updates design, device.md sections 10.1 and 12): what
// perch-apd is (service, program, packages, where it is installed, the
// files a release may write), the boot guard it shares with the Wi-Fi
// config plane, and the AP's one write lock (internal/applylock).
//
// The lock: agent.update.install takes it as holder Update before the plan
// is written, and a process keeps it while a plan is in progress (the old
// version until the watchdog stops it, the new one through its check until
// the confirm or the rollback). Device groups and the Wi-Fi plane are
// refused meanwhile (busy, reason update_pending); an update is refused
// while either of them holds it (busy_pending_apply, reason groups_pending
// or plane_pending), both at stage (the preflight's busy hook) and at
// install.
//
// Nothing here writes /etc/config/perch-apd: self_update and update_key are
// only read.
package update

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	kit "github.com/capthndsme/perch-agentkit/update"

	"github.com/capthndsme/perch-agentkit/rpc"
	"github.com/capthndsme/perch-apd/internal/applylock"
	"github.com/capthndsme/perch-apd/internal/config"
	"github.com/capthndsme/perch-apd/internal/version"
)

// Where perch-apd keeps its update state and run files (protocol.md 7).
const (
	StateDir = "/etc/perch-apd/update"
	RunDir   = "/tmp/perch-update/perch-apd"
)

// The boot guard: one init script with the Wi-Fi config plane, START=15,
// the update step first (openwrt/perch-apd/files/perch-apd-guard.init).
const (
	GuardInit = "/etc/init.d/perch-apd-guard"
	GuardLink = "/etc/rc.d/S15perch-apd-guard"
)

// GuardScript is the package's files/perch-apd-guard.init (a test keeps
// the copies identical: this one, the package's and the Wi-Fi plane's).
//
//go:embed perch-apd-guard.init
var GuardScript []byte

// selfInstalledLine marks a guard the daemon wrote itself, as the Wi-Fi
// plane's EnsureGuard writes it (internal/wifiplane/guard.go): the same
// line, so the two writers of the file agree on its content.
const selfInstalledLine = "# Installed by the perch-apd daemon: the package on this device did not ship it."

// SelfInstalledScript is the guard the daemon writes on a device whose
// package did not ship it: the package's script with the marker line.
func SelfInstalledScript() []byte {
	i := bytes.IndexByte(GuardScript, '\n')
	out := make([]byte, 0, len(GuardScript)+len(selfInstalledLine)+1)
	out = append(out, GuardScript[:i+1]...)
	out = append(out, selfInstalledLine+"\n"...)
	return append(out, GuardScript[i+1:]...)
}

// IsSelfInstalled reports a guard script the daemon wrote itself.
func IsSelfInstalled(script []byte) bool {
	return bytes.Contains(script, []byte(selfInstalledLine))
}

// guardContent is the guard the updater keeps in place: the package's
// script where the device has exactly that, else the self-installed one
// (the Wi-Fi plane keeps a self-installed guard and leaves a package's
// alone, so the updater must not turn one into the other).
func guardContent(root string) []byte {
	if b, err := os.ReadFile(filepath.Join(rootOr(root), GuardInit)); err == nil && bytes.Equal(b, GuardScript) {
		return GuardScript
	}
	return SelfInstalledScript()
}

func rootOr(root string) string {
	if root == "" {
		return "/"
	}
	return root
}

// Target is perch-apd for the kit's updater.
func Target(cfg *config.Config, root string) kit.Target {
	return kit.Target{
		Product:    kit.ProductAPD,
		Service:    "perch-apd",
		Program:    "perch-apd",
		Version:    version.Version,
		Arch:       version.Arch(),
		Candidates: []string{"/usr/bin/perch-apd", "/opt/perch-apd/perch-apd"},
		Packages:   []string{"perch-apd"},
		StateDir:   StateDir,
		RunDir:     RunDir,
		Guard:      kit.GuardSpec{InitPath: GuardInit, RCLink: GuardLink, Content: guardContent(root)},
		FileAllow: []kit.FileRule{
			{Path: "/etc/init.d/perch-apd"},
			{Path: GuardInit},
			{Path: "/lib/upgrade/keep.d/perch-apd"},
		},
		// An AP whose flash cannot hold the old binary beside the new one
		// keeps it in RAM (and fetches it again after a reboot).
		RollbackStores: []string{kit.StoreFlash, kit.StoreRAM},
		Enabled:        cfg.SelfUpdate,
		ExtraKeys:      cfg.UpdateKeys,
		TLSInsecure:    cfg.TLSInsecure,
		CAFile:         cfg.CAFile,
		Root:           root,
	}
}

// Options build an Updater.
type Options struct {
	Config *config.Config
	// Lock is the AP's write lock (shared with device groups and the
	// Wi-Fi plane).
	Lock *applylock.Lock
	// HTTP downloads from the controller (the daemon's client).
	HTTP *http.Client
	Log  *slog.Logger

	// Tests: a fixture tree, the state and run directories in it, the
	// running binary, and the lock watcher's interval.
	target func(kit.Target) kit.Target
	poll   time.Duration
}

// Updater is the kit's updater with perch-apd's write lock around it.
type Updater struct {
	*kit.Updater
	lock     *applylock.Lock
	stateDir string
	version  string
	log      *slog.Logger
	poll     time.Duration

	mu   sync.Mutex
	hold *applylock.Hold
}

// New builds the updater. It reads nothing but the configuration it is
// given; Startup acts on what a previous run left.
func New(o Options) (*Updater, error) {
	log := o.Log
	if log == nil {
		log = slog.Default()
	}
	lock := o.Lock
	if lock == nil {
		lock = applylock.New()
	}
	t := Target(o.Config, "")
	if o.target != nil {
		t = o.target(t)
	}
	u := &Updater{lock: lock, stateDir: t.StateDir, version: t.Version, log: log, poll: o.poll}
	if u.poll <= 0 {
		u.poll = 2 * time.Second
	}
	t.Busy = u.busy
	ku, err := kit.New(t, o.HTTP, o.Config.Controller, log)
	if err != nil {
		return nil, err
	}
	u.Updater = ku
	return u, nil
}

// busy is the preflight's busy hook: why an update must wait now ("" =
// it need not). Our own hold is not a reason: the kit refuses a second
// update by itself (busy).
func (u *Updater) busy(context.Context) string {
	info, held := u.lock.Current()
	if !held || info.Holder == applylock.Update {
		return ""
	}
	return info.Holder.Reason()
}

// Register adds agent.update.* to the dispatcher: the kit's methods, with
// install taking the write lock first.
func (u *Updater) Register(d *rpc.Dispatcher) {
	u.Updater.Register(d)
	d.Register("agent.update.install", u.rpcInstall)
}

func (u *Updater) rpcInstall(ctx context.Context, raw json.RawMessage) (any, error) {
	var p struct {
		UpdateID string `json:"updateId"`
	}
	if err := rpc.Params(raw, &p); err != nil {
		return nil, err
	}
	res, err := u.Install(ctx, p.UpdateID)
	if err != nil {
		return nil, err
	}
	return res, nil
}

// Install is agent.update.install: the write lock as holder Update, then
// the kit's install. The lock stays held while the plan is in progress (the
// watchdog stops this process soon); a failed install frees it at once.
func (u *Updater) Install(ctx context.Context, id string) (*kit.InstallResult, error) {
	hold, err := u.lock.TryAcquire(applylock.Update, id)
	if err != nil {
		return nil, busyError(err)
	}
	res, err := u.Updater.Install(ctx, id)
	if err != nil {
		hold.Release()
		return nil, kit.RPCError(err)
	}
	u.keep(hold)
	return res, nil
}

// busyError is the refusal while another writer holds the lock: another
// update is busy, a groups or Wi-Fi change waiting for its confirm is
// busy_pending_apply; data.reason names the holder as groups.apply does.
func busyError(err error) *rpc.Error {
	code := kit.CodeBusyPendingApply
	var b *applylock.BusyError
	if errors.As(err, &b) && b.Holder == applylock.Update {
		code = kit.CodeBusy
	}
	return &rpc.Error{Code: rpc.CodeCommandFailed, Message: err.Error(),
		Data: map[string]string{"error": code, "reason": applylock.Reason(err)}}
}

// Startup runs before the daemon dials (device.md section 8): the kit's
// candidate checks, then the write lock for as long as a plan is in
// progress (the new version in its check, or a restore still running).
func (u *Updater) Startup(ctx context.Context) {
	u.Updater.Startup(ctx)
	p, err := kit.ReadPlan(u.stateDir)
	if err != nil || kit.Terminal(p.Phase) {
		return
	}
	hold, err := u.lock.TryAcquire(applylock.Update, p.UpdateID)
	if err != nil {
		u.log.Warn("update in progress, but the write lock is held", "update", p.UpdateID, "err", err)
		return
	}
	u.log.Info("update in progress: device groups and Wi-Fi changes wait", "update", p.UpdateID, "phase", p.Phase)
	u.keep(hold)
}

// keep records the hold and starts the watcher that frees it once the plan
// is over.
func (u *Updater) keep(hold *applylock.Hold) {
	u.mu.Lock()
	u.hold = hold
	u.mu.Unlock()
	go u.releaseWhenDone(hold)
}

// releaseWhenDone frees the hold once no plan is in progress: the confirm
// was finalised, a rollback restored, or the watchdog gave up before it
// touched anything (this process then keeps running).
func (u *Updater) releaseWhenDone(hold *applylock.Hold) {
	t := time.NewTicker(u.poll)
	defer t.Stop()
	for hold.Active() {
		p, err := kit.ReadPlan(u.stateDir)
		if err != nil || kit.Terminal(p.Phase) {
			u.mu.Lock()
			if u.hold == hold {
				u.hold = nil
			}
			u.mu.Unlock()
			hold.Release()
			u.log.Debug("update over: the write lock is free")
			return
		}
		<-t.C
	}
}

// Holding reports whether this process holds the write lock for an update.
func (u *Updater) Holding() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.hold.Active()
}

// WaitProbation holds the first dial of a new version back until the
// watchdog has put its plan in probation (at most max): the controller
// reads the update block once per session, and a candidate that reports
// "installing" there would never be confirmed. Anything else returns at
// once.
func (u *Updater) WaitProbation(ctx context.Context, max time.Duration) {
	deadline := time.Now().Add(max)
	for {
		p, err := kit.ReadPlan(u.stateDir)
		if err != nil || p.Phase != kit.PhaseInstalling || kit.CompareVersions(p.To, u.version) != 0 || !kit.ParseVersion(u.version).Known {
			return
		}
		if time.Now().After(deadline) {
			u.log.Warn("the update plan is still installing; dialing anyway", "update", p.UpdateID)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(250 * time.Millisecond):
		}
	}
}
