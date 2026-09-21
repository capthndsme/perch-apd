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
| 5xx / network | — | exponential backoff 1 s → 60 s |

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
exponential backoff with jitter, 1 s doubling to 60 s, reset after a session
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
  "capabilities": ["metrics", "clients", "kick", "locate", "reboot"],
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
`reboot` on OpenWrt. `band` in `interfaces` is `2.4`, `5`, `6` or `60` (the
server's convention); `band` in `radios` is UCI's (`2g`, `5g`, `6g`, `60g`).

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
           "collectedAt":"2026-09-21T11:20:36Z","durationMs":14,"seq":42}}
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

### 2.4 Other notifications the agent sends

| Method | Params | |
|---|---|---|
| `locate.ended` | `{"reason": "timeout" \| "stopped"}` | LEDs restored |

The server ignores notifications it does not know; agents ignore requests they
do not know with -32601.

## 3. Security notes

- The agent listens on no port: it dials out to the controller and nothing
  can connect to it.
- The agent executes a fixed set of methods. There is no remote shell: a
  compromised controller can kick clients, blink LEDs and reboot APs, nothing
  more.
- Join tokens are stored hashed (and encrypted with `APP_KEY` so an admin can
  show one again). Agent secrets are stored as SHA-256 hashes.
- Failed joins and failed WebSocket authentications are rate-limited per
  client address (20 per 15 minutes).
- TLS is verified by default. `tls_insecure '1'` accepts a self-signed
  certificate, `ca_file` adds a private CA.
