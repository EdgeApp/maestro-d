# Hubs: a device mesh

A **hub** is a long-running maestro-d daemon that shares the devices on its
machine with clients on the network. Hubs on several machines link into one
**mesh**: a client connects to any hub, lists every device on every hub,
leases one, and drives it with the one-shot CLI, `run flow.yaml`, the
JavaScript library or REST. The hub the client talks to (the **entry hub**)
forwards each request to the hub that owns the device.

```
 client (CLI / JS / REST / run flow.yaml)
   │  HTTP(S), one request per Maestro command
   ▼
 hub "hub-b"  ──── WebSocket peer link (peer token) ────  hub "hub-a"
   │  owns pixel@hub-b                                       │  owns 99FA…@hub-a
   │  routes x@hub-a requests to hub-a ─────────────────────►│  runs the command on its own device
```

Every hub owns the devices attached to its machine — it alone drives them and
decides their leases — and is a gateway for everyone else's. There is no
central server and no replicated state: a network partition can make a
device unreachable, never leased twice.

Where things run: selector lookup, waits, retries, `runScript`/`evalScript`
and whole YAML flows run on the hub that owns the device; the client's own
JavaScript runs on the client and sends one command per round trip. Over a
100–300 ms link each command costs one client-to-entry round trip plus the
entry-to-owner hop; a bundled `run` costs a few round trips in all.

## Setup

```sh
# every machine: the same two secrets, in files only you can read
umask 077; printf '%s\n' "$CLIENT_TOKEN" > ~/.maestro-d/hub-token; printf '%s\n' "$PEER_TOKEN" > ~/.maestro-d/peer-token

# machine A
maestro-d hub start --name hub-a --token-file ~/.maestro-d/hub-token --peer-token-file ~/.maestro-d/peer-token \
  --share-device "iPhone 16"
# machine B, on the same LAN: finds hub-a with mDNS at launch
maestro-d hub start --name hub-b --token-file ~/.maestro-d/hub-token --peer-token-file ~/.maestro-d/peer-token
# a machine on another network joins through --peer
maestro-d hub start --name hub-c --no-mdns --peer hub-a.example.com:7433 \
  --token-file ~/.maestro-d/hub-token --peer-token-file ~/.maestro-d/peer-token

# any client
export MAESTRO_D_TOKEN=$(cat ~/.maestro-d/hub-token)
maestro-d hub status --target-hub=hub-b.local
maestro-d device list --target-hub=hub-b.local            # every hub's devices: <id>@<hub>
maestro-d lease --device "$UDID@hub-a" --target-hub=hub-b.local          # leasing is always explicit
maestro-d tapOn General --target-hub=hub-b.local --device "$UDID@hub-a"   # attaches and runs on hub-a
```

Every hub of a mesh uses the **same client token** (the owner checks the
token of every request it is forwarded) and the **same peer token**.

## `hub start`

| Flag | Default | Meaning |
| --- | --- | --- |
| `--name NAME` | the short host name, lowercased | The hub's name: `[a-z0-9-]{1,32}`, unique in the mesh |
| `--listen ADDR` | `:7433` | The client and peer listener; binds every interface |
| `--advertise HOST:PORT` | the address each peer reaches this hub at | The address peers are told to dial (join propagation) |
| `--peer HOST[:PORT]` | none (repeatable) | Static peers; `https://` dials WSS |
| `--no-mdns` | off | Skip LAN discovery |
| `--token T` / `--token-file F` | required | The client bearer token. A hub always listens on TCP and every TCP caller is remote |
| `--peer-token P` / `--peer-token-file F` | required | The mesh's shared secret, sent on every peer link |
| `--tls-cert F --tls-key F` | none | Serve HTTPS and WSS |
| `--peer-ca F` | system roots | Trust root for peers' TLS |
| `--share-device ID\|NAME` | every device (repeatable) | Only these local devices join the pool; the rest stay private to this machine |
| `--lease-max [DEVICE=]DURATION` | 8h | The longest lease window (`8h`, `90m`, or seconds): the whole hub's, or one device's by id or name (repeatable). A longer `ttlSec` is `USAGE` naming it |
| `--allow-remote-shell` | off | Let remote callers run `runShell` |
| `--pass-env NAME` | none (repeatable) | A variable of the hub's environment that sessions may import |
| `--foreground` | off | Run in the foreground, for launchd or another supervisor |
| `--daemon NAME` | `hub` | The daemon name the hub runs under (its run directory and unix socket) |

