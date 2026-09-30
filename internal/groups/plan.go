// Package groups puts the controller's device groups on the access point
// (controller docs/gateway/device-groups.md section 7): per-group Wi-Fi
// passphrases and MAC bindings on shared SSIDs (OpenWrt `wifi-station`),
// the VLANs they land in (`wifi-vlan`), and the plumbing that carries those
// VLANs tagged to the gateway over the AP's trunk port.
//
// Perch owns the sections it creates (their names are in the ledger, below)
// and nothing else: other `perch_*` sections belong to the Wi-Fi config
// plane (`perch_n*`) and are never touched here. Where it must touch a
// section it does not own it records the old value in the ledger and puts
// it back when no longer needed:
//
//   - `dynamic_vlan '1'` on the managed `wifi-iface`s (hostapd assigns the
//     VLANs only with it);
//   - an untagged bridge is converted to VLAN filtering: the bridge gets a
//     `bridge-vlan` for its untagged traffic (every port `:u*`) and the
//     interfaces on the bare bridge move to `<bridge>.<untagged vid>`, the
//     same conversion the gateway's networks do.
//
// Plan is pure: configs in, configs out. The engine (engine.go) writes
// them, reloads, and rolls back unless the controller confirms.
package groups

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/capthndsme/perch-agentkit/openwrt/uci"
)

// Desired is groups.apply's params: the complete state the AP should hold.
type Desired struct {
	Revision       int64     `json:"revision"`
	ConfirmSeconds int       `json:"confirmSeconds"`
	Trunk          string    `json:"trunk"` // "auto" or the port carrying the VLANs to the gateway
	SSIDs          []string  `json:"ssids"`
	VLANs          []VLAN    `json:"vlans"`
	Stations       []Station `json:"stations"`
}

// VLAN is a group's VLAN on the trunk.
type VLAN struct {
	VID int `json:"vid"`
}

// Station is a passphrase that puts a device in a VLAN (a group key), or a
// binding: MACs that keep the SSID's own passphrase but land in the VLAN.
type Station struct {
	Key  string   `json:"key,omitempty"`
	VID  int      `json:"vid"`
	MACs []string `json:"macs,omitempty"`
}

// Ledger is what the groups engine created, and what it changed in
// sections it does not own.
type Ledger struct {
	// Owned are the names of the sections the engine created (in wireless
	// and network). The next plan removes exactly these before it writes
	// its own; every other section, `perch_*` or not, stays. nil = a ledger
	// written before the engine recorded them (or none yet): the names are
	// then the ones matching the engine's naming patterns (OwnedName). A
	// plan always records a list, `[]` when it created nothing, so the two
	// cases survive state.json.
	Owned []string `json:"owned"`
	// DynamicVLAN: wifi-iface section → its dynamic_vlan before Perch set it
	// ("" = it had none).
	DynamicVLAN map[string]string `json:"dynamicVlan,omitempty"`
	// Converted: an untagged bridge Perch converted to VLAN filtering.
	Converted *Conversion `json:"converted,omitempty"`
}

// Conversion records a bridge Perch converted.
type Conversion struct {
	Bridge       string `json:"bridge"`
	UntaggedVLAN int    `json:"untaggedVlan"`
	// Moved: interface section → the device it had (the bare bridge).
	Moved map[string]string `json:"moved"`
}

// Facts are what the engine found on the device.
type Facts struct {
	// TrunkPort is the port the gateway is behind ("" = unknown).
	TrunkPort string
}

// Result is a plan.
type Result struct {
	Wireless, Network *uci.Config
	Ledger            Ledger
	TrunkPort         string
	// Bridge is the trunk port's bridge ("" = the port stands alone).
	Bridge    string
	Converted bool
	// Managed are the wifi-iface sections the groups use.
	Managed []string
	Issues  []string
	// BindingKeys are the SHA-256 digests (hex) of the passphrases bindings
	// reuse: hostapd must never hold one of them for any MAC.
	BindingKeys []string
}

