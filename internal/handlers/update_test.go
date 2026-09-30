package handlers

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/capthndsme/perch-agentkit/rpc"
	"github.com/capthndsme/perch-agentkit/update"
)

type fakeUpdater struct {
	capability string
	registered bool
}

func (f *fakeUpdater) Status(context.Context) update.Status {
	return update.Status{Protocol: 1, Enabled: true, InstallKind: update.InstallSwapped, Methods: []string{update.MethodBinary}, Results: []update.Result{}}
}
func (f *fakeUpdater) Capability() string { return f.capability }
func (f *fakeUpdater) Register(d *rpc.Dispatcher) {
	f.registered = true
	d.Register("agent.update.status", func(ctx context.Context, _ json.RawMessage) (any, error) { return f.Status(ctx), nil })
}

// system.info carries the update block; agent_update is a capability only
// while nothing refuses an update; agent.update.* is registered.
func TestSystemInfoUpdateBlock(t *testing.T) {
	d, _, _ := newDeps(t)
	info := d.SystemInfo(context.Background())
	if info.Update != nil || slices.Contains(info.Capabilities, "agent_update") {
		t.Fatalf("without an updater: %+v", info.Update)
	}
	b, _ := json.Marshal(info)
	var raw map[string]any
	json.Unmarshal(b, &raw)
	if _, ok := raw["update"]; ok {
		t.Fatal("an update key without an updater")
	}

	u := &fakeUpdater{capability: update.Capability}
	d.Update = u
	disp := rpc.NewDispatcher()
	Register(disp, d)
	if !u.registered {
		t.Fatal("agent.update.* not registered")
	}
	resp := invoke(t, disp, "system.info", "")
	if resp.Error != nil {
		t.Fatal(resp.Error)
	}
	var got struct {
		Capabilities []string       `json:"capabilities"`
		Update       *update.Status `json:"update"`
	}
	json.Unmarshal(resp.Result, &got)
	if got.Update == nil || got.Update.InstallKind != update.InstallSwapped || !slices.Contains(got.Capabilities, "agent_update") {
		t.Fatalf("system.info %s", resp.Result)
	}
	if resp := invoke(t, disp, "agent.update.status", "{}"); resp.Error != nil {
		t.Fatal(resp.Error)
	}
	// Refused (self_update '0', no key…): the block says why, no capability.
	u.capability = ""
	info = d.SystemInfo(context.Background())
	if info.Update == nil || slices.Contains(info.Capabilities, "agent_update") {
		t.Fatalf("refused: %+v %v", info.Update, info.Capabilities)
	}
}
