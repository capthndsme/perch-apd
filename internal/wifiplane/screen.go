package wifiplane

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/capthndsme/perch-agentkit/openwrt/plane"
	"github.com/capthndsme/perch-agentkit/openwrt/uci"
	"github.com/capthndsme/perch-apd/internal/groups"
)

// The device-groups engine and the plane share wireless and network (Wi-Fi
// design protocol.md 6, decision D11): the groups' sections are marked
// owner "groups" in reads, and the plane never adopts, writes, deletes,
// orders or creates a section the groups own or could own (their names:
// perch_ws<n>, perch_wv<vid>_<iface>, perch_v<vid>, ...). Two more rules the
// controller keeps and the agent enforces: Perch never creates, renames or
// removes a radio (wifi-device), and never reorders the existing Wi-Fi
// interfaces (their order is the order of the BSSIDs).

// groupsOwned snapshots what the groups engine holds, for the reads' owner
// marks (the Owner hook runs per section).
func (s *Service) groupsOwned() groups.OwnedView {
	view := groups.OwnedView{Sections: []string{}, DynamicVLAN: []string{}}
	var set map[string]bool
	if s.o.Groups != nil {
		view = s.o.Groups.Owned()
		set = make(map[string]bool, len(view.Sections))
		for _, n := range view.Sections {
			set[n] = true
		}
	}
	s.mu.Lock()
	s.ownedSet = set
	s.mu.Unlock()
	return view
}

// owner is the plane's Owner hook.
func (s *Service) owner(config, section string) string {
	if config != "wireless" && config != "network" {
		return ""
	}
	s.mu.Lock()
	set := s.ownedSet
	s.mu.Unlock()
	if set[section] || groups.OwnedName(section) {
		return "groups"
	}
	return ""
}

// groupsName: a section the groups engine owns now, or a name it creates.
func (s *Service) groupsName(set map[string]bool, name string) bool {
	return name != "" && (set[name] || groups.OwnedName(name))
}

// applyScreen is what the screen reads of an apply.
type applyScreen struct {
	DryRun bool `json:"dryRun"`
	Ops    []struct {
		Op       string   `json:"op"`
		Config   string   `json:"config"`
		Section  string   `json:"section"`
		RenameTo string   `json:"renameTo"`
		Sections []string `json:"sections"`
	} `json:"ops"`
	Ledger struct {
		Set []struct {
			Config  string `json:"config"`
			Section string `json:"section"`
		} `json:"set"`
	} `json:"ledger"`
	Guards *struct {
		PSKWildcardDigests []string `json:"pskWildcardDigests"`
	} `json:"guards"`
}

// screenApply refuses what the plane must never do on an AP before the kit
// sees the request: a groups section, a PSK guard this agent cannot run,
// and (not a dry run) an apply without the boot guard. A signed request is
// screened on its payload; the kit verifies the signature afterwards, so
// the screen only ever refuses more. Params it cannot read pass to the kit,
// which refuses them properly.
func (s *Service) screenApply(raw json.RawMessage) error {
	if s.p.ConfiguredAccess() != plane.AccessWrite {
		return nil // the kit refuses with not_managed
	}
	params := []byte(raw)
	var env struct {
		Payload *string         `json:"payload"`
		Sig     json.RawMessage `json:"sig"`
	}
	if json.Unmarshal(raw, &env) == nil && env.Payload != nil && len(env.Sig) > 0 {
		params = []byte(*env.Payload)
	}
	var a applyScreen
	if json.Unmarshal(params, &a) != nil {
		return nil
	}
	if a.Guards != nil && len(a.Guards.PSKWildcardDigests) > 0 {
		return plane.Errorf(plane.CodeBadParams, "this perch-apd has no PSK guard yet (guards.pskWildcardDigests); update the agent")
	}
	set := map[string]bool{}
	if s.o.Groups != nil {
		for _, n := range s.o.Groups.Owned().Sections {
			set[n] = true
		}
	}
	var hits []string
	check := func(config, name string) {
		if (config == "wireless" || config == "network") && s.groupsName(set, name) {
			hits = append(hits, config+"."+name)
		}
	}
	for _, op := range a.Ops {
		check(op.Config, op.Section)
		check(op.Config, op.RenameTo)
		for _, n := range op.Sections {
			check(op.Config, n)
		}
	}
	for _, e := range a.Ledger.Set {
		check(e.Config, e.Section)
	}
	if len(hits) > 0 {
		sort.Strings(hits)
		e := plane.Errorf(plane.CodeNotOwned, "%s belongs to the device groups on this AP (or carries one of their names); the Wi-Fi plane never touches it", hits[0])
		e.Data = map[string]any{"sections": hits, "owner": "groups"}
		return e
	}
	if !a.DryRun && s.openwrt && GuardStatus(s.o.Root) == GuardMissing {
		return plane.Errorf(ErrGuardMissing, "the boot guard %s is missing, so a reboot inside the confirm window would not be undone before the network starts; restart perch-apd to install it", GuardInit)
	}
	return nil
}

// validators are the AP's rules on the result of every apply.
func (s *Service) validators() []plane.Validator {
	return []plane.Validator{{
		Configs: []string{"wireless"},
		Check: func(get plane.ConfigLookup) error {
			after := get("wireless")
			l, err := s.files.Load("wireless")
			if err != nil || after == nil {
				return nil // nothing to compare with (a new file, or none left: the kit judges)
			}
			return checkWireless(l.Config, after)
		},
	}}
}

// checkWireless compares the wireless config before and after an apply.
func checkWireless(before, after *uci.Config) error {
	radios := func(c *uci.Config) []string {
		var out []string
		for _, s := range c.OfType("wifi-device") {
			out = append(out, s.Name)
		}
		sort.Strings(out)
		return out
	}
	b, a := radios(before), radios(after)
	if fmt.Sprint(b) != fmt.Sprint(a) {
		return plane.Errorf(plane.CodeInvalidConfig, "Perch never creates, renames or removes radios (wifi-device sections %v would become %v)", b, a)
	}
	// The interfaces that exist before and after keep their relative order.
	still := map[string]bool{}
	for _, s := range after.OfType("wifi-iface") {
		still[s.Name] = true
	}
	var was []string
	for _, s := range before.OfType("wifi-iface") {
		if still[s.Name] {
			was = append(was, s.Name)
		}
	}
	existed := map[string]bool{}
	for _, n := range was {
		existed[n] = true
	}
	var now []string
	for _, s := range after.OfType("wifi-iface") {
		if existed[s.Name] {
			now = append(now, s.Name)
		}
	}
	if fmt.Sprint(was) != fmt.Sprint(now) {
		return plane.Errorf(plane.CodeInvalidConfig, "Perch never reorders the existing Wi-Fi interfaces (their order is the BSSIDs' order): %v would become %v", was, now)
	}
	return nil
}
