// Package wifiplane is perch-apd's Wi-Fi config plane (Wi-Fi design
// protocol.md): the kit's openwrt/plane on /etc/config/wireless and
// /etc/config/network, served as wifi.* on the controller session, plus
// what only an access point has: the health check that gates every apply
// (radios up, every expected BSS beaconing, radar checks waited out), the
// boot guard, the device-groups engine as a second writer (one write lock,
// its sections marked and never touched by the plane), and the Wi-Fi facts
// of wifi.capabilities.
//
// The owner of the AP opts in with option wifi_config (none, read, write)
// in /etc/config/perch-apd; the controller can never change it (perch-apd
// is on the plane's denylist), and only wireless and network are ever read
// or written.
package wifiplane

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/capthndsme/perch-agentkit/openwrt/plane"
	"github.com/capthndsme/perch-agentkit/openwrt/ubus"
	"github.com/capthndsme/perch-agentkit/openwrt/uci"
	"github.com/capthndsme/perch-agentkit/rpc"
	"github.com/capthndsme/perch-apd/internal/applylock"
	"github.com/capthndsme/perch-apd/internal/groups"
	"github.com/capthndsme/perch-apd/internal/wificaps"
)

// Where the plane keeps its state and names its methods.
const (
	Prefix = "wifi"
	// StateDir survives reboots (snapshots, pending.json, results.json,
	// later the pairing key); kept over sysupgrade by the package's keep.d.
	StateDir = "/etc/perch-apd/plane"
	// RunDir is on tmpfs: the markers of pending applies.
	RunDir = "/var/run/perch-apd"
	// MaxSecrets caps one apply's secrets (a passphrase per network).
	MaxSecrets = 64
	// ErrOff is data.error of every wifi.* refusal while the plane is off
	// (access none, or not OpenWrt): -32001.
	ErrOff = "wifi_config_off"
	// ErrGuardMissing refuses an apply while the boot guard is missing.
	ErrGuardMissing = "guard_missing"
)

// The wifi.* methods the plane serves (pairing, neighbour lists and the
// groups hand-over come with later packages).
const (
	MethodCapabilities = "wifi.capabilities"
	MethodHealth       = "wifi.health"
	// NotifyHealthChanged is sent when a radio or an expected BSS went down
	// or came back (debounced).
	NotifyHealthChanged = "wifi.health.changed"
)

// Groups is the device-groups engine as the plane sees it.
type Groups interface {
	Owned() groups.OwnedView
	State(ctx context.Context) groups.StateResult
}

// Options wire a Service.
type Options struct {
	// Access is none, read or write (option wifi_config).
	Access string
	// Allow are the configs the plane may use; only wireless and network
	// are ever kept.
	Allow []string
	// AllowInsecure: signed writes over an unverified transport
	// (wifi_config_insecure).
	AllowInsecure bool
	// ConfirmMax caps every confirm window, seconds.
	ConfirmMax int
	// TransportOK: the controller is https with the certificate verified.
	TransportOK bool
	// ServerURL is the controller (its route is the management path).
	ServerURL string
	// Lock is the AP's one write lock (device groups, updates).
	Lock *applylock.Lock
	// Groups is the device-groups engine; nil when it is off.
	Groups Groups
	// GroupsEnabled: the engine takes groups.apply (capability wifi_groups).
	GroupsEnabled bool
	// Prober finds the Wi-Fi facts (and the radios of a wireless uplink).
	Prober *wificaps.Prober
	// Ubus is the kit's client (rpcd staging, network.wireless status,
	// hostapd); nil = ubus.New().
	Ubus *ubus.Client
	Log  *slog.Logger

	// Root prefixes every path ("" = /; tests).
	Root string
	// OpenWrt overrides the /etc/openwrt_release check (tests).
	OpenWrt *bool
	// Backend, Clock, Run, LookupHost: the kit's (tests).
	Backend    plane.Backend
	Clock      plane.Clock
	Run        ubus.Runner
	LookupHost func(ctx context.Context, host string) ([]string, error)
	// SettleMin and SettleMax bound the wait for netifd after a commit
	// (defaults 2 s and 30 s); HealthInterval is the pause between checks.
	SettleMin, SettleMax time.Duration
	HealthInterval       time.Duration
	// MonitorInterval is the pause between the health monitor's checks
	// outside applies (default 30 s).
	MonitorInterval time.Duration
}

