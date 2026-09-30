package wifiplane

import (
	"bytes"
	_ "embed"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/capthndsme/perch-agentkit/openwrt/plane"
	"github.com/capthndsme/perch-agentkit/openwrt/uci"
)

// The boot guard (Wi-Fi design protocol.md 3.4): /etc/init.d/perch-apd-guard
// runs `perch-apd config-guard` at START=15, before netifd reads the
// configs (network is START=20; the daemon itself starts at 95). When the AP
// rebooted inside a confirm window, it restores the snapshot without a
// reload; the daemon reports the outcome (reason reboot) once connected.
// The package ships the script. An AP that runs a swapped binary over an
// older package has none: with wifi_config 'write' the daemon writes it
// itself at start (self_installed), and applies are refused while it is
// missing (guard_missing).

// GuardScript is the package's files/perch-apd-guard.init (a test keeps
// the two identical).
//
//go:embed perch-apd-guard.init
var GuardScript []byte

// Where the guard lives, and its states in wifi.capabilities.
const (
	GuardInit = "/etc/init.d/perch-apd-guard"
	// GuardLink enables it (what `/etc/init.d/perch-apd-guard enable` makes).
	GuardLink          = "/etc/rc.d/S15perch-apd-guard"
	GuardInstalled     = "installed"
	GuardSelfInstalled = "self_installed"
	GuardMissing       = "missing"
)

// selfInstalledLine marks a guard the daemon wrote.
const selfInstalledLine = "# Installed by the perch-apd daemon: the package on this device did not ship it."

// SelfInstalledScript is what the daemon writes: the package's script
// with a line saying who put it there.
func SelfInstalledScript() []byte {
	i := bytes.IndexByte(GuardScript, '\n')
	out := make([]byte, 0, len(GuardScript)+len(selfInstalledLine)+1)
	out = append(out, GuardScript[:i+1]...)
	out = append(out, selfInstalledLine+"\n"...)
	return append(out, GuardScript[i+1:]...)
}

// GuardStatus reports the boot guard: installed (by the package),
// self_installed (by the daemon) or missing (no executable script, or not
// enabled).
func GuardStatus(root string) string {
	path := rooted(root, GuardInit)
	st, err := os.Stat(path)
	if err != nil || st.Mode()&0o111 == 0 {
		return GuardMissing
	}
	if _, err := os.Lstat(rooted(root, GuardLink)); err != nil {
		return GuardMissing
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return GuardMissing
	}
	if bytes.Contains(b, []byte(selfInstalledLine)) {
		return GuardSelfInstalled
	}
	return GuardInstalled
}

// EnsureGuard puts the guard in place: the script when there is none (or
// a self-installed one from an older daemon), executable, and enabled. It
// returns the status afterwards and whether anything changed.
func EnsureGuard(root string) (status string, changed bool, err error) {
	path := rooted(root, GuardInit)
	want := SelfInstalledScript()
	b, rerr := os.ReadFile(path)
	switch {
	case errors.Is(rerr, os.ErrNotExist),
		rerr == nil && bytes.Contains(b, []byte(selfInstalledLine)) && !bytes.Equal(b, want):
		if err := writeAtomic(path, want, 0o755); err != nil {
			return GuardMissing, false, err
		}
		changed = true
	case rerr != nil:
		return GuardMissing, false, rerr
	default:
		if st, err := os.Stat(path); err == nil && st.Mode()&0o111 == 0 {
			if err := os.Chmod(path, 0o755); err != nil {
				return GuardMissing, false, err
			}
			changed = true
		}
	}
	link := rooted(root, GuardLink)
	if _, err := os.Lstat(link); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
			return GuardMissing, changed, err
		}
		if err := os.Symlink("../init.d/perch-apd-guard", link); err != nil {
			return GuardMissing, changed, err
		}
		changed = true
	}
	return GuardStatus(root), changed, nil
}

func writeAtomic(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".perch-tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
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
	if err := os.Chmod(tmp, perm); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// GuardOptions are the plane's options the guard needs (no daemon, no
// configuration: the state directories are fixed).
func GuardOptions(root string) plane.Options {
	return plane.Options{Name: "perch-apd", Prefix: Prefix, StateDir: StateDir, RunDir: RunDir, Root: root,
		Redactor: uci.Redactor{Unbound: true}}
}

// RunGuard is `perch-apd config-guard`: nil when there was nothing to do.
func RunGuard(root string, now time.Time) (*plane.Result, error) {
	return plane.Guard(GuardOptions(root), now)
}