Token files must be mode 0600. A detached hub gets its tokens through its
environment, never its command line, and `daemon.json` records the start
arguments with tokens redacted.

`hub start` returns once the hub is ready: after its first dial to every
`--peer` has had its answer, or after 10 s. If every `--peer` refused it and
mDNS found no other hub, it exits with the refusal's code (below).

`hub stop` shuts the hub down cleanly: its leases end, its devices are
detached, and they leave every peer's listing as soon as the links close.

`hub status [--target-hub…] [--json]` (REST `GET /v1/hub`) prints the hub's
name, version and listener, and for each peer its address, link state
(`linked`, `down`, `stopped`, `connecting`, `refused`), direction, RTT and
how many devices it shares.

## Discovery and membership

- **mDNS once, at launch.** The hub advertises `_maestro-d._tcp` (TXT: name,
  API version, `tls=0|1`), browses for 3 s, and dials every hub it found plus
  every `--peer`. On macOS the browse goes through the system's mDNS
  (`dns-sd`), which owns UDP 5353; elsewhere it uses zeroconf. Of a hub's
  advertised addresses it dials the first that accepts a connection. It never searches the network again: a hub's restart is
  its next discovery. The log has one `mdns browse` line per launch.
- **Join propagation.** The first `hello` on every link carries the sender's
  linked peers, and the receiver dials the ones it is not linked to. One seed
  fills the mesh, and a hub behind NAT joins just by dialing out — a link,
  once open, carries requests both ways. Every machine may share one config
  naming the seed: on the seed, a `--peer` that reaches the hub itself is
  dropped (it logs `is this hub itself`), not refused.
- **Duplicates.** When two hubs dial each other at once, the link dialed by
  the lexically lower name survives.
- **Liveness.** A ping every 10 s; a link is down after 3 missed pongs (about
  30–40 s for a hub that hangs; at once for one that exits).
- **Reconnect.** The dialing side redials with backoff from 1 s up to 30 s,
  for the life of the process.
- **Refusals.** A wrong peer token is refused 401 (`USAGE`, exit 2); a hub
  named like the receiver, `HUB_NAME_TAKEN` (409, exit 5); another API
  version, `HUB_VERSION` (409, exit 5). For these the dialing hub logs the
  refusal and stops dialing that address. A name a live link from another
  process at another address holds is refused `HUB_NAME_TAKEN` too
  (`details.addr`), but that is usually the same hub restarted at a new
  address before its old link missed its pongs: the dialer keeps retrying
  with backoff and links once the old link is down. So does an answer
  without a hub's error envelope (a proxy's 404, 429 or 502).

## Device ids

