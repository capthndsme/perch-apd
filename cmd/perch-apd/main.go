// Command perch-apd is the Perch AP Daemon: it pushes the access point's
// metrics (a prometheus-node-exporter-lua replacement) to the Perch Network
// Controller over a WebSocket it opens itself, and serves the controller's
// commands on the same connection. It listens on no port.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/capthndsme/perch-agentkit/hoststat"
	"github.com/capthndsme/perch-agentkit/openwrt/ubus"
	"github.com/capthndsme/perch-agentkit/rpc"
	"github.com/capthndsme/perch-apd/internal/agent"
	"github.com/capthndsme/perch-apd/internal/applylock"
	"github.com/capthndsme/perch-apd/internal/collect"
	"github.com/capthndsme/perch-apd/internal/config"
	"github.com/capthndsme/perch-apd/internal/groups"
	"github.com/capthndsme/perch-apd/internal/handlers"
	"github.com/capthndsme/perch-apd/internal/install"
	"github.com/capthndsme/perch-apd/internal/leds"
	"github.com/capthndsme/perch-apd/internal/nl80211"
	"github.com/capthndsme/perch-apd/internal/sysinfo"
	"github.com/capthndsme/perch-apd/internal/version"
	"github.com/capthndsme/perch-apd/internal/wireless"
	"os/exec"
	"regexp"
)

const usage = `perch-apd %s: Perch AP Daemon

Usage:
  perch-apd install     [--controller URL] [--token TOKEN] [--yes] [--no-start] [--force]
                        copy to /opt/perch-apd, set up the service, join the controller
  perch-apd join        [--controller URL] [--token TOKEN] [--yes]
                        (re)join a controller, e.g. after "Forget agent" in the dashboard
  perch-apd uninstall   [--purge]
                        remove the /opt install and its service (--purge: also the config)
  perch-apd run         run the daemon (what the init script starts)
  perch-apd metrics     print the Prometheus metrics once
  perch-apd clients     print the associated Wi-Fi clients (JSON)
  perch-apd info        print what the controller sees in system.info (JSON)
  perch-apd ports       print the Ethernet ports and their link state (JSON)
  perch-apd wifi caps | read | health
                        print the Wi-Fi's capabilities, configuration (secrets as
                        fingerprints) or health as the controller sees them (JSON, read-only)
  perch-apd wifi access [none|read|write]
                        show or set what the controller may do with this AP's Wi-Fi
  perch-apd config-guard
                        boot guard of Wi-Fi changes (init script perch-apd-guard)
  perch-apd version

--install and --uninstall work too. Every command takes --config PATH
(default /etc/config/perch-apd).
`

func main() {
	os.Exit(run(os.Args[1:]))
}

// Where the commands write, and the root `ports` reads (tests swap them).
var (
	stdout io.Writer = os.Stdout
	sysFS            = hoststat.FS{}
)

