package wifiplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/capthndsme/perch-agentkit/openwrt/plane"
	"github.com/capthndsme/perch-agentkit/openwrt/uci"
)

// The health check (Wi-Fi design protocol.md 4). After a commit's reload
// settled and the session was dropped, the kit asks every few seconds until
// the check passes, fails, or its time is up (the health wait, longer while
// a radar check runs, never past the confirm deadline): a change is kept
// only when every radio it expects is up and every BSS it expects beacons
// with its SSID. Expected are the enabled AP interfaces on enabled radios of
// the committed config, plus whatever the controller listed in the apply's
// expect (the union: the controller's list catches a bug in this one). What
// was already broken before the apply is reported and never rolled back
// for (a stale radio section, a BSS that was down).
//
// Sources: `ubus call network.wireless status` (radios up, setup failures,
// the BSS sections' ifnames) and `ubus call hostapd.<ifname> get_status`
// (status ENABLED / DFS / ACS / ..., the SSID, the radar check's seconds
// left). The monitor outside applies (wifi.health.changed) asks nl80211
// first and hostapd only for a BSS that does not beacon.

// Problem codes of a health report.
const (
	ProblemRadioDown          = "radio_down"
	ProblemRadioSetupFailed   = "radio_setup_failed"
	ProblemBSSMissing         = "bss_missing"
	ProblemBSSDisabled        = "bss_disabled"
	ProblemSSIDMismatch       = "ssid_mismatch"
	ProblemCACRunning         = "cac_running"
	ProblemACSRunning         = "acs_running"
	ProblemHostapdUnreachable = "hostapd_unreachable"
	ProblemConfigUnreadable   = "config_unreadable"
	ProblemStatusUnreadable   = "status_unreadable"
)

// cacMargin is added to a radar check's seconds left: hostapd starts
// beaconing a moment after the check ends.
const cacMargin = 15 * time.Second

// Report is a health report: wifi.health's result, the health field of
// the hello, refusals and results during an apply.
type Report struct {
	CheckedAt string `json:"checkedAt"`
	// OK: nothing expected is down (problems that were there before the
	// apply do not count).
	OK bool `json:"ok"`
	// Pending: not OK, but still coming (a radar check, a channel scan, a
	// radio being set up).
	Pending  bool          `json:"pending"`
	Radios   []RadioHealth `json:"radios"`
	BSS      []BSSHealth   `json:"bss"`
	PSKGuard string        `json:"pskGuard"`
	Problems []Problem     `json:"problems"`
}

// RadioHealth is one radio as netifd runs it.
type RadioHealth struct {
	Section          string `json:"section"`
	Up               bool   `json:"up"`
	Pending          bool   `json:"pending,omitempty"`
	Disabled         bool   `json:"disabled,omitempty"`
	RetrySetupFailed bool   `json:"retrySetupFailed"`
	Channel          int    `json:"channel,omitempty"`
	DFS              *DFS   `json:"dfs,omitempty"`
	Expected         bool   `json:"expected"`
}

// DFS is a radio's radar check.
type DFS struct {
	CACActive      bool `json:"cacActive"`
	CACSecondsLeft int  `json:"cacSecondsLeft"`
}

// BSSHealth is one BSS (a wifi-iface section in AP mode).
type BSSHealth struct {
	Section  string `json:"section"`
	Radio    string `json:"radio,omitempty"`
	Ifname   string `json:"ifname,omitempty"`
	SSID     string `json:"ssid,omitempty"`
	Status   string `json:"status"`
	Expected bool   `json:"expected"`
	BSSID    string `json:"bssid,omitempty"`
}

// Problem is one thing that is not as expected.
type Problem struct {
	Code    string `json:"code"`
	Section string `json:"section,omitempty"`
	Message string `json:"message"`
	// Preexisting: it was so before the apply (reported, never a reason to
	// roll back).
	Preexisting bool `json:"preexisting,omitempty"`
}

// Expect is an apply's expect: the BSS sections that must beacon and the
// radios that must be up afterwards.
type Expect struct {
	BSS    []string `json:"bss"`
	Radios []string `json:"radios"`
}

// before is what HealthBefore records at snapshot time: what was broken.
type before struct {
	Radios []string `json:"brokenRadios"`
	BSS    []string `json:"brokenBss"`
}

