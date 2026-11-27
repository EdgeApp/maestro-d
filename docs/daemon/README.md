# The maestro-d daemon

maestro-d is [maestro-runner](https://github.com/devicelab-dev/maestro-runner)
plus a persistent background process. maestro-runner runs a YAML flow file from
start to finish: it opens a driver on the device, executes the steps, writes a
report and closes the driver. That is the right shape for a test suite but the
wrong one for an agent or a script that wants to *drive* a device — tap,
look, decide, tap again — where writing a flow file per step and paying the
driver start-up (seconds on Android, tens of seconds for WebDriverAgent on a
physical iPhone) every time is unworkable.

The daemon keeps the driver open. Every YAML command becomes something you can
call one at a time:

| surface | example | docs |
| --- | --- | --- |
| CLI | `maestro-d tapOn Login --device FAKEPIXEL0002` | [cli.md](cli.md) |
| REST | `POST /v1/devices/FAKEPIXEL0002/commands/tapOn` with body `"Login"` | [rest.md](rest.md) |
| JavaScript | `await device.tapOn('Login')` | [js.md](js.md) |

The [command reference](commands.md) lists every command with its fields;
it is generated from the flow parser, so it cannot drift from what the
runner accepts. [network.md](network.md) covers traffic shaping through
each host's configured shaper calls. [streaming.md](streaming.md) covers live screen streams,
the browser viewer and recordings. [cloud.md](cloud.md) covers where each
device is (local, mesh or cloud), the listing filters, and real phones on
AWS Device Farm. [UPSTREAM.md](UPSTREAM.md) describes
how the fork tracks maestro-runner.

## How it works

```
maestro-d tapOn Login ──┐
node script (maestro-d) ─┼── unix socket ──▶ maestro-d serve ──▶ driver ──▶ device
curl http://127.0.0.1:7788 ──┘   (or TCP)         (one process per --daemon)
```

1. **First call spawns the daemon.** `maestro-d tapOn …`, `MaestroD.connect()`
   and `maestro-d start` all look for a daemon called `<name>` (`--daemon`,
   `$MAESTRO_D`, default `default`). If none is running they start
   `maestro-d serve --daemon <name>` detached, wait for it to write
   `daemon.json` and answer on its socket, then proceed. This is the
   Edge CLI engine pattern: the caller never manages the process.
2. **The daemon attaches devices.** Attaching opens a driver on a device
   (UIAutomator2, WebDriverAgent, Playwright, the computer device's VNC
   connection to its own Mac — [macos.md](../macos.md) — …) exactly as
   `maestro-runner test` would, using the same run configuration
   (`--platform`, `--team-id`, `--app-file`, …). A daemon can hold several
   devices; each has its own session — variables, JavaScript engine, report
   directory. Attaching never launches or restarts an app: the driver
   session it opens (WebDriverAgent's, on iOS) names no app, so whatever
   is on screen stays as it is, and `--app-id` only becomes the default
   for a later `launchApp`. An attach that cannot open that session fails
   with `DEVICE_ERROR` rather than leaving a device on which every tap
   would fail.
3. **Steps go through the runner unchanged.** A command arrives as the YAML
   value it would have in a flow file (`Login`, `{id: submit}`), is parsed by
   the flow parser and executed by the flow runner with the device's session.
   Screenshots, hierarchies and reports land where a flow run would put them.
   Steps on one device are serialised; steps on different devices run in
   parallel.
4. **The daemon goes away on its own.** After 30 minutes without a request
   (`--idle-timeout`; 0 disables) it detaches everything, shuts down any
   simulator or emulator *it* booted, and exits. `maestro-d stop` does
   the same immediately.

## Names, devices and sharing

- **One daemon per name, many devices per daemon.** Names are how
  independent processes stay out of each other's way: an agent working on a
  task uses `--daemon <task>` (or exports `MAESTRO_D=<task>`) and gets a
  daemon nobody else touches.
- **A device belongs to one daemon at a time.** Attaching a device another
  daemon holds fails with `DEVICE_IN_USE`. `maestro-d ps` shows every
  daemon of the current user and the devices each holds; check it before
  picking a device.
- **The daemon only stops what it started.** Devices and simulators that
  were running before the daemon attached are left running when it stops.
  Simulators/emulators booted through the daemon (`device start`,
  `POST /v1/devices`) are recorded in `devices.json` and shut down at exit;
  `device stop --force` overrides the rule for one device and
  `device stop --orphans` cleans up after a daemon that died.

## Files

Everything lives under `~/.maestro-d/run/<name>/` (`$MAESTRO_D_HOME`
overrides the root):

| file | |
| --- | --- |
| `daemon.sock` | unix socket the CLI and library use |
| `daemon.json` | pid, version, socket path, TCP address, start time — removed on clean exit |
| `daemon.lock` | locked by the running daemon from before it binds the socket; a second start of the same name fails `DAEMON_UNAVAILABLE` instead of taking the socket (a hub writes `daemon.json` only once its mesh has started) |
| `devices.json` | devices this daemon booted (ownership survives a crash so `--orphans` can clean up) |
| `startup.log` | the spawned process's stdout/stderr until it was ready (shown when spawning fails) |
| `daemon.log` | the daemon's log |
| `reports/<device>/<timestamp>/` | report directory of each attach: `report.json`, `junit-report.xml`, `flows/`, `assets/` (failure screenshots, hierarchies), `outputs/`; and `client.log` with `MAESTRO_D_ANDROID_TRACE=1` |

Drivers, caches and downloaded browsers live in the same `~/.maestro-d`,
under `bin/`, `drivers/`, `cache/` and `browsers/`. The binary resolves that
home the way maestro-runner resolves its own: `$MAESTRO_RUNNER_HOME` (still
upstream's variable name), else the parent of its `bin/` directory — so
installing to `~/.maestro-d/bin/maestro-d` is all it takes. maestro-d
deliberately keeps its own home rather than sharing `~/.maestro-runner`, so
installing it cannot disturb an existing maestro-runner.

### What maestro-d keeps on disk

maestro-d keeps only what a later run reuses. Each daemon sweeps its home
half a minute after it starts and then hourly (`maestro-d gc` runs the same sweep on demand); the iOS
runners prune their build caches when they start.

| path | kept | removed |
| --- | --- | --- |
| `blobs/sha256/`, `blobs/partial/` | uploaded app packages, reused by later installs | when unused for 24 hours |
| `run/<name>/reports/<device>/<ts>/` | the session's report, after detach too | 3 days after its last write, unless a live daemon still lists it; at detach with `discardReports` |
| `run/<name>/reports/…/blobs/`, `bundles/` | while a session in that directory is attached | at detach of the last session using the directory |
| `run/<name>/` | while its daemon runs | 7 days after its last write, once the daemon is gone |
| `$TMPDIR/rod/user-data/<id>/` | while the web session runs | when the session closes; an orphan a day idle (a week where `ps` is unavailable) |
| `$TMPDIR/maestro-d-web-recording-<id>/` | a browser recording's frames while it runs ([streaming.md](streaming.md#recording-the-browser)) | at `stopRecording`, or when the web session closes |
| `cache/wda-builds/<config>/` | the WebDriverAgent build | unused for 30 days; a real-device build only once this host has compiled a build for its team since |
| `cache/wda-builds/<config>/results/` | the running test's result bundle | when the runner stops |
| `cache/devicelab-ios-runner-builds/`, `cache/embedded-runner-src/` | the runner build and source in use | unused for 30 days |
| `bin/`, `drivers/`, `browsers/`, `profiles/`, `hub/`, `leases.json` | always | never |
| `computer.json` (in `$MAESTRO_D_HOME`) | the computer device's Screen Sharing and macOS credentials, mode 0600 ([macos.md](../macos.md)) | never |
| `cache/text-helper/<hash>/MaestroDText` | the computer device's text-recognition helper, built on first use | when its source changes |

On Windows, where neither a daemon's lock nor its pid can be tested, the
sweep leaves report and run directories alone and deletes only blobs and
Chrome profiles.

The Android drivers trace nothing by default. With
`MAESTRO_D_ANDROID_TRACE=1` they write `client.log` into the session's
report directory, truncated at 50 MB, with typed and pasted text replaced
by `<redacted N chars>`.

## Errors and exit codes

One table serves all three surfaces. The CLI exits with the code, the REST
API answers with the status and the JSON error body, the JavaScript library
throws a `MaestroError` carrying all of it.

| `error.code` | exit | HTTP | when |
| --- | --- | --- | --- |
| — | 0 | 200 | the step(s) succeeded |
| `COMMAND_FAILED` | 1 | 422 | the step ran and failed: element not found, assertion false, timeout, script threw |
| `USAGE` | 2 | 400 | unknown command, bad flag or field, unparsable YAML, no device given |
| `DEVICE_NOT_ATTACHED` | 2 | 404 | the device is known but has no driver open (attach first) |
| `DAEMON_UNAVAILABLE` | 3 | 503 | could not connect to or spawn the daemon; API version mismatch; daemon shutting down |
| `DEVICE_ERROR` | 4 | 502 | the driver failed: device disconnected, WDA/UIAutomator2 died, panic contained |
| `DEVICE_NOT_FOUND` | 4 | 404 | no such device id |
| `INTERNAL` | 4 | 500 | a daemon bug |
| `CAPTURE_UNAVAILABLE` | 4 | 503 | the device has no screen capture, or it failed or sent no frames; `details.reason` says why ([streaming](streaming.md#errors)) |
| `UNSUPPORTED` | 2 | 501 | this host or device cannot do it: `setTrafficShape`/`resetTrafficShape` on a device with no traffic shaper configured on its host ([network](network.md)), or airplane mode on iOS |
| `BUSY` | 5 | 409 | `--no-wait` and the device is running another step |
| `DEVICE_IN_USE` | 5 | 409 | another daemon holds the device |
| `DEVICE_EXTERNAL` | 5 | 409 | `device stop` on a device the daemon did not boot (use `--force`) |
| `INTERRUPTED` | 130 | 499 | Ctrl-C / `AbortSignal`; the in-flight step is cancelled on the device |
| `FORBIDDEN` | 2 | 403 | a remote caller (a TCP client from beyond loopback, or any TCP client of a hub) asked for the host: `runShell`, a host path, `cwd`, a web attach |
| `PEER_UNAVAILABLE` | 3 | 503 | hub mesh: the hub that owns the device is unreachable; clients retry it |
| `HUB_NAME_TAKEN`, `HUB_VERSION` | 5 | 409 | hub mesh: a joining hub's name is taken, or it speaks another API version |
| `LEASE_REQUIRED`, `LEASE_EXPIRED` | 5 | 409 | hub: a mutating request without the device's lease, or with an expired one |
| `LEASE_NOT_STARTED` | 5 | 409 | hub: the request's lease is a reservation whose window has not started (`details.startsAt`) |

Every failure is the same JSON, on stderr for the CLI and as the body for
REST:

```json
{
  "ok": false,
  "exitCode": 1,
  "error": {
    "code": "COMMAND_FAILED",
    "message": "Element not found: text=\"Login\" (timeout 15000ms)",
    "step": "tapOn: text=\"Login\"",
    "details": {
      "reportDir": "…/reports/FAKEPIXEL0002/20260910-101500",
      "screenshot": "assets/flow-000/cmd-003-after.png",
      "hierarchy": "assets/flow-000/cmd-003-hierarchy.xml"
    }
  }
}
```

A failed step marked `optional: true` is *not* a failure: the result has
`ok: true` and the `error` object attached for inspection.

## Install

```sh
git clone https://github.com/EdgeApp/maestro-d
cd maestro-d
make build          # → ~/.maestro-d/bin/maestro-d (and copies drivers/)
export PATH="$HOME/.maestro-d/bin:$PATH"
maestro-d doctor
```

Or from npm, which brings the binary along: `npm install maestro-d`
(see [js.md](js.md)).

## A first session

```sh
maestro-d devices                                   # what this machine can see
maestro-d attach --device FAKEPIXEL0002 --app-id co.edgesecure.app
maestro-d launchApp                                 # uses the attach --app-id
maestro-d tapOn "Get started"
maestro-d get screenshot -o now.png
maestro-d assertVisible "Create account" || echo "not there (exit $?)"
maestro-d stop
```

The same session from JavaScript:

```js
import { MaestroD } from 'maestro-d'
const m = await MaestroD.attach({ device: 'FAKEPIXEL0002', appId: 'co.edgesecure.app' })
await m.launchApp()
await m.tapOn('Get started')
await fs.promises.writeFile('now.png', await m.screenshot())
```
