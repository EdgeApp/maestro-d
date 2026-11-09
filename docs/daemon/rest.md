# REST API

The daemon is an HTTP server. The CLI and the JavaScript library are clients
of it; anything that can speak HTTP can drive a device the same way.

## Reaching the daemon

| transport | when | how |
| --- | --- | --- |
| unix socket | always | `~/.maestro-d/run/<name>/daemon.sock` (`$MAESTRO_D_HOME` overrides the root) |
| TCP | daemon started with `--http ADDR` | `http://ADDR`, with `Authorization: Bearer <token>` when `--token` was given |
| TCP + TLS | `serve --http ADDR --tls-cert F --tls-key F` | `https://ADDR`; a self-signed certificate needs its trust root on the client (`--ca`, `ca`) |
| hub | `hub start --listen ADDR --token T` (default `:7433`, every interface) | `http(s)://ADDR` with the bearer token; devices are [leased](#leases-hubs) |

```sh
maestro-d start --daemon api --http 127.0.0.1:7788 --token s3cret
curl -s --unix-socket ~/.maestro-d/run/api/daemon.sock http://d/v1/status
curl -s -H 'Authorization: Bearer s3cret' http://127.0.0.1:7788/v1/status
```

The examples below assume `D=http://127.0.0.1:7788` and `H='Authorization: Bearer s3cret'`.

Two headers matter:

- `X-Maestro-Api-Version: 1` — optional on requests; a mismatch is refused
  with `DAEMON_UNAVAILABLE`. Every response carries it.
- `Authorization: Bearer <token>` — required on TCP when the daemon has a
  token; never checked on the unix socket (file permissions do that).
  A missing or wrong token is `401` with a `USAGE` body. A daemon whose
  `--http` address reaches beyond loopback (`0.0.0.0:7433`, a LAN address)
  refuses to start without `--token`.
- `X-Maestro-Request-Id: <uuid>` — optional; see
  [Request ids and retries](#request-ids-and-retries).
- `X-Maestro-Lease: <lease id>` — on a hub, required on every route that
  drives a device; see [Leases](#leases-hubs).

Every request resets the idle timer.

## Conventions

- Request bodies are JSON. Responses are JSON except `GET …/screenshot`
  (PNG) and `GET …/hierarchy?format=raw`.
- Every JSON response is an **envelope**: `{"ok": true, "exitCode": 0, …}`
  on success, and on failure

  ```json
  {
    "ok": false,
    "exitCode": 1,
    "error": {
      "code": "COMMAND_FAILED",
      "message": "Element not found: text=\"Nope\" (timeout 2000ms)",
      "step": "assertVisible: text=\"Nope\"",
      "details": { "reportDir": "…", "screenshot": "assets/flow-000/cmd-004-after.png" }
    }
  }
  ```

  `exitCode` is what the CLI would exit with; the HTTP status follows the
  code (table below). `step`, `details` and `details.*` are present when
  known.
- A device is addressed by the id `GET /v1/devices` reports: the Android
  serial, the iOS UDID, a simulator UDID, or any string for the mock
  platform.
- Steps on one device run one at a time. A request that arrives while
  another step is running waits for its turn; `?wait=false` makes it fail
  at once with `BUSY` (409) instead.
- Closing the connection mid-step cancels the step on the device
  (`INTERRUPTED`, 499 if the response can still be written). A TCP request
  that carries a request id is the exception: it keeps running for 10 s so
  a retry can collect its outcome, and is cancelled only if none arrives.

## Remote callers

The daemon decides for **each request** whether its sender may reach the
host it runs on, not by who attached the session:

- The unix socket is always local.
- On `serve --http`, a TCP request from a loopback address is local, as
  before, and one from any other address is **remote**. (On a hub, from
  `hub start`, every TCP request is remote, loopback included.)

A remote caller drives devices but not the host. These are refused with
`FORBIDDEN` (403, exit 2), with the step and field in `details`:

- `runShell`, at the top level or nested in `runFlow`/`retry`/`repeat`
  commands, a `/steps` list or an inline flow. The call stops at that step.
- A path on the host in a step: `addMedia` files, `runScript` file (a value
  ending in `.js`; other values are inline JavaScript and run), `uploadFile`
  path(s), `runWebViewScript`/`runBrowserScript` file, `loadAuthState`,
  and the `runFlow`/`retry` file — whether absolute, relative, or built
  from a `${VAR}`, and whether in a one-shot or inside a bundled flow (a
  `..` that leaves the bundle counts). Nothing on the host is read. Upload the file instead
  ([blobs](#blobs-uploaded-files)) and name it
  `blob:sha256:<hex>/<basename>`; the daemon gives the step its own copy,
  with that basename and extension. A ref whose hash is not 64 lowercase
  hex digits, or whose basename is not a plain file name, is `FORBIDDEN`
  before any file is opened. A flow file goes as a
  [flow bundle](#flow-bundles) instead: `bundle:sha256:<manifest>/<root>`
  in `runFlow`'s `file` or `else`, or `retry`'s `file`.
- An output path that is absolute or contains `..` (`takeScreenshot`,
  `saveAuthState`). A relative one is written under the session's report
  directory, in `outputs/<path>` (`takeScreenshot` adds `.png` when it has
  no extension), and the result lists it in [`outputs`](#outputs). Inside a
  bundle the path is taken relative to the bundle root from the step's own
  flow directory: `takeScreenshot: x` in `sub/open.yaml` is `outputs/sub/x.png`.
  `startRecording`'s path is a device path on Android and passes through.
- `assertScreenshot`'s reference: a `blob:` ref is compared; a relative
  path that names no uploaded file is a **baseline**, written to
  `outputs/<path>` (`.png` added) as a local first run writes it beside the
  flow; an absolute host path is `FORBIDDEN`. On a mismatch the diff image
  is written to `outputs/`, named after the caller's reference —
  `outputs/<first 8 hex of the ref's sha256>/<basename>_diff.png` for a
  `blob:` ref — and listed in `outputs`; the failure message names that
  output, not a path on the daemon's host.
- `?cwd=` on a command, `cwd` in a `/steps` or `/flows` body, and `file` in
  a `/flows` body: send the flow as a `bundle` (or its `yaml`) instead.
- A [flow bundle](#flow-bundles) manifest with a name or hash that could
  leave its directory.
- An attach with `platform: web` (or a browser driver), `outputDir`,
  `flowDir`, `extra.userDataDir` or `extra.browser`, or an `appFile` that
  is a host path rather than an uploaded `blob:` ref.
- An Appium attach: `driver: appium` (any case), `appiumUrl`, or any
  `extra.capsFile`, even a `blob:` ref. Appium capabilities can name host
  files and programs (`appium:app`, `appium:chromedriverExecutable`), and
  `appiumUrl` would open sessions to any address the daemon can reach. A
  unix-socket caller attaches with Appium as before.

A daemon listening beyond loopback also keeps its own environment to
itself: sessions import none of its variables (so `${HOME}`, `GET …/vars`,
`eval` and scripts see only what callers set), except the names listed with
`serve --pass-env NAME`. A unix-socket-only or loopback-only daemon imports
its environment as before.

### Outputs

A step, batch or flow result carries `outputs`, the files its output
fields wrote, as paths under the report directory (`outputs/<path>`),
nested steps and subflows included. Fetch each with
[`GET …/artifacts/{path}`](#get-v1devicesidartifactspath). It is present
for a remote caller, and for any caller that sends
`X-Maestro-Client-Outputs: 1`: a client that names its own output files
(the CLI with `--target-hub`, the JavaScript library on a hub) sends it, so
its relative output paths go to `outputs/` even when it reaches the daemon
over loopback. A local call without it writes where it always has and has
no `outputs`.

The CLI and the JavaScript library, when they target another machine,
never send their own output path: they send
`<first 8 hex of sha256(caller path)>/<basename>`, then fetch the result's
`outputs` back to the caller's path (relative to their own working
directory, or absolute). `takeScreenshot --path name` writes `name.png` on
the client, as a local run would. For a [bundle](#flow-bundles) they fetch
each `outputs/<path>` to `<path>` under the bundle's directory on the
client (its files' common directory), where a local run writes it.

## Request ids and retries

`POST …/commands/{name}`, `…/steps`, `…/flows` and `…/apps` may carry
`X-Maestro-Request-Id`. The daemon keeps each device's last 256 outcomes
by id for 10 minutes: a repeat of an id that is still running waits for
that run's outcome, and a repeat of a finished one is answered from the
cache (with `X-Maestro-Replayed: true`), so a retried `inputText` never
types twice.

The CLI and the JavaScript library send a fresh id on each of these calls
over TCP and, after a reset or refused connection, `PEER_UNAVAILABLE`, or
a 502/504 from a gateway (not the daemon's own `DEVICE_ERROR`), retry it
up to 3 times with the same id, waiting 0.5 s, 1 s and 2 s. Reads and
attaches are retried the same way.

## Errors

| `error.code` | HTTP | `exitCode` | when |
| --- | --- | --- | --- |
| `COMMAND_FAILED` | 422 | 1 | the step ran and failed: element not found, assertion false, timeout, script threw |
| `USAGE` | 400 | 2 | unknown command or route, bad JSON, bad field, unparsable YAML, bad token |
| `DEVICE_NOT_ATTACHED` | 404 | 2 | the device has no driver open; attach first |
| `DAEMON_UNAVAILABLE` | 503 | 3 | API version mismatch; daemon shutting down |
| `DEVICE_ERROR` | 502 | 4 | the driver failed: device disconnected, WDA/UIAutomator2 died, boot or shutdown failed |
| `DEVICE_NOT_FOUND` | 404 | 4 | no such device id |
| `INTERNAL` | 500 | 4 | a daemon bug (a panic is caught and reported here) |
| `BUSY` | 409 | 5 | `?wait=false` and the device is running another step |
| `DEVICE_IN_USE` | 409 | 5 | another daemon holds the device, or (hub) another client's lease does; `details` name the owner or the holder and `expiresAt` |
| `DEVICE_EXTERNAL` | 409 | 5 | `DELETE` on a device the daemon did not boot, or `?delete=true` on one maestro-d did not create (use `force`) |
| `INTERRUPTED` | 499 | 130 | the request was cancelled mid-step |
| `FORBIDDEN` | 403 | 2 | a [remote caller](#remote-callers) asked for the host: `runShell`, a host path, `cwd`, a web attach |
| `PEER_UNAVAILABLE` | 503 | 3 | hub mesh: the hub that owns the device cannot be reached (`details.hub`); clients retry it |
| `HUB_NAME_TAKEN` | 409 | 5 | hub mesh: a joining hub's name is already in use |
| `HUB_VERSION` | 409 | 5 | hub mesh: a joining hub speaks another API version |
| `LEASE_REQUIRED` | 409 | 5 | hub: a request that drives a device without its `X-Maestro-Lease` (or with another device's lease); the message names `maestro-d lease --device <id>` and `md.lease()`, `details.hint` the command |
| `LEASE_EXPIRED` | 409 | 5 | hub: the request's lease has expired or was released |

`PEER_UNAVAILABLE`, `HUB_NAME_TAKEN` and `HUB_VERSION` belong to the hub
mesh and are part of the protocol now so every client maps them; the two
lease codes come from a hub. A `serve` daemon answers with none of them.

A failed step with `optional: true` is *not* an error: `200`, `ok: true`,
and `error` filled in for inspection.

## Daemon

### `GET /v1/status`

```sh
curl -s -H "$H" $D/v1/status
```

```json
{
  "ok": true, "exitCode": 0,
  "daemon": {
    "name": "api", "pid": 41234, "apiVersion": "1", "version": "0.1.0",
    "socketPath": "/Users/me/.maestro-d/run/api/daemon.sock",
    "httpAddr": "127.0.0.1:7788", "startedAt": "2026-09-10T10:00:00Z",
    "runDir": "/Users/me/.maestro-d/run/api",
    "reportsDir": "/Users/me/.maestro-d/run/api/reports",
    "idleTimeout": 1800,
    "tls": false,
    "hub": "lab-mac"
  },
  "devices": [ … as GET /v1/devices, only the ones this daemon has attached or booted … ],
  "idleShutdownAt": "2026-09-10T10:30:00Z"
}
```

`daemon.hub` is the hub's name, present only on a hub (`hub start`):
clients use it to know the daemon leases its devices. The name is the
host's short name, lowercased.

The only route that still answers while the daemon is shutting down. It
runs no device discovery (adb, simctl), so it answers quickly and clients
use it as their liveness check.

### `POST /v1/shutdown`

Detaches every device, shuts down the simulators and emulators the daemon
booted, and exits. Answers `{"ok": true}` first; the process is gone a
moment later. No body. On a hub it is served only on the unix socket
(`maestro-d hub stop`): a TCP caller, or a request a peer forwarded, gets
`FORBIDDEN`.

### `GET /v1/events` — server-sent events

```sh
curl -sN -H "$H" "$D/v1/events?device=29271FDH200ABP"
```

```
id: 17
event: step
data: {"seq":17,"time":"2026-09-10T10:01:02Z","type":"step","device":"29271FDH200ABP","step":"tapOn: text=\"Login\"","passed":true,"durationMs":312}

id: 18
event: device.detached
data: {"seq":18,"time":"…","type":"device.detached","device":"29271FDH200ABP"}
```

| query / header | |
| --- | --- |
| `?device=ID` | only that device's events (daemon-wide events such as `daemon.shutdown` have no device and are filtered out too) |
| `?after=SEQ` or `Last-Event-ID: SEQ` | replay buffered events with `seq > SEQ` first — reconnecting clients miss nothing that is still in the ring buffer |

Event types: `step` (`step`, `passed`, `durationMs`, `error`, `depth` for
nested steps), `device.attached`, `device.detached`, `device.booted`,
`device.stopped`, `daemon.shutdown`, and on a hub `lease.granted`,
`lease.released` and `lease.expired` (`message` names the holder).
`lease.expired` is a lease whose window ran out; every other end of a lease
is `lease.released`: a release, its device stopped or deleted, the attach in
its `POST /v1/leases` failing, or the hub stopping. `message` carries free text where
there is any. The stream never ends on its own; close the connection to
stop.

## Devices

### `GET /v1/devices[?platform=android|ios|web]`

Everything the host can see — connected devices, simulators (booted or
not), emulator AVDs — merged with what this and other daemons hold.

```json
{
  "ok": true, "exitCode": 0,
  "devices": [
    { "id": "29271FDH200ABP", "platform": "android", "kind": "device", "name": "Pixel 7", "state": "device", "ready": true, "attached": true, "attachedAt": "…", "appId": "co.edgesecure.app", "driver": "uiautomator2", "reportDir": "…/reports/29271FDH200ABP/20260910-100000" },
    { "id": "CD9D3952-58FA-4E81-AC64-736189D71D1E", "platform": "ios", "kind": "simulator", "name": "iPhone 13 Pro", "osVersion": "26.0", "state": "Shutdown", "ready": false, "attached": false },
    { "id": "00008110-0006098A02FA801E", "platform": "ios", "kind": "device", "name": "iPhone", "ready": true, "attached": false, "owner": "other-daemon" },
    { "id": "emulator-5554", "platform": "android", "kind": "emulator", "name": "Pixel_8_API_35", "state": "device", "ready": true, "attached": false },
    { "id": "avd:ci-pixel", "platform": "android", "kind": "emulator", "name": "ci-pixel", "state": "Shutdown", "ready": false, "attached": false, "createdBy": "maestro-d" }
  ]
}
```

A running emulator's `name` is its AVD name. An AVD that is not running is
listed once, as `avd:<name>`; booting it returns the emulator's serial.

| field | |
| --- | --- |
| `ready` | booted / online, so it can be attached |
| `attached`, `attachedAt`, `appId`, `driver`, `reportDir` | this daemon's attachment |
| `busy` | a step is running on it right now |
| `bootedBy`, `bootedAt` | the daemon that booted it (it will shut it down at exit) |
| `owner` | another daemon has it attached — attaching here fails with `DEVICE_IN_USE` |
| `createdBy` | `"maestro-d"` when `POST /v1/devices` with `create` made it (recorded in `<home>/created.json`) |
| `lease` | hub only: `{holder, expiresAt}` while the device is leased |
| `available` | hub only: `true` for a ready device that is neither leased nor attached (here or by another daemon), nor still being released after its lease ended |
| `maxSec` | hub only: the longest lease window the device takes (`--lease-max`; default 28800, 8 hours) |

On a hub started with `--share-device`, a remote caller sees only the
shared devices, here and in `GET /v1/status`; every other device is
`DEVICE_NOT_FOUND` on every route, and booting or creating one is refused.
Its leases are left out of `GET /v1/leases`, and to that caller are
`LEASE_EXPIRED` on the `/v1/leases/{lid}` routes, as an unknown lease is.
The hub's unix socket sees every device. Whether an id is in the pool is
looked up once, not per request or event: a device id is remembered for the
hub's life, while an emulator serial (whichever AVD boots on that port next
reuses it) and an id outside the pool are remembered for 30 s, or until a
device is booted, stopped, created or deleted on the hub, or a lease ends.

### `GET /v1/catalog[?platform=ios|android][&available=true]`

What `create` can use. iOS comes from `simctl list runtimes -j` and
`simctl list devicetypes -j` (iPhone and iPad types only); Android from
`sdkmanager --list_installed` (`--list` with `available=true`, which adds
the images that can be downloaded) and `avdmanager list device -c`.

```json
{
  "ok": true, "exitCode": 0,
  "ios": {
    "runtimes": [ { "id": "com.apple.CoreSimulator.SimRuntime.iOS-26-5", "name": "iOS 26.5", "version": "26.5", "build": "23F77", "available": true, "deviceTypes": ["com.apple.CoreSimulator.SimDeviceType.iPhone-16", "…"] } ],
    "deviceTypes": [ { "id": "com.apple.CoreSimulator.SimDeviceType.iPhone-16", "name": "iPhone 16", "productFamily": "iPhone", "minRuntimeVersion": "18.0.0" } ]
  },
  "android": {
    "images": [ { "id": "system-images;android-35;google_apis;arm64-v8a", "api": "android-35", "tag": "google_apis", "abi": "arm64-v8a", "installed": true } ],
    "profiles": ["pixel_8", "pixel_9", "…"]
  }
}
```

Without `platform`, a platform whose tools are missing is left out and
named in `"errors": {"android": "sdkmanager not found; …"}`. With
`platform`, a missing tool fails `DEVICE_ERROR` naming the tool and how to
install it.

### `POST /v1/devices` — boot, or create and boot, a simulator or emulator

```sh
curl -s -H "$H" $D/v1/devices -d '{"platform":"ios","name":"iPhone 16","bootTimeout":120}'
curl -s -H "$H" $D/v1/devices -d '{"name":"avd:ci-pixel"}'
```

`name` is a simulator name or UDID, or an Android AVD name (or
`avd:<name>`). `platform` may be left out: `avd:` means android, and a name
or UDID that `GET /v1/devices` lists picks that device's platform; anything
else fails `USAGE`. Waits until the device is ready and answers
`{"ok": true, "device": {…}}` with the booted device (its `id` is what to
attach — an emulator's serial). The device is recorded as booted by this
daemon.

With `create`, the device is created first, recorded in
`<home>/created.json`, and booted unless `"boot": false`:

```sh
curl -s -H "$H" $D/v1/devices -d '{"platform":"ios","name":"ci-phone","create":{"type":"iPhone 16","runtime":"iOS-26-5"}}'
curl -s -H "$H" $D/v1/devices -d '{"platform":"android","name":"ci-pixel","boot":false,
  "create":{"image":"system-images;android-35;google_apis;arm64-v8a","profile":"pixel_8","dataSize":"8G"}}'
```

| `create` field | |
| --- | --- |
| `name` | defaults to the request's `name` |
| `type`, `runtime` | iOS: a device type name or id, and a runtime id, short form (`iOS-26-5`), name or version; defaults are the newest installed runtime and the newest iPhone it supports |
| `image`, `profile` | Android: an sdkmanager package path and an avdmanager profile; defaults are the newest installed `google_apis` image for the host's ABI (`arm64-v8a` on Apple silicon) and `pixel_8` |
| `dataSize` | Android: the data partition (`disk.dataPartition.size`), default `6G`, refused above `10G` |
| `installImage`, `acceptLicenses` | Android: download a missing image with `sdkmanager --install`; `installImage` without `acceptLicenses` fails `USAGE` |

A duplicate name, an unknown runtime, type, image or profile, a missing
image without `installImage`, and a field of the other platform fail
`USAGE` before anything is created. Without `boot: false` the answer is the
booted device; with it, the shut-down one (an AVD as `avd:<name>`).

### `DELETE /v1/devices/{id}`

Detaches if attached, then shuts the simulator/emulator down. Refuses with
`DEVICE_EXTERNAL` unless the daemon booted it; body `{"force": true}` or
`?force=true` overrides. Answers `{"ok": true}`.

With `?delete=true` (or body `{"delete": true}`) it deletes the simulator
or AVD instead: `{id}` may be a UDID, a simulator name, `avd:<name>`, an
AVD name or a running emulator's serial (which deletes its AVD). Only
devices `create` made are deleted without `force`; anything else fails
`DEVICE_EXTERNAL` and is left untouched. A device that is attached is
detached, a running one is shut down, then `simctl delete` or
`avdmanager delete avd` runs. Answers `{"ok": true, "device": {…}}` with
the deleted device.

### `POST /v1/devices/{id}/attach`

Opens a driver on the device with the same configuration `maestro-runner
test` takes. Idempotent: attaching an attached device answers with its
current state and ignores the body.

```sh
curl -s -H "$H" $D/v1/devices/00008110-0006098A02FA801E/attach -d '{
  "platform": "ios",
  "appId": "co.edgesecure.app",
  "teamId": "G5LQ7MERPK",
  "env": { "USER": "alice" }
}'
```

| field | maps to `test` flag | |
| --- | --- | --- |
| `platform` | `--platform` | `android`, `ios`, `web`, `mock`; inferred from the id when omitted |
| `driver` | `--driver` | |
| `appFile` | `--app-file` | installed on attach unless `noAppInstall`; a host path, or an uploaded `blob:sha256:<hex>/<basename>` ref (the only form a remote caller may send) |
| `appId` | `--app-id` | default for `launchApp`, `stopApp`, `clearState`, … |
| `url` | `--url` | web |
| `teamId` | `--team-id` | physical iOS: signs WebDriverAgent; omitted, the daemon's own `MAESTRO_TEAM_ID` / `DEVELOPMENT_TEAM` |
| `wdaBundleId` | `--wda-bundle-id` | |
| `appiumUrl` | `--appium-url` | |
| `env` | `-e` | session variables |
| `artifacts` | `--artifacts` | `all`, `none`, `on-failure` |
| `noAppInstall`, `noDriverInstall` | `--no-app-install`, `--no-driver-install` | |
| `driverStartTimeout` | `--driver-start-timeout` | seconds |
| `waitForIdleTimeout` | `--wait-for-idle-timeout` | ms |
| `conditionTimeout` | `--condition-timeout` | ms |
| `stepDelay` | `--step-delay` | ms |
| `typingFrequency` | `--typing-frequency` | |
| `commandTimeout` | `--command-timeout` | ms |
| `alertMonitor` | `--alert-monitor` | default true |
| `outputDir` | `--output-dir` | report directory; default `<reportsDir>/<id>/<timestamp>` |
| `flowDir` | | directory relative paths in steps resolve against |
| `extra` | | any other `test` flag by its long name, e.g. `{"caps": "…", "headed": true}` |

Answers `{"ok": true, "device": {…}}`. Errors: `DEVICE_NOT_FOUND`,
`DEVICE_IN_USE`, `DEVICE_ERROR` (driver would not start — the message has
the driver's own error), and for a [remote caller](#remote-callers)
`FORBIDDEN` on `web`, `outputDir`, `flowDir`, `extra.userDataDir`,
`extra.browser`, a host-path `appFile`, or Appium (`driver: appium`,
`appiumUrl`, `extra.capsFile`). A `blob:` ref in `appFile` (or, from a
local caller, `extra.capsFile`) that is malformed is `FORBIDDEN`, and one
the daemon was never sent is `USAGE`.

### `POST /v1/devices/{id}/detach`

Closes the driver; the device keeps running. `{"ok": true}`, or
`DEVICE_NOT_ATTACHED`.

### `GET /v1/devices/{id}`

One device, same shape as the list entries. `DEVICE_NOT_FOUND` if the host
cannot see it and the daemon does not hold it.

## Leases (hubs)

A hub (`maestro-d hub start`) lends each device to one client at a time.
A client takes a **lease** on the device, then sends its id in
`X-Maestro-Lease` on every request for that device. A plain daemon has no
leases: its lease routes answer `USAGE`, and attach-and-own works as
before.

- **Which routes need it.** Every `/v1/devices/{id}/…` route — attach,
  detach, commands, steps, flows, apps, blobs, artifacts, screenshot,
  hierarchy, state, vars and eval — and `DELETE /v1/devices/{id}` while the
  device is leased. A `DELETE` that names the device by its name is checked
  as the device the name resolves to. Without the header: `LEASE_REQUIRED` (409, exit 5);
  with a lease that has ended: `LEASE_EXPIRED` (409, exit 5).
  `GET /v1/devices/{id}` and `…/info` stay open, and so do the listings.
  This holds over the hub's unix socket too.
- **A fixed window.** A lease runs from its grant (or its last explicit
  renew) for `ttlSec` seconds: 8 hours (28800) by default. Using it never
  moves its expiry; only `POST /v1/leases/{lid}/renew` does.
- **Maximum.** Each device has a maximum window: 8 hours, or the hub's
  `--lease-max DURATION`, or one device's `--lease-max DEVICE=DURATION` (by
  id or name, as for `--share-device`). A `ttlSec` over it is `USAGE` naming
  the maximum (`details.maxSec`), and nothing is granted; the same goes for
  renew. Leases and device listings carry `maxSec`.
- **Expiry.** At the end of the window the hub ends the lease: a request
  still running under it is cancelled (it answers `INTERRUPTED` when its step
  unwinds within about a second, else `LEASE_EXPIRED`; see below), the
  device is detached, and shut down if the hub booted it for the lease, and
  it is listed as available again. Every later request with that lease is
  `LEASE_EXPIRED`. Releasing does the same at once. Work under the lease is
  cancelled as far as its driver allows: a WebDriverAgent HTTP call runs to
  its own timeout, and a WebDriverAgent start to its end (minutes when
  cold). A request whose work has not unwound within about a second answers
  `LEASE_EXPIRED` without it (an attach at once, and the driver it starts is
  discarded). Until every request and attach under the ended lease has
  returned, the device stays attached to it and is not detached, not
  available, and a new lease on it is `DEVICE_IN_USE` ("being released"),
  so that work never touches a later holder's session. A request whose
  answer has already begun when the lease ends finishes that answer itself.
- **Stopping the device.** Stopping or deleting a leased device
  (`DELETE /v1/devices/{id}`, with its lease) ends the lease once the stop or
  delete succeeds (`lease.released`). A lease that ends while that stop runs
  waits for it and does not shut the device down again. While a device is
  being released, a stop or delete without its lease is `DEVICE_IN_USE`.
- **Clients never lease on their own.** The CLI and the JavaScript library
  send a lease only when the client holds one (`maestro-d lease`, `md.lease()`,
  recorded in `<home>/leases.json`); without one the request goes bare and
  the hub answers `LEASE_REQUIRED`.

### `POST /v1/leases`

```sh
curl -s -H "$H" -X POST $D/v1/leases \
  -d '{"device":"99FA590E-…","ttlSec":3600,"holder":"ci@builder","attach":{}}'
curl -s -H "$H" -X POST $D/v1/leases -d '{"select":{"platform":"ios","kind":"simulator"}}'
```

| field | |
| --- | --- |
| `device` | the device to lease: one the hub (or, through the mesh, a peer) lists. An id no hub lists is `DEVICE_NOT_FOUND` and nothing is granted; when a peer did not answer it is `PEER_UNAVAILABLE` instead, since the device may be that peer's |
| `select` | or the first free device that matches `{platform, kind, name, osVersion, hub, boot}`: platform and kind exactly, `name` the device's name (any case) or id, `osVersion` the version or a prefix (`"26"` matches `26.5`), `hub` this hub's name. Free means ready and neither leased nor attached; with `boot: true` a shut-down simulator or AVD is a candidate too, after the ready ones, and the hub boots it for the lease (`booted: true`) and shuts it down when the lease ends |
| `ttlSec` | the window: default 28800 (8 hours), or the device's maximum when that is shorter; over the maximum is `USAGE` naming it. A selector skips a candidate whose maximum is shorter, and answers that `USAGE` when none is left |
| `holder` | who holds it, as listings show; default the caller's address |
| `attach` | an attach body ([`POST …/attach`](#post-v1devicesidattach)); the device is attached in the same call, and a failed attach ends the lease. Without it the device is only reserved |

```json
{
  "ok": true, "exitCode": 0,
  "lease": { "id": "5ae28c45-60e9-46f6-89e0-66b98f06b61d", "device": "99FA590E-…", "holder": "ci@builder", "expiresAt": "2026-11-08T13:00:00Z", "ttlSec": 3600, "maxSec": 28800 },
  "device": { …as GET /v1/devices/{id}, with lease… }
}
```

A device that another lease holds, or another daemon on the hub's machine
has attached, is `DEVICE_IN_USE` (`details.holder`/`expiresAt`, or
`details.owner`); so is one whose ended lease is still detaching it or
shutting it down (a selector passes over it). Those teardowns hold up no
lease call for another device. A selector that matches nothing is `DEVICE_NOT_FOUND`
naming the selector; one whose matches are all taken is `DEVICE_IN_USE`
listing them in `details.candidates`. The request takes an
`X-Maestro-Request-Id`, so a retried request is granted once. With
`attach`, the attach runs under the lease: if the window ends first, the
answer is `LEASE_EXPIRED` at once, the attach is cancelled (or its driver
discarded when it returns) and the device is left detached; it is
released only after that attach has unwound.

### `GET /v1/leases`, `GET /v1/leases/{lid}`

The live leases (`{leases: [Lease…]}`), or one (`{lease}`); a lease that
has ended is `LEASE_EXPIRED`. On a hub started with `--share-device`, a
remote caller gets only the shared devices' leases: another device's lease
is `LEASE_EXPIRED` to it here, on renew and on `DELETE`, and is left as it
is.

### `POST /v1/leases/{lid}/renew`

Body `{ttlSec?}`. Sets the expiry to now plus `ttlSec`, which becomes the
lease's window (default the lease's own window); answers `{lease}`. Over
the device's maximum is `USAGE` naming it, and the lease is left as it was;
a lease that has ended is `LEASE_EXPIRED`.

```sh
curl -s -H "$H" -X POST $D/v1/leases/$LEASE/renew -d '{"ttlSec":1800}'
```

### `DELETE /v1/leases/{lid}`

Ends the lease: the device is detached (and shut down when the hub booted
it for the lease). Answers the lease as it was; `LEASE_EXPIRED` when it had
already ended.

## Steps

### `POST /v1/devices/{id}/commands/{name}`

The command name is the last element of the path; everything else is in the
body. `{name}` is any command from the [command reference](commands.md)
(`tapOn`, `inputText`, `assertVisible`, …) and the body is a JSON object of
that command's fields — its parameters and its modifiers together, exactly
the keys the YAML map form takes:

```sh
C=$D/v1/devices/29271FDH200ABP/commands
curl -s -H "$H" $C/tapOn       -d '{"text":"Login"}'
curl -s -H "$H" $C/tapOn       -d '{"id":"submit","index":1,"timeout":5000,"optional":true}'
curl -s -H "$H" $C/tapOn       -d '{"text":"Delete","childOf":{"id":"row-3"},"label":"delete row"}'
curl -s -H "$H" $C/swipe       -d '{"direction":"UP","duration":400}'
curl -s -H "$H" $C/launchApp   -d '{"appId":"co.edgesecure.app","clearState":true}'
curl -s -H "$H" $C/copyTextFrom -d '{"id":"balance"}' | jq -r .data
```

`optional`, `label`, `timeout` and `platform` are accepted by every command;
the rest are per command and listed in [commands.md](commands.md) or by
`maestro-d commands <name>`.

**Unknown fields are rejected** with `USAGE` (400) naming the key, so a typo
is an error rather than a step that quietly does the wrong thing:

```console
$ curl -s -H "$H" $C/tapOn -d '{"txt":"Login"}' | jq -r .error.message
tapOn has no field "txt"; it takes text, id, width, height, … and 32 more (`maestro-d commands tapOn` lists them all)
```

Three shorthands are accepted for brevity, all equivalent to the object form:

```sh
curl -s -H "$H" $C/tapOn      -d '"Login"'      # bare scalar → the command's scalar field (text)
curl -s -H "$H" $C/back                          # no body → a value-less command
curl -s -H "$H" $C/repeat     -d '{"times":3,"commands":[{"tapOn":{"text":"Next"}},"waitForAnimationToEnd"]}'
```

Which field a bare scalar fills is in [commands.md](commands.md) ("a bare
value sets `text`"). Nested steps inside `commands` / `elseCommands` are
validated the same way as the outer command.

| query | |
| --- | --- |
| `wait=false` | `BUSY` instead of queueing behind a running step |
| `cwd=DIR` | directory relative paths in the value (`runFlow: file`, `runScript`, `addMedia`) resolve against; local callers only (`FORBIDDEN` from a [remote caller](#remote-callers)) |

The response is a **step result**:

```json
{
  "ok": true, "exitCode": 0,
  "device": "29271FDH200ABP",
  "step": "tapOn: text=\"Login\"",
  "type": "tapOn",
  "index": 3,
  "durationMs": 312,
  "element": { "id": "login_btn", "text": "Login", "bounds": {"x": 40, "y": 812, "width": 320, "height": 48} },
  "artifacts": { "screenshotAfter": "assets/flow-000/cmd-003-after.png" },
  "reportDir": "/Users/me/.maestro-d/run/api/reports/29271FDH200ABP/20260910-100000"
}
```

| field | |
| --- | --- |
| `step` | the step as the report prints it |
| `type`, `index` | command name; position in this attachment's report |
| `durationMs` | |
| `message` | driver/runner text, e.g. the assertion that held |
| `skipped` | the step's `platform` did not match the device |
| `optional`, `error` | the step failed but was optional: `ok` stays true |
| `element` | the element a selector resolved to, when there was one |
| `data` | command output where there is one: the copied text for `copyTextFrom`, the typed text for `inputRandom*`, the script's value for `evalWebViewScript`, the file path for `startRecording` |
| `artifacts` | screenshot / hierarchy paths relative to `reportDir` |
| `outputs` | for a remote or client-named call, the files its output fields wrote (`outputs/<path>`, relative to `reportDir`); see [outputs](#outputs) |
| `subSteps` | nested results for `repeat`, `retry`, `runFlow` |

Unknown `{name}`: `USAGE`. A value the parser rejects (unknown field, wrong
type): `USAGE` with the parser's message. The step failing: `COMMAND_FAILED`
with `error.step`, `error.details.screenshot` and `error.details.hierarchy`
when artifacts were captured.

### `POST /v1/devices/{id}/steps` — a batch

```sh
curl -s -H "$H" $D/v1/devices/29271FDH200ABP/steps -d '{
  "steps": [ {"launchApp": "co.edgesecure.app"}, {"tapOn": "Login"}, "back" ],
  "continueOnError": false
}'
```

`steps` is a JSON list of steps (or `yaml` a string holding the YAML list,
or `bundle` an uploaded [flow bundle](#flow-bundles) whose root file is the
step list).
Each entry is `{"<command>": <body>}`, where `<body>` is what the single
command route takes, so the same field checking applies; a bad field reports
which step it was in (`step 2: tapOn has no field "txt"`).
Steps run in order in the device's session; the first failure stops the
batch unless `continueOnError` is set. Either way the response is a
batch result — `results` holds every step that ran, `error` is the first
failure and the status is its code's (`422` for `COMMAND_FAILED`):

```json
{
  "ok": true, "exitCode": 0,
  "device": "29271FDH200ABP",
  "results": [ {step result}, {step result}, … ],
  "passed": 2, "failed": 1, "skipped": 0
}
```

A batch's `outputs` collects every result's `outputs`.

With `bundle`, the daemon builds the bundle (below) and runs its root file's
steps with the session's flow directory set, for that call only, to the
root file's own directory, so relative `runFlow`, `runScript` and
`addMedia` resolve inside the bundle as they would beside the file; the
session's flow directory is restored afterwards.

Query `wait` and `cwd` as for commands.

### `POST /v1/devices/{id}/flows` — a flow file

```sh
curl -s -H "$H" $D/v1/devices/29271FDH200ABP/flows -d '{
  "file": "/Users/me/edge/maestro/login.yaml",
  "env": { "USER": "alice" }
}'
```

`file` (a path on the daemon's host; local callers only), `yaml` (the
file's text, with the `appId:` / `---` header) or `bundle` (an uploaded
[flow bundle](#flow-bundles); its root file runs, with the flow directory
set to the root's directory for the whole call, hooks included). The flow runs as `maestro-runner test` would —
`onFlowStart`/`onFlowComplete` hooks, the flow's own `env`, `runFlow`
relative to `cwd` or the file's directory — inside the device's session,
so the session's variables are visible to it and `output.*` it sets stay.
The response is one step result of type `runFlow` whose `subSteps` are the
flow's steps.

### Flow bundles

A client that runs a flow on another machine's daemon packs it with its
dependency closure (`maestro-d pack --flow FILE --json`): each file is
uploaded as a [blob](#blobs-uploaded-files), and the **manifest** names
them relative to their common directory:

```json
{ "root": "main.yaml",
  "files": { "main.yaml": "<sha256>", "sub/open-general.yaml": "<sha256>",
             "scripts/compute.js": "<sha256>", "photo.png": "<sha256>" } }
```

`POST …/flows {"bundle": <manifest>, "env": {…}}` and
`POST …/steps {"bundle": <manifest>, "continueOnError": …}` take it inline.
In a one-shot's flow field (`runFlow` `file`/`else`, `retry` `file`) it is
`bundle:sha256:<manifest sha256>/<root>`, with the manifest's JSON uploaded
as a blob too; `<root>` may be any file of the bundle. The daemon builds
the directory `bundles/<manifest sha256>/` in the session's report
directory, with hard links from the blob store, and runs from there; that
directory is one of the call's allowed roots, so the flow's relative
references, and its subflows', resolve inside it. A file not yet uploaded
is `USAGE`, naming it.

Before it creates anything the daemon checks every name and hash, and
answers `FORBIDDEN` otherwise: each `files` key and `root` must be a
relative, slash-separated path with no empty, `.` or `..` element (and no
backslash); `root` must be a key of `files`; each hash, and a `bundle:`
ref's manifest hash, must be 64 lowercase hex digits. A bundle's
`runShell` and host paths are refused as for any [remote
caller](#remote-callers).

```sh
B=$(maestro-d pack --flow main.yaml --json)
echo "$B" | jq -r '.bundle.units[] | "\(.sha256) \(.file)"' | while read SUM F; do
  curl -s -H "$H" -X PUT --data-binary @"$F" $D/v1/devices/$ID/blobs/$SUM >/dev/null
done
curl -s -H "$H" $D/v1/devices/$ID/flows -d "{\"bundle\": $(echo "$B" | jq -c .bundle.manifest)}"
```

## Blobs (uploaded files)

Every daemon keeps a content-addressed store at
`$MAESTRO_D_HOME/blobs/sha256/<hex>` (default `~/.maestro-d`). A client
that targets a daemon on another machine uploads each file a step, an
install or an attach needs, then names it `blob:sha256:<hex>/<basename>`.
The routes are per device, so on a mesh they reach the device's owner; the
device need not be attached. Blobs unused for 7 days are deleted when the
daemon starts. `{sha256}` must be 64 lowercase hex digits, or the request
is `FORBIDDEN` before any file is opened.

### `HEAD /v1/devices/{id}/blobs/{sha256}`

`200` when the daemon has the blob, `404` when it does not. Either way
`Upload-Offset` says how many bytes it holds (of a partial upload, for a
404).

### `PUT /v1/devices/{id}/blobs/{sha256}`

The body is the file's bytes, streamed (not subject to the 32 MiB JSON
cap). `Content-Range: bytes START-END/TOTAL` resumes an upload at
`START`, which must not be past `Upload-Offset`; without it the body is the
whole file. Bytes that arrive before a dropped connection are kept. When
the file is complete its sha256 is checked before it becomes visible: a
mismatch is `USAGE` and nothing is kept.

```json
{ "ok": true, "exitCode": 0, "sha256": "…", "offset": 81234, "complete": true }
```

`201` when the blob was stored, `200` when the daemon already had it,
`202` when a range left it incomplete. Concurrent uploads of one hash are
serialised.

Over TCP the CLI and the JavaScript library upload this way: after a reset
or refused connection, a `503` (`DAEMON_UNAVAILABLE` or `PEER_UNAVAILABLE`,
when a hub loses its link to the device's hub) or a gateway's 502/504, on
the `HEAD` or the `PUT`, they ask again and resume from `Upload-Offset`, up
to 3 times, waiting 0.5 s, 1 s and 2 s.

```sh
SUM=$(shasum -a 256 photo.png | cut -d' ' -f1)
curl -s -H "$H" -I $D/v1/devices/$ID/blobs/$SUM                     # 404: not there yet
curl -s -H "$H" -X PUT --data-binary @photo.png $D/v1/devices/$ID/blobs/$SUM
curl -s -H "$H" $D/v1/devices/$ID/commands/addMedia -d "{\"files\":[\"blob:sha256:$SUM/photo.png\"]}"
```

`maestro-d pack PATH… --json` prints each file's `sha256`, `size` and `ref`.
A directory (an iOS `.app`) is packed as a deterministic zip — sorted
entries, fixed timestamps — named after the directory, and the daemon
unpacks it into the step's or installer's copy.

## Apps

### `POST /v1/devices/{id}/apps`

```sh
curl -s -H "$H" $D/v1/devices/emulator-5554/apps -d '{"app": "blob:sha256:…/settings_apk-debug.apk"}'
```

Installs an app without attaching, with the installers `attach --app-file`
uses (`adb install -r -g`; `simctl install`; `devicectl`, then go-ios, on a
physical iPhone). `app` is a `blob:` ref, or — local callers only — a path
on the daemon's host (`.apk`, `.app` or `.ipa`). `reinstall: true`
uninstalls the app first; `platform` overrides the platform the file and
the device imply. The answer names the app:

```json
{ "ok": true, "exitCode": 0, "device": "emulator-5554", "appId": "io.appium.settings" }
```

`appId` is the `CFBundleIdentifier` of an `.app` or an `.ipa`'s
`Payload/*.app`, or an `.apk`'s package name from `aapt2 dump packagename`
(the newest `build-tools` under `$ANDROID_HOME`); without `aapt2` it is
left out. An installer failure is `DEVICE_ERROR`.

### `DELETE /v1/devices/{id}/apps/{appId}`

Uninstalls by bundle id or package name.

## Artifacts

### `GET /v1/devices/{id}/artifacts`

The attached session's report directory: screenshots, `outputs/`,
recordings, hierarchy dumps, the report files. Uploaded inputs (`blobs/`)
are left out.

```json
{ "ok": true, "exitCode": 0, "device": "…", "reportDir": "…",
  "artifacts": [ { "path": "outputs/3f2a91c0/photos.png", "size": 81234, "modTime": "2026-11-08T20:01:02Z" }, … ] }
```

### `GET /v1/devices/{id}/artifacts/{path…}`

One file's bytes. `path` is relative to the report directory, as the
listing and `outputs` name it; an absolute path, a `..` element, or a
symlink that leads out of the directory is `FORBIDDEN`.

## Reading state

### `GET /v1/devices/{id}/screenshot`

PNG bytes (`Content-Type: image/png`), or with `?format=json` /
`Accept: application/json` the envelope `{"ok": true, "data": "<base64>"}`.

```sh
curl -s -H "$H" $D/v1/devices/29271FDH200ABP/screenshot -o now.png
```

### `GET /v1/devices/{id}/hierarchy`

`{"ok": true, "data": {…}}` where `data` is the normalized tree
`maestro-runner hierarchy` prints — the same shape on every platform:

```json
{ "type": "FrameLayout", "bounds": {"x": 0, "y": 0, "width": 1080, "height": 2400},
  "children": [ { "type": "Button", "id": "login_btn", "text": "Login", "bounds": {…} }, … ] }
```

Nodes carry `type`, `id`, `text`, `bounds` and `children`; `enabled`
(false), `checked`, `selected` and `focused` (true) only when notable.
`?format=raw` returns the driver's own document (UIAutomator/WDA XML or
JSON) with its content type.

### `GET /v1/devices/{id}/state`

`{"ok": true, "data": {…}}` — foreground app, orientation and whatever else
the platform reports.

### `GET /v1/devices/{id}/info`

`{"ok": true, "data": {"platform": "android", "osVersion": "15", "deviceName": "Pixel 7", "deviceId": "29271FDH200ABP", "isSimulator": false, "screenWidth": 1080, "screenHeight": 2400, "appId": "co.edgesecure.app", "appVersion": "4.30.1", …}}`.

### `GET /v1/devices/{id}/vars`, `PUT|POST /v1/devices/{id}/vars`

Session variables: attach `env`, values set here, and `output.*` from
scripts and `copyTextFrom`. `PUT` merges the body — `{"USER": "alice"}` or
`{"vars": {"USER": "alice"}}`; values must be strings — and answers the full
set: `{"ok": true, "vars": {…}}`.

### `POST /v1/devices/{id}/eval`

```sh
curl -s -H "$H" $D/v1/devices/29271FDH200ABP/eval -d '{"script": "maestro.copiedText.length"}'
```

Runs JavaScript in the session's engine (same `maestro`, `output`,
variables and `http` as `evalScript`) and answers `{"ok": true, "value":
…}`. A thrown error is `COMMAND_FAILED`; an empty script `USAGE`.

## A whole session in curl

```sh
D=http://127.0.0.1:7788; H='Authorization: Bearer s3cret'
ID=29271FDH200ABP
maestro-d start --daemon api --http ${D#http://} --token s3cret

curl -sf -H "$H" $D/v1/devices/$ID/attach -d '{"appId":"co.edgesecure.app"}' >/dev/null
curl -sf -H "$H" $D/v1/devices/$ID/commands/launchApp -d '{"clearState":true}' >/dev/null
curl -sf -H "$H" $D/v1/devices/$ID/commands/tapOn -d '"Get started"' >/dev/null
curl -s  -H "$H" $D/v1/devices/$ID/screenshot -o step1.png
if ! curl -sf -H "$H" $D/v1/devices/$ID/commands/assertVisible -d '{"text":"Create account","timeout":5000}'; then
  echo "not there"
fi
curl -s -X POST -H "$H" $D/v1/shutdown
```

`curl -f` turns any non-2xx into exit 22, which is the shell equivalent of
the CLI's non-zero exit; drop it and read `.ok` / `.error.code` from the
body when the distinction matters.
