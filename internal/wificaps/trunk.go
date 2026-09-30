package wificaps

import (
	"path/filepath"

	"github.com/capthndsme/perch-agentkit/openwrt/uci"
	"github.com/capthndsme/perch-apd/internal/groups"
)

// Trunk is the port towards the gateway, where VLAN-bound SSIDs would be
// carried tagged.
type Trunk struct {
	// Port is the bridge port behind which the default gateway's MAC is
	// learned, or the default route's own port (the device-groups
	// detection).
	Port string `json:"port"`
	// Bridge is the bridge the port is in ("" = none).
	Bridge string `json:"bridge,omitempty"`
	// VLANFiltering: the bridge filters VLANs now.
	VLANFiltering bool `json:"vlanFiltering"`
	// Swconfig: the network config has a pre-DSA `switch` section (VLANs on
	// such a switch are not modeled).
	Swconfig bool `json:"swconfig,omitempty"`
	// Source is "auto" (the controller's override wins when it renders).
	Source string `json:"source"`
	// Error: why the port is not known.
	Error string `json:"error,omitempty"`
}

func (p *Prober) trunk() *Trunk {
	root := p.Root
	if root == "" {
		root = "/"
	}
	t := &Trunk{Source: "auto"}
	if l, err := (uci.Files{Dir: p.path(uci.DefaultDir)}).Load("network"); err == nil {
		t.Swconfig = len(l.Config.OfType("switch")) > 0
	}
	port, err := groups.DetectTrunk(groups.FS{Root: root})
	if err != nil {
		t.Error = err.Error()
		return t
	}
	t.Port = port
	if master, err := filepath.EvalSymlinks(p.path("/sys/class/net/" + port + "/master")); err == nil {
		br := filepath.Base(master)
		if p.exists("/sys/class/net/" + br + "/bridge") {
			t.Bridge = br
			t.VLANFiltering = p.read("/sys/class/net/"+br+"/bridge/vlan_filtering") == "1"
		}
	} else if p.exists("/sys/class/net/" + port + "/bridge") {
		// The route's device is the bridge itself (no port found behind it).
		t.Bridge = port
		t.VLANFiltering = p.read("/sys/class/net/"+port+"/bridge/vlan_filtering") == "1"
	}
	return t
}
