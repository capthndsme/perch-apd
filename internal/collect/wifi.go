package collect

import (
	"context"
	"strconv"
	"strings"

	"github.com/capthndsme/perch-apd/internal/nl80211"
	"github.com/capthndsme/perch-apd/internal/prom"
	"github.com/capthndsme/perch-apd/internal/wireless"
)

// Wifi: wifi_network_{quality,noise_dbm,bitrate,signal_dbm} per interface,
// computed the way iwinfo computes them for prometheus-node-exporter-lua-wifi:
// signal and bitrate are the (incremental integer) average over associated
// stations, quality is that signal on iwinfo's -110..-40 dBm scale, noise is
// the in-use survey channel's. No stations: signal -255, quality 0, bitrate 0.
type Wifi struct{ Src *wireless.Source }

func (Wifi) Name() string { return "wifi" }

func (c Wifi) Collect(ctx context.Context, b *prom.Builder) error {
	inv, err := c.Src.Inventory(ctx)
	if err != nil {
		return err
	}
	country := c.Src.RegDomain()
	for _, ifi := range inv.Interfaces {
		labels := []prom.Label{
			{Name: "mode", Value: ifi.Type.IwinfoMode()},
			{Name: "ifname", Value: ifi.Ifname},
			{Name: "ssid", Value: ifi.SSID},
		}
		if ifi.Channel > 0 {
			labels = append(labels, prom.Label{Name: "channel", Value: strconv.Itoa(ifi.Channel)})
		}
		labels = append(labels,
			prom.Label{Name: "device", Value: ifi.Radio},
			prom.Label{Name: "bssid", Value: ifi.BSSID},
		)
		cc := country
		if cc == "" {
			cc = ifi.Country
		}
		if cc != "" {
			labels = append(labels, prom.Label{Name: "country", Value: cc})
		}
		if ifi.FrequencyMHz > 0 {
			labels = append(labels, prom.Label{Name: "frequency", Value: strconv.Itoa(ifi.FrequencyMHz)})
		}

		stations, _ := c.Src.StationsOf(ifi)
		signal, hasSignal, bitrateKbps := iwinfoAverages(stations)

		quality := 0
		if hasSignal {
			quality = iwinfoQualityPercent(signal)
		} else {
			signal = -255
		}
		b.AddInt("wifi_network_quality", prom.Gauge, labels, int64(quality))
		b.AddInt("wifi_network_noise_dbm", prom.Gauge, labels, int64(c.Src.Noise(ifi)))
		b.AddInt("wifi_network_bitrate", prom.Gauge, labels, int64(bitrateKbps))
		b.AddInt("wifi_network_signal_dbm", prom.Gauge, labels, int64(signal))
	}
	return nil
}

// iwinfoAverages replicates nl80211_fill_signal(): an incremental average
// with C integer division, over stations that report a signal (SIGNAL) and
// over those that report a legacy u16 TX bitrate. Bitrate is in kbit/s.
func iwinfoAverages(stations []nl80211.Station) (signal int, hasSignal bool, bitrateKbps int) {
	rssi, rssiN := 0, 0
	rate, rateN := 0, 0
	for _, st := range stations {
		if st.HasSignal {
			rssi = (rssi*rssiN + int(st.Signal)) / (rssiN + 1) // Go truncates toward zero, like C
			rssiN++
		}
		if st.HasTxBitrate16 {
			rate = (rate*rateN + int(st.TxBitrate16)) / (rateN + 1)
			rateN++
		}
	}
	if rateN > 0 {
		bitrateKbps = rate * 100
	}
	return rssi, rssiN > 0, bitrateKbps
}

// iwinfoQualityPercent: iwinfo's quality (signal clamped to -110..-40,
// +110, out of 70), scaled to percent by the lua exporter with floor.
func iwinfoQualityPercent(signal int) int {
	q := signal
	if signal < 0 {
		switch {
		case signal < -110:
			q = 0
		case signal > -40:
			q = 70
		default:
			q = signal + 110
		}
	}
	if q <= 0 {
		return 0
	}
	return int((100.0 / 70.0) * float64(q))
}

// WifiStations: per-station metrics (prometheus-node-exporter-lua-wifi_stations),
// plus the byte counters the lua binding never managed to emit.
type WifiStations struct{ Src *wireless.Source }

func (WifiStations) Name() string { return "wifi_stations" }

func (c WifiStations) Collect(ctx context.Context, b *prom.Builder) error {
	inv, err := c.Src.Inventory(ctx)
	if err != nil {
		return err
	}
	for _, n := range []struct{ name, typ string }{
		{"wifi_stations", prom.Gauge},
		{"wifi_station_signal_dbm", prom.Gauge},
		{"wifi_station_inactive_milliseconds", prom.Gauge},
		{"wifi_station_expected_throughput_kilobits_per_second", prom.Gauge},
		{"wifi_station_transmit_kilobits_per_second", prom.Gauge},
		{"wifi_station_receive_kilobits_per_second", prom.Gauge},
		{"wifi_station_transmit_bytes_total", prom.Counter},
		{"wifi_station_receive_bytes_total", prom.Counter},
		{"wifi_station_transmit_packets_total", prom.Counter},
		{"wifi_station_receive_packets_total", prom.Counter},
	} {
		b.Declare(n.name, n.typ)
	}
	for _, ifi := range inv.Interfaces {
		stations, err := c.Src.StationsOf(ifi)
		if err != nil {
			continue
		}
		for _, st := range stations {
			labels := prom.L("ifname", ifi.Ifname, "mac", strings.ToUpper(st.MAC.String()))
			if sig, ok := wireless.StationSignal(st); ok {
				b.AddInt("wifi_station_signal_dbm", prom.Gauge, labels, int64(sig))
			}
			b.AddUint("wifi_station_inactive_milliseconds", prom.Gauge, labels, uint64(st.InactiveMs))
			if st.HasExpectedThroughput && st.ExpectedThroughputKbps != 0 {
				b.AddUint("wifi_station_expected_throughput_kilobits_per_second", prom.Gauge, labels, uint64(st.ExpectedThroughputKbps))
			}
			if st.HasTxBitrate && st.TxBitrate != 0 {
				b.AddUint("wifi_station_transmit_kilobits_per_second", prom.Gauge, labels, uint64(st.TxBitrate)*100)
			}
			if st.HasRxBitrate && st.RxBitrate != 0 {
				b.AddUint("wifi_station_receive_kilobits_per_second", prom.Gauge, labels, uint64(st.RxBitrate)*100)
			}
			if st.HasBytes {
				b.AddUint("wifi_station_transmit_bytes_total", prom.Counter, labels, st.TxBytes)
				b.AddUint("wifi_station_receive_bytes_total", prom.Counter, labels, st.RxBytes)
			}
			if st.HasPackets {
				b.AddUint("wifi_station_transmit_packets_total", prom.Counter, labels, uint64(st.TxPackets))
				b.AddUint("wifi_station_receive_packets_total", prom.Counter, labels, uint64(st.RxPackets))
			}
		}
		b.AddInt("wifi_stations", prom.Gauge, prom.L("ifname", ifi.Ifname), int64(len(stations)))
	}
	return nil
}
