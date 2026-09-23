package agent

import (
	"encoding/json"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/capthndsme/perch-agentkit/hoststat"
	"github.com/capthndsme/perch-agentkit/rpc"
)

type decodedPush struct {
	Format      string `json:"format"`
	Text        string `json:"text"`
	CollectedAt string `json:"collectedAt"`
	DurationMs  int64  `json:"durationMs"`
	Seq         uint64 `json:"seq"`
}

// viaEncodingJSON is what the text decodes to on the controller when it goes
// through encoding/json, the reference for appendJSONString.
func viaEncodingJSON(t testing.TB, s []byte) string {
	t.Helper()
	b, err := json.Marshal(string(s))
	if err != nil {
		t.Fatal(err)
	}
	var out string
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func checkString(t testing.TB, s []byte) {
	t.Helper()
	got := appendJSONString(nil, s)
	if !json.Valid(got) {
		t.Fatalf("invalid JSON for %q: %s", s, got)
	}
	var decoded string
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("%q: %v", s, err)
	}
	if want := viaEncodingJSON(t, s); decoded != want {
		t.Fatalf("%q decodes to %q, encoding/json gives %q", s, decoded, want)
	}
}

func TestAppendJSONStringMatchesEncodingJSON(t *testing.T) {
	for _, s := range []string{
		"",
		"plain",
		`wifi_station_signal_dbm{ifname="phy1-ap0",mac="02:00:00:00:00:01",ssid="Example"} -61` + "\n",
		`quote " backslash \ slash /`,
		"tab\there\r\nnewline\x00nul\x01\x1f\x7f",
		"<script>&amp;</script>",
		"café ☕ 🛜",
		"line sep para",
		"bad \xff byte, truncated \xe2\x82 rune, lone \x80 continuation",
		"\xed\xa0\x80 surrogate half",
		"trailing \xc3",
	} {
		checkString(t, []byte(s))
	}
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 2000; i++ {
		b := make([]byte, rng.Intn(64))
		rng.Read(b)
		checkString(t, b)
	}
}

func FuzzAppendJSONString(f *testing.F) {
	for _, s := range []string{"", "a\"b\\c\n", "\xff\xfe", " ", "é"} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, s []byte) { checkString(t, s) })
}

func TestPushParamsDecodeLikeNotify(t *testing.T) {
	text := []byte("# HELP node_load1 1m load average.\n# TYPE node_load1 gauge\nnode_load1 0.04\n" +
		`wifi_network_quality{channel="36",ssid="Caf\xe9 \"5G\""} 70` + "\n")
	at := time.Date(2026, 9, 22, 4, 5, 6, 789, time.FixedZone("x", 8*3600))
	params := pushParams(nil, text, at, 1234567*time.Microsecond, 42, nil)

	var got decodedPush
	if err := json.Unmarshal(params, &got); err != nil {
		t.Fatalf("%v: %s", err, params)
	}
	want := decodedPush{
		Format:      "prometheus-text",
		Text:        viaEncodingJSON(t, text),
		CollectedAt: "2026-09-21T20:05:06Z",
		DurationMs:  1234,
		Seq:         42,
	}
	if got != want {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}

	// The same push through the generic path must decode identically.
	generic, err := rpc.Notification("metrics.push", map[string]any{
		"format": "prometheus-text", "text": string(text), "collectedAt": want.CollectedAt,
		"durationMs": want.DurationMs, "seq": want.Seq,
	})
	if err != nil {
		t.Fatal(err)
	}
	var msg struct {
		Params decodedPush `json:"params"`
	}
	if err := json.Unmarshal(generic, &msg); err != nil {
		t.Fatal(err)
	}
	if msg.Params != got {
		t.Fatalf("generic path %+v, one pass %+v", msg.Params, got)
	}
}