func run(args []string) int {
	cmd := "run"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	} else if len(args) > 0 {
		switch args[0] {
		case "--install", "-install":
			cmd, args = "install", args[1:]
		case "--uninstall", "-uninstall":
			cmd, args = "uninstall", args[1:]
		case "--version", "-version", "-v":
			cmd, args = "version", args[1:]
		case "--help", "-help", "-h":
			cmd, args = "help", args[1:]
		}
	}

	fs := flag.NewFlagSet("perch-apd "+cmd, flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprintf(os.Stderr, usage, version.Version) }
	cfgPath := fs.String("config", config.DefaultPath, "UCI configuration file")
	controller := fs.String("controller", "", "controller URL (install, join)")
	token := fs.String("token", "", "join token (install, join)")
	yes := fs.Bool("yes", false, "never prompt (install, join)")
	fs.BoolVar(yes, "y", false, "never prompt")
	noStart := fs.Bool("no-start", false, "do not enable/start the service (install)")
	force := fs.Bool("force", false, "install even without /etc/openwrt_release")
	purge := fs.Bool("purge", false, "also remove the configuration (uninstall)")
	collectors := fs.String("collect", "", "comma-separated collectors (metrics)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch cmd {
	case "run":
		return runDaemon(ctx, *cfgPath)
	case "install":
		env := install.DefaultEnv()
		if err := env.Install(ctx, install.Options{Controller: *controller, Token: *token, Yes: *yes, Force: *force, NoStart: *noStart}); err != nil {
			fmt.Fprintln(os.Stderr, "install:", err)
			return 1
		}
	case "join":
		env := install.DefaultEnv()
		if err := env.Join(ctx, install.Options{Controller: *controller, Token: *token, Yes: *yes}); err != nil {
			fmt.Fprintln(os.Stderr, "join:", err)
			return 1
		}
	case "uninstall":
		if err := install.DefaultEnv().Uninstall(install.UninstallOptions{Purge: *purge}); err != nil {
			fmt.Fprintln(os.Stderr, "uninstall:", err)
			return 1
		}
	case "metrics":
		d := newDevice(slog.New(slog.NewTextHandler(io.Discard, nil)))
		defer d.close()
		var names []string
		if *collectors != "" {
			names = strings.Split(*collectors, ",")
		}
		stdout.Write(d.registry.Gather(ctx, names))
	case "clients", "info":
		d := newDevice(slog.New(slog.NewTextHandler(io.Discard, nil)))
		defer d.close()
		var out any
		if cmd == "info" {
			// As the daemon answers: no "ports" when the configuration turns
			// them off (a missing or broken file means the defaults).
			if cfg, err := config.Load(*cfgPath); err == nil {
				if !cfg.Ports {
					d.deps.Ports = nil
				}
				if w := cliWifiPlane(cfg, d); w != nil {
					d.deps.Wifi = w
				}
			}
			out = d.deps.SystemInfo(ctx)
		} else {
			clients, err := d.wireless.Clients(ctx, "")
			if err != nil {
				fmt.Fprintln(os.Stderr, "clients:", err)
				return 1
			}
			out = clients
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		enc.Encode(out)
	case "ports":
		// Straight from /sys/class/net and /etc/board.json: no configuration,
		// no ubus or nl80211, no controller. Safe to run from /tmp on a live AP.
		ports := sysFS.Ports(hoststat.PortOptions{Counters: true})
		if ports == nil {
			fmt.Fprintln(os.Stderr, "ports: cannot list /sys/class/net")
			return 1
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		enc.Encode(ports)
	case "wifi":
		return runWifi(ctx, *cfgPath, fs.Args())
	case "config-guard":
		return configGuardCommand(fs.Args(), stdout, os.Stderr, "")
	case "version":
		fmt.Fprintf(stdout, "perch-apd %s (%s)\n", version.Version, version.Arch())
	case "help":
		fmt.Fprintf(stdout, usage, version.Version)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		fmt.Fprintf(os.Stderr, usage, version.Version)
		return 2
	}
	return 0
}

// oneProc reports whether the daemon runs on a single P: on 32-bit CPUs,
// unless GOMAXPROCS is set. There Go emulates 64-bit atomics with locks
// (MIPS32, ARMv5), and with a P per hardware thread the scheduler and the
// idle GC workers spin on them: on an MT7621 (two cores, four threads) one P
// cut the CPU per push by a third. The daemon's work is sequential anyway.
func oneProc(intSize int, getenv func(string) string) bool {
	return intSize == 32 && getenv("GOMAXPROCS") == ""
}

// device bundles the collaborators every command that touches the hardware needs.
type device struct {
	nl       *nl80211.Client
	ubus     *ubus.Client
	info     *sysinfo.Info
	wireless *wireless.Source
	locator  *leds.Locator
	registry *collect.Registry
	ports    *hoststat.PortReader
	deps     *handlers.Deps
	// applyLock is the AP's one write lock: device groups, the Wi-Fi config
	// plane and agent updates each hold it through their confirm window.
	applyLock *applylock.Lock
	// groupsWrote tells the Wi-Fi config plane what the device groups wrote
	// (set before the engine starts).
	groupsWrote func(applyID string, hashes map[string]string)
}

// newDevice opens what the hardware commands use (ubus, nl80211, LEDs). A
// variable so the tests can see which commands do.
var newDevice = func(log *slog.Logger) *device {
	d := &device{locator: leds.New(), applyLock: applylock.New()}
	d.info = &sysinfo.Info{}
	d.wireless = &wireless.Source{}
	if ub := ubus.New(); ub.Available() {
		d.ubus = ub
		d.info.Ubus = ub
		d.wireless.Ubus = ub
	}
	if nl, err := nl80211.Dial(); err == nil {
		d.nl = nl
		d.wireless.NL = nl
	} else {
		log.Info("no nl80211: Wi-Fi metrics and client commands are off", "err", err)
	}
	fs := collect.FS{Root: "/"}
	d.registry = collect.NewRegistry(log,
		collect.OpenWrt{Info: d.info},
		collect.Uname{},
		collect.Time{},
		collect.Stat{FS: fs},
		collect.Loadavg{FS: fs},
		collect.Meminfo{FS: fs},
		collect.Netdev{FS: fs},
		collect.Netclass{FS: fs},
		collect.Conntrack{FS: fs},
		collect.Filefd{FS: fs},
		collect.Entropy{FS: fs},
		collect.Wifi{Src: d.wireless},
		collect.WifiStations{Src: d.wireless},
	)
	d.ports = &hoststat.PortReader{FS: fs, Options: hoststat.PortOptions{Counters: true}}
	d.deps = &handlers.Deps{
		Log:      log,
		Wireless: d.wireless,
		Locator:  d.locator,
		Info:     d.info,
		Ports:    d.ports,
	}
	if d.ubus != nil {
		d.deps.Ubus = d.ubus // only a non-nil client: a typed nil would not compare equal to nil
	}
	return d
}

// newGroupsEngine wires the device groups engine to the real files, the
// reload commands and nl80211's AP_VLAN interfaces.
func newGroupsEngine(d *device, log *slog.Logger) (*groups.Engine, error) {
	return groups.New(groups.Options{
		StateDir:     "/etc/perch-apd/groups",
		Lock:         d.applyLock,
		WirelessPath: "/etc/config/wireless",
		NetworkPath:  "/etc/config/network",
		FS:           groups.FS{Root: "/"},
		Run: func(ctx context.Context, name string, args ...string) error {
			out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
			if err != nil {
				return fmt.Errorf("%s: %v: %s", name, err, strings.TrimSpace(string(out)))
			}
			return nil
		},
		Stations: func() ([]groups.StationSeen, error) { return groupStations(d.nl) },
		Wrote: func(applyID string, hashes map[string]string) {
			if d.groupsWrote != nil {
				d.groupsWrote(applyID, hashes)
			}
		},
		Log: log,
	})
}

var groupVLANName = regexp.MustCompile(`-g([0-9]{1,4})$`)

// groupStations lists the stations hostapd moved to group VLANs (AP_VLAN
// interfaces named <ifname>-g<vid>).
func groupStations(nl *nl80211.Client) ([]groups.StationSeen, error) {
	out := []groups.StationSeen{}
	if nl == nil {
		return out, nil
	}
	ifaces, err := nl.Interfaces()
	if err != nil {
		return nil, err
	}
	for _, ifi := range ifaces {
		if ifi.Type != nl80211.IfTypeAPVLAN {
			continue
		}
		m := groupVLANName.FindStringSubmatch(ifi.Name)
		if m == nil {
			continue
		}
		vid, _ := strconv.Atoi(m[1])
		stas, err := nl.Stations(ifi.Index)
		if err != nil {
			continue
		}
		for _, s := range stas {
			out = append(out, groups.StationSeen{MAC: s.MAC.String(), VID: vid, Ifname: ifi.Name})
		}
	}
	return out, nil
}

func (d *device) close() {
	if d.nl != nil {
		d.nl.Close()
	}
}

// portNames is the log form of a port list.
func portNames(ports []hoststat.Port) string {
	switch {
	case ports == nil:
		return "unreadable"
	case len(ports) == 0:
		return "none"
	}
	names := make([]string, len(ports))
	for i, p := range ports {
		names[i] = p.Name
	}
	return strings.Join(names, ",")
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: lvl,
		// logread stamps every line already.
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey && len(groups) == 0 {
				return slog.Attr{}
			}
			return a
		},
	}))
}

