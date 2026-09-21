package agent

import (
	"encoding/json"
	"math/rand"
	"testing"
	"time"

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
	params := pushParams(nil, text, at, 1234567*time.Microsecond, 42)

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
	buf := pushParams(nil, text, at, time.Millisecond, 1)
	allocs := testing.AllocsPerRun(20, func() {
		buf = pushParams(buf, text, at, time.Millisecond, 2)
	})
	if allocs != 0 {
		t.Fatalf("%v allocations per push with a reused buffer", allocs)
	}
}
