// Package prom renders the Prometheus text exposition format (0.0.4).
//
// A Builder collects samples per metric family and renders every family
// once, samples grouped, in first-seen order: a collector may add samples to
// a family in any order.
package prom

import (
	"bytes"
	"math"
	"strconv"
	"strings"
)

// Metric types.
const (
	Gauge   = "gauge"
	Counter = "counter"
)

// Label is one name="value" pair. Order is kept as given.
type Label struct {
	Name  string
	Value string
}

// L builds a label list from name, value pairs.
func L(pairs ...string) []Label {
	out := make([]Label, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, Label{pairs[i], pairs[i+1]})
	}
	return out
}

type family struct {
	name    string
	typ     string
	samples bytes.Buffer
}

// Builder accumulates families.
type Builder struct {
	order    []*family
	families map[string]*family
}

// NewBuilder returns an empty Builder.
func NewBuilder() *Builder { return &Builder{families: map[string]*family{}} }

func (b *Builder) family(name, typ string) *family {
	if f, ok := b.families[name]; ok {
		return f
	}
	f := &family{name: name, typ: typ}
	b.families[name] = f
	b.order = append(b.order, f)
	return f
}

// Add records a float sample.
func (b *Builder) Add(name, typ string, labels []Label, v float64) {
	b.write(name, typ, labels, formatFloat(v))
}

// AddInt records an integer sample.
func (b *Builder) AddInt(name, typ string, labels []Label, v int64) {
	b.write(name, typ, labels, strconv.FormatInt(v, 10))
}

// AddUint records an unsigned sample (64-bit counters stay exact).
func (b *Builder) AddUint(name, typ string, labels []Label, v uint64) {
	b.write(name, typ, labels, strconv.FormatUint(v, 10))
}

// Declare makes a family appear (with its TYPE line) even without samples,
// the way node_exporter-lua prints families it has no data for.
func (b *Builder) Declare(name, typ string) { b.family(name, typ) }

func (b *Builder) write(name, typ string, labels []Label, value string) {
	f := b.family(name, typ)
	f.samples.WriteString(name)
	if len(labels) > 0 {
		f.samples.WriteByte('{')
		for i, l := range labels {
			if i > 0 {
				f.samples.WriteByte(',')
			}
			f.samples.WriteString(l.Name)
			f.samples.WriteString(`="`)
			f.samples.WriteString(escapeLabel(l.Value))
			f.samples.WriteByte('"')
		}
		f.samples.WriteByte('}')
	}
	f.samples.WriteByte(' ')
	f.samples.WriteString(value)
	f.samples.WriteByte('\n')
}

// Len is the number of families.
func (b *Builder) Len() int { return len(b.order) }

// Bytes renders the exposition.
func (b *Builder) Bytes() []byte {
	var out bytes.Buffer
	for _, f := range b.order {
		out.WriteString("# TYPE ")
		out.WriteString(f.name)
		out.WriteByte(' ')
		out.WriteString(f.typ)
		out.WriteByte('\n')
		out.Write(f.samples.Bytes())
	}
	return out.Bytes()
}

var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

func escapeLabel(s string) string {
	if !strings.ContainsAny(s, "\\\"\n") {
		return s
	}
	return labelEscaper.Replace(s)
}

func formatFloat(v float64) string {
	switch {
	case math.IsNaN(v):
		return "NaN"
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case v == math.Trunc(v) && math.Abs(v) < 1e15:
		return strconv.FormatInt(int64(v), 10)
	default:
		return strconv.FormatFloat(v, 'g', -1, 64)
	}
}