func TestPushParamsReuseTheBuffer(t *testing.T) {
	text := make([]byte, 0, 32<<10)
	for len(text) < 30<<10 {
		text = append(text, `wifi_station_receive_bytes_total{ifname="phy0-ap0",mac="02:00:00:00:00:01"} 7366133107`+"\n"...)
	}
	at := time.Now()
	buf := pushParams(nil, text, at, time.Millisecond, 1, nil)
	allocs := testing.AllocsPerRun(20, func() {
		buf = pushParams(buf, text, at, time.Millisecond, 2, nil)
	})
	if allocs != 0 {
		t.Fatalf("%v allocations per push with a reused buffer", allocs)
	}
	// With ports: encoding/json writes into the same buffer, which does not
	// grow again; what is left is the encoder itself.
	ports := examplePorts()
	buf = pushParams(buf, text, at, time.Millisecond, 3, ports)
	allocs = testing.AllocsPerRun(20, func() {
		buf = pushParams(buf, text, at, time.Millisecond, 4, ports)
	})
	if allocs > 3 && !raceEnabled {
		t.Fatalf("%v allocations per push with ports", allocs)
	}
}

func examplePorts() []hoststat.Port {
	up, down := true, false
	speed, changes := 1000, uint64(3)
	return []hoststat.Port{
		{Name: "wan", Label: "wan", Role: "wan", Medium: "copper", MAC: "02:00:00:00:00:11", AdminUp: &up, Carrier: &down, Operstate: "down"},
		{Name: "lan1", Label: "lan1", Role: "lan", Medium: "copper", MAC: "02:00:00:00:00:10", AdminUp: &up, Carrier: &up,
			Operstate: "up", SpeedMbps: &speed, Duplex: "full", CarrierChanges: &changes},
	}
}

func TestPushParamsCarryPorts(t *testing.T) {
	text := []byte("# TYPE node_load1 gauge\nnode_load1 0.04\n")
	at := time.Date(2026, 9, 23, 11, 20, 36, 0, time.UTC)
	ports := examplePorts()
	params := pushParams(nil, text, at, 14*time.Millisecond, 42, ports)
	if !json.Valid(params) {
		t.Fatalf("invalid JSON: %s", params)
	}
	// The member as the controller receives it, in report order, ahead of
	// the text.
	const member = `,"ports":[` +
		`{"name":"wan","label":"wan","role":"wan","medium":"copper","mac":"02:00:00:00:00:11","adminUp":true,"carrier":false,"operstate":"down"},` +
		`{"name":"lan1","label":"lan1","role":"lan","medium":"copper","mac":"02:00:00:00:00:10","adminUp":true,"carrier":true,"operstate":"up","speedMbps":1000,"duplex":"full","carrierChanges":3}` +
		`],"text":`
	if !strings.Contains(string(params), member) {
		t.Fatalf("ports member missing or different:\n%s", params)
	}
	var got struct {
		decodedPush
		Ports []hoststat.Port `json:"ports"`
	}
	if err := json.Unmarshal(params, &got); err != nil {
		t.Fatal(err)
	}
	if want := (decodedPush{"prometheus-text", string(text), "2026-09-23T11:20:36Z", 14, 42}); got.decodedPush != want || !reflect.DeepEqual(got.Ports, ports) {
		t.Fatalf("decoded %+v", got)
	}

	// [] means "looked, found none"; nil leaves the member out.
	for _, tc := range []struct {
		ports []hoststat.Port
		want  string // "" = no ports member
	}{{[]hoststat.Port{}, "[]"}, {nil, ""}} {
		params := pushParams(nil, text, at, time.Millisecond, 1, tc.ports)
		var m map[string]json.RawMessage
		if err := json.Unmarshal(params, &m); err != nil {
			t.Fatalf("%v: %s", err, params)
		}
		if raw, ok := m["ports"]; string(raw) != tc.want || ok != (tc.want != "") {
			t.Fatalf("ports %#v sent as %q (present %v)", tc.ports, raw, ok)
		}
	}
}
