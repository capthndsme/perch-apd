package handlers

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/capthndsme/perch-agentkit/rpc"
)

// fakeWifi is a Wi-Fi config plane that answers with what it was asked.
type fakeWifi struct {
	served []string
}

func (f *fakeWifi) Hello(context.Context) any { return map[string]any{"protocol": 1, "access": "read"} }
func (f *fakeWifi) Methods() []string         { return []string{"wifi.capabilities", "wifi.config.read"} }
func (f *fakeWifi) Serve(_ context.Context, method string, params json.RawMessage) (any, error) {
	f.served = append(f.served, method+" "+string(params))
	if method == "wifi.config.read" {
		return nil, &rpc.Error{Code: rpc.CodeUnsupported, Message: "off", Data: map[string]string{"error": "wifi_config_off"}}
	}
	return map[string]string{"method": method}, nil
}

// The plane's methods are on the dispatcher, system.info carries its block
// and the capability wifi_config (whatever the access).
func TestWifiPlaneIsRegisteredAndAnnounced(t *testing.T) {
	d, _, _ := newDeps(t)
	w := &fakeWifi{}
	d.Wifi = w
	disp := rpc.NewDispatcher()
	Register(disp, d)
	if m := invoke(t, disp, "wifi.capabilities", `{"x":1}`); m.Error != nil || string(m.Result) != `{"method":"wifi.capabilities"}` {
		t.Fatalf("capabilities %s %+v", m.Result, m.Error)
	}
	if m := invoke(t, disp, "wifi.config.read", ""); m.Error == nil || m.Error.Code != rpc.CodeUnsupported {
		t.Fatalf("read %+v", m.Error)
	}
	if len(w.served) != 2 || w.served[0] != `wifi.capabilities {"x":1}` {
		t.Fatalf("served %v", w.served)
	}
	if m := invoke(t, disp, "wifi.pair.begin", "{}"); m.Error == nil || m.Error.Code != rpc.CodeMethodNotFound {
		t.Fatalf("pairing is not served yet: %+v", m.Error)
	}
	info := d.SystemInfo(context.Background())
	if !slices.Contains(info.Capabilities, "wifi_config") || info.WifiConfig == nil {
		t.Fatalf("system.info %+v", info)
	}
	b, _ := json.Marshal(info)
	var raw map[string]json.RawMessage
	json.Unmarshal(b, &raw)
	if string(raw["wifiConfig"]) != `{"access":"read","protocol":1}` {
		t.Fatalf("wifiConfig %s", raw["wifiConfig"])
	}
	// Without a plane: neither.
	d.Wifi = nil
	info = d.SystemInfo(context.Background())
	b, _ = json.Marshal(info)
	raw = nil
	json.Unmarshal(b, &raw)
	if slices.Contains(info.Capabilities, "wifi_config") {
		t.Fatal("capability without a plane")
	}
	if _, ok := raw["wifiConfig"]; ok {
		t.Fatal("wifiConfig without a plane")
	}
}