// Default settle bounds (protocol.md 3.4).
const (
	DefaultSettleMin       = 2 * time.Second
	DefaultSettleMax       = 30 * time.Second
	DefaultMonitorInterval = 30 * time.Second
)

// Service is the Wi-Fi config plane of one AP. Safe for concurrent use.
type Service struct {
	o       Options
	p       *plane.Plane
	log     *slog.Logger
	ubus    *ubus.Client
	files   uci.Files
	openwrt bool
	lock    *planeLock

	mu       sync.Mutex
	sessions func(ctx context.Context) (uint64, string)
	send     func(method string, params any) bool
	lastMode string
	ownedSet map[string]bool
	mon      monitorState
	// checked is told every apply health check's answer (tests).
	checked func(plane.HealthReport)
}

// New builds the plane from the daemon's settings. It never fails on the
// device's state: a device that is not OpenWrt gets access none.
func New(o Options) (*Service, error) {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Ubus == nil {
		o.Ubus = ubus.New()
	}
	if o.Prober == nil {
		o.Prober = &wificaps.Prober{Root: o.Root}
	}
	if o.SettleMin == 0 {
		o.SettleMin = DefaultSettleMin
	}
	if o.SettleMax == 0 {
		o.SettleMax = DefaultSettleMax
	}
	if o.MonitorInterval == 0 {
		o.MonitorInterval = DefaultMonitorInterval
	}
	s := &Service{o: o, log: o.Log, ubus: o.Ubus, files: uci.Files{Dir: rooted(o.Root, uci.DefaultDir)}}
	s.openwrt = plane.ReadOpenWrt(o.Root) != nil
	if o.OpenWrt != nil {
		s.openwrt = *o.OpenWrt
	}
	access := o.Access
	if !s.openwrt {
		access = plane.AccessNone
	}
	if o.Lock != nil {
		s.lock = &planeLock{l: o.Lock}
	}
	var allow []string
	for _, c := range o.Allow {
		if c == "wireless" || c == "network" {
			allow = append(allow, c)
		}
	}
	po := plane.Options{
		Name: "perch-apd", Prefix: Prefix, StateDir: StateDir, RunDir: RunDir,
		Access: access, Allow: allow,
		ApplyOrder:    []string{"network", "wireless"},
		ForeignStaged: []string{"network", "wireless"},
		AllowInsecure: o.AllowInsecure, ConfirmMax: o.ConfirmMax, MaxSecrets: MaxSecrets,
		TransportOK: o.TransportOK, ServerURL: o.ServerURL,
		// Unbound: a network's passphrase fingerprints alike on every
		// section and AP; the key arrives with agent.configure.
		Redactor:       uci.Redactor{Unbound: true},
		Validators:     s.validators(),
		WirelessRadios: func(ctx context.Context, dev string) []string { return o.Prober.ManagementRadios(ctx, dev) },
		Owner:          s.owner,
		ReloadKind:     reloadKind,
		Health:         s.healthCheck,
		HealthBefore:   s.healthBefore,
		HealthInterval: o.HealthInterval,
		Settle:         s.settle,
		Root:           o.Root, Ubus: o.Ubus, Backend: o.Backend, Clock: o.Clock, Run: o.Run, LookupHost: o.LookupHost,
		Log: func(format string, args ...any) { s.log.Info("wifi config: " + fmt.Sprintf(format, args...)) },
	}
	if s.lock != nil {
		po.Lock = s.lock
	}
	p, err := plane.New(po)
	if err != nil {
		return nil, err
	}
	s.p = p
	return s, nil
}

