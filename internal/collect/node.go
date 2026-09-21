package collect

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/capthndsme/perch-agentkit/hoststat"
	"github.com/capthndsme/perch-apd/internal/prom"
	"github.com/capthndsme/perch-apd/internal/sysinfo"
)

// userHZ is the kernel's USER_HZ, 100 on every Linux the agent targets.
const userHZ = 100

// Stat: /proc/stat (boot time, context switches, CPU seconds, forks,
// interrupts, running/blocked processes), with the lua exporter's names.
type Stat struct{ FS FS }

func (Stat) Name() string { return "stat" }

func (c Stat) Collect(_ context.Context, b *prom.Builder) error {
	data, err := c.FS.Read("/proc/stat")
	if err != nil {
		return err
	}
	modes := []string{"user", "nice", "system", "idle", "iowait", "irq", "softirq", "steal", "guest", "guest_nice"}
	scalars := map[string]struct{ name, typ string }{
		"btime":         {"node_boot_time_seconds", prom.Gauge},
		"ctxt":          {"node_context_switches_total", prom.Counter},
		"intr":          {"node_intr_total", prom.Counter},
		"processes":     {"node_forks_total", prom.Counter},
		"procs_running": {"node_procs_running_total", prom.Gauge},
		"procs_blocked": {"node_procs_blocked_total", prom.Gauge},
	}
	values := map[string]uint64{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64*1024), 1<<20) // the intr line is long
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		key := fields[0]
		if strings.HasPrefix(key, "cpu") && key != "cpu" {
			for i, mode := range modes {
				if i+1 >= len(fields) {
					break
				}
				v, err := strconv.ParseUint(fields[i+1], 10, 64)
				if err != nil {
					break
				}
				b.Add("node_cpu_seconds_total", prom.Counter, prom.L("cpu", key, "mode", mode), float64(v)/userHZ)
			}
			continue
		}
		if _, ok := scalars[key]; ok {
			if v, err := strconv.ParseUint(fields[1], 10, 64); err == nil {
				values[key] = v
			}
		}
	}
	for _, key := range []string{"btime", "ctxt", "intr", "processes", "procs_running", "procs_blocked"} {
		if v, ok := values[key]; ok {
			s := scalars[key]
			b.AddUint(s.name, s.typ, nil, v)
		}
	}
	return nil
}

// Loadavg: node_load1/5/15.
type Loadavg struct{ FS FS }

func (Loadavg) Name() string { return "loadavg" }

func (c Loadavg) Collect(_ context.Context, b *prom.Builder) error {
	l, err := c.FS.Loadavg()
	if err != nil {
		return err
	}
	b.Add("node_load1", prom.Gauge, nil, l.Load1)
	b.Add("node_load5", prom.Gauge, nil, l.Load5)
	b.Add("node_load15", prom.Gauge, nil, l.Load15)
	return nil
}

// Meminfo: node_memory_<Key>_bytes for every /proc/meminfo line ("kB"
// values in bytes; "Active(anon)" becomes "Active_anon", as in the lua exporter).
type Meminfo struct{ FS FS }

func (Meminfo) Name() string { return "meminfo" }

func (c Meminfo) Collect(_ context.Context, b *prom.Builder) error {
	entries, err := c.FS.Meminfo()
	if err != nil {
		return err
	}
	rename := strings.NewReplacer("(", "_", ")", "")
	for _, e := range entries {
		b.AddUint("node_memory_"+rename.Replace(e.Key)+"_bytes", prom.Gauge, nil, e.Value)
	}
	return nil
}

// Netdev: /proc/net/dev counters per device.
type Netdev struct{ FS FS }

func (Netdev) Name() string { return "netdev" }

func (c Netdev) Collect(_ context.Context, b *prom.Builder) error {
	devs, err := c.FS.NetDev()
	if err != nil {
		return err
	}
	// Family by family, like node_exporter-lua.
	for i, f := range hoststat.NetDevFields {
		for _, d := range devs {
			b.AddUint("node_network_"+f+"_total", prom.Counter, prom.L("device", d.Name), d.Counters[i])
		}
	}
	return nil
}

// Netclass: /sys/class/net attributes per device.
type Netclass struct{ FS FS }

func (Netclass) Name() string { return "netclass" }