// wirelessStatus is `ubus call network.wireless status`.
type wirelessStatus map[string]struct {
	Up               bool `json:"up"`
	Pending          bool `json:"pending"`
	Disabled         bool `json:"disabled"`
	RetrySetupFailed bool `json:"retry_setup_failed"`
	Interfaces       []struct {
		Section string `json:"section"`
		Ifname  string `json:"ifname"`
	} `json:"interfaces"`
}

// hostapdStatus is `ubus call hostapd.<ifname> get_status`.
type hostapdStatus struct {
	Status  string `json:"status"`
	BSSID   string `json:"bssid"`
	SSID    string `json:"ssid"`
	Channel int    `json:"channel"`
	DFS     *struct {
		CACActive      bool `json:"cac_active"`
		CACSecondsLeft int  `json:"cac_seconds_left"`
	} `json:"dfs"`
}

// bssRun is a BSS as it runs.
type bssRun struct {
	radio, ifname string
	reachable     bool
	st            hostapdStatus
}

// runState is the Wi-Fi as it runs.
type runState struct {
	radios wirelessStatus
	err    error
	// bss by section: those netifd lists with an ifname.
	bss map[string]*bssRun
}

func (s *Service) wirelessStatus(ctx context.Context) (wirelessStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var st wirelessStatus
	if err := s.ubus.Call(ctx, "network.wireless", "status", nil, &st); err != nil {
		return nil, err
	}
	return st, nil
}

// gather reads the running state of the BSS sections in want (the
// others netifd lists, stations and mesh points, are not asked about).
// full asks hostapd about every one; otherwise a BSS nl80211 shows
// beaconing its SSID is taken as ENABLED.
func (s *Service) gather(ctx context.Context, want map[string]bool, full bool) runState {
	rs := runState{bss: map[string]*bssRun{}}
	rs.radios, rs.err = s.wirelessStatus(ctx)
	beaconing := map[string]string{} // ifname → SSID nl80211 reports
	if !full && s.o.Prober != nil && s.o.Prober.NL != nil {
		if list, err := s.o.Prober.NL.Interfaces(); err == nil {
			for _, ifi := range list {
				if ifi.SSID != "" && ifi.FrequencyMHz > 0 {
					beaconing[ifi.Name] = ifi.SSID
				}
			}
		}
	}
	for radio, r := range rs.radios {
		for _, i := range r.Interfaces {
			if i.Section == "" || i.Ifname == "" || !want[i.Section] {
				continue
			}
			b := &bssRun{radio: radio, ifname: i.Ifname}
			rs.bss[i.Section] = b
			if ssid, ok := beaconing[i.Ifname]; ok {
				b.reachable, b.st = true, hostapdStatus{Status: "ENABLED", SSID: ssid}
				continue
			}
			cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			err := s.ubus.Call(cctx, "hostapd."+i.Ifname, "get_status", nil, &b.st)
			cancel()
			b.reachable = err == nil
		}
	}
	return rs
}

// expectation is what must run.
type expectation struct {
	radios map[string]bool
	// bss are the expected BSS sections: their radios and SSID.
	bss map[string]expBSS
	// ap are every AP-mode interface of the config (expected or not).
	ap map[string]expBSS
}

type expBSS struct {
	radios []string
	ssid   string
}

// want are the sections worth asking hostapd about.
func (e expectation) want() map[string]bool {
	w := make(map[string]bool, len(e.ap)+len(e.bss))
	for n := range e.ap {
		w[n] = true
	}
	for n := range e.bss {
		w[n] = true
	}
	return w
}

func enabled(s *uci.Section) bool {
	v, _ := s.Get("disabled")
	switch strings.ToLower(strings.TrimSpace(v.Str())) {
	case "1", "on", "true", "yes", "enabled":
		return false
	}
	return true
}

func str(s *uci.Section, name string) string {
	v, _ := s.Get(name)
	return v.Str()
}