func rooted(root, p string) string {
	if root == "" || root == "/" {
		return p
	}
	return filepath.Join(root, p)
}

// Plane is the kit's plane underneath (tests, the CLI).
func (s *Service) Plane() *plane.Plane { return s.p }

// Access is the effective access: none off OpenWrt.
func (s *Service) Access() string { return s.p.Access() }

// OnOpenWrt reports whether the device is OpenWrt.
func (s *Service) OnOpenWrt() bool { return s.openwrt }

// Start runs once at daemon start, before any session: a pending apply is
// resumed or restored (a reboot, a missed deadline), and with write access
// the boot guard is put in place when the package did not ship it.
func (s *Service) Start() {
	if s.p.ConfiguredAccess() == plane.AccessWrite && s.openwrt {
		status, changed, err := EnsureGuard(s.o.Root)
		switch {
		case err != nil:
			s.log.Error("wifi config: the boot guard is missing and could not be installed; applies are refused", "err", err)
		case changed:
			s.log.Info("wifi config: installed the boot guard "+GuardInit, "status", status)
		}
	}
	s.p.Start()
	switch s.Access() {
	case plane.AccessNone:
		if !s.openwrt {
			s.log.Info("wifi config: off (not OpenWrt)")
		} else {
			s.log.Info("wifi config: off (option wifi_config 'none'): the controller cannot read or manage Wi-Fi here")
		}
	case plane.AccessWrite:
		if !s.o.TransportOK && !s.o.AllowInsecure {
			s.log.Warn("wifi config: write access, but writes need an https controller with a verified certificate (or wifi_config_insecure '1' and signed requests); until then read only",
				"configs", strings.Join(s.p.Allowed(), " "))
		} else {
			s.log.Info("wifi config: write access", "configs", strings.Join(s.p.Allowed(), " "))
		}
	default:
		s.log.Info("wifi config: read access", "configs", strings.Join(s.p.Allowed(), " "))
	}
}

// Session wires the plane to the session loop: which session a request came
// on, sending notifications, and dropping the session after an apply.
func (s *Service) Session(sessions func(ctx context.Context) (uint64, string), send func(method string, params any) bool, reconnect func(reason string)) {
	s.mu.Lock()
	s.sessions, s.send = sessions, send
	s.mu.Unlock()
	methods := s.p.Methods()
	s.p.SetHooks(plane.Hooks{
		Reconnect: reconnect,
		Result:    func(r plane.Result) bool { return s.notify(methods.Result, r) },
	})
}

func (s *Service) notify(method string, params any) bool {
	s.mu.Lock()
	send := s.send
	s.mu.Unlock()
	return send != nil && send(method, params)
}

func (s *Service) sessionRef(ctx context.Context) plane.SessionRef {
	s.mu.Lock()
	f := s.sessions
	s.mu.Unlock()
	if f == nil {
		return plane.SessionRef{}
	}
	gen, challenge := f(ctx)
	return plane.SessionRef{Gen: gen, Challenge: challenge}
}

// Configure takes agent.configure's params: its wifiConfig block sets the
// controller's mode (off, observe, managed), the watch timing, the health
// wait and the fleet fingerprint key. No block is mode off.
func (s *Service) Configure(params json.RawMessage) {
	var p struct {
		WifiConfig *plane.Configure `json:"wifiConfig"`
	}
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			s.log.Warn("wifi config: bad wifiConfig in agent.configure; mode off", "err", err)
		}
	}
	c := plane.Configure{}
	if p.WifiConfig != nil {
		c = *p.WifiConfig
	}
	s.p.Configure(c)
	mode := s.p.Mode()
	s.mu.Lock()
	changed := mode != s.lastMode
	s.lastMode = mode
	s.mu.Unlock()
	if changed {
		s.log.Info("wifi config: mode from the controller", "mode", mode, "authoritative", c.Authoritative)
	}
}