// Refusal is a plan that cannot be applied, with a machine code.
type Refusal struct {
	Code    string
	Message string
	// Reason qualifies a `busy` refusal: groups_pending, plane_pending,
	// update_pending, luci_pending.
	Reason string
}

func (r *Refusal) Error() string { return r.Message }

func refuse(code, format string, args ...any) *Refusal {
	return &Refusal{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Prefix of the sections the engine creates.
const Prefix = "perch_"

// OwnedName reports whether name is one the groups engine creates:
// perch_ws<n> (wifi-station), perch_wv<vid>_<iface> (wifi-vlan),
// perch_v<vid> (interface), perch_bv<vid> and perch_bvu (bridge-vlan),
// perch_dv<vid> and perch_bd<vid> (devices), i.e.
// ^perch_(ws\d+|wv\d+_\w+|v\d+|bv\d+|bvu|dv\d+|bd\d+)$. The Wi-Fi config
// plane's names (perch_n<id>_<radio>, perch_nv<vid>, perch_nbv<vid>,
// perch_nbvu) never match.
func OwnedName(name string) bool {
	rest, ok := strings.CutPrefix(name, Prefix)
	if !ok {
		return false
	}
	if rest == "bvu" {
		return true
	}
	for _, p := range []string{"ws", "bv", "dv", "bd", "v"} {
		if n, ok := strings.CutPrefix(rest, p); ok && allDigits(n) {
			return true
		}
	}
	if n, ok := strings.CutPrefix(rest, "wv"); ok {
		vid, iface, found := strings.Cut(n, "_")
		return found && allDigits(vid) && iface != "" && strings.Trim(iface, wordChars) == ""
	}
	return false
}

const wordChars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_"

func allDigits(s string) bool {
	return s != "" && strings.Trim(s, "0123456789") == ""
}

// MaxVLANs and MaxStations bound one desired state.
const (
	MaxVLANs    = 256
	MaxStations = 4096
)

func str(s *uci.Section, name string) string {
	if v, ok := s.Get(name); ok {
		return v.Str()
	}
	return ""
}

func listOf(s *uci.Section, name string) []string {
	if v, ok := s.Get(name); ok {
		if v.IsList {
			return append([]string(nil), v.Items...)
		}
		return strings.Fields(v.Str())
	}
	return nil
}

// ValidMAC accepts 02:00:00:00:00:01 (lowercase is what is written).
func validMAC(s string) bool {
	if len(s) != 17 {
		return false
	}
	for i := 0; i < 17; i++ {
		c := s[i]
		if i%3 == 2 {
			if c != ':' {
				return false
			}
			continue
		}
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// validKey is a WPA passphrase: 8 to 63 printable ASCII characters.
func validKey(k string) bool {
	if len(k) < 8 || len(k) > 63 {
		return false
	}
	for i := 0; i < len(k); i++ {
		if k[i] < 0x20 || k[i] > 0x7e {
			return false
		}
	}
	return true
}

func pskEncryption(enc string) bool {
	return strings.HasPrefix(enc, "psk") || strings.HasPrefix(enc, "sae")
}

// Validate checks a desired state's shape.
func Validate(d Desired) error {
	if d.Revision < 1 {
		return refuse("bad_params", "revision must be 1 or more")
	}
	macs := 0
	for _, st := range d.Stations {
		macs += len(st.MACs)
	}
	if len(d.VLANs) > MaxVLANs || len(d.Stations) > MaxStations || macs > MaxStations {
		return refuse("bad_params", "at most %d VLANs, %d stations and %d bound MACs", MaxVLANs, MaxStations, MaxStations)
	}
	seen := map[int]bool{}
	for _, v := range d.VLANs {
		if v.VID < 1 || v.VID > 4094 {
			return refuse("bad_params", "VLAN %d is outside 1-4094", v.VID)
		}
		if seen[v.VID] {
			return refuse("bad_params", "VLAN %d is listed twice", v.VID)
		}
		seen[v.VID] = true
	}
	for i, st := range d.Stations {
		if !seen[st.VID] {
			return refuse("bad_params", "station %d names VLAN %d, which is not listed", i, st.VID)
		}
		if st.Key != "" && !validKey(st.Key) {
			return refuse("bad_params", "station %d: a passphrase is 8 to 63 printable ASCII characters", i)
		}
		if st.Key == "" && len(st.MACs) == 0 {
			return refuse("bad_params", "station %d has neither a passphrase nor MACs", i)
		}
		for _, m := range st.MACs {
			if !validMAC(m) {
				return refuse("bad_params", "station %d: %q is not a lowercase MAC", i, m)
			}
		}
	}
	for _, s := range d.SSIDs {
		if len(s) < 1 || len(s) > 32 {
			return refuse("bad_params", "an SSID is 1 to 32 bytes")
		}
	}
	if d.Trunk != "" && d.Trunk != "auto" && !validIfname(d.Trunk) {
		return refuse("bad_params", "trunk %q is not a device name", d.Trunk)
	}
	return nil
}

func validIfname(s string) bool {
	if len(s) < 1 || len(s) > 15 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '-' || c == '_' || c == '@') {
			return false
		}
	}
	return true
}

// Plan works out the configs for a desired state from the current ones and
// the ledger of the last confirmed apply.
func Plan(wireless, network *uci.Config, d Desired, f Facts, prev Ledger) (*Result, error) {
	if err := Validate(d); err != nil {
		return nil, err
	}
	w, n := wireless.Clone(), network.Clone()
	kept := undo(w, n, prev)

	p := &planner{}
	res := &Result{Wireless: w, Network: n, Ledger: Ledger{DynamicVLAN: map[string]string{}, Converted: kept}}
	if kept != nil {
		// The bridge stays converted: it carries VLANs that are not the
		// groups' (undo).
		res.Converted = true
		res.Bridge = kept.Bridge
	}
	owned := func() []string {
		out := append([]string{}, p.created...)
		if kept != nil {
			out = append(out, Prefix+"bvu")
		}
		return out
	}
	if len(d.VLANs) == 0 {
		// Nothing to carry: the groups hold no sections and change nothing
		// (but a conversion other VLANs now depend on).
		finish(w, n)
		res.Ledger.DynamicVLAN = nil
		res.Ledger.Owned = owned()
		return res, nil
	}

	// The managed interfaces: access points on the listed SSIDs with a
	// passphrase.
	ssids := map[string]bool{}
	for _, s := range d.SSIDs {
		ssids[s] = true
	}
	type iface struct {
		name string
		key  string
	}
	var managed []iface
	for _, s := range w.OfType("wifi-iface") {
		if str(s, "mode") != "ap" || !ssids[str(s, "ssid")] || str(s, "disabled") == "1" {
			continue
		}
		if !pskEncryption(str(s, "encryption")) {
			res.Issues = append(res.Issues, fmt.Sprintf("%s (%s) has no passphrase: group keys need WPA-PSK or SAE", s.Name, str(s, "ssid")))
			continue
		}
		if s.Anonymous {
			res.Issues = append(res.Issues, fmt.Sprintf("%s (%s) is an anonymous section: name it so a later edit cannot renumber it", s.Name, str(s, "ssid")))
		}
		managed = append(managed, iface{name: s.Name, key: str(s, "key")})
	}
	if len(managed) == 0 {
		return nil, refuse("no_managed_iface", "no access point interface broadcasts %s with a passphrase", strings.Join(d.SSIDs, ", "))
	}

	// The trunk.
	port := f.TrunkPort
	if d.Trunk != "" && d.Trunk != "auto" {
		port = d.Trunk
	}
	if port == "" {
		return nil, refuse("trunk_unknown", "the port towards the gateway is not known: set it on the controller")
	}
	res.TrunkPort = port
	vids := make([]int, 0, len(d.VLANs))
	for _, v := range d.VLANs {
		vids = append(vids, v.VID)
	}
	sort.Ints(vids)

	bridge := bridgeOf(n, port)
	if bridge != nil {
		name := str(bridge, "name")
		res.Bridge = name
		switch {
		case kept != nil && kept.Bridge == name:
			// Still converted from before. Its untagged VLAN cannot also
			// be a group's.
			for _, v := range vids {
				if v == kept.UntaggedVLAN {
					return nil, refuse("untagged_vlan_conflict", "VLAN %d carries %s's untagged traffic since Perch converted it", v, name)
				}
			}
		case len(bridgeVLANs(n, name)) == 0:
			if kept != nil {
				return nil, refuse("conversion_conflict", "Perch keeps %s converted for other VLANs and cannot also convert %s", kept.Bridge, name)
			}
			conv, err := convert(p, n, bridge, vids, prev.Converted)
			if err != nil {
				return nil, err
			}
			res.Ledger.Converted = conv
			res.Converted = true
		}
		for _, v := range vids {
			if s := routerVLAN(n, name, v, p); s != nil {
				// Someone else (the router, the Wi-Fi config plane) carries
				// it already: use it as it is.
				res.Issues = append(res.Issues, fmt.Sprintf("VLAN %d is already on %s; Perch adds nothing to it", v, name))
				if !taggedMember(s, port) {
					res.Issues = append(res.Issues, fmt.Sprintf("VLAN %d on %s does not carry %s tagged: its clients may not reach the gateway", v, name, port))
				}
				continue
			}
			p.add(n, "bridge-vlan", fmt.Sprintf("%sbv%d", Prefix, v), []uci.Option{
				{Name: "device", Value: uci.String(name)},
				{Name: "vlan", Value: uci.String(strconv.Itoa(v))},
				{Name: "ports", Value: uci.List(port + ":t")},
			})
		}
		for _, v := range vids {
			p.add(n, "interface", fmt.Sprintf("%sv%d", Prefix, v), []uci.Option{
				{Name: "proto", Value: uci.String("none")},
				{Name: "device", Value: uci.String(fmt.Sprintf("%s.%d", name, v))},
			})
		}
	} else {
		// A port of its own: an 802.1Q device and a bridge per VLAN.
		for _, v := range vids {
			vdev := fmt.Sprintf("%s.%d", port, v)
			br := fmt.Sprintf("br-pv%d", v)
			p.add(n, "device", fmt.Sprintf("%sdv%d", Prefix, v), []uci.Option{
				{Name: "type", Value: uci.String("8021q")},
				{Name: "ifname", Value: uci.String(port)},
				{Name: "vid", Value: uci.String(strconv.Itoa(v))},
				{Name: "name", Value: uci.String(vdev)},
			})
			p.add(n, "device", fmt.Sprintf("%sbd%d", Prefix, v), []uci.Option{
				{Name: "type", Value: uci.String("bridge")},
				{Name: "name", Value: uci.String(br)},
				{Name: "ports", Value: uci.List(vdev)},
			})
			p.add(n, "interface", fmt.Sprintf("%sv%d", Prefix, v), []uci.Option{
				{Name: "proto", Value: uci.String("none")},
				{Name: "device", Value: uci.String(br)},
			})
		}
	}

	// dynamic_vlan on the managed interfaces.
	for _, m := range managed {
		s := w.Section(m.name)
		was := str(s, "dynamic_vlan")
		if was != "1" {
			res.Ledger.DynamicVLAN[m.name] = was
			s.Set("dynamic_vlan", uci.String("1"))
		}
		res.Managed = append(res.Managed, m.name)
	}

	// A wifi-vlan per VLAN and managed interface.
	for _, m := range managed {
		for _, v := range vids {
			p.add(w, "wifi-vlan", fmt.Sprintf("%swv%d_%s", Prefix, v, shortName(m.name)), []uci.Option{
				{Name: "iface", Value: uci.String(m.name)},
				{Name: "name", Value: uci.String("g" + strconv.Itoa(v))},
				{Name: "vid", Value: uci.String(strconv.Itoa(v))},
				{Name: "network", Value: uci.String(fmt.Sprintf("%sv%d", Prefix, v))},
			})
		}
	}

	// A wifi-station per station and managed interface; a binding gets one
	// per MAC and uses the interface's own passphrase. The MAC is a single
	// `option mac`: 24.10 reads a string (a `list` is dropped, and the entry
	// then matches every client), 25.12 an array, which netifd splits from
	// a string option.
	idx := 0
	for _, st := range d.Stations {
		macs := append([]string(nil), st.MACs...)
		sort.Strings(macs)
		for _, m := range managed {
			key := st.Key
			if key != "" && key == m.key {
				// Every client of the SSID would land in this VLAN.
				res.Issues = append(res.Issues, fmt.Sprintf("a group key equals %s's own passphrase; skipped", m.name))
				continue
			}
			if key == "" {
				key = m.key
				if !validKey(key) {
					res.Issues = append(res.Issues, fmt.Sprintf("%s has no usable passphrase for bindings", m.name))
					continue
				}
				res.BindingKeys = appendUnique(res.BindingKeys, keyDigest(key))
			}
			opts := []uci.Option{
				{Name: "iface", Value: uci.String(m.name)},
				{Name: "key", Value: uci.String(key)},
				{Name: "vid", Value: uci.String(strconv.Itoa(st.VID))},
			}
			if len(macs) == 0 {
				p.add(w, "wifi-station", fmt.Sprintf("%sws%d", Prefix, idx), opts)
				idx++
				continue
			}
			for _, mac := range macs {
				withMAC := append(append([]uci.Option(nil), opts...), uci.Option{Name: "mac", Value: uci.String(mac)})
				p.add(w, "wifi-station", fmt.Sprintf("%sws%d", Prefix, idx), withMAC)
				idx++
			}
		}
	}
	if p.taken != "" {
		return nil, refuse("name_taken", "a section named %s exists that the groups engine did not create: rename or delete it", p.taken)
	}
	if len(res.Ledger.DynamicVLAN) == 0 {
		res.Ledger.DynamicVLAN = nil
	}
	res.Ledger.Owned = owned()
	finish(w, n)
	return res, nil
}

// shortName keeps a section-name suffix within libuci's rules and short.
func shortName(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s) && len(out) < 24; i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' {
			out = append(out, c)
		}
	}
	return string(out)
}

