// Package install implements `perch-apd install | uninstall | join`:
// the manual install into /opt/perch-apd (the prebuilt-binary path) and
// the configure-and-join step that also serves opkg/apk package installs.
package install

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/capthndsme/perch-agentkit/link"
	"github.com/capthndsme/perch-apd/internal/agent"
	"github.com/capthndsme/perch-apd/internal/config"
	"github.com/capthndsme/perch-apd/internal/sysinfo"
	"github.com/capthndsme/perch-apd/internal/version"
)

// InitScript is the procd init script, identical to the OpenWrt package's
// files/perch-apd.init (a test keeps them in sync).
//
//go:embed perch-apd.init
var InitScript []byte

// Paths, relative to Env.Root.
const (
	OptDir         = "/opt/perch-apd"
	OptBin         = "/opt/perch-apd/perch-apd"
	PkgBin         = "/usr/bin/perch-apd"
	InitPath       = "/etc/init.d/perch-apd"
	KeepFile       = "/lib/upgrade/keep.d/perch-apd"
	ConfigPath     = config.DefaultPath
	OpenWrtRelease = "/etc/openwrt_release"
	CABundle       = "/etc/ssl/certs/ca-certificates.crt"
	NodeExporter   = "/etc/init.d/prometheus-node-exporter-lua"
	ApkDB          = "/lib/apk/db/installed" // apk-tools (OpenWrt 25.x); opkg before
)

// keepList makes a manual install survive sysupgrade: sysupgrade's config
// backup includes every path listed under /lib/upgrade/keep.d (files and
// symlinks, so the rc.d links that enable the service come back too). The
// list names itself: the new firmware does not ship it, so without that line
// the first upgrade keeps the daemon and the second one drops it. It also
// names the configuration, which base-files keeps with all of /etc/config/,
// so the credentials do not depend on that.
const keepList = `# perch-apd installed by 'perch-apd install': keep it across sysupgrade.
/lib/upgrade/keep.d/perch-apd
/opt/perch-apd/
/etc/init.d/perch-apd
/etc/rc.d/S95perch-apd
/etc/rc.d/K10perch-apd
/etc/config/perch-apd
`

// Env is the environment an install runs in; tests point Root at a temp dir
// and replace Run and HTTP.
type Env struct {
	Root   string
	Stdin  io.Reader
	Stdout io.Writer
	Self   string // path of the running binary
	IsRoot bool
	// Run executes a service action (`/etc/init.d/perch-apd enable`).
	Run func(name string, args ...string) error
	// RunQuiet is Run without output, for steps whose failure is expected
	// (stopping a service that is not running); nil = Run.
	RunQuiet func(name string, args ...string) error
	// HTTP overrides the client built from the configuration (tests).
	HTTP *http.Client
	// Info describes the device for the join (nil = sysinfo on Root).
	Info *sysinfo.Info

	in *bufio.Reader
}

// DefaultEnv is the real system.
func DefaultEnv() *Env {
	self, _ := os.Executable()
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}
	return &Env{
		Root:   "/",
		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		Self:   self,
		IsRoot: os.Geteuid() == 0,
		Run: func(name string, args ...string) error {
			cmd := exec.Command(name, args...)
			cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
			return cmd.Run()
		},
		RunQuiet: func(name string, args ...string) error {
			return exec.Command(name, args...).Run()
		},
	}
}

// restartService stops (quietly: procd complains about a service that is
// not running) and starts, so a new binary or new credentials take effect.
func (e *Env) restartService(initScript string) error {
	quiet := e.RunQuiet
	if quiet == nil {
		quiet = e.Run
	}
	_ = quiet(initScript, "stop")
	return e.Run(initScript, "start")
}

func (e *Env) path(p string) string {
	if e.Root == "" || e.Root == "/" {
		return p
	}
	return filepath.Join(e.Root, p)
}

func (e *Env) exists(p string) bool {
	_, err := os.Stat(e.path(p))
	return err == nil
}

func (e *Env) printf(format string, args ...any) { fmt.Fprintf(e.Stdout, format, args...) }

func (e *Env) prompt(label, def string) (string, error) {
	if e.in == nil {
		e.in = bufio.NewReader(e.Stdin)
	}
	if def != "" {
		e.printf("%s [%s]: ", label, def)
	} else {
		e.printf("%s: ", label)
	}
	line, err := e.in.ReadString('\n')
	if err != nil && (line == "" || !errors.Is(err, io.EOF)) {
		if errors.Is(err, io.EOF) {
			return "", errors.New("no input (pass --controller and --token to run without prompts)")
		}
		return "", err
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return def, nil
	}
	return line, nil
}