// EndSession is a session's end: mode off until the next configure (the
// fingerprint key stays for a rollback while disconnected).
func (s *Service) EndSession() {
	s.p.EndSession()
	s.mu.Lock()
	s.lastMode = plane.ModeOff
	s.mu.Unlock()
}

// RedialFast: the session loop should redial every couple of seconds.
func (s *Service) RedialFast() bool { return s.p.RedialFast() }

// Trigger is SIGHUP (procd's reload trigger on wireless or network).
func (s *Service) Trigger() { s.p.Trigger() }

// GroupsWrote is the device-groups engine's Wrote hook: its writes are
// reported as origin perch with applyId groups-<revision>, never as a
// router edit.
func (s *Service) GroupsWrote(applyID string, hashes map[string]string) {
	for config, hash := range hashes {
		s.p.RecordOwn(config, hash, applyID)
	}
}

// Run watches for changes (wifi.config.changed) and the Wi-Fi's health
// (wifi.health.changed) until ctx ends.
func (s *Service) Run(ctx context.Context) {
	methods := s.p.Methods()
	go s.monitor(ctx)
	s.p.Run(ctx, func(c plane.Changed) bool { return s.notify(methods.Changed, c) })
}

// GroupsHello is the device-groups engine's state in the hello, so the
// controller never runs both writers at once.
type GroupsHello struct {
	Engine     bool   `json:"engine"`
	Enabled    bool   `json:"enabled"`
	State      string `json:"state"`
	HandedOver bool   `json:"handedOver"`
}

// Hello is system.info's wifiConfig block.
type Hello struct {
	*plane.Hello
	AllowInsecure bool        `json:"allowInsecure"`
	Groups        GroupsHello `json:"groups"`
}

// Hello builds system.info's wifiConfig block for the session ctx belongs
// to; it is also the baseline of the change notifications that follow.
func (s *Service) Hello(ctx context.Context) any {
	sess := s.sessionRef(ctx)
	return &Hello{Hello: s.p.Hello(ctx, sess.Challenge), AllowInsecure: s.o.AllowInsecure, Groups: s.groupsHello()}
}

func (s *Service) groupsHello() GroupsHello {
	g := GroupsHello{Engine: true, Enabled: s.o.Groups != nil && s.o.GroupsEnabled, State: "idle"}
	if s.o.Groups != nil && s.o.Groups.Owned().Pending {
		g.State = "pending_confirm"
	}
	return g
}

// Methods are the wifi.* methods served.
func (s *Service) Methods() []string {
	m := s.p.Methods()
	return []string{MethodCapabilities, MethodHealth, m.Read, m.Apply, m.Confirm, m.Rollback, m.Ack}
}

// Serve runs one wifi.* method for the session ctx belongs to.
func (s *Service) Serve(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	if s.Access() == plane.AccessNone {
		msg := "Wi-Fi management is off on this access point (option wifi_config 'none' in /etc/config/perch-apd)"
		if !s.openwrt {
			msg = "Wi-Fi management needs OpenWrt"
		}
		return nil, &rpc.Error{Code: rpc.CodeUnsupported, Message: msg, Data: map[string]string{"error": ErrOff}}
	}
	sess := s.sessionRef(ctx)
	m := s.p.Methods()
	switch method {
	case MethodCapabilities:
		return s.Capabilities(ctx, sess.Challenge), nil
	case MethodHealth:
		return s.HealthNow(ctx), nil
	case m.Read:
		var a plane.ReadParams
		if len(raw) > 0 && string(raw) != "null" {
			if err := json.Unmarshal(raw, &a); err != nil {
				return nil, &rpc.Error{Code: rpc.CodeInvalidParams, Message: "params: " + err.Error(), Data: map[string]string{"error": plane.CodeBadParams}}
			}
		}
		res, err := s.Read(a.Configs)
		if err != nil {
			return nil, plane.RPCError(err)
		}
		return res, nil
	case m.Apply:
		if err := s.screenApply(raw); err != nil {
			return nil, plane.RPCError(err)
		}
	case m.Confirm, m.Rollback, m.Ack:
	default:
		return nil, rpc.Errorf(rpc.CodeMethodNotFound, "method %q not found", method)
	}
	// ServeWrite, not Serve: the pairing methods (a later package) stay out
	// of the binary.
	res, err := s.p.ServeWrite(ctx, method, raw, sess)
	if err != nil {
		return nil, plane.RPCError(err)
	}
	return res, nil
}