// planner adds the plan's sections and remembers their names (the next
// ledger's Owned).
type planner struct {
	created []string
	// taken: a name the plan needs that a section it does not own holds.
	taken string
}

func (p *planner) add(c *uci.Config, typ, name string, opts []uci.Option) {
	if c.Section(name) != nil {
		// Two sections of one name would merge in libuci. undo removed
		// every section the engine owns, so this one is someone else's.
		if p.taken == "" {
			p.taken = c.Name + "." + name
		}
		return
	}
	c.Sections = append(c.Sections, &uci.Section{Name: name, Type: typ, Options: opts})
	p.created = append(p.created, name)
}

func (p *planner) createdHere(name string) bool {
	for _, n := range p.created {
		if n == name {
			return true
		}
	}
	return false
}

func finish(w, n *uci.Config) {
	w.Reindex()
	n.Reindex()
}

// ownedSet is the names the engine created according to the ledger, or by
// pattern for a ledger without the list.
func ownedSet(prev Ledger, w, n *uci.Config) map[string]bool {
	out := map[string]bool{}
	if prev.Owned != nil {
		for _, name := range prev.Owned {
			out[name] = true
		}
		return out
	}
	for _, c := range []*uci.Config{w, n} {
		for _, s := range c.Sections {
			if !s.Anonymous && OwnedName(s.Name) {
				out[s.Name] = true
			}
		}
	}
	return out
}