// expectFrom computes the expectation: the enabled AP interfaces on
// enabled radios of the committed config, and the controller's list.
func expectFrom(cfg *uci.Config, ex Expect) expectation {
	e := expectation{radios: map[string]bool{}, bss: map[string]expBSS{}, ap: map[string]expBSS{}}
	all := map[string]expBSS{}
	if cfg != nil {
		on := map[string]bool{}
		for _, r := range cfg.OfType("wifi-device") {
			if enabled(r) {
				on[r.Name] = true
			}
		}
		for _, i := range cfg.OfType("wifi-iface") {
			v, _ := i.Get("device")
			b := expBSS{radios: v.Items, ssid: str(i, "ssid")}
			all[i.Name] = b
			if mode := str(i, "mode"); mode != "" && mode != "ap" {
				continue
			}
			e.ap[i.Name] = b
			if !enabled(i) {
				continue
			}
			var live []string
			for _, r := range b.radios {
				if on[r] {
					live = append(live, r)
				}
			}
			if len(live) == 0 {
				continue // on a missing or disabled radio: nothing to run
			}
			e.bss[i.Name] = expBSS{radios: live, ssid: b.ssid}
			for _, r := range live {
				e.radios[r] = true
			}
		}
	}
	for _, r := range ex.Radios {
		if r != "" {
			e.radios[r] = true
		}
	}
	for _, name := range ex.BSS {
		if _, ok := e.bss[name]; ok || name == "" {
			continue
		}
		b := all[name] // the controller expects it whatever the config says
		e.bss[name] = b
		for _, r := range b.radios {
			e.radios[r] = true
		}
	}
	return e
}