On a hub every device id is `<localId>@<hub>` — `emulator-5554@hub-a`,
`99FA590E-…@hub-a` — and every lease id `<uuid>@<hub>`. Entries carry `hub`
and `localId`, plus `machine` (the owning host's name, `os.Hostname()`),
`addresses` (the owning hub's `--advertise` address, when set, then that
host's non-loopback IPs) and `maxSec` (the device's longest lease window),
all set by the owner and carried through the mesh: a client compares
`machine` and `addresses` with its own to find the devices attached to its
own machine, whichever hub it asked. Hub names are lowercase; a hub name a client sends, in a
suffix, a selector, a boot body or a catalog query, is read in any case. Inside a hub ids stay local: the registry, `devices.json`, the
attached map and `ps` use the local id, so a plain daemon on the same machine
still sees the hub's attachments. An unqualified id resolves to the local
device if there is one, else to the one mesh device with that local id;
several matches fail `USAGE`, listing them. If a linked hub does not answer
the lookup in time, or a known hub's link is down (not a clean `hub stop`),
and no hub lists the id, it fails `PEER_UNAVAILABLE` (exit 3, retryable)
naming that hub, since the id may be its device. A local id is remembered
for 30 s, and one that resolved to a peer for 10 s (while that peer stays
linked, and until a device is booted or created on this hub), so a run of
steps by bare id does not repeat the lookup for every step.

## Routing

- Any `/v1/devices/{id}/…` or `/v1/leases/{lid}/…` route goes to the hub its
  suffix names.
- `GET /v1/devices` and `GET /v1/leases` ask every linked peer in parallel
  (3 s each; answers cached for 2 s) and merge; a peer that does not answer,
  or a known hub whose link is down, is listed under `unreachable`
  (`device list` prints it on stderr). `?scope=local` lists the entry hub
  only.
- `POST /v1/leases` routes by its body's `device`, resolved as on the device
  routes. An id that neither the entry hub nor any peer lists is
  `DEVICE_NOT_FOUND` and no hub grants it (`PEER_UNAVAILABLE` when a peer did
  not answer). A `select` is tried on the
  entry hub first, then on the peers by RTT; ready devices on every hub come
  before any shut-down one is booted (`boot: true`). A hub whose devices'
  maximum window is shorter than the `ttlSec` asked is passed over; when no
  hub grants it, the answer is that owner's `USAGE` naming the maximum.
- `GET /v1/events?device=<id>` streams from the device's hub (an
  unqualified id resolves as on the device routes); without
  `device` the stream merges every hub's events, each naming its device by
  qualified id. It follows the mesh: a hub that links later joins it, and
  one that restarts or relinks is subscribed again (events in between are
  not replayed). Each hub numbers its own events, so the merged stream has
  no `id:` lines and cannot be resumed: `?after=` or `Last-Event-ID` on it
  is `USAGE`. Follow one device (`?device=`) or one hub (`?scope=local`) to
  resume.
- `POST /v1/devices` (boot or create) and `GET /v1/catalog` take `hub` in the
  body or query; the CLI's `device catalog|create|start` take `--hub NAME`.
- Uploads over 1 MiB, artifacts, screenshots and event streams travel on a
  short-lived link of their own (at most 4 per peer), so a large APK never
  delays another device's `tapOn` or the pings. A hub that dialed in gets
  them only once a test link to its address opens; until then (a hub behind
  NAT), and for a minute after one fails, they go on the control link at
  once. Only addresses a hub has dialed are passed on to its peers. Such a
  link closes with the peer's control link, so an event stream from a hub
  that vanished (missed pongs) ends rather than staying open and silent.
- If the owner's link is down, the request fails `PEER_UNAVAILABLE` (503,
  exit 3) with `details.hub`; so does one in flight when its link drops.
  Clients retry it with the same request id, so it runs at most once.

## Security

- The entry hub authenticates the client and forwards the request with the
  mesh's client token and the client's address as the **principal**: the
  owner's checks, leases and log name the real client (`… via hub hub-b`).
- Every request through a peer link is remote on the owner, and so is every
  TCP request on a hub, loopback included: `runShell` (unless
  `--allow-remote-shell`), host paths, `?cwd=`, web attaches and Appium
  attaches (`driver: appium`, `appiumUrl`, `extra.capsFile`) are
  `FORBIDDEN`, and so is `POST /v1/shutdown`: a hub is stopped only from
  its own machine (`hub stop`). Only the hub's unix socket is local.
- Sessions import none of the hub's environment except `--pass-env` names.
  Targeted clients send their `-e`/`--env-file` values (CLI) and `env` (JS),
  but not their own shell environment — the one difference from local use.
- `--share-device` keeps a hub's other devices out of the mesh: peers never
  list them or their leases, requests for them are `DEVICE_NOT_FOUND`, and
  their leases are `LEASE_EXPIRED` to a peer, as an unknown lease is.
- On a LAN plain HTTP is accepted. Across the internet run with
  `--tls-cert/--tls-key` (or behind a TLS proxy) and dial `https://` peers.