// undo removes the engine's sections and puts back what the ledger
// recorded. A bridge conversion stays while the bridge carries a VLAN the
// engine does not own (the router's, or one the Wi-Fi config plane added
// after the conversion): moving its interfaces back would break those. It
// returns that conversion (nil when undone or there was none).
func undo(w, n *uci.Config, prev Ledger) (kept *Conversion) {
	owned := ownedSet(prev, w, n)
	if c := prev.Converted; c != nil {
		for _, s := range bridgeVLANs(n, c.Bridge) {
			if !owned[s.Name] {
				kept = c
				break
			}
		}
		if kept != nil {
			delete(owned, Prefix+"bvu")
		}
	}
	drop := func(c *uci.Config) {
		out := c.Sections[:0]
		for _, s := range c.Sections {
			if s.Anonymous || !owned[s.Name] {
				out = append(out, s)
			}
		}
		c.Sections = out
	}
	drop(w)
	drop(n)
	for name, was := range prev.DynamicVLAN {
		if s := w.Section(name); s != nil {
			if was == "" {
				s.Delete("dynamic_vlan")
			} else {
				s.Set("dynamic_vlan", uci.String(was))
			}
		}
	}
	if c := prev.Converted; c != nil && kept == nil {
		for name, dev := range c.Moved {
			if s := n.Section(name); s != nil && str(s, "device") == fmt.Sprintf("%s.%d", c.Bridge, c.UntaggedVLAN) {
				s.Set("device", uci.String(dev))
			}
		}
	}
	w.Reindex()
	n.Reindex()
	return kept
}

