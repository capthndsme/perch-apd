package groups

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/capthndsme/perch-agentkit/openwrt/uci"
	"github.com/capthndsme/perch-apd/internal/applylock"
)

// Runner runs a command (the reloads).
type Runner func(ctx context.Context, name string, args ...string) error

// StationSeen is a station on a group VLAN (an AP_VLAN interface `<ifname>-g<vid>`).
type StationSeen struct {
	MAC    string `json:"mac"`
	VID    int    `json:"vid"`
	Ifname string `json:"ifname"`
}

// Options wire an engine.
type Options struct {
	// StateDir keeps the state and the rollback copies (/etc/perch-apd/groups).
	StateDir string
	// WirelessPath and NetworkPath are the configs (/etc/config/...).
	WirelessPath, NetworkPath string
	// StagingDir is where uci keeps uncommitted changes (/tmp/.uci).
	StagingDir string
	// RpcdDir is rpcd's state (/var/run/rpcd): LuCI's staged changes
	// (uci-<session>/) and its apply-with-rollback snapshot
	// (snapshot-files/).
	RpcdDir string
	// Lock is the AP's one write lock, shared with the Wi-Fi config plane
	// and agent updates (nil = a lock of the engine's own: tests).
	Lock *applylock.Lock
	// Wrote, when set, is told after the engine wrote wireless or network
	// (an apply or its rollback) with the SHA-256 (uci.FileHash) of each
	// file it wrote, so the Wi-Fi config plane attributes the change to
	// "groups-<revision>" instead of the router. Called with the engine's
	// mutex held: it must not call back into the engine.
	Wrote func(applyID string, hashes map[string]string)
	// FS is where trunk detection reads /proc and /sys.
	FS  FS
	Run Runner
	// Stations lists the stations on group VLANs (nil = none reported).
	Stations func() ([]StationSeen, error)
	Log      *slog.Logger
	Now      func() time.Time
}

// Engine applies desired states with a confirm window: the previous configs
// come back unless the controller confirms in time.
type Engine struct {
	o     Options
	mu    sync.Mutex
	st    state
	timer *time.Timer
	// hold is the write lock while an apply waits for its confirm.
	hold *applylock.Hold
}

type state struct {
	AppliedRevision int64     `json:"appliedRevision"`
	Ledger          Ledger    `json:"ledger"`
	Pending         *pending  `json:"pending,omitempty"`
	LastRollback    *rollback `json:"lastRollback,omitempty"`
	TrunkPort       string    `json:"trunkPort,omitempty"`
}

type pending struct {
	Revision int64     `json:"revision"`
	Deadline time.Time `json:"deadline"`
	Ledger   Ledger    `json:"ledger"`
	// Network: the network config changed (a rollback reloads the network).
	Network bool `json:"network"`
	// BindingKeys: digests of the passphrases bindings reuse (psk.go).
	BindingKeys []string `json:"bindingKeys,omitempty"`
	Result      ApplyResult
}

type rollback struct {
	Revision int64     `json:"revision"`
	At       time.Time `json:"at"`
	Reason   string    `json:"reason"`
}

// ApplyResult is groups.apply's result.
type ApplyResult struct {
	Revision  int64    `json:"revision"`
	State     string   `json:"state"` // pending_confirm | noop
	Deadline  string   `json:"deadline,omitempty"`
	TrunkPort string   `json:"trunkPort,omitempty"`
	Bridge    string   `json:"bridge,omitempty"`
	Converted bool     `json:"converted"`
	Managed   []string `json:"managed"`
	Issues    []string `json:"issues"`
}

// StateResult is groups.state's result.
type StateResult struct {
	AppliedRevision int64         `json:"appliedRevision"`
	Pending         *PendingView  `json:"pending"`
	LastRollback    *rollback     `json:"lastRollback,omitempty"`
	TrunkPort       string        `json:"trunkPort,omitempty"`
	Stations        []StationSeen `json:"stations"`
	Issues          []string      `json:"issues"`
}

// PendingView is an apply waiting for its confirm.
type PendingView struct {
	Revision int64  `json:"revision"`
	Deadline string `json:"deadline"`
}

