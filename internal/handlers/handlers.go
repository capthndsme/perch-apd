// Package handlers implements the methods the controller can call on the
// agent (PROTOCOL.md §2.2). This fixed list is the whole remote surface of
// the agent: there is deliberately no way to run arbitrary commands.
package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os/exec"
	"strings"
	"time"

	"github.com/capthndsme/perch-agentkit/rpc"
	"github.com/capthndsme/perch-apd/internal/leds"
	"github.com/capthndsme/perch-apd/internal/sysinfo"
	"github.com/capthndsme/perch-apd/internal/ubus"
	"github.com/capthndsme/perch-apd/internal/version"
	"github.com/capthndsme/perch-apd/internal/wireless"
)

// ProtocolVersion is the protocol this agent speaks.
const ProtocolVersion = 1

// Ubus is what the handlers need from ubus.
type Ubus interface {
	Call(ctx context.Context, object, method string, args any, out any) error
	List(ctx context.Context, pattern string) ([]string, error)
	Available() bool
}

// Deps are the handlers' collaborators.
type Deps struct {
	Log      *slog.Logger
	Wireless *wireless.Source
	Ubus     Ubus // nil off OpenWrt
	Locator  *leds.Locator
	Info     *sysinfo.Info
	// Reboot reboots the device; nil = `reboot`.
	Reboot func() error
	Now    func() time.Time
}

