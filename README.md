# Perch AP Daemon (`perch-apd`)

The access point daemon of [Perch](https://github.com/capthndsme/perch-controller): one
static binary per OpenWrt access point that

1. **replaces `prometheus-node-exporter-lua-*`.** It collects the AP's Wi-Fi interfaces
   and associated stations (from nl80211, the same kernel calls iwinfo makes), network
   counters, CPU, memory and conntrack with the lua exporter's metric names and labels,
   and **pushes** them to the Perch Network Controller on the interval the controller sets. It also
   fills the per-station byte counters the lua exporter declares but never emits, and sends
   the AP's Ethernet ports with their link state (which socket has a cable, at what speed)
   for the controller's infrastructure view.
2. **bridges the AP to the Perch Network Controller** on the same WebSocket.
   Through it the dashboard can kick or steer a client, blink the AP's LEDs to find it on
   a shelf, and reboot it, without SSH keys on the server.

The daemon opens no port: it dials out to the controller (so NAT and firewalls are not a
problem) and nothing can connect to it. It executes a fixed list of methods
(PROTOCOL.md); there is no remote shell.

## Install

In the dashboard: **Settings → Wi-Fi sources → New join token**. It shows these commands
with your controller URL and token filled in.

**One-liner** (picks the binary for the router's architecture and checks its checksum):

```sh
wget -qO- https://github.com/capthndsme/perch-apd/releases/latest/download/install.sh \
  | sh -s -- --controller http://192.168.1.10:8080 --token mlap_...
```

The controller URL is the address you open the dashboard at. For the default Docker
install that is plain HTTP, as above, which is supported but encrypts nothing between
the AP and the controller: keep the controller and the APs' management addresses on a
management VLAN that client devices cannot reach (worth it with HTTPS too), or serve the
controller over HTTPS (`--controller https://perch.example.com`). The dashboard marks APs that connect over
plain HTTP. Why and how:
[Plain HTTP and a management VLAN](https://github.com/capthndsme/perch-controller#plain-http-and-a-management-vlan).

**By hand:** download the binary for your AP and let it install itself.

| Asset | For |
|---|---|
| `perch-apd-linux-mipsle` | MT7621, MT7628 (`mipsel_24kc`) |
| `perch-apd-linux-mips` | Atheros/QCA ath79 (`mips_24kc`) |
| `perch-apd-linux-armv7` | IPQ40xx, IPQ806x, mvebu, sunxi (ARMv7 with VFP) |
| `perch-apd-linux-armv5` | kirkwood, bcm53xx and other ARM without VFP |
| `perch-apd-linux-arm64` | Filogic MT798x, IPQ807x, BCM27xx 64-bit (`aarch64`) |
| `perch-apd-linux-amd64` | x86_64 |

```sh
cd /tmp
wget -O perch-apd https://github.com/capthndsme/perch-apd/releases/latest/download/perch-apd-linux-mipsle
chmod +x perch-apd
./perch-apd --install              # asks for the controller URL and the join token
```

`--install` copies the binary to `/opt/perch-apd/`, installs the procd service
(`/etc/init.d/perch-apd`, enabled at boot), writes `/etc/config/perch-apd`,
joins the controller right away (so a wrong token shows up now, not in a log later)
and starts the service. The files are listed in `/lib/upgrade/keep.d/perch-apd`,
so a sysupgrade that keeps settings keeps the daemon too. Add `--controller`, `--token`
and `--yes` for an unattended install.

**As an OpenWrt package:** every release also carries `.ipk` files for OpenWrt 24.10
(opkg) and `.apk` files for 25.12 (apk-tools), built with the official SDKs for
`mipsel_24kc` (MT7621, MT7628), `mips_24kc` (ath79), `aarch64_cortex-a53` (Filogic
MT798x, IPQ807x), `arm_cortex-a7_neon-vfpv4` (IPQ40xx) and `x86_64`.
`. /etc/openwrt_release; echo $DISTRIB_ARCH` names the AP's package architecture (on 25.12
`apk --print-arch` prints only the base one, e.g. `mipsel`).

```sh
V=0.1.2 ARCH=mipsel_24kc
BASE=https://github.com/capthndsme/perch-apd/releases/download/v$V
# OpenWrt 24.10
opkg update && opkg install $BASE/perch-apd_$V-r1_$ARCH.ipk
# OpenWrt 25.12
wget -O /tmp/perch-apd.apk $BASE/perch-apd_$V-r1_$ARCH.apk
apk add --allow-untrusted /tmp/perch-apd.apk      # signed with the SDK's build key, not OpenWrt's
perch-apd join --controller http://192.168.1.10:8080 --token mlap_...
/etc/init.d/perch-apd enable && /etc/init.d/perch-apd start
```

**Upgrading** is the same install with the newer package; the configuration and the
agent's credentials stay (both package managers keep a modified config and put the new
default beside it as `perch-apd-opkg` / `perch-apd.apk-new`, which can be deleted). The
daemon moves to the new version by itself: opkg stops it before replacing the binary, and
the package restarts it after an apk upgrade (the upgrade to 0.1.2 included; upgrading to
0.1.1 or older with apk needs `/etc/init.d/perch-apd restart`). Old and new binary take
flash side by side for a moment: on a 16 MB-flash router, have about 3.5 MB free.

The package installs `/usr/bin/perch-apd` and the same init script and config as
`--install`, and depends only on `ca-bundle`. It is built by the SDK's Go and linked
against the router's musl libc (OpenWrt's Go packaging enables cgo), so it is smaller
than the static binary: on `mipsel_24kc` (OpenWrt 24.10, Go 1.23) the package is
2.4 MB and the installed binary 7.0 MB. On 16 MB-flash routers the package inside
the squashfs image is the better fit.

