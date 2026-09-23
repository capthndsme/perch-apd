package groups

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// A binding reuses the SSID's own passphrase for one MAC. If the MAC were
// lost on the way to hostapd (an OpenWrt that reads the station's `mac`
// differently), the entry would match every client, and everyone on the
// building's key would land in that group's VLAN. Before a revision is
// kept, the PSK files hostapd was given are read back: an own passphrase
// held for any MAC fails the confirm and the AP rolls back.

// wildcardMAC is hostapd's "any station".
const wildcardMAC = "00:00:00:00:00:00"

// pskDir is where netifd writes hostapd's PSK files
// (/var/run/hostapd-<ifname>.psk).
const pskDir = "/var/run"

func keyDigest(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func appendUnique(list []string, s string) []string {
	for _, x := range list {
		if x == s {
			return list
		}
	}
	return append(list, s)
}

// pskEntry is one line of a hostapd wpa_psk_file:
// `[keyid=<id>] [vlanid=<vid>] [wps=1] <mac> <passphrase or PSK>`.
type pskEntry struct {
	MAC, Key, VLAN string
}

func parsePSKLine(line string) (pskEntry, bool) {
	rest := strings.TrimSpace(line)
	if rest == "" || rest[0] == '#' {
		return pskEntry{}, false
	}
	var e pskEntry
	for {
		i := strings.IndexAny(rest, " \t")
		if i < 0 {
			return pskEntry{}, false
		}
		tok := rest[:i]
		rest = strings.TrimLeft(rest[i:], " \t")
		if k, v, ok := strings.Cut(tok, "="); ok {
			if k == "vlanid" {
				e.VLAN = v
			}
			continue
		}
		e.MAC = strings.ToLower(tok)
		// The passphrase is the rest of the line: it may hold spaces.
		e.Key = rest
		return e, e.Key != ""
	}
}

// checkPSKFiles reports a binding's passphrase that hostapd holds for any
// MAC (nil = none, or no PSK file to read).
func checkPSKFiles(fs FS, bindingKeys []string) error {
	if len(bindingKeys) == 0 {
		return nil
	}
	guard := map[string]bool{}
	for _, d := range bindingKeys {
		guard[d] = true
	}
	names, err := fs.list(pskDir)
	if err != nil {
		return nil
	}
	for _, name := range names {
		if !strings.HasPrefix(name, "hostapd-") || !strings.HasSuffix(name, ".psk") {
			continue
		}
		b, err := fs.read(pskDir + "/" + name)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n") {
			e, ok := parsePSKLine(line)
			if !ok || e.MAC != wildcardMAC || !guard[keyDigest(e.Key)] {
				continue
			}
			vlan := e.VLAN
			if vlan == "" {
				vlan = "none"
			}
			return fmt.Errorf("%s gives the SSID's own passphrase to every client (VLAN %s): the binding lost its MAC", name, vlan)
		}
	}
	return nil
}
