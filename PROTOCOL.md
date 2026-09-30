# Perch AP Daemon ↔ Perch Network Controller protocol (v1)

The agent on the access point (`perch-apd`, the Perch AP Daemon) talks to the
Perch Network Controller (the *controller*) over two endpoints. Every connection is opened by
the agent; it listens on no port. Metrics are pushed by the agent on the
schedule the controller sets; commands travel the other way on the same
WebSocket.

| Step | Endpoint | Auth |
|---|---|---|
| Join, once | `POST {controller}/api/v1/ap-agent/join` | join token in the body |
| Session, always | `GET {controller}/api/v1/ap-agent/ws` (WebSocket) | `Authorization: Bearer <agentId>.<agentSecret>` |

`{controller}` is the URL the admin typed at install time, e.g.
`https://perch.example.com` (a path prefix is kept: `https://example.com/perch`
→ `https://example.com/perch/api/v1/...`). `http` maps to `ws`, `https` to `wss`.

Everything is JSON (UTF-8). Responses from the REST endpoint are wrapped in
`{ "data": ... }` like the rest of the controller's API.

## 1. Join

The admin creates a *join token* in the dashboard (Settings → Wi-Fi sources).
The agent trades it for its own credentials:

```http
POST /api/v1/ap-agent/join
Content-Type: application/json
User-Agent: perch-apd/0.1.0 (linux/mipsle)

{
  "token": "mlap_7fQ2…",
  "hostname": "ap-garage",
  "model": "TP-Link Archer AX23 v1",
  "boardName": "tplink,archer-ax23-v1",
  "release": "25.12.4",
  "revision": "r32933-4ccb782af7",
  "target": "ramips/mt7621",
  "arch": "mipsle",
  "kernel": "6.12.87",
  "agentVersion": "0.1.0",
  "macs": ["02:00:00:00:00:10", "02:00:00:00:00:11"]
}
```

| Field | Rules |
|---|---|
| `token` | required, 1–128 chars |
| `hostname` | required, 1–120 chars |
| `agentVersion` | required, 1–32 chars |
| `macs` | required array, 0–64 entries, each `xx:xx:xx:xx:xx:xx` (case-insensitive, the server lowercases). Every non-loopback interface MAC of the device, wireless BSSIDs included: this is how the server recognises an AP it already knows. |
| `model`, `boardName` | optional, ≤ 120 chars |
| `release` | optional, ≤ 50 chars (OpenWrt `DISTRIB_RELEASE`) |
| `revision`, `target`, `kernel` | optional, ≤ 64 chars |
| `arch` | optional, ≤ 32 chars (`amd64`, `arm64`, `armv7`, `armv5`, `mipsle`, `mips`) |

**201 Created**

```json
{
  "data": {
    "agentId": "4b9d0c1e2f3a4b5c6d7e8f9012345678",
    "agentSecret": "q0cV8kX1…(43 chars, base64url)",
    "apId": 4,
    "apName": "ap-garage",
    "outcome": "linked"
  }
}
```

`outcome`:

- `rejoined`: an AP that already had an agent matched by MAC (reinstall). Its
  credentials are rotated; the old ones stop working and a live session on
  them is closed with code 4001.
- `linked`: an AP that was so far scraped over HTTP (node_exporter) matched by
  BSSID. It switches to the agent; its history continues under the same `apId`.
- `created`: a new AP.

Errors:

| Status | Body | Agent behaviour |
|---|---|---|
| 401 | `{"error":"invalid_join_token","message":"…"}` (unknown, revoked, expired or used up) | log, retry every 5 min (the admin may fix it) |
| 422 | validation errors | log, retry every 5 min |
| 429 | `{"error":"rate_limited","message":"…","retryAfterSeconds":N}` + `Retry-After` | wait `retryAfterSeconds` |
| 5xx / network | — | exponential backoff 1 s → 30 s (a `Retry-After` under 30 s on a 5xx is honoured) |

The agent stores `agentId` + `agentSecret` in `/etc/config/perch-apd`
and clears `join_token` there. A join token is only ever needed again after
the admin forgets the agent.

## 2. Session (WebSocket)

```http
GET /api/v1/ap-agent/ws HTTP/1.1
Upgrade: websocket
Authorization: Bearer 4b9d0c1e2f3a4b5c6d7e8f9012345678.q0cV8kX1…
Sec-WebSocket-Protocol: perch-ap.v1
Sec-WebSocket-Extensions: permessage-deflate; client_no_context_takeover; server_no_context_takeover
User-Agent: perch-apd/0.1.1 (linux/mipsle)
```

The server selects the subprotocol `perch-ap.v1`. A client that offers
no subprotocol is treated as v1.

Transport: `wss://` when the controller URL is `https://`, plain `ws://` when it is
`http://` (the default Docker install). Plain carries the bearer and everything else
unencrypted; the controller README's "Plain HTTP and a management VLAN" says how to run
it safely.