func (c Netclass) Collect(_ context.Context, b *prom.Builder) error {
	entries, err := c.FS.ReadDir("/sys/class/net")
	if err != nil {
		return err
	}
	type num struct {
		file, metric, typ string
		hex               bool
		scale             float64
	}
	nums := []num{
		{"addr_assign_type", "node_network_address_assign_type", prom.Gauge, false, 1},
		{"carrier", "node_network_carrier", prom.Gauge, false, 1},
		{"carrier_changes", "node_network_carrier_changes_total", prom.Counter, false, 1},
		{"carrier_down_count", "node_network_carrier_down_changes_total", prom.Counter, false, 1},
		{"carrier_up_count", "node_network_carrier_up_changes_total", prom.Counter, false, 1},
		{"dev_id", "node_network_device_id", prom.Gauge, true, 1},
		{"dormant", "node_network_dormant", prom.Gauge, false, 1},
		{"flags", "node_network_flags", prom.Gauge, true, 1},
		{"ifindex", "node_network_iface_id", prom.Gauge, false, 1},
		{"iflink", "node_network_iface_link", prom.Gauge, false, 1},
		{"link_mode", "node_network_iface_link_mode", prom.Gauge, false, 1},
		{"mtu", "node_network_mtu_bytes", prom.Gauge, false, 1},
		{"name_assign_type", "node_network_name_assign_type", prom.Gauge, false, 1},
		{"netdev_group", "node_network_net_dev_group", prom.Gauge, false, 1},
		{"type", "node_network_protocol_type", prom.Gauge, false, 1},
		{"speed", "node_network_speed_bytes", prom.Gauge, false, 1000 * 1000 / 8},
		{"tx_queue_len", "node_network_transmit_queue_length", prom.Gauge, false, 1},
	}
	var devs []string
	for _, e := range entries {
		devs = append(devs, e.Name())
	}
	for _, n := range nums {
		for _, dev := range devs {
			s, err := c.FS.ReadTrim("/sys/class/net/" + dev + "/" + n.file)
			if err != nil || s == "" {
				continue // e.g. speed of a down or virtual interface: EINVAL
			}
			var v float64
			if n.hex {
				u, err := strconv.ParseUint(strings.TrimPrefix(s, "0x"), 16, 64)
				if err != nil {
					continue
				}
				v = float64(u)
			} else {
				i, err := strconv.ParseInt(s, 10, 64)
				if err != nil {
					continue
				}
				if n.file == "speed" && i < 0 {
					continue
				}
				v = float64(i)
			}
			b.Add(n.metric, n.typ, prom.L("device", dev), v*n.scale)
		}
	}
	for _, dev := range devs {
		read := func(f string) string {
			s, _ := c.FS.ReadTrim("/sys/class/net/" + dev + "/" + f)
			return s
		}
		b.Add("node_network_info", prom.Gauge, prom.L(
			"device", dev,
			"address", read("address"),
			"broadcast", read("broadcast"),
			"duplex", read("duplex"),
			"operstate", read("operstate"),
			"ifalias", read("ifalias"),
		), 1)
	}
	return nil
}

// Conntrack: node_nf_conntrack_entries(_limit); silently nothing on an AP
// without the conntrack module.
type Conntrack struct{ FS FS }

func (Conntrack) Name() string { return "conntrack" }

func (c Conntrack) Collect(_ context.Context, b *prom.Builder) error {
	ct := c.FS.Conntrack()
	if ct.HasEntries {
		b.AddUint("node_nf_conntrack_entries", prom.Gauge, nil, ct.Entries)
	}
	if ct.HasLimit {
		b.AddUint("node_nf_conntrack_entries_limit", prom.Gauge, nil, ct.Limit)
	}
	return nil
}

// Filefd: node_filefd_allocated / _maximum.
type Filefd struct{ FS FS }

func (Filefd) Name() string { return "filefd" }

func (c Filefd) Collect(_ context.Context, b *prom.Builder) error {
	s, err := c.FS.ReadTrim("/proc/sys/fs/file-nr")
	if err != nil {
		return err
	}
	f := strings.Fields(s)
	if len(f) < 3 {
		return errors.New("short file-nr")
	}
	alloc, _ := strconv.ParseUint(f[0], 10, 64)
	max, _ := strconv.ParseUint(f[2], 10, 64)
	b.AddUint("node_filefd_allocated", prom.Gauge, nil, alloc)
	b.AddUint("node_filefd_maximum", prom.Gauge, nil, max)
	return nil
}

// Entropy: node_entropy_available_bits / _pool_size_bits.
type Entropy struct{ FS FS }

func (Entropy) Name() string { return "entropy" }

func (c Entropy) Collect(_ context.Context, b *prom.Builder) error {
	avail, err := c.FS.ReadTrim("/proc/sys/kernel/random/entropy_avail")
	if err != nil {
		return err
	}
	if v, err := strconv.ParseUint(avail, 10, 64); err == nil {
		b.AddUint("node_entropy_available_bits", prom.Gauge, nil, v)
	}
	if pool, err := c.FS.ReadTrim("/proc/sys/kernel/random/poolsize"); err == nil {
		if v, err := strconv.ParseUint(pool, 10, 64); err == nil {
			b.AddUint("node_entropy_pool_size_bits", prom.Gauge, nil, v)
		}
	}
	return nil
}

// Time: node_time_seconds (whole seconds, like the lua exporter).
type Time struct{ Now func() time.Time }

func (Time) Name() string { return "time" }

func (c Time) Collect(_ context.Context, b *prom.Builder) error {
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	b.AddInt("node_time_seconds", prom.Counter, nil, now().Unix())
	return nil
}

// Uname: node_uname_info.
type Uname struct {
	Uname func(*unix.Utsname) error
}

func (Uname) Name() string { return "uname" }

func (c Uname) Collect(_ context.Context, b *prom.Builder) error {
	var u unix.Utsname
	fn := c.Uname
	if fn == nil {
		fn = unix.Uname
	}
	if err := fn(&u); err != nil {
		return err
	}
	s := func(v []byte) string { return unix.ByteSliceToString(v) }
	b.Add("node_uname_info", prom.Gauge, prom.L(
		"domainname", s(u.Domainname[:]),
		"machine", s(u.Machine[:]),
		"nodename", s(u.Nodename[:]),
		"release", s(u.Release[:]),
		"sysname", s(u.Sysname[:]),
		"version", s(u.Version[:]),
	), 1)
	return nil
}

// OpenWrt: node_openwrt_info from `system board` / /etc/openwrt_release.
type OpenWrt struct{ Info *sysinfo.Info }

func (OpenWrt) Name() string { return "openwrt" }

func (c OpenWrt) Collect(ctx context.Context, b *prom.Builder) error {
	bd := c.Info.Board(ctx)
	if bd.Distribution == "" && bd.Release == "" {
		return errors.New("not OpenWrt")
	}
	b.Add("node_openwrt_info", prom.Gauge, prom.L(
		"board_name", bd.BoardName,
		"id", bd.Distribution,
		"model", bd.Model,
		"release", bd.Release,
		"revision", bd.Revision,
		"system", bd.System,
		"target", bd.Target,
	), 1)
	return nil
}
