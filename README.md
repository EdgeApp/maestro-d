<div align="center">

# maestro-d

**Drive a real phone one command at a time — from a shell, over HTTP, or from JavaScript.**

[CLI](docs/daemon/cli.md) · [REST API](docs/daemon/rest.md) · [JavaScript](docs/daemon/js.md) · [Commands](docs/daemon/commands.md) · [How it works](docs/daemon/README.md)

</div>

`maestro-d` is [maestro-runner](https://github.com/devicelab-dev/maestro-runner)
plus a persistent daemon. Upstream runs a YAML flow start to finish; this fork
keeps the device session open between calls, so every Maestro command is also a
CLI command, a REST route and a JavaScript method — no flow file, no relaunching
the app, no re-attaching the driver.

```sh
maestro-d launchApp co.edgesecure.app --device 29271FDH200ABP   # ~4s: spawns the daemon, attaches
maestro-d tapOn "Create account"                                # ~1s
maestro-d assertVisible "Write these words down" --timeout 5000 # ~1s
maestro-d get screenshot -o step.png
```

The first call starts a background daemon and attaches the device. Every call
after it reuses that session, so the loop is a second or two instead of a full
flow run. Exit status is the result: `0` passed, `1` the step failed, and a JSON
error envelope on stderr says why.

Everything upstream does still works unchanged — `maestro-d test flows/` runs
your existing YAML. See the [upstream README](docs/maestro-runner.md).

## Why

Writing a flow file, running it, and reading a report is the wrong loop for
three cases this is built for:

- **Exploring an app.** Tap, look, tap again — the same way you would by hand,
  but scriptable and reproducible.
- **Agents and scripts.** An LLM agent or a shell script can issue one command,
  read the exit code and the JSON, and decide what to do next. `get hierarchy`
  and `get screenshot` tell it what is on screen.
- **Building a flow.** Get the steps right interactively, then paste them into
  a `.yaml` file that upstream runs in CI.

## Three ways in

**CLI** — one subcommand per YAML command, flags for its fields:

```sh
maestro-d tapOn --id submit --index 1     # - tapOn: {id: submit, index: 1}
maestro-d inputText "alice@example.com"
maestro-d swipe --direction UP --duration 400
maestro-d copyTextFrom --id balance --json | jq -r .data
maestro-d run steps.yaml                  # a batch, still on the open session
```

**REST** — the same daemon over a unix socket, or TCP with a bearer token:

```sh
maestro-d start --daemon api --http 127.0.0.1:7788 --token "$TOKEN"
curl -X POST -H "Authorization: Bearer $TOKEN" \
  "http://127.0.0.1:7788/v1/devices/$UDID/commands/tapOn" \
  -d '{"text": "Login", "timeout": 5000, "optional": true}'
curl -N "http://127.0.0.1:7788/v1/events"      # server-sent step and device events
```

**JavaScript** — `npm i maestro-d`, one method per command, errors as `Error`s:

```js
import { MaestroD } from 'maestro-d'

const m = await MaestroD.attach({ device: '29271FDH200ABP', appId: 'co.edgesecure.app' })
await m.launchApp({ clearState: true })
await m.tapOn({ text: 'Get started' })
try {
  await m.assertVisible({ text: 'Create account', timeout: 5_000 })
} catch (e) {
  if (e.code !== 'COMMAND_FAILED') throw e
  console.log('not there:', e.result?.artifacts.screenshotAfter)
}
await m.detach()
```

All three speak the same protocol to the same daemon, so a script and a shell
can share one device session.

## What the fork adds

- **A daemon per name.** `--daemon <name>` (or `$MAESTRO_D`) picks one; each is
  a separate process holding its own devices, so parallel agents never collide.
  A device attached elsewhere fails fast with `DEVICE_IN_USE` naming the owner.
- **Many devices per daemon.** Attach a Pixel and an iPhone to one session and
  address them with `--device`.
- **Every YAML command, generated from the parser.** All ~90 step types, their
  fields and docs come from the same structs the flow parser uses — CLI help,
  the TypeScript methods and [commands.md](docs/daemon/commands.md) cannot drift
  from what the runner actually accepts.
- **Session state between calls.** Variables (`set`, `-e`), `eval` for
  JavaScript in the flow engine, and `${VAR}` expansion, all persisting across
  commands.
- **Inspection.** `get screenshot`, `get hierarchy` (normalized the same way on
  Android, iOS and web, with `--find` and `--compact`), `get info`, `get state`.
- **Device lifecycle.** `device list`, `device start` to boot a simulator or
  emulator, `device stop`, `ps`, and automatic shutdown of anything the daemon
  booted.
- **Device management.** `device catalog`, `device create` and `device delete`
  make and remove iOS simulators and Android AVDs (with the AVD's data
  partition size), from the CLI, REST (`GET /v1/catalog`, `POST /v1/devices`
  with `create`, `DELETE …?delete=true`) and JavaScript (`catalog()`,
  `createDevice()`, `deleteDevice()`). Only what maestro-d created is deleted
  without `--force`.
- **Devices on another machine.** `--target-hub[=HOST[:PORT]]` (or
  `$MAESTRO_D_HUB`) points any client command at a daemon started with
  `serve --http ADDR --token T` (`--tls-cert`/`--tls-key` for HTTPS);
  JavaScript connects with `MaestroD.connect({hub})`. Callers from beyond
  loopback drive devices but not the host: `runShell` and host paths are
  refused with `FORBIDDEN`, and the daemon's environment stays on the host.
  Request ids make a retried command over a lossy link run once.
- **Files across machines.** Against another machine, a command's input
  files (`addMedia photo.png`, `runScript x.js`, `assertScreenshot ref.png`,
  `attach --app-file`) are uploaded — once, resumably, by content hash — and
  its outputs (`takeScreenshot --path shot.png`) come back to the path you
  gave. `install FILE` and `uninstall APP_ID` manage apps without attaching,
  `get artifacts` fetches the session's report files, and `pack` shows the
  upload units.
- **Whole flows on the device's host.** `run flow.yaml --target-hub=…` packs
  the flow with everything it uses (`pack --flow`: subflows, scripts, media,
  hooks), uploads what the host lacks, and runs it there in one request;
  its screenshots come back beside the flow. `runFlow sub/x.yaml` and the
  JavaScript `flow()`, `run(steps)` and `runYaml()` ship their closure the
  same way.
- **A device hub.** `hub start --token-file F --share-device NAME` shares
  this machine's devices on the network (`:7433`, every interface; every TCP
  caller is remote). Clients lease a device explicitly before driving it:
  `lease --device ID` or by selector (`--platform ios --kind simulator`);
  no command takes a lease on its own, and without one the hub answers
  `LEASE_REQUIRED`. A lease is a fixed window (`--ttl`, default 8 hours,
  at most the hub's `--lease-max`, hub-wide or per device); using it never
  extends it, only `renew` does, and at its end the hub detaches the device.
  `device list` shows who holds what; `release` frees a device.
  JavaScript: `md.lease({select})` and `device.renew()`; `attach()` on a hub
  uses a lease from `md.lease()` or `maestro-d lease`.
- **Machine-readable failure.** One error table shared by all three interfaces:
  `COMMAND_FAILED` 1/422, `USAGE` 2/400, `FORBIDDEN` 2/403,
  `DAEMON_UNAVAILABLE` 3/503, `DEVICE_ERROR` 4/502, `DEVICE_IN_USE` 5/409,
  `INTERRUPTED` 130/499.
- **Events.** Server-sent events for every step and device transition, resumable
  with `Last-Event-ID`.

And everything upstream already gives it: Android via UIAutomator2 or the
DeviceLab on-device driver, iOS simulators **and physical devices** via
WebDriverAgent, desktop browsers via CDP, cloud grids via Appium, React Native
and Flutter element finding, HTML/JUnit/Allure reports, and a single binary with
no JVM.

## Install

**From a release** — one command, no Go, Node.js or checkout (macOS and
Linux, arm64 and x64):

```sh
curl -fsSL https://github.com/EdgeApp/maestro-d/releases/latest/download/install.sh | bash
```

It downloads the latest release's tarball for this computer, checks it
against the release's `SHA256SUMS`, installs it into `~/.maestro-d`
(`bin/maestro-d` and `drivers/`), and links `maestro-d` into
`/usr/local/bin` or `/opt/homebrew/bin` when writable, else `~/.local/bin`.
No sudo. Running it again upgrades in place and keeps `~/.maestro-d/cache`
(WebDriverAgent builds). After an upgrade, `maestro-d stop --all` so running
daemons pick up the new binary.

```sh
curl -fsSL …/install.sh | bash -s -- --version 2.0.0   # a specific release
curl -fsSL …/install.sh | bash -s -- uninstall         # remove it (keeps cache/)
```

`--prefix <dir>` and `--bin-dir <dir>` install elsewhere. Each release's
tarballs (`maestro-d-<version>-<os>-<arch>.tar.gz`, one self-contained home)
are on the [releases page](https://github.com/EdgeApp/maestro-d/releases) too.

**From source** — Go 1.23+, into the same home:

```sh
git clone https://github.com/EdgeApp/maestro-d
cd maestro-d && make build            # → ~/.maestro-d/bin/maestro-d
export PATH="$HOME/.maestro-d/bin:$PATH"
```

Then check the toolchain and see what is plugged in:

```sh
maestro-d doctor
maestro-d devices
```

Android testing needs `adb`; iOS needs Xcode's command-line tools (and
`--team-id` for physical devices); web testing needs Chrome or Chromium.

## Documentation

| | |
| --- | --- |
| [docs/daemon/README.md](docs/daemon/README.md) | How the daemon works: lifecycle, ownership, run files, error table |
| [docs/daemon/cli.md](docs/daemon/cli.md) | Every CLI command and flag |
| [docs/daemon/rest.md](docs/daemon/rest.md) | REST reference with curl examples |
| [docs/daemon/js.md](docs/daemon/js.md) | The npm package |
| [docs/daemon/commands.md](docs/daemon/commands.md) | All YAML commands and their fields |
| [docs/daemon/UPSTREAM.md](docs/daemon/UPSTREAM.md) | Which upstream files the fork touches, and how to rebase |
| [docs/maestro-runner.md](docs/maestro-runner.md) | Upstream's README — the flow runner, drivers, cloud providers |

## License

Apache License 2.0 — see [LICENSE](LICENSE). A fork of
[devicelab-dev/maestro-runner](https://github.com/devicelab-dev/maestro-runner);
the Go module path is deliberately unchanged so rebasing stays a plain
`git rebase`.

For native iOS background recovery tests, see [screen lock and unlock](docs/ios-screen-lock.md).