Compression: since 0.1.1 the agent offers permessage-deflate without context
takeover in either direction (a `metrics.push` is 20–40 KB of Prometheus text
that deflates about 7×; the agent compresses messages of 512 bytes and more).
A controller that does not enable the extension answers without it and the
session runs uncompressed; the controller compresses its own messages of 1 KB
and more.

Handshake failures are plain HTTP responses before the upgrade:

| Status | Meaning | Agent behaviour |
|---|---|---|
| 400 | `{"error":"unsupported_protocol"}`: the agent offered subprotocols, none of them `perch-ap.v1` | log "update the daemon or the controller", retry every 5 min |
| 401 | credentials unknown or revoked | if a `join_token` is configured, join again; else retry every 5 min |
| 404 | the controller has no AP daemon support (older version) | retry every 5 min |
| 429 | too many failed attempts from this address | wait `Retry-After` |
| 503 | server shutting down, or just started (`gateway_starting`, `Retry-After: 1`) | exponential backoff |

Close codes the server uses:

| Code | Meaning | Agent behaviour |
|---|---|---|
| 1001 | server shutting down | reconnect after 2–5 s |
| 4001 | credentials revoked (agent forgotten, AP deleted, rejoined elsewhere) | as a 401 |
| 4002 | replaced by a newer session with the same credentials | reconnect after ≥ 60 s |

Liveness: the server sends a WebSocket ping every 30 s and drops a
connection that has not answered the previous one. The agent pings every
30 s as well (10 s timeout) and reconnects when that fails. Reconnects use
exponential backoff with jitter, 1 s doubling to 30 s (60 s before 1.0.0-rc.2), reset after a session
that lasted a minute.

Frames: one JSON-RPC 2.0 object per text frame, no batches. Max frame 4 MiB.

### 2.1 JSON-RPC

Request (id is a number or string, unique per connection and direction):

```json
{"jsonrpc":"2.0","id":7,"method":"client.kick","params":{"mac":"02:00:00:00:00:01"}}
```

Result / error:

```json
{"jsonrpc":"2.0","id":7,"result":{"mac":"02:00:00:00:00:01","ifname":"phy0-ap0","banTimeMs":0}}
{"jsonrpc":"2.0","id":7,"error":{"code":-32002,"message":"02:00:00:00:00:01 is not associated with this AP"}}
```

Notification (no id, no reply):

```json
{"jsonrpc":"2.0","method":"locate.ended","params":{"reason":"timeout"}}
```

Error codes:

| Code | Name | When |
|---|---|---|
| -32700 | parse error | frame is not JSON |
| -32600 | invalid request | not a JSON-RPC 2.0 request |
| -32601 | method not found | unknown method (older agent) |
| -32602 | invalid params | bad or missing params |
| -32603 | internal error | bug |
| -32000 | command failed | the underlying OS command failed; `message` carries its error |
| -32001 | unsupported | this device cannot do it (no hostapd ubus, no LEDs, …) |
| -32002 | not found | e.g. the client is not associated |

Unknown params are ignored, so the server can send new optional params to
older agents.

### 2.2 Methods the server calls on the agent

Right after every connect the server sends `agent.configure` (§2.3) and then
calls `system.info`.

#### `system.info` → device identity and capabilities

```json
{
  "agentVersion": "0.1.0",
  "protocol": 1,
  "hostname": "ap-garage",
  "model": "TP-Link Archer AX23 v1",
  "boardName": "tplink,archer-ax23-v1",
  "system": "MediaTek MT7621 ver:1 eco:3",
  "release": "25.12.4",
  "revision": "r32933-4ccb782af7",
  "target": "ramips/mt7621",
  "kernel": "6.12.87",
  "arch": "mipsle",
  "uptimeSeconds": 193172,
  "macs": ["02:00:00:00:00:10", "02:00:00:00:00:11"],
  "capabilities": ["metrics", "clients", "kick", "locate", "reboot", "ports"],
  "radios": [
    {"name": "radio0", "band": "2g", "channel": 6, "htmode": "HE20", "country": "PH", "up": true}
  ],
  "interfaces": [
    {"ifname": "phy0-ap0", "radio": "radio0", "mode": "ap", "ssid": "Example", "bssid": "02:00:00:00:00:11",
     "frequencyMhz": 2437, "channel": 6, "band": "2.4", "stations": 3}
  ]
}
```

