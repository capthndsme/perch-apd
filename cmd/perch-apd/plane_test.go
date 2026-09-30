//go:build !noplane

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/capthndsme/perch-agentkit/openwrt/uci"
)

// `perch-apd config-guard`: nothing pending is fine; a change pending at a
// reboot (no marker on tmpfs) is restored and reported; bad arguments are 2.
func TestConfigGuardCommand(t *testing.T) {
	root := t.TempDir()
	var out, errb bytes.Buffer
	if code := configGuardCommand(nil, &out, &errb, root); code != 0 || !strings.Contains(out.String(), "no pending Wi-Fi change") {
		t.Fatalf("nothing pending: %d %q %q", code, out.String(), errb.String())
	}
	before := []byte("config wifi-device 'radio0'\n\toption channel '1'\n")
	write := func(p string, b []byte) {
		full := filepath.Join(root, p)
		os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("/etc/config/wireless", []byte("config wifi-device 'radio0'\n\toption channel '6'\n"))
	write("/etc/perch-apd/plane/rollback/a4-000000000001/before/wireless", before)
	rec, _ := json.Marshal(map[string]any{"applyId": "a4-000000000001", "kind": "apply",
		"createdAt": time.Now().UTC(), "deadline": time.Now().Add(time.Minute).UTC(), "confirmSeconds": 90,
		"configs": []string{"wireless"}, "hashesBefore": map[string]string{"wireless": uci.FileHash(before)}, "committed": true})
	write("/etc/perch-apd/plane/rollback/pending.json", rec)
	out.Reset()
	if code := configGuardCommand(nil, &out, &errb, root); code != 0 ||
		!strings.Contains(out.String(), "Wi-Fi change a4-000000000001 was pending at the reboot: rolled_back") {
		t.Fatalf("pending: %d %q %q", code, out.String(), errb.String())
	}
	if b, _ := os.ReadFile(filepath.Join(root, "/etc/config/wireless")); !bytes.Equal(b, before) {
		t.Fatalf("not restored: %s", b)
	}
	if code := configGuardCommand([]string{"--now"}, &out, &errb, root); code != 2 {
		t.Fatalf("bad argument: %d", code)
	}
}

// `perch-apd wifi access`: the owner's local step; the controller has no
// way to it.
func TestWifiAccessCommand(t *testing.T) {
	path := filepath.Join(t.TempDir(), "perch-apd")
	os.WriteFile(path, []byte("config agent 'main'\n\toption controller 'http://192.168.1.10:8080'\n"), 0o600)
	var out bytes.Buffer
	oldOut, oldRestart := stdout, restartDaemon
	defer func() { stdout, restartDaemon = oldOut, oldRestart }()
	stdout = &out
	restarts := 0
	restartDaemon = func() error { restarts++; return nil }

	if code := run([]string{"wifi", "--config", path, "access"}); code != 0 || out.String() != "none\n" {
		t.Fatalf("show: %d %q", code, out.String())
	}
	out.Reset()
	if code := run([]string{"wifi", "--config", path, "access", "write"}); code != 0 || restarts != 1 ||
		!strings.Contains(out.String(), "manage this AP's Wi-Fi") || !strings.Contains(out.String(), "not https") {
		t.Fatalf("write: %d %d %q", code, restarts, out.String())
	}
	if b, _ := os.ReadFile(path); !strings.Contains(string(b), "option wifi_config 'write'") {
		t.Fatalf("file:\n%s", b)
	}
	if code := run([]string{"wifi", "--config", path, "access", "admin"}); code != 2 || restarts != 1 {
		t.Fatalf("bad level: %d", code)
	}
}
