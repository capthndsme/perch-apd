// Package collect produces the agent's metrics: node_exporter-lua's metric
// names and labels, so the Perch Network Controller's parser (and any
// Grafana dashboard built on prometheus-node-exporter-lua) reads it as is.
package collect

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/capthndsme/perch-agentkit/hoststat"
	"github.com/capthndsme/perch-apd/internal/prom"
)

// Collector writes one group of metrics.
type Collector interface {
	Name() string
	Collect(ctx context.Context, b *prom.Builder) error
}

// Registry runs collectors in a fixed order.
type Registry struct {
	collectors []Collector
	log        *slog.Logger
}

// NewRegistry returns a registry over the given collectors.
func NewRegistry(log *slog.Logger, cs ...Collector) *Registry {
	if log == nil {
		log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	return &Registry{collectors: cs, log: log}
}

// Names lists the collectors in order.
func (r *Registry) Names() []string {
	out := make([]string, len(r.collectors))
	for i, c := range r.collectors {
		out[i] = c.Name()
	}
	return out
}

// Gather renders the selected collectors (all when names is empty) plus
// node_exporter's per-collector scrape duration and success.
func (r *Registry) Gather(ctx context.Context, names []string) []byte {
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	b := prom.NewBuilder()
	type result struct {
		name    string
		seconds float64
		ok      bool
	}
	var results []result
	for _, c := range r.collectors {
		if len(want) > 0 && !want[c.Name()] {
			continue
		}
		start := time.Now()
		err := c.Collect(ctx, b)
		if err != nil {
			r.log.Debug("collector failed", "collector", c.Name(), "err", err)
		}
		results = append(results, result{c.Name(), time.Since(start).Seconds(), err == nil})
	}
	for _, res := range results {
		b.Add("node_scrape_collector_duration_seconds", prom.Gauge, prom.L("collector", res.name), res.seconds)
	}
	for _, res := range results {
		v := 0.0
		if res.ok {
			v = 1
		}
		b.Add("node_scrape_collector_success", prom.Gauge, prom.L("collector", res.name), v)
	}
	return b.Bytes()
}

// FS reads files under a root, so tests can point collectors at a fixture
// tree instead of /. It is the kit's reader, so the node collectors and the
// collector daemon's gateway stats share one parser per /proc file.
type FS = hoststat.FS