`capabilities` lists what this build **and** this device can do right now:
`metrics` (pushes, §2.3) always; `clients` when nl80211 is reachable; `kick` when hostapd
exposes `hostapd.<ifname>` on ubus; `locate` when `/sys/class/leds` has LEDs;
`reboot` on OpenWrt; `ports` (1.0.0 and later) when the pushes carry the Ethernet ports
(`option ports '1'`, the default) and the device has at least one; `wifi_config` (1.2.0 and
later) in every build with the Wi-Fi config plane, whatever the AP allows: the result then
also carries `wifiConfig`, the plane's hello (§2.5); `agent_update` (1.2.0 and later) when
the AP can update itself now (§2.6). Every build with the updater adds the `update` block,
also when something refuses an update (its `refusal` says why). `band` in
`interfaces` is `2.4`, `5`, `6` or `60` (the server's convention); `band` in `radios`
is UCI's (`2g`, `5g`, `6g`, `60g`).

#### `clients.list` → associated stations, live

Params: `{"ifname": "phy0-ap0"}` (optional filter).

```json
{"clients": [
  {"mac": "02:00:00:00:00:01", "ifname": "phy0-ap0", "ssid": "Example", "radio": "radio0",
   "band": "2.4", "frequencyMhz": 2437, "signalDbm": -41, "inactiveMs": 10,
   "connectedSeconds": 3600, "rxRateKbps": 72200, "txRateKbps": 72200,
   "expectedThroughputKbps": 64960, "rxBytes": 7366132107, "txBytes": 465135575,
   "rxPackets": 9054069, "txPackets": 7037518, "authorized": true}
]}
```

`rx*` = received by the AP from the client, `tx*` = sent to the client.
Fields the driver does not report are omitted.

#### `client.kick` → disconnect a client

Params:

| Param | Default | |
|---|---|---|
| `mac` | required | client MAC |
| `ifname` | located | hint; when the client is not on it the agent searches every interface |
| `banTimeMs` | `0` | 0–3 600 000; hostapd refuses the client for this long (steering) |
| `reason` | `5` | IEEE 802.11 reason code, 1–65535 |
| `deauth` | `true` | deauthenticate (`true`) or only disassociate |

Runs `ubus call hostapd.<ifname> del_client {addr, reason, deauth, ban_time}`.
Result: `{"mac": "…", "ifname": "phy0-ap0", "banTimeMs": 0}`. Errors: -32602
(bad MAC), -32002 (not associated), -32001 (no hostapd ubus), -32000.

#### `locate.start` / `locate.stop` → blink the LEDs

`locate.start` params: `{"durationSeconds": 30}` (1–600, default 30). Blinks
every LED of the device (timer trigger, 200 ms on/off) and restores each
LED's previous trigger and settings afterwards. Calling it again while
active restarts the timer. Result:
`{"active": true, "durationSeconds": 30, "leds": 6, "endsAt": "2026-09-21T11:21:06Z"}`.
When the time is up the agent sends the notification
`locate.ended {"reason": "timeout"}`.

`locate.stop` → `{"active": false}`.

Errors: -32001 when the device has no LEDs.

#### `system.reboot`

Params: `{"delaySeconds": 2}` (0–60, default 2). The agent answers
`{"scheduled": true, "delaySeconds": 2}` first, then reboots.

#### `ping`

→ `{"pong": true, "time": "2026-09-21T11:20:36.123Z"}`. Used to measure the
round trip.

#### `groups.apply` / `groups.confirm` / `groups.state` → device groups on the AP

Only with `option wifi_groups '1'` (capability `wifi_groups`; over plain `ws://` also
`option wifi_groups_insecure '1'`, else `-32001` with `data.error` `insecure_transport`).
The controller's device groups (controller `docs/gateway/device-groups.md` section 7): the
groups' passphrases and MAC bindings on the listed SSIDs, and the VLANs they land in,
carried tagged to the gateway over the trunk port.

```json
{"revision":7,"confirmSeconds":120,"trunk":"auto","ssids":["Apartment"],
 "vlans":[{"vid":101},{"vid":102}],
 "stations":[{"key":"<group passphrase>","vid":101},{"vid":102,"macs":["02:00:00:00:00:21"]}]}
```

A station without `key` is a binding: the MACs keep the SSID's own passphrase but land in
the VLAN, one `wifi-station` per MAC with a single `option mac` (OpenWrt 24.10 reads `mac` as a
string and drops a `list`; 25.12 splits the option into its array). A group key equal to a
managed interface's own passphrase is skipped with an issue. The agent writes only the sections it
creates plus `dynamic_vlan '1'` on the managed `wifi-iface`s; a trunk port inside an untagged bridge
converts that bridge to VLAN filtering (its interfaces move to `<bridge>.1`). What it changed
in sections it does not own is recorded and put back when no longer needed.
The names it creates are `perch_ws<n>`, `perch_wv<vid>_<iface>`, `perch_v<vid>`, `perch_bv<vid>`,
`perch_bvu`, `perch_dv<vid>`, `perch_bd<vid>`; its state records which ones it created, and each apply
removes exactly those (a state written before 1.2 falls back to these patterns). Any other section,
including other `perch_*` ones (the Wi-Fi config plane's `perch_n*`), is never removed or changed. A
VLAN another section already carries on the bridge is used as it is (an issue says so, and another if
the trunk port is not a tagged member of it); a converted bridge stays converted while it carries a
VLAN the groups did not create. A section of one of these names that the groups did not create
refuses the apply with `name_taken`. `trunk: "auto"`
is the bridge port behind which the default gateway's MAC is learned
(`/sys/class/net/<bridge>/brforward`), or the default route's own port.

Result: `{revision, state: "pending_confirm" | "noop", deadline, trunkPort, bridge, converted,
managed[], issues[]}`. The same revision again answers the same; another one while an apply
waits is `busy` with `data.reason` `groups_pending`. An apply holds the AP's one write lock from
its snapshot until the confirm or the rollback, the lock the Wi-Fi config plane and agent updates
take too: while one of them has a window open, `groups.apply` is `busy` with `data.reason`
`plane_pending` or `update_pending` (nothing is written; the controller retries), and while a LuCI
apply-with-rollback waits for its confirm, `busy` / `luci_pending`. Unless `groups.confirm {revision}` arrives before `deadline`
(`confirmSeconds`, 30-600, default 120) the agent restores the previous `wireless` and
`network` byte for byte and reloads (also at start when the window passed while it was down).
Before it keeps a revision, `groups.confirm` reads back hostapd's PSK files
(`/var/run/hostapd-*.psk`): a binding's passphrase held for any MAC (`00:00:00:00:00:00`, a
binding that lost its MAC on the way to hostapd, which would put every client of the SSID in
that VLAN) rolls back at once and refuses the confirm with `unsafe_binding`.
Refusals (`-32000`, `data.error`): `no_managed_iface`, `trunk_unknown`, `uncommitted`
(changes staged with the uci CLI or in a LuCI session), `busy` (+ `data.reason`), `apply_failed`,
`not_pending`, `unsafe_binding`, `name_taken`, `untagged_vlan_conflict` (a group on the untagged VLAN
of a bridge the groups keep converted), `conversion_conflict`; `-32602` `bad_params`.

`groups.state` → `{appliedRevision, pending: {revision, deadline} | null, lastRollback?,
trunkPort, stations: [{mac, vid, ifname}], issues[]}`: `stations` are the clients on group
VLANs (AP_VLAN interfaces `<ifname>-g<vid>`).

Reloads: the network (`/etc/init.d/network reload`) when the network config changed, else
`wifi reload` (new keys and bindings reach hostapd's PSK list without dropping clients).
After a binding the controller kicks the client (`client.kick`) so it rejoins in its VLAN.

### 2.3 Metrics: pushed by the agent

**Server → agent: `agent.configure`** (notification), the first frame of every
session and again whenever the AP's poll interval or enabled flag changes:

```json
{"jsonrpc":"2.0","method":"agent.configure",
 "params":{"metricsIntervalSeconds":5,
           "collectors":["openwrt","uname","stat","loadavg","meminfo","conntrack","netdev","wifi","wifi_stations"]}}
```

| Param | |
|---|---|
| `metricsIntervalSeconds` | push period; `0` pauses pushing (AP disabled in the dashboard). The agent clamps it to 1–3600 s. |
| `collectors` | which collectors to include (optional; default the list above). Names: `openwrt`, `uname`, `time`, `stat`, `loadavg`, `meminfo`, `netdev`, `netclass`, `conntrack`, `filefd`, `entropy`, `wifi`, `wifi_stations`; unknown names are ignored. |
| `wifiConfig` | (1.2.0 and later) the Wi-Fi config plane's mode for this session (§2.5); absent = off |

**Agent → server: `metrics.push`** (notification): the first right after the
first `agent.configure`, then one per interval, measured from the start of the
previous push (a slow collection does not drift the cadence). A later
`agent.configure` keeps the cadence with the new interval. If no
`agent.configure` arrives within 10 s the agent pushes every 15 s with the
default collectors.

```json
{"jsonrpc":"2.0","method":"metrics.push",
 "params":{"format":"prometheus-text",
           "text":"# TYPE node_load1 gauge\nnode_load1 0.04\n…",
           "collectedAt":"2026-09-21T11:20:36Z","durationMs":14,"seq":42,
           "ports":[
             {"name":"wan","label":"wan","role":"wan","medium":"copper","mac":"02:00:00:00:00:11",
              "adminUp":true,"carrier":false,"operstate":"down","carrierChanges":2},
             {"name":"lan1","label":"lan1","role":"lan","medium":"copper","mac":"02:00:00:00:00:10",
              "adminUp":true,"carrier":true,"operstate":"up","speedMbps":1000,"duplex":"full","carrierChanges":3,
              "rxBytes":17260817141,"txBytes":275427536004,"counterScope":"port"}]}}
```

`text` uses node_exporter-lua's metric names and labels, so the server's
existing parser (and any Grafana dashboard built on the lua exporter) reads
it unchanged. On top of the lua exporter, `wifi_station_{receive,transmit}_bytes_total`
are actually filled (the lua binding never emitted them). Byte and packet
counters in the `wifi_station_*` family are from the AP's side: `receive` =
sent by the client (client upload). `collectedAt` is the agent's clock (for
logs only; the server places data by its own receive time). `seq` counts pushes
since the agent started. The server may drop a push that arrives early or while
the AP is disabled; nothing is resent.

`ports` (1.0.0 and later) lists the AP's Ethernet ports with their link state, on
every push, so inventory and state always travel together. It is the array the Perch
Network Collector reports as `gateway.ports`, made by the same code
([perch-agentkit](https://github.com/capthndsme/perch-agentkit) `hoststat`, from
`/sys/class/net` and `/etc/board.json`). A port is a DSA user port (not the switch's CPU
conduit), a per-port netdev or a second MAC used as WAN; bridges, VLANs, bonds, Wi-Fi
interfaces, tunnels and the like are not. **Absent** means the agent does not report
ports (`option ports '0'`, `/sys/class/net` unreadable, or a version before 1.0.0);
**`[]`** means it looked and found none. The order is the display order: WAN first,
then the board's LAN order, then the rest in natural order (`lan2` before `lan10`); at
most 64 entries. A field the kernel does not answer is left out.

| Field | |
|---|---|
| `name` | the netdev (`lan1`, `wan`, `eth1`): the port's key |
| `label` | the board's label for the socket (devicetree), else the name |
| `role` | `wan` or `lan` as `/etc/board.json` names the port; absent when it does not. A WAN socket used as a LAN uplink (bridged) stays `wan` |
| `medium` | `copper`, `sfp` (an SFP cage in the devicetree), `virtual` (a veth, or a paravirtual NIC such as virtio), `wireless` (an LTE modem) |
| `mac` | the interface's MAC; DSA user ports share their switch's |
| `adminUp` | administratively up |
| `carrier` | a link is detected; absent while the port is administratively down |
| `operstate` | the kernel's word: `up`, `down`, `lowerlayerdown` (a switch port with no cable), … |
| `speedMbps`, `duplex` | the negotiated link (`duplex` `full` or `half`); absent without a link |
| `carrierChanges` | link changes since the interface came up |
| `rxBytes`, `txBytes` | (1.1.0 and later) bytes the port received from its cable and sent into it, cumulative (they restart with the device or the driver); only while the port has a link |
| `counterScope` | what those counters cover: `port`, every frame through the socket (a DSA switch port reads the switch's own counters, `ethtool -S`); `cpu`, only what the AP's CPU sent and received (a switch whose byte counters perch-apd does not know; frames switched between two ports are missing) |

An older controller ignores `ports` (it reads only `format`, `text` and `durationMs`).

### 2.4 Other notifications the agent sends

| Method | Params | |
|---|---|---|
| `locate.ended` | `{"reason": "timeout" \| "stopped"}` | LEDs restored |
| `wifi.config.changed` | `{hashes, changed, origin, applyId?, author, at, uncommitted}` | §2.5 |
| `wifi.config.result` | `{applyId, kind, outcome, reason, at, hashes, discarded?, health?, detail?}` | §2.5 |
| `wifi.health.changed` | `{at, ok, problems}` | §2.5 |
| `agent.update.progress` | `{updateId, phase, file, bytes, totalBytes}` | §2.6 |
| `agent.update.result` | `{updateId, outcome, reason, detail, fromVersion, toVersion, at}` | §2.6 |

The server ignores notifications it does not know; agents ignore requests they
do not know with -32601.

### 2.5 The Wi-Fi config plane (`wifi.*`, 1.2.0 and later)

The controller reads the AP's `/etc/config/wireless` and `/etc/config/network` (secrets only
as fingerprints), hears about every change with who likely made it, and, when the AP allows
it, changes them with a confirm window: the same config plane as the gateway's (controller
`docs/gateway/config-plane.md`), from perch-agentkit's `openwrt/plane`, with a health check of
the AP's own. Only these two configs are ever read or written.

**The owner's opt-in**, in `/etc/config/perch-apd` (the controller can never write that file):

| Option | Default | |
|---|---|---|
| `wifi_config` | `none` without the option; the package's file says `read` | `none`: every `wifi.*` answers `-32001` `{"error":"wifi_config_off"}`; `read`: reads and change notifications; `write`: changes too. Also `-32001` off OpenWrt. `perch-apd wifi access none\|read\|write` sets it and restarts the daemon. |
| `wifi_config_allow` | `wireless`, `network` | a list; any other name is ignored (logged) |
| `wifi_config_insecure` | `0` | `1`: writes over plain `ws://` or an unverified certificate are accepted when signed with a paired key (pairing comes with a later release; until then such writes are refused `not_paired`) |
| `wifi_config_confirm_max` | `900` | upper bound of every confirm window, 30–1800 s |

Writes also need an `https` controller with the certificate verified (`tls_insecure '0'`;
`ca_file` counts as verified), else `insecure_transport`, and the controller's mode `managed`
(else `not_managed`).

**Hello.** `system.info` carries `wifiConfig`: `{protocol: 1, access, accessConfigured?,
transportOk, allowInsecure, allowedConfigs, hashes: {wireless, network, perch-managed},
apply: {state: idle | applying | pending_confirm | rolling_back, applyId?, kind?, deadline?,
protected?, health?: pending | ok | failed}, results: [outcomes not acknowledged, ≤ 32],
signing?: {required, challenge, key, keyId?, windowSeconds}, management?: {network, device,
radios, controllerAddress, reportedAt}, groups: {engine, enabled, state: idle |
pending_confirm, handedOver}}`. Its `hashes` are the baseline of the notifications that follow;
`challenge` is new for every session.

**Mode.** `agent.configure` carries `wifiConfig: {mode: off | observe | managed, authoritative,
watchSeconds (10–600, default 30), debounceSeconds (1–60, 5), healthWaitSeconds (10–300, 45),
fingerprintKey (64 hex)}` at the start of every session and when the mode or the settings
change. Without it (an older controller), and after a session ends, the mode is `off`: no
watching, no applies; confirm, rollback and ack still work.

**Methods** (errors `-32000` with `data.error`, `-32602` `bad_params`):

| Method | Params → result |
|---|---|
| `wifi.capabilities` | `{}` → the plane's part (`protocol, access, allowedConfigs, transportOk, allowInsecure, confirmMaxSeconds, backend: "ubus" \| "uci-cli" \| null, configs, hashes, uncommitted, luciPending, apply, signing?, management`) and the Wi-Fi facts of `perch-apd wifi caps` (`openwrt, packageManager, packages, wifiScripts, schema, hostapd {binary, variant, ubus, features}, regulatory, radios [...], trunk, networks`), plus `guard: "installed" \| "self_installed" \| "missing"` and `groups: {engine, enabled, state, appliedRevision, owned, handedOver}` |
| `wifi.config.read` | `{configs?}` → `{readAt, configs: [{name, hash, missing?, sections: [{name, type, anonymous, index, options, secrets?, hash, owner?}]}], ledger, uncommitted, luciPending, groupsOwned: {dynamicVlan}}`. Secrets are unbound fingerprints: `"hmac:" + 16 hex` of HMAC-SHA256(fingerprintKey, `"<config>.*.<option>=<value>"`), equal for one value on every section and AP (key 32 × `0x44`, `wireless.*.key=correct horse battery` → `hmac:ac349a3eb980336c`). `owner: "groups"` marks the device groups' sections. Refusals `config_not_allowed`, `read_too_large` (a file over 2 MiB or 2000 sections), `read_failed` |
| `wifi.config.apply` | `{applyId (a<apId>-<12 hex>), kind: apply \| revert \| adopt, protected?, confirmTimeoutSeconds?, dryRun?, base, ops, ledger, secrets?, cacAllowanceSeconds?, guards?, expect?: {bss, radios}}` → `{state: pending_confirm \| applied \| noop \| dry_run, applyId, deadline, confirmTimeoutSeconds, protected, hashes, reload: wifi \| network \| none, changes?}`. The ops are the gateway's (`put` / `adopt` / `delete` / `order`, `{"$keep":true}`, `{"$secret":ref}`), at most 2000, and at most 64 secrets (plain strings over verified TLS only). |
| `wifi.config.confirm` | `{applyId}` → `{state: "confirmed", applyId, hashes, health}` |
| `wifi.config.rollback` | `{applyId}` → `{state: "rolling_back", applyId}` (the restore runs after the reply) |
| `wifi.config.ack` | `{applyIds}` → `{acked}`: those results leave the hello |
| `wifi.health` | `{}` → the Wi-Fi as it runs, judged against the committed config: `{checkedAt, ok, pending, radios: [{section, up, pending?, disabled?, retrySetupFailed, channel?, dfs?: {cacActive, cacSecondsLeft}, expected}], bss: [{section, radio, ifname, ssid, status, expected, bssid}], pskGuard: "skipped", problems: [{code, section, message, preexisting?}]}` |

Apply refusals: `not_managed`, `config_not_allowed`, `insecure_transport`,
`signature_required`, `not_paired`, `bad_signature`, `stale_signature`, `replayed`,
`stale_base` (+`hashes`), `busy` (+`reason`: `apply_pending`, `luci_pending`, `uncommitted`,
`groups_pending`, `update_pending`), `foreign_staged`, `not_owned`, `name_taken`, `no_section`,
`invalid_config`, `apply_failed` (+`rolledBack`, `result`), `guard_missing`. The AP refuses
on its own: any op on a section the device groups own or could own (their names `perch_ws*`,
`perch_wv*`, `perch_v*`, `perch_bv*`, `perch_bvu`, `perch_dv*`, `perch_bd*`) → `not_owned`;
creating, renaming or removing a radio (`wifi-device`), or reordering the existing
`wifi-iface`s (their order is the BSSIDs') → `invalid_config`; a non-empty
`guards.pskWildcardDigests` → `bad_params` (this release has no PSK guard); an apply
(not a dry run) while the boot guard is missing → `guard_missing`.

**An apply, on the AP.** The AP's one write lock is taken (the device groups and agent
updates share it: `busy` / `groups_pending`, and `groups.apply` answers `plane_pending` while a
Wi-Fi window is open); `wireless`, `network` and the ledger `/etc/config/perch-managed` are
snapshotted to `/etc/perch-apd/plane/rollback/` (flash) with a marker in `/var/run/perch-apd`
(tmpfs); the change is staged in a private rpcd session and committed (network before
wireless), so procd reloads netifd as after a LuCI save. The reply is `pending_confirm`. The
agent then waits until netifd has no radio or interface pending (up to 30 s), closes the
session (`1000`, "reconnecting after apply …") and dials a new one at once, then every 2 s
until connected. A job on the AP's own path to the controller (its interface, bridge and
VLANs, or the radios of a wireless uplink) gets at least 300 s (`protected`).

