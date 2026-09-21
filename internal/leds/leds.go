// Package leds implements "locate": blink every LED of the device so it can
// be found on a shelf, then put each LED back exactly as it was.
//
// Each LED's trigger and the trigger's writable settings (netdev's
// device_name/link/rx/tx, timer's delays, pattern…) are saved before the
// blink and written back after it. The saved state also goes to a file under
// /var/run, so an agent that dies mid-blink restores the LEDs when it starts
// again.
package leds

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ErrNoLEDs: the device exposes no controllable LED.
var ErrNoLEDs = errors.New("no controllable LEDs in /sys/class/leds")

// Blink timing, milliseconds on / off.
const blinkMs = "200"

// Files that are part of every LED, not of its trigger.
var baseFiles = map[string]bool{
	"brightness": true, "max_brightness": true, "trigger": true, "uevent": true,
	"brightness_hw_changed": true, "multi_index": true, "multi_intensity": true,
}

type ledState struct {
	Name       string            `json:"name"`
	Trigger    string            `json:"trigger"`
	Brightness string            `json:"brightness"`
	Attrs      map[string]string `json:"attrs,omitempty"`
}

// Locator blinks and restores LEDs.
type Locator struct {
	Dir       string // /sys/class/leds
	StateFile string // /var/run/perch-apd/locate.json
	OnEnd     func(reason string)
	Now       func() time.Time

	mu     sync.Mutex
	saved  []ledState
	timer  *time.Timer
	active bool
	endsAt time.Time
	gen    int
}

// New returns a locator over the standard paths.
func New() *Locator {
	return &Locator{Dir: "/sys/class/leds", StateFile: "/var/run/perch-apd/locate.json"}
}

func (l *Locator) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

// Count returns how many LEDs exist.
func (l *Locator) Count() int {
	entries, err := os.ReadDir(l.Dir)
	if err != nil {
		return 0
	}
	return len(entries)
}

// Active reports whether a blink is in progress and when it ends.
func (l *Locator) Active() (bool, time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.active, l.endsAt
}

// Start blinks every LED for d (restarting the timer if already blinking)
// and returns how many LEDs blink.
func (l *Locator) Start(d time.Duration) (int, time.Time, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.active {
		states, err := l.snapshot()
		if err != nil {
			return 0, time.Time{}, err
		}
		if len(states) == 0 {
			return 0, time.Time{}, ErrNoLEDs
		}
		if err := l.persist(states); err != nil {
			return 0, time.Time{}, fmt.Errorf("saving LED state: %w", err)
		}
		var blinking []ledState
		for _, s := range states {
			if l.blink(s.Name) == nil {
				blinking = append(blinking, s)
			} else {
				l.restoreOne(s) // leave an LED that refused the timer as it was
			}
		}
		if len(blinking) == 0 {
			l.clearPersisted()
			return 0, time.Time{}, ErrNoLEDs
		}
		l.saved = states
		l.active = true
	}
	if l.timer != nil {
		l.timer.Stop()
	}
	l.gen++
	gen := l.gen
	l.endsAt = l.now().Add(d)
	l.timer = time.AfterFunc(d, func() { l.finish(gen, "timeout") })
	return len(l.saved), l.endsAt, nil
}

// Stop ends a blink now. Stopping when idle is not an error.
func (l *Locator) Stop() {
	l.mu.Lock()
	gen := l.gen
	l.mu.Unlock()
	l.finish(gen, "stopped")
}

func (l *Locator) finish(gen int, reason string) {
	l.mu.Lock()
	if !l.active || gen != l.gen {
		l.mu.Unlock()
		return
	}
	if l.timer != nil {
		l.timer.Stop()
		l.timer = nil
	}
	for _, s := range l.saved {
		l.restoreOne(s)
	}
	l.saved = nil
	l.active = false
	l.endsAt = time.Time{}
	l.clearPersisted()
	onEnd := l.OnEnd
	l.mu.Unlock()
	if onEnd != nil {
		onEnd(reason)
	}
}