// Confirm windows (seconds).
const (
	MinConfirmSeconds     = 30
	MaxConfirmSeconds     = 600
	DefaultConfirmSeconds = 120
)

// New loads the engine's state.
func New(o Options) (*Engine, error) {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.StagingDir == "" {
		o.StagingDir = "/tmp/.uci"
	}
	if o.RpcdDir == "" {
		o.RpcdDir = uci.RpcdDir
	}
	if o.Lock == nil {
		o.Lock = applylock.New()
	}
	e := &Engine{o: o}
	if err := os.MkdirAll(filepath.Join(o.StateDir, "rollback"), 0o700); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(o.StateDir, "state.json"))
	if err == nil {
		if err := json.Unmarshal(b, &e.st); err != nil {
			o.Log.Warn("groups: state file unreadable; starting empty", "err", err)
			e.st = state{}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return e, nil
}

// Start rolls back an apply whose window passed while the daemon was down
// (a reboot during the window), else re-arms its timer.
func (e *Engine) Start(ctx context.Context) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.st.Pending == nil {
		return
	}
	// The window is still open: it holds the write lock again (nobody else
	// can have it this early; if someone does, their window and this one
	// overlapped before the restart and the files are whatever they are).
	if hold, err := e.o.Lock.TryAcquire(applylock.Groups, applyID(e.st.Pending.Revision)); err == nil {
		e.hold = hold
	} else {
		e.o.Log.Error("groups: a pending apply found the write lock taken", "revision", e.st.Pending.Revision, "err", err)
	}
	left := e.st.Pending.Deadline.Sub(e.o.Now())
	if left <= 0 {
		e.rollbackLocked(ctx, "the confirm window passed while the daemon was down")
		return
	}
	e.armLocked(left)
}

func (e *Engine) armLocked(d time.Duration) {
	if e.timer != nil {
		e.timer.Stop()
	}
	rev := e.st.Pending.Revision
	e.timer = time.AfterFunc(d, func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.st.Pending != nil && e.st.Pending.Revision == rev {
			e.rollbackLocked(context.Background(), "no confirm from the controller")
		}
	})
}

// applyID is the name the engine's writes carry for the Wi-Fi config plane
// (its change log's author) and in the write lock.
func applyID(revision int64) string { return fmt.Sprintf("groups-%d", revision) }

func (e *Engine) releaseLocked() {
	e.hold.Release()
	e.hold = nil
}

func (e *Engine) wrote(revision int64, files map[string][]byte) {
	if e.o.Wrote == nil || len(files) == 0 {
		return
	}
	hashes := make(map[string]string, len(files))
	for name, b := range files {
		hashes[name] = uci.FileHash(b)
	}
	e.o.Wrote(applyID(revision), hashes)
}

func (e *Engine) persistLocked() {
	b, _ := json.Marshal(e.st)
	if err := writeAtomic(filepath.Join(e.o.StateDir, "state.json"), b, 0o600); err != nil {
		e.o.Log.Error("groups: saving the state", "err", err)
	}
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".perch-tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// uncommitted reports a config with changes staged in uci but not committed
// (the uci CLI, or a LuCI session): writing under it would mix the two.
func (e *Engine) uncommitted(name string) bool {
	staged := func(path string) bool {
		st, err := os.Stat(path)
		return err == nil && st.Mode().IsRegular() && st.Size() > 0
	}
	if staged(filepath.Join(e.o.StagingDir, name)) {
		return true
	}
	sessions, _ := filepath.Glob(filepath.Join(e.o.RpcdDir, "uci-*"))
	for _, dir := range sessions {
		if staged(filepath.Join(dir, name)) {
			return true
		}
	}
	return false
}

// luciPending reports a LuCI "apply with rollback" waiting for its confirm:
// rpcd restores its snapshot unless confirmed, over anything written now.
func (e *Engine) luciPending() bool {
	entries, err := os.ReadDir(filepath.Join(e.o.RpcdDir, "snapshot-files"))
	return err == nil && len(entries) > 0
}

func busy(reason, format string, args ...any) *Refusal {
	r := refuse("busy", format, args...)
	r.Reason = reason
	return r
}

func readConfig(path, name string) ([]byte, *uci.Config, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		b = nil
	} else if err != nil {
		return nil, nil, err
	}
	c, err := uci.Parse(name, b)
	if err != nil {
		return nil, nil, err
	}
	return b, c, nil
}