// Options for install and join.
type Options struct {
	Controller string
	Token      string
	Yes        bool // never prompt
	Force      bool // install on a system without /etc/openwrt_release
	NoStart    bool // do not enable/start the service
	Rejoin     bool // ask for a token even when already joined
}

// Install installs (or refreshes) the manual install and configures it.
func (e *Env) Install(ctx context.Context, o Options) error {
	if !e.IsRoot {
		return errors.New("run the installer as root")
	}
	if !e.exists(OpenWrtRelease) && !o.Force {
		return errors.New("this does not look like OpenWrt (no /etc/openwrt_release); pass --force to install anyway")
	}
	e.printf("perch-apd %s installer\n", version.Version)

	pkg := e.exists(PkgBin)
	if pkg {
		e.printf("The perch-apd package is installed (%s): configuring it, not copying a second binary.\n", PkgBin)
	} else {
		if err := e.installFiles(); err != nil {
			return err
		}
	}
	return e.configureAndJoin(ctx, o, true)
}

// Join configures the controller and joins, for any kind of install.
func (e *Env) Join(ctx context.Context, o Options) error {
	if !e.IsRoot {
		return errors.New("run as root (the configuration lives in /etc/config)")
	}
	installed := e.exists(InitPath)
	if !installed {
		e.printf("Note: the service is not installed; run 'perch-apd install' to run it at boot.\n")
	}
	o.Rejoin = true
	return e.configureAndJoin(ctx, o, installed)
}

func (e *Env) installFiles() error {
	if err := os.MkdirAll(e.path(OptDir), 0o755); err != nil {
		return err
	}
	target := e.path(OptBin)
	same := false
	if e.Self != "" {
		if a, err := os.Stat(e.Self); err == nil {
			if b, err := os.Stat(target); err == nil && os.SameFile(a, b) {
				same = true
			}
		}
	}
	if !same {
		if e.Self == "" {
			return errors.New("cannot find the running binary to copy")
		}
		if err := copyFile(e.Self, target, 0o755); err != nil {
			return fmt.Errorf("copying the binary to %s: %w", OptBin, err)
		}
		e.printf("Installed %s\n", OptBin)
	}
	if err := writeFileAtomic(e.path(InitPath), InitScript, 0o755); err != nil {
		return fmt.Errorf("writing %s: %w", InitPath, err)
	}
	if err := writeFileAtomic(e.path(KeepFile), []byte(keepList), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", KeepFile, err)
	}
	e.printf("Installed %s (kept across sysupgrade via %s)\n", InitPath, KeepFile)
	return nil
}

