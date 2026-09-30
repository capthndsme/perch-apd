//go:build !noplane

package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/capthndsme/perch-agentkit/openwrt/plane"
	"github.com/capthndsme/perch-apd/internal/config"
	"github.com/capthndsme/perch-apd/internal/handlers"
	"github.com/capthndsme/perch-apd/internal/wifiplane"
)

// The Wi-Fi config plane (internal/wifiplane). `go build -tags noplane`
// leaves it out (plane_noplane.go): `make size` compares the two builds.

// planeOptions maps the daemon's settings onto the plane's.
func planeOptions(cfg *config.Config, d *device, log *slog.Logger) wifiplane.Options {
	o := wifiplane.Options{
		Access: cfg.WifiConfig, Allow: cfg.WifiConfigAllow, AllowInsecure: cfg.WifiConfigInsecure,
		ConfirmMax: cfg.WifiConfigConfirmMax, TransportOK: cfg.TransportVerified(), ServerURL: cfg.Controller,
		Lock: d.applyLock, Prober: newProber(d), Ubus: d.ubus, Log: log,
	}
	if d.deps.Groups != nil {
		o.Groups = d.deps.Groups // a nil *Engine would not compare equal to nil
		o.GroupsEnabled = d.deps.GroupsRefusal == ""
	}
	return o
}

// newWifiPlane builds the daemon's plane; nil (logged) when it cannot be.
func newWifiPlane(cfg *config.Config, d *device, log *slog.Logger) daemonPlane {
	for _, name := range cfg.WifiConfigIgnored {
		log.Warn("wifi_config_allow: only wireless and network can be managed; ignored", "config", name)
	}
	svc, err := wifiplane.New(planeOptions(cfg, d, log))
	if err != nil {
		log.Error("the Wi-Fi config plane did not start", "err", err)
		return nil
	}
	return svc
}

// cliWifiPlane is the plane `perch-apd info` shows (as the daemon would).
func cliWifiPlane(cfg *config.Config, d *device) handlers.WifiPlane {
	svc, err := wifiplane.New(planeOptions(cfg, d, slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		return nil
	}
	return svc
}

const configGuardUsage = `usage: perch-apd config-guard

The Wi-Fi config plane's boot guard (init script perch-apd-guard, before
the network starts): when a Wi-Fi change from the controller was waiting for
its confirm and the access point rebooted, put wireless and network back
from the snapshot in /etc/perch-apd/plane/rollback. Nothing is reloaded (the
network starts afterwards); the daemon reports the outcome to the
controller. Does nothing when no change is pending, or when the daemon owns
it (no reboot). Exits 0 unless it cannot read its state.
`

// configGuardCommand is `perch-apd config-guard`.
func configGuardCommand(args []string, out, errw io.Writer, root string) int {
	for _, a := range args {
		switch a {
		case "-h", "-help", "--help", "help":
			fmt.Fprint(out, configGuardUsage)
			return 0
		}
		fmt.Fprintf(errw, "unexpected argument %q\n\n%s", a, configGuardUsage)
		return 2
	}
	pending, _ := plane.PendingApply(wifiplane.GuardOptions(root))
	res, err := wifiplane.RunGuard(root, time.Now())
	if err != nil {
		fmt.Fprintf(errw, "perch-apd config-guard: %v\n", err)
		return 1
	}
	if res == nil {
		if pending != nil {
			// Its marker is still there: no reboot, the daemon keeps the window.
			fmt.Fprintf(out, "perch-apd config-guard: Wi-Fi change %s waits for its confirm and the device did not reboot; the daemon keeps it\n", pending.ApplyID)
			return 0
		}
		fmt.Fprintln(out, "perch-apd config-guard: no pending Wi-Fi change")
		return 0
	}
	fmt.Fprintf(out, "perch-apd config-guard: Wi-Fi change %s was pending at the reboot: %s", res.ApplyID, res.Outcome)
	if res.Detail != "" {
		fmt.Fprintf(out, " (%s)", res.Detail)
	}
	fmt.Fprintln(out)
	return 0
}

// runPlaneWifi runs the read-only `wifi` subcommands that need the plane:
// caps (wifi.capabilities), read (wifi.config.read) and health
// (wifi.health). handled is false for anything else.
func runPlaneWifi(ctx context.Context, cfgPath string, args []string) (code int, handled bool) {
	if len(args) == 0 {
		return 0, false
	}
	sub := args[0]
	switch sub {
	case "caps", "read", "health":
	default:
		return 0, false
	}
	fs := flag.NewFlagSet("perch-apd wifi "+sub, flag.ContinueOnError)
	cfgFlag := fs.String("config", cfgPath, "UCI configuration file")
	keyFile := fs.String("fp-key-file", "", "file with the 64-hex fleet fingerprint key (read)")
	if err := fs.Parse(args[1:]); err != nil {
		return 2, true
	}
	cfg, err := config.Load(*cfgFlag)
	if err != nil {
		// A missing or broken file: the defaults (access none).
		cfg, _ = config.Load(os.DevNull)
	}
	d := newDevice(slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer d.close()
	o := planeOptions(cfg, d, slog.New(slog.NewTextHandler(io.Discard, nil)))
	o.Groups = nil // the engine is the daemon's; its sections go by their names here
	if sub == "read" && o.Access == plane.AccessNone {
		// Local root reads its own files: the owner's opt-in is about the
		// controller.
		o.Access = plane.AccessRead
	}
	svc, err := wifiplane.New(o)
	if err != nil {
		fmt.Fprintln(os.Stderr, "wifi:", err)
		return 1, true
	}
	var out any
	switch sub {
	case "caps":
		out = svc.Capabilities(ctx, "")
	case "health":
		out = svc.HealthNow(ctx)
	case "read":
		c := plane.Configure{Mode: plane.ModeObserve}
		if *keyFile != "" {
			b, err := os.ReadFile(*keyFile)
			if err != nil {
				fmt.Fprintln(os.Stderr, "wifi read:", err)
				return 1, true
			}
			key := strings.TrimSpace(string(b))
			if k, err := hex.DecodeString(key); err != nil || len(k) != 32 {
				fmt.Fprintln(os.Stderr, "wifi read: --fp-key-file must hold 64 hex digits")
				return 2, true
			}
			c.FingerprintKey = key
		}
		svc.Plane().Configure(c)
		res, err := svc.Read(fs.Args())
		if err != nil {
			fmt.Fprintln(os.Stderr, "wifi read:", err)
			return 1, true
		}
		out = res
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		return 1, true
	}
	return 0, true
}