func fileMode(path string) os.FileMode {
	if st, err := os.Stat(path); err == nil {
		return st.Mode().Perm()
	}
	return 0o600
}

// Apply puts a desired state on the AP and waits for the confirm.
func (e *Engine) Apply(ctx context.Context, d Desired) (ApplyResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if p := e.st.Pending; p != nil {
		if p.Revision == d.Revision {
			return p.Result, nil
		}
		return ApplyResult{}, busy(applylock.Groups.Reason(), "revision %d is waiting for its confirm", p.Revision)
	}
	if err := Validate(d); err != nil {
		return ApplyResult{}, err
	}
	// The write lock from the snapshot to the confirm: no other writer
	// (the Wi-Fi config plane, an update) may have a window open meanwhile.
	hold, err := e.o.Lock.TryAcquire(applylock.Groups, applyID(d.Revision))
	if err != nil {
		return ApplyResult{}, busy(applylock.Reason(err), "%v", err)
	}
	keep := false
	defer func() {
		if !keep {
			hold.Release()
		}
	}()
	if e.luciPending() {
		return ApplyResult{}, busy("luci_pending", "a LuCI apply is waiting for its confirm: confirm or let it roll back first")
	}
	if e.uncommitted("wireless") || e.uncommitted("network") {
		return ApplyResult{}, refuse("uncommitted", "the wireless or network config has uncommitted changes (a LuCI session): apply or revert them first")
	}
	wRaw, wc, err := readConfig(e.o.WirelessPath, "wireless")
	if err != nil {
		return ApplyResult{}, refuse("config_unreadable", "reading %s: %v", e.o.WirelessPath, err)
	}
	nRaw, nc, err := readConfig(e.o.NetworkPath, "network")
	if err != nil {
		return ApplyResult{}, refuse("config_unreadable", "reading %s: %v", e.o.NetworkPath, err)
	}
	var facts Facts
	var detectErr error
	if d.Trunk == "" || d.Trunk == "auto" {
		facts.TrunkPort, detectErr = DetectTrunk(e.o.FS)
	}
	res, err := Plan(wc, nc, d, facts, e.st.Ledger)
	if err != nil {
		var r *Refusal
		if errors.As(err, &r) && r.Code == "trunk_unknown" && detectErr != nil {
			r.Message += " (" + detectErr.Error() + ")"
		}
		return ApplyResult{}, err
	}
	if res.TrunkPort != "" {
		e.st.TrunkPort = res.TrunkPort
	}
	out := ApplyResult{Revision: d.Revision, TrunkPort: res.TrunkPort, Bridge: res.Bridge,
		Converted: res.Converted, Managed: res.Managed, Issues: res.Issues}
	if out.Managed == nil {
		out.Managed = []string{}
	}
	if out.Issues == nil {
		out.Issues = []string{}
	}
	newW, newN := uci.Render(res.Wireless), uci.Render(res.Network)
	wChanged := !bytes.Equal(newW, uci.Render(wc))
	nChanged := !bytes.Equal(newN, uci.Render(nc))
	if !wChanged && !nChanged {
		e.st.AppliedRevision = d.Revision
		e.st.Ledger = res.Ledger
		e.persistLocked()
		out.State = "noop"
		return out, nil
	}
	// The copies to go back to.
	rb := filepath.Join(e.o.StateDir, "rollback")
	if err := writeAtomic(filepath.Join(rb, "wireless"), wRaw, 0o600); err != nil {
		return ApplyResult{}, refuse("apply_failed", "saving the rollback copy: %v", err)
	}
	if err := writeAtomic(filepath.Join(rb, "network"), nRaw, 0o600); err != nil {
		return ApplyResult{}, refuse("apply_failed", "saving the rollback copy: %v", err)
	}
	confirm := d.ConfirmSeconds
	if confirm == 0 {
		confirm = DefaultConfirmSeconds
	}
	if confirm < MinConfirmSeconds {
		confirm = MinConfirmSeconds
	}
	if confirm > MaxConfirmSeconds {
		confirm = MaxConfirmSeconds
	}
	deadline := e.o.Now().Add(time.Duration(confirm) * time.Second)
	out.State = "pending_confirm"
	out.Deadline = deadline.UTC().Format(time.RFC3339)
	e.st.Pending = &pending{Revision: d.Revision, Deadline: deadline, Ledger: res.Ledger, Network: nChanged,
		BindingKeys: res.BindingKeys, Result: out}
	// From here the window owns the lock; rollbackLocked or Confirm
	// release it.
	e.hold, keep = hold, true
	e.persistLocked()
	written := map[string][]byte{}
	if wChanged {
		if err := writeAtomic(e.o.WirelessPath, newW, fileMode(e.o.WirelessPath)); err != nil {
			e.rollbackLocked(ctx, "writing the wireless config failed")
			return ApplyResult{}, refuse("apply_failed", "writing %s: %v", e.o.WirelessPath, err)
		}
		written["wireless"] = newW
	}
	if nChanged {
		if err := writeAtomic(e.o.NetworkPath, newN, fileMode(e.o.NetworkPath)); err != nil {
			e.rollbackLocked(ctx, "writing the network config failed")
			return ApplyResult{}, refuse("apply_failed", "writing %s: %v", e.o.NetworkPath, err)
		}
		written["network"] = newN
	}
	e.wrote(d.Revision, written)
	if err := e.reload(ctx, nChanged); err != nil {
		e.rollbackLocked(ctx, "the reload failed: "+err.Error())
		return ApplyResult{}, refuse("apply_failed", "reloading: %v", err)
	}
	// The engine's clock, not the wall clock: the deadline came from it.
	e.armLocked(deadline.Sub(e.o.Now()))
	e.o.Log.Info("groups: applied, waiting for the confirm", "revision", d.Revision,
		"deadline", out.Deadline, "network", nChanged, "trunk", res.TrunkPort, "converted", res.Converted)
	return out, nil
}

