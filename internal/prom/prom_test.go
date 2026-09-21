package prom

import (
	"math"
	"testing"
)

func TestBuilderGroupsFamiliesInFirstSeenOrder(t *testing.T) {
	b := NewBuilder()
	b.Add("node_load1", Gauge, nil, 0.04)
	b.AddUint("node_network_receive_bytes_total", Counter, L("device", "lan1"), 18148368942)
	b.Add("node_load5", Gauge, nil, 0.03)
	b.AddUint("node_network_receive_bytes_total", Counter, L("device", "wan"), 0)
	b.Declare("wifi_station_signal_dbm", Gauge)
	b.AddInt("x", Gauge, L("ssid", `a "quoted"\ssid`, "nl", "a\nb"), -44)
	b.Add("f", Gauge, nil, 1.5)
	b.Add("nan", Gauge, nil, math.NaN())
	b.Add("big", Counter, nil, 1e20)

	want := `# TYPE node_load1 gauge
node_load1 0.04
# TYPE node_network_receive_bytes_total counter
node_network_receive_bytes_total{device="lan1"} 18148368942
node_network_receive_bytes_total{device="wan"} 0
# TYPE node_load5 gauge
node_load5 0.03
# TYPE wifi_station_signal_dbm gauge
# TYPE x gauge
x{ssid="a \"quoted\"\\ssid",nl="a\nb"} -44
# TYPE f gauge
f 1.5
# TYPE nan gauge
nan NaN
# TYPE big counter
big 1e+20
`
	if got := string(b.Bytes()); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	if b.Len() != 8 {
		t.Fatalf("Len = %d", b.Len())
	}
}

func TestUint64StaysExact(t *testing.T) {
	b := NewBuilder()
	b.AddUint("c", Counter, nil, math.MaxUint64)
	if got := string(b.Bytes()); got != "# TYPE c counter\nc 18446744073709551615\n" {
		t.Fatalf("got %q", got)
	}
}