// GroupsOwned is what the device-groups engine set on sections it does not
// own (the controller keeps those options out of the plane).
type GroupsOwned struct {
	DynamicVLAN []string `json:"dynamicVlan"`
}

// ReadResult is wifi.config.read's result.
type ReadResult struct {
	*plane.ReadResult
	GroupsOwned GroupsOwned `json:"groupsOwned"`
}

// Read is wifi.config.read: the committed configs with secrets
// fingerprinted, the device groups' sections marked owner "groups".
func (s *Service) Read(configs []string) (*ReadResult, error) {
	view := s.groupsOwned()
	res, err := s.p.Read(configs)
	if err != nil {
		return nil, err
	}
	return &ReadResult{ReadResult: res, GroupsOwned: GroupsOwned{DynamicVLAN: view.DynamicVLAN}}, nil
}

// GroupsCaps is the device-groups engine in wifi.capabilities.
type GroupsCaps struct {
	GroupsHello
	AppliedRevision int64    `json:"appliedRevision"`
	Owned           []string `json:"owned"`
}

// Capabilities is wifi.capabilities: the plane's part (access, transport,
// backend, hashes, apply state, management path) and the Wi-Fi facts.
type Capabilities struct {
	*plane.Capabilities
	wificaps.Facts
	// Both parts know the firmware and the package manager; these win.
	OpenWrt        *plane.OpenWrt `json:"openwrt"`
	PackageManager *string        `json:"packageManager"`
	// Guard is the boot guard: installed (by the package), self_installed
	// (by the daemon) or missing.
	Guard  string     `json:"guard"`
	Groups GroupsCaps `json:"groups"`
}

// Capabilities describes the AP and what the plane may do there.
func (s *Service) Capabilities(ctx context.Context, challenge string) *Capabilities {
	pc := s.p.Capabilities(ctx, challenge)
	c := &Capabilities{Capabilities: pc, Facts: s.o.Prober.Facts(ctx), OpenWrt: pc.OpenWrt,
		PackageManager: pc.PackageManager, Guard: GuardStatus(s.o.Root)}
	c.Groups.GroupsHello = s.groupsHello()
	c.Groups.Owned = []string{}
	if s.o.Groups != nil {
		st := s.o.Groups.State(ctx)
		c.Groups.AppliedRevision = st.AppliedRevision
		c.Groups.Owned = append(c.Groups.Owned, s.o.Groups.Owned().Sections...)
	}
	return c
}

// planeLock adapts the AP's write lock to the plane's Lock.
type planeLock struct {
	l    *applylock.Lock
	mu   sync.Mutex
	hold *applylock.Hold
}

func (pl *planeLock) Acquire(id string) (bool, string) {
	h, err := pl.l.TryAcquire(applylock.Plane, id)
	if err != nil {
		if reason := applylock.Reason(err); reason != "" {
			return false, reason
		}
		return false, "locked"
	}
	pl.mu.Lock()
	pl.hold = h
	pl.mu.Unlock()
	return true, ""
}

func (pl *planeLock) Release() {
	pl.mu.Lock()
	h := pl.hold
	pl.hold = nil
	pl.mu.Unlock()
	h.Release()
}

// reloadKind says what a commit of configs reloads: the network (and the
// Wi-Fi with it), the Wi-Fi alone, or nothing.
func reloadKind(configs []string) string {
	kind := "none"
	for _, c := range configs {
		switch c {
		case "network":
			return "network"
		case "wireless":
			kind = "wifi"
		}
	}
	return kind
}