// reload makes netifd take the configs: the network (which brings the
// wireless along) when it changed, else only the wireless (stations alone
// reload hostapd's PSK list without dropping clients).
func (e *Engine) reload(ctx context.Context, network bool) error {
	if e.o.Run == nil {
		return nil
	}
	rctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if network {
		return e.o.Run(rctx, "/etc/init.d/network", "reload")
	}
	return e.o.Run(rctx, "wifi", "reload")
}

// Confirm keeps a pending apply, unless hostapd holds a binding's
// passphrase for every client: then it rolls back (psk.go).
func (e *Engine) Confirm(ctx context.Context, revision int64) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	p := e.st.Pending
	if p == nil || p.Revision != revision {
		if p == nil && e.st.AppliedRevision == revision {
			return nil // confirmed already
		}
		return refuse("not_pending", "revision %d is not waiting for a confirm", revision)
	}
	if err := checkPSKFiles(e.o.FS, p.BindingKeys); err != nil {
		e.rollbackLocked(context.WithoutCancel(ctx), err.Error())
		return refuse("unsafe_binding", "rolled back: %v", err)
	}
	if e.timer != nil {
		e.timer.Stop()
		e.timer = nil
	}
	e.st.AppliedRevision = revision
	e.st.Ledger = p.Ledger
	e.st.Pending = nil
	e.persistLocked()
	e.releaseLocked()
	rb := filepath.Join(e.o.StateDir, "rollback")
	os.Remove(filepath.Join(rb, "wireless"))
	os.Remove(filepath.Join(rb, "network"))
	e.o.Log.Info("groups: confirmed", "revision", revision)
	return nil
}