func runDaemon(ctx context.Context, cfgPath string) int {
	// SIGHUP is procd's reload trigger on wireless and network (the init
	// script's reload_service): the Wi-Fi plane re-reads at once. Caught from
	// the start: by default it would end the daemon.
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)

	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "perch-apd:", err)
		return 1
	}
	log := newLogger(cfg.LogLevel)
	if !cfg.Enabled {
		log.Info("disabled in the configuration (option enabled '0')")
		return 0
	}
	if os.Getenv("GOMEMLIMIT") == "" {
		// Soft limit: small routers have 64-128 MB. The daemon itself needs ~10 MB.
		debug.SetMemoryLimit(32 << 20)
	}
	if oneProc(strconv.IntSize, os.Getenv) {
		runtime.GOMAXPROCS(1)
	}
	log.Info("starting", "version", version.Version, "arch", version.Arch(), "config", cfgPath,
		"gomaxprocs", runtime.GOMAXPROCS(0))

	d := newDevice(log)
	defer d.close()
	if !cfg.Ports {
		d.deps.Ports = nil // no "ports" capability either
	}
	if restored, err := d.locator.RecoverStale(); restored {
		log.Info("restored LEDs left blinking by a previous run")
	} else if err != nil {
		log.Warn("restoring LEDs from a previous run", "err", err)
	}
	defer d.locator.Stop()

	if cfg.WifiGroups {
		eng, err := newGroupsEngine(d, log)
		if err != nil {
			log.Error("device groups are off: the engine did not start", "err", err)
		} else {
			d.deps.Groups = eng
			if !strings.HasPrefix(cfg.Controller, "https://") && !cfg.WifiGroupsInsecure {
				d.deps.GroupsRefusal = "device groups over a plain http:// controller need option wifi_groups_insecure '1'"
				log.Warn(d.deps.GroupsRefusal)
			} else {
				log.Info("device groups on: the controller may put group keys and VLANs on this AP")
			}
		}
	}

	// The Wi-Fi config plane, then both writers resume what a restart
	// interrupted (a pending window takes the write lock again).
	wp := newWifiPlane(cfg, d, log)
	if wp != nil {
		d.deps.Wifi = wp
		d.groupsWrote = wp.GroupsWrote
	}
	if d.deps.Groups != nil {
		d.deps.Groups.Start(ctx)
	}
	if wp != nil {
		wp.Start()
	}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-hup:
				if wp != nil {
					wp.Trigger()
				}
			}
		}
	}()

	disp := rpc.NewDispatcher()
	handlers.Register(disp, d.deps)

	if cfg.Controller == "" {
		log.Warn("no controller configured; run: perch-apd join --controller <url> --token <token>")
		// Stay up quietly instead of exiting: procd would respawn us in a loop.
		<-ctx.Done()
		return 0
	}

	var ports agent.PortSource // a nil interface when off, not a typed nil
	if cfg.Ports {
		ports = d.ports
		log.Info("reporting the Ethernet ports", "ports", portNames(d.ports.Read()))
	} else {
		log.Info("not reporting the Ethernet ports (option ports '0')")
	}
	aopts := agent.Options{Config: cfg, Log: log, Dispatcher: disp, Info: d.info, Metrics: d.registry, Ports: ports}
	if wp != nil {
		aopts.OnConfigure, aopts.OnSessionEnd, aopts.RedialFast = wp.Configure, wp.EndSession, wp.RedialFast
	}
	ag, err := agent.New(aopts)
	if err != nil {
		log.Error("cannot start the controller session", "err", err)
		return 1
	}
	if wp != nil {
		wp.Session(ag.SessionRef, ag.Send, ag.Reconnect)
		go wp.Run(ctx)
	}
	d.locator.OnEnd = func(reason string) {
		ag.Notify("locate.ended", map[string]string{"reason": reason})
	}
	ag.Run(ctx)
	log.Info("stopping")
	// Give the close frame a moment to leave.
	time.Sleep(100 * time.Millisecond)
	return 0
}