func (d *Deps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// Register installs every method on the dispatcher.
func Register(disp *rpc.Dispatcher, d *Deps) {
	disp.Register("system.info", d.systemInfo)
	disp.Register("clients.list", d.clientsList)
	disp.Register("client.kick", d.clientKick)
	disp.Register("locate.start", d.locateStart)
	disp.Register("locate.stop", d.locateStop)
	disp.Register("system.reboot", d.systemReboot)
	disp.Register("ping", d.ping)
}

// SystemInfo is the result of system.info.
type SystemInfo struct {
	AgentVersion  string           `json:"agentVersion"`
	Protocol      int              `json:"protocol"`
	Hostname      string           `json:"hostname"`
	Model         string           `json:"model,omitempty"`
	BoardName     string           `json:"boardName,omitempty"`
	System        string           `json:"system,omitempty"`
	Release       string           `json:"release,omitempty"`
	Revision      string           `json:"revision,omitempty"`
	Target        string           `json:"target,omitempty"`
	Kernel        string           `json:"kernel,omitempty"`
	Arch          string           `json:"arch"`
	UptimeSeconds int64            `json:"uptimeSeconds"`
	MACs          []string         `json:"macs"`
	Capabilities  []string         `json:"capabilities"`
	Radios        []wireless.Radio `json:"radios"`
	Interfaces    []wireless.Iface `json:"interfaces"`
}

// SystemInfo gathers the system.info result (also used by `perch-apd info`).
func (d *Deps) SystemInfo(ctx context.Context) SystemInfo {
	b := d.Info.Board(ctx)
	info := SystemInfo{
		AgentVersion: version.Version,
		Protocol:     ProtocolVersion,
		Hostname:     b.Hostname,
		Model:        b.Model,
		BoardName:    b.BoardName,
		System:       b.System,
		Release:      b.Release,
		Revision:     b.Revision,
		Target:       b.Target,
		Kernel:       b.Kernel,
		Arch:         version.Arch(),
		MACs:         sysinfo.MACs(),
		Radios:       []wireless.Radio{},
		Interfaces:   []wireless.Iface{},
	}
	if up, err := d.Info.Uptime(); err == nil {
		info.UptimeSeconds = int64(up)
	}
	if info.MACs == nil {
		info.MACs = []string{}
	}
	if d.Wireless != nil {
		if inv, err := d.Wireless.Inventory(ctx); err == nil {
			info.Radios = append(info.Radios, inv.Radios...)
			for _, ifi := range inv.Interfaces {
				if st, err := d.Wireless.StationsOf(ifi); err == nil {
					ifi.Stations = len(st)
				}
				info.Interfaces = append(info.Interfaces, ifi)
			}
		}
	}
	info.Capabilities = d.Capabilities(ctx)
	return info
}

// Capabilities lists what this build and device can do right now.
func (d *Deps) Capabilities(ctx context.Context) []string {
	caps := []string{"metrics"}
	hasWifi := false
	if d.Wireless != nil && d.Wireless.NL != nil {
		if inv, err := d.Wireless.Inventory(ctx); err == nil && len(inv.Interfaces) > 0 {
			hasWifi = true
		}
	}
	if hasWifi {
		caps = append(caps, "clients")
		if d.Ubus != nil && d.Ubus.Available() {
			if names, err := d.Ubus.List(ctx, "hostapd.*"); err == nil && len(names) > 0 {
				caps = append(caps, "kick")
			}
		}
	}
	if d.Locator != nil && d.Locator.Count() > 0 {
		caps = append(caps, "locate")
	}
	if d.Info != nil && d.Info.IsOpenWrt() {
		caps = append(caps, "reboot")
	}
	return caps
}

func (d *Deps) systemInfo(ctx context.Context, _ json.RawMessage) (any, error) {
	return d.SystemInfo(ctx), nil
}

func (d *Deps) clientsList(ctx context.Context, raw json.RawMessage) (any, error) {
	var p struct {
		Ifname string `json:"ifname"`
	}
	if err := rpc.Params(raw, &p); err != nil {
		return nil, err
	}
	if d.Wireless == nil || d.Wireless.NL == nil {
		return nil, rpc.Errorf(rpc.CodeUnsupported, "this device has no Wi-Fi (nl80211 unavailable)")
	}
	clients, err := d.Wireless.Clients(ctx, p.Ifname)
	if err != nil {
		return nil, err
	}
	return map[string]any{"clients": clients}, nil
}

// validMAC accepts 02:00:00:00:00:01 and 02-00-00-00-00-01.
func validMAC(s string) bool {
	if len(s) != 17 {
		return false
	}
	for i := 0; i < 17; i++ {
		c := s[i]
		if i%3 == 2 {
			if c != ':' && c != '-' {
				return false
			}
			continue
		}
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// KickResult is the result of client.kick.
type KickResult struct {
	MAC       string `json:"mac"`
	Ifname    string `json:"ifname"`
	BanTimeMs int    `json:"banTimeMs"`
}

func (d *Deps) clientKick(ctx context.Context, raw json.RawMessage) (any, error) {
	p := struct {
		MAC       string `json:"mac"`
		Ifname    string `json:"ifname"`
		BanTimeMs *int   `json:"banTimeMs"`
		Reason    *int   `json:"reason"`
		Deauth    *bool  `json:"deauth"`
	}{}
	if err := rpc.Params(raw, &p); err != nil {
		return nil, err
	}
	if !validMAC(p.MAC) {
		return nil, rpc.Errorf(rpc.CodeInvalidParams, "mac must look like 02:00:00:00:00:01")
	}
	mac, _ := net.ParseMAC(strings.ReplaceAll(p.MAC, "-", ":"))
	ban, reason, deauth := 0, 5, true
	if p.BanTimeMs != nil {
		ban = *p.BanTimeMs
	}
	if p.Reason != nil {
		reason = *p.Reason
	}
	if p.Deauth != nil {
		deauth = *p.Deauth
	}
	if ban < 0 || ban > 3_600_000 {
		return nil, rpc.Errorf(rpc.CodeInvalidParams, "banTimeMs must be 0..3600000")
	}
	if reason < 1 || reason > 65535 {
		return nil, rpc.Errorf(rpc.CodeInvalidParams, "reason must be 1..65535")
	}
	if d.Wireless == nil || d.Wireless.NL == nil {
		return nil, rpc.Errorf(rpc.CodeUnsupported, "this device has no Wi-Fi (nl80211 unavailable)")
	}
	if d.Ubus == nil || !d.Ubus.Available() {
		return nil, rpc.Errorf(rpc.CodeUnsupported, "ubus is not available; kicking needs hostapd's ubus interface")
	}
	ifi, found, err := d.Wireless.FindClient(ctx, mac, p.Ifname)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, rpc.Errorf(rpc.CodeNotFound, "%s is not associated with this AP", strings.ToLower(mac.String()))
	}
	args := map[string]any{
		"addr":     strings.ToLower(mac.String()),
		"reason":   reason,
		"deauth":   deauth,
		"ban_time": ban,
	}
	err = d.Ubus.Call(ctx, "hostapd."+ifi.Ifname, "del_client", args, nil)
	if errors.Is(err, ubus.ErrNotFound) {
		return nil, rpc.Errorf(rpc.CodeUnsupported, "hostapd has no ubus object for %s", ifi.Ifname)
	}
	if err != nil {
		return nil, rpc.Errorf(rpc.CodeCommandFailed, "%v", err)
	}
	d.Log.Info("kicked client", "mac", args["addr"], "ifname", ifi.Ifname, "ban_ms", ban, "reason", reason)
	return KickResult{MAC: strings.ToLower(mac.String()), Ifname: ifi.Ifname, BanTimeMs: ban}, nil
}

func (d *Deps) locateStart(_ context.Context, raw json.RawMessage) (any, error) {
	p := struct {
		DurationSeconds *int `json:"durationSeconds"`
	}{}
	if err := rpc.Params(raw, &p); err != nil {
		return nil, err
	}
	secs := 30
	if p.DurationSeconds != nil {
		secs = *p.DurationSeconds
	}
	if secs < 1 || secs > 600 {
		return nil, rpc.Errorf(rpc.CodeInvalidParams, "durationSeconds must be 1..600")
	}
	if d.Locator == nil {
		return nil, rpc.Errorf(rpc.CodeUnsupported, "locate is not available")
	}
	n, endsAt, err := d.Locator.Start(time.Duration(secs) * time.Second)
	if errors.Is(err, leds.ErrNoLEDs) {
		return nil, rpc.Errorf(rpc.CodeUnsupported, "this device has no controllable LEDs")
	}
	if err != nil {
		return nil, rpc.Errorf(rpc.CodeCommandFailed, "%v", err)
	}
	d.Log.Info("locate started", "seconds", secs, "leds", n)
	return map[string]any{
		"active":          true,
		"durationSeconds": secs,
		"leds":            n,
		"endsAt":          endsAt.UTC().Format(time.RFC3339),
	}, nil
}

func (d *Deps) locateStop(context.Context, json.RawMessage) (any, error) {
	if d.Locator != nil {
		d.Locator.Stop()
	}
	return map[string]any{"active": false}, nil
}

func (d *Deps) systemReboot(_ context.Context, raw json.RawMessage) (any, error) {
	p := struct {
		DelaySeconds *int `json:"delaySeconds"`
	}{}
	if err := rpc.Params(raw, &p); err != nil {
		return nil, err
	}
	delay := 2
	if p.DelaySeconds != nil {
		delay = *p.DelaySeconds
	}
	if delay < 0 || delay > 60 {
		return nil, rpc.Errorf(rpc.CodeInvalidParams, "delaySeconds must be 0..60")
	}
	if d.Info != nil && !d.Info.IsOpenWrt() && d.Reboot == nil {
		return nil, rpc.Errorf(rpc.CodeUnsupported, "reboot is only supported on OpenWrt")
	}
	reboot := d.Reboot
	if reboot == nil {
		reboot = func() error { return exec.Command("reboot").Run() }
	}
	d.Log.Warn("reboot requested by the controller", "delay_s", delay)
	// Answer first; the response is written long before the delay is up.
	time.AfterFunc(time.Duration(delay)*time.Second+200*time.Millisecond, func() {
		if err := reboot(); err != nil {
			d.Log.Error("reboot failed", "err", err)
		}
	})
	return map[string]any{"scheduled": true, "delaySeconds": delay}, nil
}

func (d *Deps) ping(context.Context, json.RawMessage) (any, error) {
	return map[string]any{"pong": true, "time": d.now().UTC().Format(time.RFC3339Nano)}, nil
}

// Describe renders a short error for logs.
func Describe(err error) string {
	var rpcErr *rpc.Error
	if errors.As(err, &rpcErr) {
		return fmt.Sprintf("%s (%d)", rpcErr.Message, rpcErr.Code)
	}
	return err.Error()
}