// rollbackLocked puts the previous configs back and reloads.
func (e *Engine) rollbackLocked(ctx context.Context, reason string) {
	p := e.st.Pending
	if p == nil {
		return
	}
	if e.timer != nil {
		e.timer.Stop()
		e.timer = nil
	}
	rb := filepath.Join(e.o.StateDir, "rollback")
	restored := map[string][]byte{}
	restore := func(name, path string) {
		b, err := os.ReadFile(filepath.Join(rb, name))
		if err != nil {
			e.o.Log.Error("groups: no rollback copy", "config", name, "err", err)
			return
		}
		if err := writeAtomic(path, b, fileMode(path)); err != nil {
			e.o.Log.Error("groups: restoring", "config", name, "err", err)
			return
		}
		restored[name] = b
	}
	restore("wireless", e.o.WirelessPath)
	restore("network", e.o.NetworkPath)
	e.wrote(p.Revision, restored)
	if err := e.reload(ctx, true); err != nil {
		e.o.Log.Error("groups: reload after the rollback", "err", err)
	}
	e.st.LastRollback = &rollback{Revision: p.Revision, At: e.o.Now().UTC(), Reason: reason}
	e.st.Pending = nil
	e.persistLocked()
	e.releaseLocked()
	e.o.Log.Warn("groups: rolled back", "revision", p.Revision, "reason", reason)
}

// State reports what the AP holds.
func (e *Engine) State(ctx context.Context) StateResult {
	e.mu.Lock()
	out := StateResult{AppliedRevision: e.st.AppliedRevision, LastRollback: e.st.LastRollback,
		TrunkPort: e.st.TrunkPort, Stations: []StationSeen{}, Issues: []string{}}
	if p := e.st.Pending; p != nil {
		out.Pending = &PendingView{Revision: p.Revision, Deadline: p.Deadline.UTC().Format(time.RFC3339)}
	}
	e.mu.Unlock()
	if out.TrunkPort == "" {
		if port, err := DetectTrunk(e.o.FS); err == nil {
			out.TrunkPort = port
		} else {
			out.Issues = append(out.Issues, err.Error())
		}
	}
	if e.o.Stations != nil {
		if list, err := e.o.Stations(); err == nil {
			out.Stations = append(out.Stations, list...)
		} else {
			out.Issues = append(out.Issues, fmt.Sprintf("stations: %v", err))
		}
	}
	return out
}

// OwnedView is what the groups engine holds on the AP, for the Wi-Fi
// config plane to keep out of its own model (reads mark these sections
// `owner: "groups"`).
type OwnedView struct {
	// Sections are the names of the sections the engine created, in
	// wireless and network, sorted.
	Sections []string `json:"sections"`
	// DynamicVLAN are the wifi-iface sections whose dynamic_vlan the engine
	// set, sorted.
	DynamicVLAN []string `json:"dynamicVlan"`
	// Converted is the bridge the engine converted to VLAN filtering ("" =
	// none).
	Converted string `json:"converted,omitempty"`
	// Pending: an apply is waiting for its confirm (its sections are
	// included).
	Pending bool `json:"pending"`
}

// Owned reports the sections and options the engine holds: the confirmed
// ledger's, plus a pending apply's (the files hold those until its window
// closes). A ledger from before the engine recorded names falls back to
// the naming patterns over the current files.
func (e *Engine) Owned() OwnedView {
	e.mu.Lock()
	ledgers := []Ledger{e.st.Ledger}
	pending := e.st.Pending != nil
	if pending {
		ledgers = append(ledgers, e.st.Pending.Ledger)
	}
	e.mu.Unlock()
	sections, dyn := map[string]bool{}, map[string]bool{}
	out := OwnedView{Sections: []string{}, DynamicVLAN: []string{}, Pending: pending}
	var w, n *uci.Config
	for _, l := range ledgers {
		if l.Owned == nil && w == nil {
			_, w, _ = readConfig(e.o.WirelessPath, "wireless")
			_, n, _ = readConfig(e.o.NetworkPath, "network")
			if w == nil {
				w = &uci.Config{Name: "wireless"}
			}
			if n == nil {
				n = &uci.Config{Name: "network"}
			}
		}
		for name := range ownedSet(l, w, n) {
			sections[name] = true
		}
		for name := range l.DynamicVLAN {
			dyn[name] = true
		}
		if l.Converted != nil {
			out.Converted = l.Converted.Bridge
		}
	}
	for name := range sections {
		out.Sections = append(out.Sections, name)
	}
	for name := range dyn {
		out.DynamicVLAN = append(out.DynamicVLAN, name)
	}
	sort.Strings(out.Sections)
	sort.Strings(out.DynamicVLAN)
	return out
}
