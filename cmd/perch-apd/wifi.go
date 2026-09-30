package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/capthndsme/perch-apd/internal/config"
	"github.com/capthndsme/perch-apd/internal/wificaps"
)

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

// runWifi runs the read-only `wifi` subcommands (safe from /tmp on a live AP).
func runWifi(ctx context.Context, cfgPath string, args []string) int {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
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
	fmt.Fprintf(os.Stderr, "usage: perch-apd wifi caps\n")
	return 2
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