To build a package yourself, from a checkout with Docker (the SDK image does the work):

```sh
scripts/openwrt-package.sh mipsel_24kc 24.10.8    # out/openwrt/perch-apd_<version>-r1_mipsel_24kc.ipk
scripts/openwrt-package.sh mipsel_24kc 25.12.5    # … .apk
```

`openwrt/sdk.env` pins the releases and architectures `.github/workflows/openwrt.yml`
builds on every tag. Or add the feed to an SDK or buildroot of your own:

```sh
echo 'src-git perch_apd https://github.com/capthndsme/perch-apd.git;main' >> feeds.conf
./scripts/feeds update perch_apd && ./scripts/feeds install perch-apd
make menuconfig                 # Network → Network Monitoring → perch-apd
make package/perch-apd/compile
```

The feed needs the packages feed's Go (`lang/golang`, Go ≥ 1.22).

## After installing

```sh
logread -e perch-apd                  # "joined the controller", "connected to the controller"
perch-apd join --token mlap_...       # after "Forget agent" in the dashboard, or a new controller
perch-apd uninstall [--purge]         # /opt install only; packages: opkg remove / apk del
```

When the dashboard shows the AP as connected, `prometheus-node-exporter-lua*` is no
longer needed: nothing scrapes the AP any more. The collector packages depend on the
base package, so they go first (`join` and `--install` print the line for the AP's
package manager):

```sh
# OpenWrt 24.10 (opkg)
opkg remove $(opkg list-installed | cut -d' ' -f1 | grep '^prometheus-node-exporter-lua-')
opkg remove --autoremove prometheus-node-exporter-lua
# OpenWrt 25.12 (apk)
apk del $(apk info | grep '^prometheus-node-exporter-lua')
```

An AP the dashboard already scraped over HTTP is recognised by its BSSIDs when it
joins, and keeps its history.

## Configuration

`/etc/config/perch-apd`, section `config agent 'main'`:

| Option | Default | |
|---|---|---|
| `enabled` | `1` | |
| `controller` | | Perch Network Controller URL, e.g. `http://192.168.1.10:8080` or `https://perch.example.com` (a path prefix is kept) |
| `join_token` | | one-time; cleared after a successful join |
| `agent_id`, `agent_secret` | | issued by the controller; forgetting the agent in the dashboard revokes them |
| `tls_insecure` | `0` | accept a self-signed certificate |
| `ca_file` | | extra CA bundle (PEM) for a private CA |
| `ports` | `1` | send the Ethernet ports and their link state with every push; `0` leaves them out |
| `log_level` | `info` | `debug`, `info`, `warn`, `error` |

How often metrics are pushed is not configured here: the server sends it (the AP's poll
interval under Settings → Wi-Fi sources) when the daemon connects, and again when it
changes.

`/etc/init.d/perch-apd restart` after editing (a `uci commit` + `reload_config` does it too).

## Commands

```
perch-apd install | --install     install into /opt/perch-apd and join
perch-apd join                    (re)join a controller
perch-apd uninstall | --uninstall remove the /opt install (--purge: also the config)
perch-apd run                     the daemon (what the init script starts)
perch-apd metrics [--collect wifi,netdev]   print the metrics once
perch-apd clients                 associated Wi-Fi clients, JSON
perch-apd info                    what the controller sees (system.info), JSON
perch-apd ports                   the Ethernet ports and their link state, JSON (reads /sys only)
perch-apd version
```

## Over the WebSocket

The daemon pushes `metrics.push` (the Prometheus text, plus the Ethernet ports unless
`option ports '0'`) every interval the controller set with `agent.configure`. The
controller can call:

