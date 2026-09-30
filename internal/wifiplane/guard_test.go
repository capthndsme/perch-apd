package wifiplane

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The daemon installs the guard when the package did not, enables it, and
// never replaces a package's own.
func TestEnsureGuard(t *testing.T) {
	root := t.TempDir()
	if s := GuardStatus(root); s != GuardMissing {
		t.Fatalf("empty root: %s", s)
	}
	status, changed, err := EnsureGuard(root)
	if err != nil || !changed || status != GuardSelfInstalled {
		t.Fatalf("first: %s %v %v", status, changed, err)
	}
	b, _ := os.ReadFile(filepath.Join(root, GuardInit))
	if !bytes.HasPrefix(b, []byte("#!/bin/sh /etc/rc.common\n"+selfInstalledLine+"\n")) || !bytes.Contains(b, []byte("START=15")) {
		t.Fatalf("script:\n%s", b)
	}
	if st, _ := os.Stat(filepath.Join(root, GuardInit)); st.Mode()&0o111 == 0 {
		t.Fatal("not executable")
	}
	if target, err := os.Readlink(filepath.Join(root, GuardLink)); err != nil || target != "../init.d/perch-apd-guard" {
		t.Fatalf("link %q %v", target, err)
	}
	if _, changed, _ := EnsureGuard(root); changed {
		t.Fatal("a second run changed something")
	}
	// Disabled by hand: enabled again.
	os.Remove(filepath.Join(root, GuardLink))
	if s := GuardStatus(root); s != GuardMissing {
		t.Fatalf("without the link: %s", s)
	}
	if s, changed, _ := EnsureGuard(root); !changed || s != GuardSelfInstalled {
		t.Fatalf("re-enabled: %s %v", s, changed)
	}
	// An older self-installed guard is replaced; a package's is kept.
	os.WriteFile(filepath.Join(root, GuardInit), []byte("#!/bin/sh /etc/rc.common\n"+selfInstalledLine+"\nSTART=15\n"), 0o755)
	if _, changed, _ := EnsureGuard(root); !changed {
		t.Fatal("an outdated self-installed guard was kept")
	}
	os.WriteFile(filepath.Join(root, GuardInit), GuardScript, 0o755)
	if s, changed, _ := EnsureGuard(root); changed || s != GuardInstalled {
		t.Fatalf("the package's guard: %s %v", s, changed)
	}
}

// The shell fallback (no daemon that knows the plane) restores the
// snapshot's files, only after a reboot, and only wireless, network and
// the ledger.
func TestGuardScriptFallback(t *testing.T) {
	ash, err := exec.LookPath("busybox")
	if err != nil {
		t.Skip("no busybox to run the init script with")
	}
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("no python3 for a jsonfilter stand-in")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	conf := filepath.Join(dir, "config")
	rb := filepath.Join(dir, "rollback")
	run := filepath.Join(dir, "run")
	for _, d := range []string{bin, conf, filepath.Join(rb, "a4-000000000001", "before"), run} {
		os.MkdirAll(d, 0o755)
	}
	// jsonfilter -i FILE -e EXPR, for the two expressions the script uses.
	os.WriteFile(filepath.Join(bin, "jsonfilter"), []byte(`#!`+py+`
import json, sys
a = sys.argv[1:]
d = json.load(open(a[a.index("-i") + 1]))
e = a[a.index("-e") + 1]
if e == "@.applyId": print(d["applyId"])
elif e == "@.configs[*]": print("\n".join(d["configs"]))
else: sys.exit(1)
`), 0o755)
	os.WriteFile(filepath.Join(conf, "wireless"), []byte("committed\n"), 0o644)
	os.WriteFile(filepath.Join(conf, "dhcp"), []byte("dhcp now\n"), 0o644)
	os.WriteFile(filepath.Join(rb, "a4-000000000001", "before", "wireless"), []byte("before\n"), 0o600)
	os.WriteFile(filepath.Join(rb, "a4-000000000001", "before", "dhcp"), []byte("dhcp before\n"), 0o600)
	pending := `{"applyId":"a4-000000000001","configs":["wireless","dhcp","perch-managed"]}`
	script := filepath.Join(dir, "guard")
	os.WriteFile(script, GuardScript, 0o755)
	sh := func() string {
		// A function: BusyBox runs its own logger applet ahead of PATH.
		cmd := exec.Command(ash, "ash", "-c", `LOG="$5"; logger() { shift 2; echo "$*" >> "$LOG"; }; . "$1"; ROLLBACK="$2"; RUNDIR="$3"; CONFDIR="$4"; guard_fallback`,
			"x", script, rb, run, conf, filepath.Join(dir, "log"))
		cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("script: %v\n%s", err, out)
		}
		return string(out)
	}

	// The daemon still owns the window (its marker exists): nothing.
	os.WriteFile(filepath.Join(rb, "pending.json"), []byte(pending), 0o600)
	os.WriteFile(filepath.Join(run, "apply-a4-000000000001"), []byte("x"), 0o600)
	sh()
	if b, _ := os.ReadFile(filepath.Join(conf, "wireless")); string(b) != "committed\n" {
		t.Fatalf("restored while the daemon owns it: %s", b)
	}
	// After a reboot (no marker): wireless back, dhcp untouched, the record
	// moved aside so nothing restores it twice.
	os.Remove(filepath.Join(run, "apply-a4-000000000001"))
	sh()
	if b, _ := os.ReadFile(filepath.Join(conf, "wireless")); string(b) != "before\n" {
		t.Fatalf("wireless %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(conf, "dhcp")); string(b) != "dhcp now\n" {
		t.Fatalf("dhcp touched: %q", b)
	}
	if _, err := os.Stat(filepath.Join(rb, "pending.json")); !os.IsNotExist(err) {
		t.Fatal("pending.json still there")
	}
	if _, err := os.Stat(filepath.Join(rb, "pending.json.restored-a4-000000000001")); err != nil {
		t.Fatal(err)
	}
	if log, _ := os.ReadFile(filepath.Join(dir, "log")); !strings.Contains(string(log), "restored "+conf+"/wireless") {
		t.Fatalf("log %s", log)
	}
}