**Health check.** From the reconnect on, every 3 s: every radio of the job's expectation is up
(`network.wireless status`: `up`, not `retry_setup_failed`) and every expected BSS beacons its
SSID (`ubus call hostapd.<ifname> get_status`: `status` `ENABLED`, `ssid` as committed).
Expected are the enabled AP interfaces on enabled radios of the committed config, plus the
apply's `expect` (the union). A radar check (`status` `DFS`, `dfs.cac_active`) extends the
wait by its seconds left + 15 s; a channel scan (`ACS`, `HT_SCAN`) or a starting BSS is
waited for. The change is kept only when the check passed **and** the controller confirmed on a
session newer than the apply's: a confirm before is `not_reconnected` (same session) or
`health_pending` (+`health`). A check still failing when its time is up (`healthWaitSeconds`,
longer during a radar check, never past the deadline minus 5 s) rolls the change back at once:
`health_failed`, and a later confirm is `unhealthy` (+`health`, `rolledBack: true`). What was
already broken before the apply (a radio netifd gave up on, a BSS that was down) is reported
with `preexisting: true` and never rolled back for. Problem codes: `radio_down`,
`radio_setup_failed`, `bss_missing`, `bss_disabled`, `ssid_mismatch`, `cac_running`,
`acs_running`, `hostapd_unreachable`, `config_unreadable`, `status_unreadable`.