// bridgeOf is the bridge device section that has port among its ports.
func bridgeOf(n *uci.Config, port string) *uci.Section {
	for _, s := range n.OfType("device") {
		if str(s, "type") != "bridge" {
			continue
		}
		for _, p := range listOf(s, "ports") {
			if p == port {
				return s
			}
		}
	}
	return nil
}

func bridgeVLANs(n *uci.Config, bridge string) []*uci.Section {
	var out []*uci.Section
	for _, s := range n.OfType("bridge-vlan") {
		if str(s, "device") == bridge {
			out = append(out, s)
		}
	}
	return out
}

// routerVLAN is a bridge-vlan for vid on bridge that this plan did not
// create (undo already removed the engine's own): the router's, or the
// Wi-Fi config plane's.
func routerVLAN(n *uci.Config, bridge string, vid int, p *planner) *uci.Section {
	for _, s := range bridgeVLANs(n, bridge) {
		if str(s, "vlan") == strconv.Itoa(vid) && !p.createdHere(s.Name) {
			return s
		}
	}
	return nil
}

// taggedMember reports whether a bridge-vlan carries port tagged (netifd:
// `<port>[:t|:u][*]`, tagged without `:u`).
func taggedMember(s *uci.Section, port string) bool {
	for _, p := range listOf(s, "ports") {
		name, flags, _ := strings.Cut(p, ":")
		if name == port && !strings.Contains(flags, "u") {
			return true
		}
	}
	return false
}