| Method | Does |
|---|---|
| `system.info` | model, release, kernel, radios, interfaces, capabilities (`ports` when the pushes carry them) |
| `clients.list` | associated stations with signal, rates, bytes, connected time |
| `client.kick` | `ubus call hostapd.<ifname> del_client` (optional ban time = steering) |
| `locate.start` / `locate.stop` | blink every LED, then restore each LED's trigger and settings |
| `system.reboot` | reboot after answering |
| `ping` | round trip |

The ports are what `perch-apd ports` prints: the switch ports (not the switch's CPU
port), per-port netdevs and a separate WAN MAC, but no bridges, VLANs or Wi-Fi
interfaces. They come in the order of the case, with `wan` / `lan` roles from
`/etc/board.json`. A WAN socket used as a LAN uplink stays `wan`.

Pushes are compressed (permessage-deflate, about 7× smaller) when the controller
enables it: Perch Network Controller newer than 0.2.0. With an older controller the
session simply runs uncompressed. Wire format and error codes: [PROTOCOL.md](PROTOCOL.md).

## Resource use

Measured on a TP-Link Archer AX23 (MT7621, OpenWrt 25.12) pushing every 5 s:

| | 0.1.0 | 0.1.1 |
|---|---|---|
| CPU per push | ~100 ms | 54 ms (about 1% of one core) |
| bytes on the wire per push | 23 KB | 3.7 KB (permessage-deflate) |
| resident memory | 14.0 MB | 12.4 MB |

On 32-bit CPUs the daemon runs Go on one thread (`GOMAXPROCS=1`) unless `GOMAXPROCS` is
set: MIPS32 and ARMv5 emulate 64-bit atomics with locks, and with a thread per hardware
thread the scheduler and the idle GC workers spent more CPU than the work did. A push is
also encoded in one pass instead of three. The daemon sets a 32 MiB soft memory limit
for the Go runtime unless `GOMEMLIMIT` is set.

The binary has no HTTP server; most of it is Go's TLS and HTTP client, which the
WebSocket needs. Its size depends on the Go release it is built with more than on
anything in this repository (MIPS, stripped, same code):

| Go | mipsle | gzip (≈ flash on JFFS2/UBIFS) | arm64 |
|---|---|---|---|
| 1.23 (OpenWrt 24.10 SDK) | 6.9 MB | 2.4 MB | 6.1 MB |
| 1.26 (release builds) | 7.9 MB | 2.8 MB | 6.6 MB |
| 1.27 | 8.5 MB | 3.0 MB | 7.1 MB |

Releases pin the older supported Go line (`release.yml`) for that reason.

## Troubleshooting

- **`join` or the log says `no such host` although the name resolves elsewhere**: the
  controller's name points at a LAN address and the AP's dnsmasq drops such answers
  (DNS rebind protection; `logread` shows `possible DNS-rebind attack detected`). Allow
  that one name on the AP (the daemon's message prints this line with the name filled
  in):
  `uci add_list dhcp.@dnsmasq[0].rebind_domain='perch.example.com'; uci commit dhcp; service dnsmasq reload`.
  It lives in `/etc/config/dhcp`, so it survives upgrades.
- **`HTTP 404 … no AP daemon support`**: the controller predates the AP daemon, or the
  URL points somewhere else.
- **WebSocket fails behind a reverse proxy**: the proxy must pass `Upgrade`. Apache 2.4.47+:
  `ProxyPass / http://127.0.0.1:12553/ upgrade=websocket`; nginx: `proxy_http_version 1.1`
  and the `Upgrade`/`Connection` headers.
- **`x509: certificate signed by unknown authority`**: install `ca-bundle`, set `ca_file`,
  or `tls_insecure '1'` for a self-signed certificate.
- **The AP was forgotten in the dashboard** ("Forget agent"): create a new join token and
  run `perch-apd join --token …`.

## Development

```sh
make test        # go test ./...
make build       # out/perch-apd for this machine
make release     # dist/: every architecture, install.sh, checksums.txt
```

Pure Go, no cgo: `CGO_ENABLED=0` cross-compiles to every target (MIPS with
`GOMIPS=softfloat`). Pushing a `v*` tag runs `.github/workflows/release.yml`, which
publishes the assets the install commands download.

The WebSocket session (dial, pings, JSON-RPC, calls, the push scheduler, reconnect
backoff) and the `/proc` parsers for load, memory, interface counters and conntrack come
from [perch-agentkit](https://github.com/capthndsme/perch-agentkit), which the
[Perch Network Collector](https://github.com/capthndsme/perch-collector) uses too. To
work on both at once, check them out side by side and use a `go.work` with
`use ./perch-apd ./perch-agentkit` plus
`replace github.com/capthndsme/perch-agentkit v0.1.0 => ./perch-agentkit`.

## License

MIT