**Rollback** (the deadline, `wifi.config.rollback`, a failed health check, a failure after
the first commit): the snapshot's files are put back, the services reloaded, the device's own
edits made during the window reported as `discarded`, and `wifi.config.result {applyId, kind,
outcome: rolled_back | failed, reason: confirm_timeout | admin | reboot | commit_failed |
reload_failed | health_failed, at, hashes, discarded?, health?, detail?}` sent (or kept in the
hello's `results` until acknowledged). The agent then redials every 2 s for two minutes.

**Restarts and reboots.** At start the daemon resumes a pending window (any session may
confirm it) or restores it: no marker = rebooted (`reboot`), not fully committed
(`commit_failed`), past its deadline (`confirm_timeout`). The boot guard
`/etc/init.d/perch-apd-guard` (START=15, before `network` at 20) runs `perch-apd config-guard`,
which restores after a reboot before netifd reads the files, without a reload; without a
daemon that knows the plane it copies the snapshot back itself. The package ships the guard;
with `wifi_config 'write'` the daemon writes and enables it when missing (`self_installed`).

**Change notifications** (`observe` and `managed`): `wifi.config.changed {hashes, changed:
[config], origin: router | perch, applyId?, author: {kind: luci | cli | perch | unknown,
user?, via: trigger | poll}, at, uncommitted}` after a quiet `debounceSeconds`. procd's reload
trigger on `wireless`/`network` makes the init script send SIGHUP (re-read at once, `via:
trigger`); polling every `watchSeconds` finds CLI commits and editors (`via: poll`). The
device groups' writes come as `origin: perch` with `applyId: "groups-<revision>"`. Held for up
to 5 minutes while a LuCI apply-with-rollback waits for its confirm.

`wifi.health.changed {at, ok, problems}`: outside a window, while the controller observes or
manages, when a radio or an expected BSS went down or came back and stayed so for 30 s (radar
checks and channel scans left out); each session gets the state once when steady.

**Read-only checks on an AP** (from `/tmp`, nothing written): `perch-apd wifi caps`,
`perch-apd wifi read [--fp-key-file FILE] [config…]` (fingerprints with an empty key unless
FILE holds the 64-hex fleet key), `perch-apd wifi health`.

### 2.6 Self-update (`agent.update.*`, 1.2.0 and later)

The controller installs signed Perch AP Daemon releases on the AP (dashboard: Settings →
Updates). The formats (release manifest, signature, version order, download URLs, the `update`
block, error codes, files on the device) are the controller's `docs/agent-updates.md`; this
is the AP's part.

- `system.info` carries `update`: `protocol`, `enabled` (`option self_update`), `refusal`
  (`self_update_off`, `no_trusted_keys`, `install_kind_unsupported`, `not_openwrt` or null),
  `keyIds`, `floor`, `installKind` (`package`, `swapped` = a package record of another
  version, `unowned` = `/usr/bin/perch-apd` without a record, `manual` = `/opt/perch-apd`),
  `methods`, the binary's path and SHA-256, the package manager and record, `openwrt`, `arch`,
  `flash` and `ram`, `guard`, `previous` (a kept older version), `active` (the update in
  progress) and `results` (outcomes the controller has not acknowledged).
- Methods: `agent.update.status` (the block), `.stage` (verify a release, check flash and
  RAM, download in the background; `dryRun` checks only), `.install` (hand the staged update
  to the watchdog: the session then closes), `.confirm` (sent to the **new** process once it
  is healthy), `.abort`, `.ack` (results). Notifications: `agent.update.progress`,
  `agent.update.result`; unacknowledged results are sent again on every new session.
- Trust: the AP installs only a release whose manifest is signed (Ed25519, signify/usign
  format) by a key it trusts: the Perch release keys built into the daemon, plus
  `list update_key` entries in `/etc/config/perch-apd` (the updater never writes that file).
  Every file is checked against the manifest's SHA-256; a release below the AP's floor, for
  another product, arch or package manager, or with files outside
  `/etc/init.d/perch-apd`, `/etc/init.d/perch-apd-guard` and `/lib/upgrade/keep.d/perch-apd`
  is refused.
- Install: `perch-update.sh` (written by the version that planned the update) keeps the old
  files (a hardlink on flash, or a copy in RAM where the flash cannot hold both binaries),
  stops the service, swaps the binary or runs `opkg install --force-downgrade` /
  `apk add --allow-untrusted`, starts it and waits. Without a confirm within the check
  window (180 s by default), on a crash loop or an abort it restores the old files and
  restarts; after a reboot inside the window `/etc/init.d/perch-apd-guard` restores before
  the network starts. A rollback never runs the package manager.
- One writer at a time: an install takes the AP's write lock (shared with device groups and
  the Wi-Fi config plane) and keeps it until the new version is confirmed or rolled back.
  Meanwhile `groups.apply` and `wifi.config.apply` are refused `busy` (reason
  `update_pending`); an update is refused `busy_pending_apply` (reason `groups_pending` or
  `plane_pending`) while one of those waits for its confirm.