// convert turns an untagged bridge into a VLAN-filtering one: its ports
// untagged on the untagged VLAN (1, else the smallest free one), and the
// interfaces on the bare bridge onto `<bridge>.<untagged>`.
func convert(p *planner, n *uci.Config, bridge *uci.Section, vids []int, prev *Conversion) (*Conversion, error) {
	name := str(bridge, "name")
	used := map[int]bool{}
	for _, v := range vids {
		used[v] = true
	}
	untagged := 1
	if prev != nil && prev.Bridge == name && !used[prev.UntaggedVLAN] {
		untagged = prev.UntaggedVLAN
	}
	for used[untagged] {
		untagged++
	}
	if untagged > 4094 {
		return nil, refuse("no_untagged_vlan", "no VLAN id is left for %s's untagged traffic", name)
	}
	var ports []string
	for _, port := range listOf(bridge, "ports") {
		ports = append(ports, port+":u*")
	}
	if len(ports) == 0 {
		return nil, refuse("bridge_empty", "%s has no ports to carry", name)
	}
	p.add(n, "bridge-vlan", Prefix+"bvu", []uci.Option{
		{Name: "device", Value: uci.String(name)},
		{Name: "vlan", Value: uci.String(strconv.Itoa(untagged))},
		{Name: "ports", Value: uci.List(ports...)},
	})
	conv := &Conversion{Bridge: name, UntaggedVLAN: untagged, Moved: map[string]string{}}
	for _, s := range n.OfType("interface") {
		if p.createdHere(s.Name) {
			continue
		}
		if str(s, "device") == name {
			conv.Moved[s.Name] = name
			s.Set("device", uci.String(fmt.Sprintf("%s.%d", name, untagged)))
		}
	}
	return conv, nil
}
