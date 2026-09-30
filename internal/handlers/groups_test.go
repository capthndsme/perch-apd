package handlers

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/capthndsme/perch-agentkit/rpc"
	"github.com/capthndsme/perch-apd/internal/applylock"
	"github.com/capthndsme/perch-apd/internal/groups"
)

func TestGroupsMethods(t *testing.T) {
	d, _, disp := newDeps(t)
	// Off by default: unsupported, and no capability.
	if r := invoke(t, disp, "groups.apply", `{"revision":1}`); r.Error == nil || r.Error.Code != rpc.CodeUnsupported {
		t.Fatalf("%+v", r)
	}
	for _, c := range d.Capabilities(context.Background()) {
		if c == "wifi_groups" {
			t.Fatal("wifi_groups offered while off")
		}
	}

	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "config"), 0o755)
	os.WriteFile(filepath.Join(dir, "config", "wireless"), []byte("\nconfig wifi-iface 'w0'\n\toption mode 'ap'\n\toption ssid 'Apartment'\n\toption encryption 'psk2'\n\toption key 'building-key-1'\n"), 0o600)
	os.WriteFile(filepath.Join(dir, "config", "network"), []byte("\nconfig interface 'lan'\n\toption device 'eth0'\n"), 0o644)
	var cmds []string
	eng, err := groups.New(groups.Options{
		StateDir: filepath.Join(dir, "state"), WirelessPath: filepath.Join(dir, "config", "wireless"),
		NetworkPath: filepath.Join(dir, "config", "network"), StagingDir: filepath.Join(dir, "uci"),
		FS: groups.FS{Root: dir}, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Run: func(_ context.Context, name string, args ...string) error {
			cmds = append(cmds, name+" "+strings.Join(args, " "))
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	d.Groups = eng
	// Plain ws:// without the insecure option: refused, not offered.
	d.GroupsRefusal = "device groups over a plain http:// controller need option wifi_groups_insecure '1'"
	r := invoke(t, disp, "groups.apply", `{"revision":1}`)
	if r.Error == nil || !strings.Contains(string(mustJSON(r.Error.Data)), "insecure_transport") {
		t.Fatalf("%+v", r.Error)
	}
	d.GroupsRefusal = ""
	found := false
	for _, c := range d.Capabilities(context.Background()) {
		found = found || c == "wifi_groups"
	}
	if !found {
		t.Fatal("wifi_groups not offered")
	}
	// Bad params are invalid params; a plan refusal carries its code.
	if r := invoke(t, disp, "groups.apply", `{"revision":0}`); r.Error == nil || r.Error.Code != rpc.CodeInvalidParams {
		t.Fatalf("%+v", r.Error)
	}
	r = invoke(t, disp, "groups.apply", `{"revision":2,"trunk":"eth0","ssids":["Apartment"],"vlans":[{"vid":101}],"stations":[{"key":"unit-101-key","vid":101}],"confirmSeconds":30}`)
	if r.Error != nil || !strings.Contains(string(r.Result), `"state":"pending_confirm"`) {
		t.Fatalf("%s %+v", r.Result, r.Error)
	}
	if r := invoke(t, disp, "groups.confirm", `{"revision":2}`); r.Error != nil {
		t.Fatalf("%+v", r.Error)
	}
	r = invoke(t, disp, "groups.state", "")
	if r.Error != nil || !strings.Contains(string(r.Result), `"appliedRevision":2`) {
		t.Fatalf("%s %+v", r.Result, r.Error)
	}
	if len(cmds) != 1 || cmds[0] != "/etc/init.d/network reload" {
		t.Fatalf("reloads %v", cmds)
	}
}

// A groups.apply while another writer holds the AP's write lock is busy
// with that writer's reason.
func TestGroupsApplyBusyReason(t *testing.T) {
	d, _, disp := newDeps(t)
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "config"), 0o755)
	os.WriteFile(filepath.Join(dir, "config", "wireless"), []byte("\nconfig wifi-iface 'w0'\n\toption mode 'ap'\n\toption ssid 'Apartment'\n\toption encryption 'psk2'\n\toption key 'building-key-1'\n"), 0o600)
	os.WriteFile(filepath.Join(dir, "config", "network"), []byte("\nconfig interface 'lan'\n\toption device 'eth0'\n"), 0o644)
	lock := applylock.New()
	eng, err := groups.New(groups.Options{
		StateDir: filepath.Join(dir, "state"), WirelessPath: filepath.Join(dir, "config", "wireless"),
		NetworkPath: filepath.Join(dir, "config", "network"), StagingDir: filepath.Join(dir, "uci"),
		RpcdDir: filepath.Join(dir, "rpcd"), FS: groups.FS{Root: dir}, Lock: lock,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	d.Groups = eng
	hold, _ := lock.TryAcquire(applylock.Plane, "a4-000000000001")
	r := invoke(t, disp, "groups.apply", `{"revision":2,"trunk":"eth0","ssids":["Apartment"],"vlans":[{"vid":101}],"stations":[{"key":"unit-101-key","vid":101}]}`)
	if r.Error == nil || r.Error.Code != rpc.CodeCommandFailed || string(mustJSON(r.Error.Data)) != `{"error":"busy","reason":"plane_pending"}` {
		t.Fatalf("%+v", r.Error)
	}
	hold.Release()
	if r := invoke(t, disp, "groups.apply", `{"revision":2,"trunk":"eth0","ssids":["Apartment"],"vlans":[{"vid":101}],"stations":[{"key":"unit-101-key","vid":101}]}`); r.Error != nil {
		t.Fatalf("%+v", r.Error)
	}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
