package leds

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fakeLED(t *testing.T, root, name string, files map[string]string, readOnly ...string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for f, v := range files {
		mode := os.FileMode(0o644)
		for _, ro := range readOnly {
			if ro == f {
				mode = 0o444
			}
		}
		if err := os.WriteFile(filepath.Join(dir, f), []byte(v+"\n"), mode); err != nil {
			t.Fatal(err)
		}
	}
}

func read(t *testing.T, root, name, file string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, name, file))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

func TestStartBlinksAndTimeoutRestores(t *testing.T) {
	root := t.TempDir()
	fakeLED(t, root, "green:lan", map[string]string{
		"trigger":     "none timer [netdev] default-on",
		"brightness":  "1",
		"device_name": "lan1",
		"link":        "1",
		"offloaded":   "0",
		"delay_on":    "",
		"delay_off":   "",
	}, "offloaded")
	fakeLED(t, root, "green:power", map[string]string{
		"trigger":    "[none] timer default-on",
		"brightness": "1",
		"delay_on":   "",
		"delay_off":  "",
	})
	state := filepath.Join(t.TempDir(), "run", "locate.json")
	ended := make(chan string, 1)
	l := &Locator{Dir: root, StateFile: state, OnEnd: func(r string) { ended <- r }}

	n, endsAt, err := l.Start(80 * time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 || time.Until(endsAt) <= 0 {
		t.Fatalf("n=%d endsAt=%v", n, endsAt)
	}
	for _, led := range []string{"green:lan", "green:power"} {
		if read(t, root, led, "trigger") != "timer" || read(t, root, led, "delay_on") != "200" {
			t.Fatalf("%s not blinking", led)
		}
	}
	if _, err := os.Stat(state); err != nil {
		t.Fatalf("state not persisted: %v", err)
	}
	if active, _ := l.Active(); !active {
		t.Fatal("not active")
	}

	select {
	case r := <-ended:
		if r != "timeout" {
			t.Fatalf("reason %q", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("never ended")
	}
	if got := read(t, root, "green:lan", "trigger"); got != "netdev" {
		t.Fatalf("lan trigger %q", got)
	}
	if got := read(t, root, "green:lan", "device_name"); got != "lan1" {
		t.Fatalf("lan device_name %q", got)
	}
	if got := read(t, root, "green:lan", "offloaded"); got != "0" {
		t.Fatalf("read-only attr touched: %q", got)
	}
	if got := read(t, root, "green:power", "trigger"); got != "none" {
		t.Fatalf("power trigger %q", got)
	}
	if got := read(t, root, "green:power", "brightness"); got != "1" {
		t.Fatalf("power brightness %q", got)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatal("state file left behind")
	}
}

func TestRestartExtendsAndStopRestores(t *testing.T) {
	root := t.TempDir()
	fakeLED(t, root, "a", map[string]string{"trigger": "[phy0tpt] timer none", "brightness": "0"})
	ended := make(chan string, 2)
	l := &Locator{Dir: root, OnEnd: func(r string) { ended <- r }}
	if _, _, err := l.Start(50 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if _, _, err := l.Start(time.Hour); err != nil { // restart: must not re-snapshot the blinking state
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if active, _ := l.Active(); !active {
		t.Fatal("first timer still fired after restart")
	}
	l.Stop()
	if r := <-ended; r != "stopped" {
		t.Fatalf("reason %q", r)
	}
	if got := read(t, root, "a", "trigger"); got != "phy0tpt" {
		t.Fatalf("trigger %q (restart re-snapshotted the blink?)", got)
	}
	l.Stop() // idle stop is a no-op
	select {
	case r := <-ended:
		t.Fatalf("second end %q", r)
	default:
	}
}

func TestNoLEDs(t *testing.T) {
	l := &Locator{Dir: filepath.Join(t.TempDir(), "missing")}
	if _, _, err := l.Start(time.Second); err != ErrNoLEDs {
		t.Fatalf("err = %v", err)
	}
	if l.Count() != 0 {
		t.Fatal("count")
	}
}

func TestRecoverStale(t *testing.T) {
	root := t.TempDir()
	fakeLED(t, root, "a", map[string]string{"trigger": "timer", "brightness": "1", "device_name": "x"})
	state := filepath.Join(t.TempDir(), "locate.json")
	os.WriteFile(state, []byte(`[{"name":"a","trigger":"netdev","brightness":"1","attrs":{"device_name":"br-lan"}}]`), 0o600)
	l := &Locator{Dir: root, StateFile: state}
	ok, err := l.RecoverStale()
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if read(t, root, "a", "trigger") != "netdev" || read(t, root, "a", "device_name") != "br-lan" {
		t.Fatal("not restored")
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatal("state file left behind")
	}
	if ok, _ := l.RecoverStale(); ok {
		t.Fatal("recovered twice")
	}
}

func TestCurrentTrigger(t *testing.T) {
	for in, want := range map[string]string{
		"none timer [netdev] default-on\n": "netdev",
		"[none] timer":                     "none",
		"none timer":                       "",
		"broken [":                         "",
		"[]":                               "",
	} {
		got, ok := currentTrigger(in)
		if got != want || ok != (want != "") {
			t.Errorf("currentTrigger(%q) = %q, %v", in, got, ok)
		}
	}
}