- A hub that serves TLS, or has `--peer-ca`, dials peers only over TLS, so
  the peer token and the client token it forwards never cross the network
  in clear. It dials every address as `https://`: an mDNS answer that says
  `tls=0`, an address from a peer's list, a dialed-in peer's address (for
  bulk links), and a `--peer` given as `HOST[:PORT]` or `http://`. It logs
  each plain address it upgrades (`… is a plain address; dialing
  https://…`). A peer that serves only plain HTTP then fails its TLS
  handshake and never links; give it `--tls-cert/--tls-key` too.

## A launchd service (macOS)

```sh
maestro-d hub service install --name hub-a --token-file ~/.maestro-d/hub-token --peer-token-file ~/.maestro-d/peer-token
maestro-d hub service status
maestro-d hub service uninstall [--purge]
```

- `install` takes `hub start`'s flags and installs a per-user LaunchAgent,
  `~/Library/LaunchAgents/app.edge.maestro-d.hub.plist` — an agent, not a
  system daemon, because simulators and WebDriverAgent need your GUI session.
  It runs `hub start --foreground` with `RunAtLoad` and
  `KeepAlive {SuccessfulExit: false}`: it starts at login and restarts after a
  crash, but not after a clean `hub stop`. Every launch is a hub launch, so it
  browses again.
- The tokens never go into the plist: they are written to
  `<home>/hub/secrets/token` and `peer-token` (mode 0600, `<home>` is
  `$MAESTRO_D_HOME`, else `~/.maestro-d`), and the plist passes
  `--token-file`/`--peer-token-file`. launchd starts with a bare environment,
  so `PATH`, `MAESTRO_D_HOME`, `ANDROID_HOME`, `ANDROID_SDK_ROOT`,
  `DEVELOPER_DIR` and `MAESTRO_TEAM_ID` are recorded when set. The hub's
  output goes to its `daemon.log`.
- While it is installed, `hub start` starts it with `launchctl kickstart` and
  `hub stop` stops it cleanly, so there is never a second hub process.
- `uninstall` boots the agent out (stopping the hub) and deletes the plist;
  `--purge` also deletes the secrets.

### Linux: a systemd user unit

The service command is macOS-only. On Linux, run `hub start --foreground`
under any supervisor; a Linux hub shares Android devices only.

```ini
# ~/.config/systemd/user/maestro-d-hub.service
[Unit]
Description=maestro-d hub
After=network-online.target

[Service]
ExecStart=%h/.maestro-d/bin/maestro-d hub start --foreground --name hub-l \
  --token-file %h/.maestro-d/hub-token --peer-token-file %h/.maestro-d/peer-token
Environment=ANDROID_HOME=%h/Android/Sdk
Restart=on-failure

[Install]
WantedBy=default.target
```

`systemctl --user enable --now maestro-d-hub`. `Restart=on-failure` matches
launchd's behaviour: a clean `hub stop` stays stopped.

## Troubleshooting

| Symptom | Cause |
| --- | --- |
| `hub start` exits 5 with `HUB_NAME_TAKEN` | The `--peer` it dialed has that name: pick another `--name`. (A name another live link holds is retried, not fatal: a second hub of that name keeps dialing and logs `not linking`) |
| `hub start` exits 2, "missing or wrong peer token" | The hubs' peer tokens differ |
| `hub status` shows no peers on a LAN | mDNS is blocked (a firewall, a VPN, a container network): use `--peer` |
| A peer shows `down` | It hangs, exited, or the network dropped; its devices are listed under `unreachable` and their requests fail `PEER_UNAVAILABLE` until it is back (the dialing side redials) |
| Forwarded requests fail 401 | The hubs' client tokens differ |
| The default name is odd | A hub's default name is the short host name, lowercased; anything outside `a-z0-9-` becomes `-`. Give `--name` |
| The service's hub cannot reach LAN peers ("no route to host") while the same hub started from a terminal can | macOS Local Network privacy: allow maestro-d under System Settings › Privacy & Security › Local Network |
| A hub that stopped minutes ago is still found and dialed | The system's mDNS cache keeps its record for a while; the dialer retries quietly until that hub is back, and it does no harm |
| Hub restarted by launchd keeps failing | Read `<home>/run/hub/daemon.log`; launchd throttles restarts to one per 10 s |