func (e *Env) configureAndJoin(ctx context.Context, o Options, manageService bool) error {
	cfgPath := e.path(ConfigPath)
	if !e.exists(ConfigPath) {
		if err := writeFileAtomic(cfgPath, config.DefaultFile, 0o600); err != nil {
			return err
		}
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		// A hand-edited controller that no longer parses: let the admin fix it here.
		e.printf("Warning: %v\n", err)
		cfg = &config.Config{Path: cfgPath}
	}

	controller := o.Controller
	if controller == "" {
		if o.Yes {
			if cfg.Controller == "" {
				return errors.New("--controller is required with --yes")
			}
			controller = cfg.Controller
		} else {
			if controller, err = e.prompt("Controller URL (your Perch dashboard, e.g. https://perch.example.com)", cfg.Controller); err != nil {
				return err
			}
		}
	}
	controller, err = config.NormalizeControllerURL(controller)
	if err != nil {
		return fmt.Errorf("controller URL: %w", err)
	}
	changedController := cfg.Controller != "" && cfg.Controller != controller

	token := strings.TrimSpace(o.Token)
	keepCredentials := cfg.HasCredentials() && !changedController
	if token == "" && (!keepCredentials || o.Rejoin) {
		if o.Yes {
			if !keepCredentials {
				return errors.New("--token is required with --yes (create one in Settings → Wi-Fi sources)")
			}
		} else {
			label := "Join token (Settings → Wi-Fi sources → New join token)"
			if keepCredentials {
				label = "Join token (Enter keeps the current credentials)"
			}
			if token, err = e.prompt(label, ""); err != nil {
				return err
			}
			if token == "" && !keepCredentials {
				return errors.New("a join token is required")
			}
		}
	}

	values := map[string]*string{"controller": &controller}
	if token != "" {
		values["join_token"] = &token
	}
	if changedController || token != "" {
		empty := ""
		values["agent_id"], values["agent_secret"] = &empty, &empty
	}
	if err := config.Update(cfgPath, values); err != nil {
		return fmt.Errorf("writing %s: %w", ConfigPath, err)
	}
	if cfg, err = config.Load(cfgPath); err != nil {
		return err
	}
	e.printf("Saved the controller address to %s\n", ConfigPath)

	if strings.HasPrefix(controller, "https://") && !e.exists(CABundle) && !cfg.TLSInsecure && cfg.CAFile == "" {
		e.printf("Warning: %s is missing; install ca-bundle (opkg install ca-bundle / apk add ca-bundle) or TLS to the controller will fail.\n", CABundle)
	}

	joined := false
	if token != "" {
		client := e.HTTP
		if client == nil {
			if client, err = agent.NewHTTPClient(cfg); err != nil {
				return err
			}
		}
		info := e.Info
		if info == nil {
			info = &sysinfo.Info{Root: e.Root}
		}
		jctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		res, err := agent.Join(jctx, client, controller, agent.BuildJoinRequest(jctx, info, token))
		cancel()
		var se *link.StatusError
		switch {
		case err == nil:
			if err := cfg.SaveCredentials(res.AgentID, res.AgentSecret); err != nil {
				return fmt.Errorf("storing the credentials: %w", err)
			}
			joined = true
			e.printf("Joined as access point #%d %q: %s\n", res.ApID, res.ApName, outcomeText(res.Outcome))
		case errors.As(err, &se) && se.Status >= 400 && se.Status < 500:
			return fmt.Errorf("the controller refused the join: %v", err)
		default:
			e.printf("Could not reach the controller right now (%v).\n", err)
			if hint := agent.DNSHint(err); hint != "" {
				e.printf("Hint: %s\n", hint)
			}
			e.printf("The token is saved; the service will keep trying to join.\n")
		}
	}

	if manageService && !o.NoStart {
		initScript := e.path(InitPath)
		if err := e.Run(initScript, "enable"); err != nil {
			return fmt.Errorf("enabling the service: %w", err)
		}
		if err := e.restartService(initScript); err != nil {
			return fmt.Errorf("starting the service: %w", err)
		}
		e.printf("Service enabled and started.\n")
	}

	e.printf("\nLogs: logread -e perch-apd\n")
	if joined {
		e.printf("The access point shows up in the dashboard under Settings → Wi-Fi sources.\n")
	}
	if e.exists(NodeExporter) {
		e.printf("prometheus-node-exporter-lua is still installed. Once the dashboard shows this AP as connected, it is no longer needed:\n")
		if e.exists(ApkDB) {
			e.printf("  apk del $(apk info | grep '^prometheus-node-exporter-lua')\n")
		} else {
			// The collector sub-packages depend on the base package, and opkg
			// refuses to remove a package others depend on: them first.
			e.printf("  opkg remove $(opkg list-installed | cut -d' ' -f1 | grep '^prometheus-node-exporter-lua-')\n" +
				"  opkg remove --autoremove prometheus-node-exporter-lua\n")
		}
		e.printf("  (keep it only if something other than Perch scrapes :9100)\n")
	}
	return nil
}

func outcomeText(o string) string {
	switch o {
	case "linked":
		return "linked to the access point the dashboard already knew (history kept)"
	case "rejoined":
		return "rejoined (new credentials for the same access point)"
	case "created":
		return "added as a new access point"
	}
	return o
}

// UninstallOptions for Uninstall.
type UninstallOptions struct {
	Purge bool // also remove /etc/config/perch-apd
}

// Uninstall removes a manual install.
func (e *Env) Uninstall(o UninstallOptions) error {
	if !e.IsRoot {
		return errors.New("run the uninstaller as root")
	}
	if e.exists(PkgBin) && !e.exists(OptBin) {
		return errors.New("perch-apd is installed as a package: opkg remove perch-apd (apk del perch-apd on 25.x)")
	}
	if e.exists(InitPath) {
		initScript := e.path(InitPath)
		_ = e.Run(initScript, "stop")
		_ = e.Run(initScript, "disable")
		if data, err := os.ReadFile(initScript); err == nil && bytes.Contains(data, []byte("perch-apd")) && !e.exists(PkgBin) {
			os.Remove(initScript)
		}
	}
	os.Remove(e.path(KeepFile))
	if err := os.RemoveAll(e.path(OptDir)); err != nil {
		return err
	}
	if o.Purge {
		os.Remove(e.path(ConfigPath))
		e.printf("Removed perch-apd and its configuration.\n")
	} else {
		e.printf("Removed perch-apd (configuration kept in %s; --purge removes it).\n", ConfigPath)
	}
	e.printf("Forget the access point in the dashboard (Settings → Wi-Fi sources) to revoke its credentials.\n")
	return nil
}

func copyFile(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
