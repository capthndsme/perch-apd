package groups

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// FS reads the files trunk detection needs (a root for tests).
type FS struct{ Root string }

func (f FS) read(path string) ([]byte, error) {
	return os.ReadFile(filepath.Join(f.Root, path))
}

func (f FS) list(path string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(f.Root, path))
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out, nil
}

// ErrNoTrunk: the port towards the gateway could not be found.
var ErrNoTrunk = errors.New("the port towards the gateway is not known")

// DetectTrunk finds the port the default gateway is behind: the default
// route's device and gateway (/proc/net/route), the gateway's MAC
// (/proc/net/arp), and, when the device is a bridge (or a VLAN of one), the
// bridge port that MAC is learned on (/sys/class/net/<br>/brforward). A
// default route over a plain port is that port.
func DetectTrunk(fs FS) (string, error) {
	dev, gw, err := defaultRoute(fs)
	if err != nil {
		return "", err
	}
	bridge := dev
	if i := strings.LastIndexByte(dev, '.'); i > 0 {
		if _, err := strconv.Atoi(dev[i+1:]); err == nil {
			bridge = dev[:i]
		}
	}
	ports, err := fs.list("sys/class/net/" + bridge + "/brif")
	if err != nil {
		// Not a bridge: the device (or the VLAN's parent) is the port.
		return bridge, nil
	}
	mac, err := neighborMAC(fs, gw)
	if err != nil {
		return "", err
	}
	raw, err := fs.read("sys/class/net/" + bridge + "/brforward")
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrNoTrunk, err)
	}
	portNo, ok := fdbPort(raw, mac)
	if !ok {
		return "", fmt.Errorf("%w: %s is not in %s's forwarding table", ErrNoTrunk, mac, bridge)
	}
	for _, p := range ports {
		b, err := fs.read("sys/class/net/" + bridge + "/brif/" + p + "/port_no")
		if err != nil {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 0, 32)
		if err == nil && int(n) == portNo {
			return p, nil
		}
	}
	return "", fmt.Errorf("%w: port %d of %s", ErrNoTrunk, portNo, bridge)
}

// defaultRoute reads the first IPv4 default route.
func defaultRoute(fs FS) (dev string, gw net.IP, err error) {
	b, err := fs.read("proc/net/route")
	if err != nil {
		return "", nil, err
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 8 || f[1] != "00000000" || f[7] != "00000000" {
			continue
		}
		v, err := strconv.ParseUint(f[2], 16, 32)
		if err != nil || v == 0 {
			continue
		}
		ip := make(net.IP, 4)
		binary.LittleEndian.PutUint32(ip, uint32(v))
		return f[0], ip, nil
	}
	return "", nil, fmt.Errorf("%w: no default route", ErrNoTrunk)
}

func neighborMAC(fs FS, ip net.IP) (net.HardwareAddr, error) {
	b, err := fs.read("proc/net/arp")
	if err != nil {
		return nil, err
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) >= 4 && f[0] == ip.String() {
			mac, err := net.ParseMAC(f[3])
			if err == nil && f[3] != "00:00:00:00:00:00" {
				return mac, nil
			}
		}
	}
	return nil, fmt.Errorf("%w: the gateway %s is not in the neighbour table", ErrNoTrunk, ip)
}

// fdbPort finds a MAC in a brforward dump (struct __fdb_entry, 16 bytes:
// mac[6], port_no, is_local, ageing_timer_value u32, port_hi, pad, unused
// u16) and returns its port number.
func fdbPort(raw []byte, mac net.HardwareAddr) (int, bool) {
	for i := 0; i+16 <= len(raw); i += 16 {
		e := raw[i : i+16]
		if bytes.Equal(e[0:6], mac) && e[7] == 0 {
			return int(e[12])<<8 | int(e[6]), true
		}
	}
	return 0, false
}