// evaluate judges the running state against an expectation. broken are the
// radios and BSSes that were already broken before an apply (reported,
// never counted). It returns the report, whether everything expected runs,
// and how much longer a radar check needs.
func evaluate(now time.Time, rs runState, e expectation, broken before) (Report, bool, time.Duration) {
	rep := Report{CheckedAt: now.UTC().Format(time.RFC3339), Radios: []RadioHealth{}, BSS: []BSSHealth{},
		PSKGuard: "skipped", Problems: []Problem{}}
	brokenRadio, brokenBSS := map[string]bool{}, map[string]bool{}
	for _, r := range broken.Radios {
		brokenRadio[r] = true
	}
	for _, b := range broken.BSS {
		brokenBSS[b] = true
	}
	counting, lasting := 0, 0
	var extend time.Duration
	// add records a problem; transient ones are on their way to fixing
	// themselves (a radar check, a channel scan, a radio being set up).
	add := func(p Problem, transient bool) {
		rep.Problems = append(rep.Problems, p)
		if p.Preexisting {
			return
		}
		counting++
		if !transient {
			lasting++
		}
	}
	if rs.err != nil {
		add(Problem{Code: ProblemStatusUnreadable, Message: "network.wireless status: " + rs.err.Error()}, false)
	}
	// Radios: every one netifd knows, and every expected one.
	names := map[string]bool{}
	for r := range rs.radios {
		names[r] = true
	}
	for r := range e.radios {
		names[r] = true
	}
	radioList := sortedKeys(names)
	radioDFS := map[string]*DFS{}
	radioChannel := map[string]int{}
	for _, b := range rs.bss {
		if !b.reachable {
			continue
		}
		if b.st.Channel > 0 && radioChannel[b.radio] == 0 {
			radioChannel[b.radio] = b.st.Channel
		}
		if d := b.st.DFS; d != nil && d.CACActive {
			radioDFS[b.radio] = &DFS{CACActive: true, CACSecondsLeft: d.CACSecondsLeft}
		}
	}
	for _, name := range radioList {
		st, known := rs.radios[name]
		h := RadioHealth{Section: name, Up: st.Up, Pending: st.Pending, Disabled: st.Disabled,
			RetrySetupFailed: st.RetrySetupFailed, Channel: radioChannel[name], DFS: radioDFS[name], Expected: e.radios[name]}
		rep.Radios = append(rep.Radios, h)
		if !h.Expected || rs.err != nil {
			continue
		}
		pre := brokenRadio[name]
		switch {
		case !known:
			add(Problem{Code: ProblemRadioDown, Section: name, Message: name + " is not in netifd's wireless status", Preexisting: pre}, false)
		case st.RetrySetupFailed:
			add(Problem{Code: ProblemRadioSetupFailed, Section: name, Message: name + ": netifd gave up setting the radio up (retry_setup_failed)", Preexisting: pre}, false)
		case !st.Up && st.Pending:
			add(Problem{Code: ProblemRadioDown, Section: name, Message: name + " is being set up", Preexisting: pre}, true)
		case !st.Up:
			add(Problem{Code: ProblemRadioDown, Section: name, Message: name + " is down", Preexisting: pre}, false)
		}
	}
	// BSSes: the expected ones and every AP interface that runs.
	sections := map[string]bool{}
	for b := range e.bss {
		sections[b] = true
	}
	for b := range rs.bss {
		sections[b] = true
	}
	cacRadio := map[string]bool{}
	for _, name := range sortedKeys(sections) {
		exp, expected := e.bss[name]
		run := rs.bss[name]
		h := BSSHealth{Section: name, Expected: expected, Status: "missing"}
		if len(exp.radios) > 0 {
			h.Radio = exp.radios[0]
		}
		if run != nil {
			h.Radio, h.Ifname = run.radio, run.ifname
			h.Status = "unreachable"
			if run.reachable {
				h.Status, h.SSID, h.BSSID = run.st.Status, run.st.SSID, run.st.BSSID
			}
		}
		rep.BSS = append(rep.BSS, h)
		if !expected || rs.err != nil {
			continue
		}
		pre := brokenBSS[name]
		for _, r := range exp.radios {
			pre = pre || brokenRadio[r]
		}
		switch {
		case run == nil:
			add(Problem{Code: ProblemBSSMissing, Section: name, Message: name + " has no running interface", Preexisting: pre}, false)
		case !run.reachable:
			add(Problem{Code: ProblemHostapdUnreachable, Section: name, Message: "hostapd." + run.ifname + " does not answer", Preexisting: pre}, false)
		default:
			switch run.st.Status {
			case "ENABLED":
				if exp.ssid != "" && run.st.SSID != exp.ssid {
					add(Problem{Code: ProblemSSIDMismatch, Section: name, Preexisting: pre,
						Message: fmt.Sprintf("%s beacons %q, the config says %q", run.ifname, run.st.SSID, exp.ssid)}, false)
				}
			case "DFS":
				left := 0
				if d := run.st.DFS; d != nil && d.CACActive {
					left = d.CACSecondsLeft
				}
				if left > 0 && !pre {
					if x := time.Duration(left)*time.Second + cacMargin; x > extend {
						extend = x
					}
				}
				if cacRadio[run.radio] {
					// One problem per radio; the BSS still counts.
					if !pre {
						counting++
					}
					continue
				}
				cacRadio[run.radio] = true
				msg := fmt.Sprintf("radar check on channel %d", run.st.Channel)
				if left > 0 {
					msg += fmt.Sprintf(", %d s left", left)
				}
				add(Problem{Code: ProblemCACRunning, Section: run.radio, Message: msg, Preexisting: pre}, true)
			case "ACS", "HT_SCAN":
				add(Problem{Code: ProblemACSRunning, Section: name, Message: run.ifname + " is choosing a channel (" + run.st.Status + ")", Preexisting: pre}, true)
			case "COUNTRY_UPDATE", "UNINITIALIZED", "":
				add(Problem{Code: ProblemBSSDisabled, Section: name, Message: run.ifname + " is starting (" + orUnknown(run.st.Status) + ")", Preexisting: pre}, true)
			default:
				add(Problem{Code: ProblemBSSDisabled, Section: name, Message: run.ifname + " is " + run.st.Status, Preexisting: pre}, false)
			}
		}
	}
	ok := counting == 0
	rep.OK = ok
	rep.Pending = !ok && lasting == 0
	return rep, ok, extend
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// loadWireless reads the committed wireless config (nil when there is none).
func (s *Service) loadWireless() (*uci.Config, error) {
	l, err := s.files.Load("wireless")
	if err != nil {
		return nil, err
	}
	return l.Config, nil
}

// healthCheck is the plane's Health hook.
func (s *Service) healthCheck(ctx context.Context, job plane.HealthJob) plane.HealthReport {
	r := s.check(ctx, job)
	s.mu.Lock()
	f := s.checked
	s.mu.Unlock()
	if f != nil {
		f(r)
	}
	return r
}

func (s *Service) check(ctx context.Context, job plane.HealthJob) plane.HealthReport {
	var ex Expect
	if len(job.Expect) > 0 {
		_ = json.Unmarshal(job.Expect, &ex)
	}
	var b before
	if len(job.Before) > 0 {
		_ = json.Unmarshal(job.Before, &b)
	}
	cfg, err := s.loadWireless()
	now := time.Now()
	if err != nil {
		rep := Report{CheckedAt: now.UTC().Format(time.RFC3339), Radios: []RadioHealth{}, BSS: []BSSHealth{}, PSKGuard: "skipped",
			Problems: []Problem{{Code: ProblemConfigUnreadable, Message: "reading the committed wireless config: " + err.Error()}}}
		return plane.HealthReport{State: plane.HealthPending, Report: rep}
	}
	e := expectFrom(cfg, ex)
	rep, ok, extend := evaluate(now, s.gather(ctx, e.want(), true), e, b)
	if ok {
		return plane.HealthReport{State: plane.HealthOK, Report: rep}
	}
	// Not there yet (or never): the kit looks again, and rolls back when the
	// wait (extended while a radar check runs) is over.
	return plane.HealthReport{State: plane.HealthPending, Report: rep, Extend: extend}
}

// healthBefore is the plane's HealthBefore hook: at snapshot time, the
// expected radios that were down or failed and the expected BSSes that did
// not beacon (a radar check or a channel scan is not broken).
func (s *Service) healthBefore(ctx context.Context) json.RawMessage {
	cfg, _ := s.loadWireless()
	e := expectFrom(cfg, Expect{})
	rs := s.gather(ctx, e.want(), true)
	if rs.err != nil {
		return nil
	}
	rep, _, _ := evaluate(time.Now(), rs, e, before{})
	var b before
	for _, p := range rep.Problems {
		switch p.Code {
		case ProblemRadioDown, ProblemRadioSetupFailed:
			b.Radios = append(b.Radios, p.Section)
		case ProblemBSSMissing, ProblemBSSDisabled, ProblemHostapdUnreachable, ProblemSSIDMismatch:
			b.BSS = append(b.BSS, p.Section)
		}
	}
	// A radio netifd gave up on is broken whether or not anything is on it
	// yet (a stale section whose hardware is gone).
	for name, st := range rs.radios {
		if st.RetrySetupFailed && !e.radios[name] {
			b.Radios = append(b.Radios, name)
		}
	}
	sort.Strings(b.Radios)
	raw, _ := json.Marshal(b)
	return raw
}

// HealthNow is wifi.health: the Wi-Fi as it runs, judged against the
// committed config.
func (s *Service) HealthNow(ctx context.Context) Report {
	return s.healthNow(ctx, true)
}

func (s *Service) healthNow(ctx context.Context, full bool) Report {
	cfg, err := s.loadWireless()
	e := expectFrom(cfg, Expect{})
	rep, _, _ := evaluate(time.Now(), s.gather(ctx, e.want(), full), e, before{})
	if err != nil && !errors.Is(err, uci.ErrNoConfig) {
		rep.Problems = append(rep.Problems, Problem{Code: ProblemConfigUnreadable, Message: "reading the wireless config: " + err.Error()})
		rep.OK, rep.Pending = false, false
	}
	return rep
}

// settle is the plane's Settle hook: netifd applies wireless per radio;
// wait until no radio (and no interface) is pending.
func (s *Service) settle(ctx context.Context) error {
	wait := func(d time.Duration) error {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			return nil
		}
	}
	// procd runs the reload trigger about a second after the commit.
	if err := wait(s.o.SettleMin); err != nil {
		return err
	}
	deadline := time.Now().Add(s.o.SettleMax)
	for time.Now().Before(deadline) {
		st, err := s.wirelessStatus(ctx)
		if err != nil {
			return nil
		}
		busy := false
		for _, r := range st {
			busy = busy || r.Pending
		}
		if !busy {
			busy = s.interfacesPending(ctx)
		}
		if !busy {
			return nil
		}
		if err := wait(500 * time.Millisecond); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) interfacesPending(ctx context.Context) bool {
	var dump struct {
		Interface []struct {
			Pending bool `json:"pending"`
		} `json:"interface"`
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if s.ubus.Call(cctx, "network.interface", "dump", nil, &dump) != nil {
		return false
	}
	for _, i := range dump.Interface {
		if i.Pending {
			return true
		}
	}
	return false
}
