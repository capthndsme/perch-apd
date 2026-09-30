package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/capthndsme/perch-apd/internal/config"
	"github.com/capthndsme/perch-apd/internal/handlers"
	"github.com/capthndsme/perch-apd/internal/wificaps"
)

// daemonPlane is what the daemon needs of the Wi-Fi config plane
// (*wifiplane.Service; none in a build without it).
type daemonPlane interface {
	handlers.WifiPlane
	// Start resumes or restores a pending apply (before any session) and
	// installs the boot guard with write access.
	Start()
	// Session wires it to the session loop.
	Session(sessions func(ctx context.Context) (uint64, string), send func(method string, params any) bool, reconnect func(reason string))
	Configure(params json.RawMessage)
	EndSession()
	RedialFast() bool
	// Trigger is SIGHUP.
	Trigger()
	// GroupsWrote is the device-groups engine's Wrote hook.
	GroupsWrote(applyID string, hashes map[string]string)
	Run(ctx context.Context)
}

// newProber wires the Wi-Fi facts to the device's nl80211 and ubus.
func newProber(d *device) *wificaps.Prober {
	p := &wificaps.Prober{}
	if d.nl != nil {
		p.NL = d.nl // only a non-nil client: a typed nil would not compare equal to nil
	}
	if d.ubus != nil {
		p.Ubus = d.ubus
	}
	return p
}

// wifiCaps is `perch-apd wifi caps`: the device facts of wifi.capabilities,
// plus the radios carrying the route to the controller. Read-only.
type wifiCaps struct {
	wificaps.Facts
	Management managementCheck `json:"management"`
}

type managementCheck struct {
	Controller string   `json:"controllerAddress,omitempty"`
	Device     string   `json:"device"`
	Radios     []string `json:"radios"`
}

const wifiUsage = `usage: perch-apd wifi <command> [--config PATH]

  caps     what the Wi-Fi hardware and software can do, and what the
           controller may do here (wifi.capabilities, JSON)
  read     [--fp-key-file FILE] [config...]  the Wi-Fi configs as the
           controller reads them (wifi.config.read, JSON): secrets are
           fingerprints, with an empty key unless FILE holds the fleet key
  health   the Wi-Fi as it runs: radios, BSSes, radar checks (wifi.health)
  access   [none|read|write]  show or set what the controller may do with
           this AP's Wi-Fi (option wifi_config), then restart the daemon

caps, read and health only read: safe from /tmp on a live access point.
`

// runWifi runs the `wifi` subcommands; all but access only read (safe from
// /tmp on a live AP).
func runWifi(ctx context.Context, cfgPath string, args []string) int {
	if code, handled := runPlaneWifi(ctx, cfgPath, args); handled {
		return code
	}
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "access":
		return wifiAccess(cfgPath, args[1:])
	case "-h", "-help", "--help", "help":
		fmt.Fprint(stdout, wifiUsage)
		return 0
	case "caps":
		d := newDevice(slog.New(slog.NewTextHandler(io.Discard, nil)))
		defer d.close()
		p := newProber(d)
		out := wifiCaps{Facts: p.Facts(ctx)}
		controller := ""
		if cfg, err := config.Load(cfgPath); err == nil {
			controller = cfg.Controller
		}
		out.Management.Controller, out.Management.Device = routeDevice(controller)
		out.Management.Radios = p.ManagementRadios(ctx, out.Management.Device)
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		enc.Encode(out)
		return 0
	}
	fmt.Fprint(os.Stderr, wifiUsage)
	return 2
}

// restartDaemon restarts the service so a new option takes effect; a
// variable for the tests.
var restartDaemon = func() error {
	const initScript = "/etc/init.d/perch-apd"
	if _, err := os.Stat(initScript); err != nil {
		return nil // not installed as a service: nothing runs
	}
	return exec.Command(initScript, "restart").Run()
}

// wifiAccess is `perch-apd wifi access [none|read|write]`: a local step
// only (the controller can never write /etc/config/perch-apd).
func wifiAccess(cfgPath string, args []string) int {
	fs := flag.NewFlagSet("perch-apd wifi access", flag.ContinueOnError)
	fs.StringVar(&cfgPath, "config", cfgPath, "UCI configuration file")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "wifi access:", err)
		return 1
	}
	if fs.NArg() == 0 {
		fmt.Fprintln(stdout, cfg.WifiConfig)
		return 0
	}
	level := fs.Arg(0)
	switch level {
	case "none", "read", "write":
	default:
		fmt.Fprintf(os.Stderr, "wifi access: %q is not none, read or write\n", level)
		return 2
	}
	if err := config.Update(cfgPath, map[string]*string{"wifi_config": &level}); err != nil {
		fmt.Fprintln(os.Stderr, "wifi access:", err)
		return 1
	}
	switch level {
	case "none":
		fmt.Fprintln(stdout, "Wi-Fi management off: the controller can neither read nor change this AP's Wi-Fi.")
	case "read":
		fmt.Fprintln(stdout, "The controller may read this AP's Wi-Fi configuration (passphrases only as fingerprints), not change it.")
	case "write":
		fmt.Fprintln(stdout, "The controller may manage this AP's Wi-Fi (wireless, and the VLANs it needs in network); every change is rolled back unless the AP stays reachable and healthy.")
		if !cfg.TransportVerified() {
			fmt.Fprintln(stdout, "Note: the controller is not https with a verified certificate, so its writes are refused until that changes (or wifi_config_insecure '1' and a pairing).")
		}
	}
	if err := restartDaemon(); err != nil {
		fmt.Fprintln(os.Stderr, "wifi access: restarting perch-apd:", err)
		return 1
	}
	return 0
}

// routeDevice is the device the controller is reached over: the interface
// holding the local address a UDP "connection" to it picks (no packet is
// sent), else the IPv4 default route's device.
func routeDevice(controller string) (addr, dev string) {
	if u, err := url.Parse(controller); err == nil && u.Hostname() != "" {
		port := u.Port()
		if port == "" {
			port = "443"
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		var d net.Dialer
		if c, err := d.DialContext(ctx, "udp4", net.JoinHostPort(u.Hostname(), port)); err == nil {
			local := c.LocalAddr().(*net.UDPAddr).IP
			addr = c.RemoteAddr().(*net.UDPAddr).IP.String()
			c.Close()
			if ifs, err := net.Interfaces(); err == nil {
				for _, ifi := range ifs {
					addrs, _ := ifi.Addrs()
					for _, a := range addrs {
						if n, ok := a.(*net.IPNet); ok && n.IP.Equal(local) {
							return addr, ifi.Name
						}
					}
				}
			}
		}
	}
	f, err := os.Open("/proc/net/route")
	if err != nil {
		return addr, ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fs := strings.Fields(sc.Text())
		if len(fs) >= 8 && fs[1] == "00000000" && fs[7] == "00000000" {
			return addr, fs[0]
		}
	}
	return addr, ""
}