- The new version reports `active.phase` `probation` in the `system.info` of its first
  session (it dials once the watchdog has set it); the controller confirms after the session
  has been up 30 s with 2 accepted pushes (its settings).

## 3. Security notes

- The agent listens on no port: it dials out to the controller and nothing
  can connect to it.
- The agent executes a fixed set of methods. There is no remote shell: a
  compromised controller can kick clients, blink LEDs, reboot APs, and install
  any genuine Perch AP Daemon release at or above the AP's version floor (with
  `option self_update '1'`, the default; §2.6), nothing more. It cannot make
  the AP run code the Perch release key did not sign: the AP checks the
  signature and every file itself, so updates are allowed over plain `http://`
  too. With `option wifi_groups '1'` it can also add Wi-Fi passphrases and
  VLANs to the listed SSIDs (Perch's own sections only, rolled back unless
  confirmed): leave it off on APs that carry no device groups.
- With `option wifi_config 'read'` (the package's default) a controller reads
  the AP's `wireless` and `network` configs, passphrases only as fingerprints
  (an offline dictionary attack on a weak passphrase is possible for whoever
  has the fleet key and a read, as with a captured WPA2 handshake). With
  `'write'` it can rewrite the whole Wi-Fi configuration and the VLAN plumbing
  in `network`: open or re-key networks, disable radios, bridge an SSID onto
  another VLAN. It can never write any other config (in particular not
  `/etc/config/perch-apd`, so it cannot grant itself write access or re-point
  the agent), run commands, or read a passphrase it did not set. Writes need
  verified TLS; every change is rolled back unless the AP reaches the
  controller on a fresh session and passes its health check.
- Join tokens are stored hashed (and encrypted with `APP_KEY` so an admin can
  show one again). Agent secrets are stored as SHA-256 hashes.
- Failed joins and failed WebSocket authentications are rate-limited per
  client address (20 per 15 minutes).
- TLS is verified by default. `tls_insecure '1'` accepts a self-signed
  certificate, `ca_file` adds a private CA.