// RecoverStale restores LEDs from a state file left by a previous run.
func (l *Locator) RecoverStale() (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.StateFile == "" {
		return false, nil
	}
	data, err := os.ReadFile(l.StateFile)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var states []ledState
	if err := json.Unmarshal(data, &states); err != nil {
		l.clearPersisted()
		return false, err
	}
	for _, s := range states {
		l.restoreOne(s)
	}
	l.clearPersisted()
	return true, nil
}

// currentTrigger picks the bracketed entry of a sysfs trigger list
// ("none timer [netdev] default-on" → "netdev").
func currentTrigger(list string) (string, bool) {
	start := strings.IndexByte(list, '[')
	if start < 0 {
		return "", false
	}
	end := strings.IndexByte(list[start+1:], ']')
	if end <= 0 {
		return "", false
	}
	return list[start+1 : start+1+end], true
}

func (l *Locator) snapshot() ([]ledState, error) {
	entries, err := os.ReadDir(l.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []ledState
	for _, e := range entries {
		dir := filepath.Join(l.Dir, e.Name())
		trig, err := os.ReadFile(filepath.Join(dir, "trigger"))
		if err != nil {
			continue
		}
		s := ledState{Name: e.Name(), Trigger: "none"}
		if t, ok := currentTrigger(string(trig)); ok {
			s.Trigger = t
		}
		if b, err := os.ReadFile(filepath.Join(dir, "brightness")); err == nil {
			s.Brightness = strings.TrimSpace(string(b))
		}
		files, _ := os.ReadDir(dir)
		for _, f := range files {
			if baseFiles[f.Name()] || !f.Type().IsRegular() {
				continue
			}
			info, err := f.Info()
			if err != nil || info.Mode().Perm()&0o200 == 0 {
				continue // read-only (e.g. netdev's "offloaded")
			}
			v, err := os.ReadFile(filepath.Join(dir, f.Name()))
			if err != nil {
				continue
			}
			if s.Attrs == nil {
				s.Attrs = map[string]string{}
			}
			s.Attrs[f.Name()] = strings.TrimSpace(string(v))
		}
		out = append(out, s)
	}
	return out, nil
}

func (l *Locator) blink(name string) error {
	dir := filepath.Join(l.Dir, name)
	if err := writeFile(filepath.Join(dir, "trigger"), "timer"); err != nil {
		return err
	}
	// The timer trigger creates these; the defaults are 500 ms.
	_ = writeFile(filepath.Join(dir, "delay_on"), blinkMs)
	_ = writeFile(filepath.Join(dir, "delay_off"), blinkMs)
	return nil
}

func (l *Locator) restoreOne(s ledState) {
	dir := filepath.Join(l.Dir, s.Name)
	_ = writeFile(filepath.Join(dir, "trigger"), s.Trigger)
	// Settings after the trigger (writing the trigger resets them);
	// netdev's device_name before its link/rx/tx modes.
	names := make([]string, 0, len(s.Attrs))
	for k := range s.Attrs {
		names = append(names, k)
	}
	sort.Slice(names, func(i, j int) bool {
		if names[i] == "device_name" || names[j] == "device_name" {
			return names[i] == "device_name"
		}
		return names[i] < names[j]
	})
	for _, k := range names {
		_ = writeFile(filepath.Join(dir, k), s.Attrs[k])
	}
	if s.Trigger == "none" && s.Brightness != "" {
		_ = writeFile(filepath.Join(dir, "brightness"), s.Brightness)
	}
}

func (l *Locator) persist(states []ledState) error {
	if l.StateFile == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(l.StateFile), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(states)
	if err != nil {
		return err
	}
	return os.WriteFile(l.StateFile, data, 0o600)
}

func (l *Locator) clearPersisted() {
	if l.StateFile != "" {
		_ = os.Remove(l.StateFile)
	}
}

// writeFile writes a sysfs attribute (no truncation games: sysfs takes one write).
func writeFile(path, value string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return err
	}
	_, werr := f.WriteString(value)
	cerr := f.Close()
	if werr != nil {
		return werr
	}
	return cerr
}
