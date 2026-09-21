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
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/capthndsme/perch-agentkit/rpc"
	"github.com/capthndsme/perch-apd/internal/agent"
	"github.com/capthndsme/perch-apd/internal/collect"
	"github.com/capthndsme/perch-apd/internal/config"
	"github.com/capthndsme/perch-apd/internal/handlers"
	"github.com/capthndsme/perch-apd/internal/install"
	"github.com/capthndsme/perch-apd/internal/leds"
	"github.com/capthndsme/perch-apd/internal/nl80211"
	"github.com/capthndsme/perch-apd/internal/sysinfo"
	"github.com/capthndsme/perch-apd/internal/ubus"
	"github.com/capthndsme/perch-apd/internal/version"
	"github.com/capthndsme/perch-apd/internal/wireless"
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
  perch-apd version

--install and --uninstall work too. Every command takes --config PATH
(default /etc/config/perch-apd).
`

func main() {
	os.Exit(run(os.Args[1:]))
}

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
		os.Stdout.Write(d.registry.Gather(ctx, names))
	case "clients", "info":
		d := newDevice(slog.New(slog.NewTextHandler(io.Discard, nil)))
		defer d.close()
		var out any
		if cmd == "info" {
			out = d.deps.SystemInfo(ctx)
		} else {
			clients, err := d.wireless.Clients(ctx, "")
			if err != nil {
				fmt.Fprintln(os.Stderr, "clients:", err)
				return 1
			}
			out = clients
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(out)
	case "version":
		fmt.Printf("perch-apd %s (%s)\n", version.Version, version.Arch())
	case "help":
		fmt.Printf(usage, version.Version)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		fmt.Fprintf(os.Stderr, usage, version.Version)
		return 2
	}
	return 0
}

// device bundles the collaborators every command that touches the hardware needs.
type device struct {
	nl       *nl80211.Client
	ubus     *ubus.Client
	info     *sysinfo.Info
	wireless *wireless.Source
	locator  *leds.Locator
	registry *collect.Registry
	deps     *handlers.Deps
}

func newDevice(log *slog.Logger) *device {
	d := &device{locator: leds.New()}
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
	d.deps = &handlers.Deps{
		Log:      log,
		Wireless: d.wireless,
		Locator:  d.locator,
		Info:     d.info,
	}
	if d.ubus != nil {
		d.deps.Ubus = d.ubus // only a non-nil client: a typed nil would not compare equal to nil
	}
	return d
}

func (d *device) close() {
	if d.nl != nil {
		d.nl.Close()
	}
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
	log.Info("starting", "version", version.Version, "arch", version.Arch(), "config", cfgPath)

	d := newDevice(log)
	defer d.close()
	if restored, err := d.locator.RecoverStale(); restored {
		log.Info("restored LEDs left blinking by a previous run")
	} else if err != nil {
		log.Warn("restoring LEDs from a previous run", "err", err)
	}
	defer d.locator.Stop()

	disp := rpc.NewDispatcher()
	handlers.Register(disp, d.deps)

	if cfg.Controller == "" {
		log.Warn("no controller configured; run: perch-apd join --controller <url> --token <token>")
		// Stay up quietly instead of exiting: procd would respawn us in a loop.
		<-ctx.Done()
		return 0
	}

	ag, err := agent.New(agent.Options{Config: cfg, Log: log, Dispatcher: disp, Info: d.info, Metrics: d.registry})
	if err != nil {
		log.Error("cannot start the controller session", "err", err)
		return 1
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
