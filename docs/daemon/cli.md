# CLI

`maestro-d` is `maestro-runner` with more commands. Everything upstream
has (`test`, `devices`, `doctor`, `hierarchy`, `screenshot`, `wda`, `lint`)
works unchanged; this page covers what the fork adds. `maestro-d --help`
lists it all, `maestro-d commands` lists the one-shot commands and
`maestro-d <command> --help` shows one command's fields.

## One-shot commands

Every YAML command is a subcommand. The positional argument is the bare value
the YAML would hold; `--<field> value` sets map fields.

```sh
maestro-d tapOn Login                     # - tapOn: Login
maestro-d tapOn --id submit --index 1     # - tapOn: {id: submit, index: 1}
maestro-d tapOn Login --optional          # - tapOn: {text: Login, optional: true}
maestro-d inputText "alice@example.com"
maestro-d swipe --direction UP --duration 400
maestro-d assertVisible Welcome --timeout 10000
maestro-d back                            # value-less commands take nothing
maestro-d launchApp co.edgesecure.app --clearState
maestro-d setLocation --latitude 37.77 --longitude -122.41
```

Rules:

- **Bare value → scalar field.** Which field is shown by `--help`
  ("A bare value sets `text`") and in [commands.md](commands.md). Commands
  without one (`setLocation`, `assertCondition`, …) take fields only.
- **No value at all** is allowed where YAML allows it: `launchApp`,
  `stopApp`, `killApp`, `clearState` (the attach `--app-id`),
  `takeScreenshot`, `scroll`, `eraseText`, `waitForAnimationToEnd`,
  `stopRecording`. Anything else without a value or field is `USAGE`.
- **Nested fields** use dots: `--childOf.id container`,
  `--visible.text "Loading"` (for `extendedWaitUntil`).
- **Lists and maps** are JSON or YAML in one argument:
  `--containsDescendants '[{"text":"A"},{"id":"b"}]'`,
  `--env '{USER: alice}'`.
- **Nested steps** (`repeat`, `retry`, `runFlow` with inline `commands`)
  come from `--yaml FILE` or `--yaml -` (stdin) holding the command's whole
  YAML value:

  ```sh
  maestro-d repeat --yaml - <<'EOF'
  times: 3
  commands:
    - tapOn: Next
    - waitForAnimationToEnd
  EOF
  ```

- **Every command** also takes `--optional`, `--label TEXT`, `--timeout MS`
  and `--platform android|ios|web` (skip the step on other platforms; on the
  first command for a device it also selects the platform to attach with).
- **Unknown fields are rejected** (`USAGE`, exit 2) before anything reaches
  the device.
- **`${VAR}`** expands from session variables (`-e`, `set`, `copyTextFrom`
  outputs) exactly as in a flow: `maestro-d assertTrue '${count > 3}'`.
  Quote it so the shell does not expand it first.

### Output

One human line per step on stdout, the JSON failure envelope on stderr:

```
$ maestro-d tapOn Login
✓ tapOn: text="Login" (312ms)
$ maestro-d assertVisible Nope --timeout 2000; echo "exit $?"
✗ assertVisible — Element not found: text="Nope" (timeout 2000ms) (2011ms)
  screenshot: assets/flow-000/cmd-004-after.png
Error: Element not found: text="Nope" (timeout 2000ms)
{ "ok": false, "exitCode": 1, "error": { "code": "COMMAND_FAILED", … } }
exit 1
```

`--json` prints the full result envelope on stdout instead (success or
failure — a failure that never reached a step, such as `DEVICE_IN_USE`,
puts the error envelope there), which is what scripts and agents should
parse:

```sh
maestro-d copyTextFrom --id balance --json | jq -r .data
```

Exit codes are in the [error table](README.md#errors-and-exit-codes).

### Which daemon, which device

Flags accepted by every command, anywhere on the line:

| flag | |
| --- | --- |
| `-n, --daemon NAME` | daemon name (default `$MAESTRO_D`, else `default`) |
| `--device, --udid ID` | device (default `$MAESTRO_DEVICE`, else the only attached device; otherwise exit 2 listing what is attached) |
| `--json` | full JSON result on stdout |
| `--verbose` | progress lines on stderr ("Starting daemon…", "Attaching…") |
| `--no-spawn` | fail (`DAEMON_UNAVAILABLE`) instead of starting the daemon |
| `--no-wait` | fail (`BUSY`) instead of queueing behind a step already running on the device |
| `--idle-timeout N`, `--http ADDR`, `--token T`, `--start-timeout N` | used only when this call spawns the daemon |
| attach flags (`--platform`, `--driver`, `--app-file`, `--app-id`, `--team-id`, `-e KEY=VALUE`, …) | used only when this call attaches the device; see `attach` below |

When a command has a field with the same name as one of these flags the
field wins — `openBrowser --url`, `openLink --browser`, `runFlow --env` — so
give the attach setting to `attach` instead. `--daemon`, `--device`, `--json`
and the other daemon flags never collide with a field (which is why the
daemon is `--daemon`, not `--name`: `tapOn --name` is the form-field
selector).

So the very first command on a fresh machine can be the whole setup:

```sh
maestro-d launchApp co.edgesecure.app \
  --device FAKE-IOS-UDID-0001 --platform ios --team-id G5LQ7MERPK
```

It spawns the daemon, attaches the iPhone (starting WebDriverAgent), runs the
step and leaves both in place for the next command. If the device is already
attached, attach flags are ignored (a warning goes to stderr when `--app-id`
differs from the one in effect; `detach` and `attach` again to change them).

### Another machine's daemon: `--target-hub`

Every client command — the one-shots, `run`, `device …`, `attach`,
`detach`, `get`, `set`, `eval`, `status`, `install`, `uninstall`, `lease`,
`renew` and `release` — also takes:

| flag | |
| --- | --- |
| `--target-hub[=HOST[:PORT]]` | talk to the daemon at `HOST:PORT` over TCP instead of the local one (default `$MAESTRO_D_HUB`). Bare means `localhost:7433`; the port defaults to 7433; `https://HOST[:PORT]` uses TLS |
| `--token T` | the bearer token it requires (default `$MAESTRO_D_TOKEN`) |
| `--ca FILE` | a PEM trust root for a daemon with a self-signed certificate (default `$MAESTRO_D_CA`) |

The value must be joined with `=`: the flag never takes the next word, so
in `maestro-d tapOn --target-hub Login`, `Login` is still the text to tap
(and the daemon is `localhost:7433`).

```sh
# on the device's machine
maestro-d-hub start --token "$T" --peer-token "$PEER_TOKEN"
# anywhere on the network
export MAESTRO_D_TOKEN=$T
maestro-d device list --target-hub=192.168.64.1
maestro-d lease --device "$UDID" --target-hub=192.168.64.1
maestro-d launchApp com.apple.Preferences --target-hub=192.168.64.1 --device "$UDID"
maestro-d get screenshot --target-hub=192.168.64.1 --device "$UDID" --out shot.png
```

A targeted command never starts a daemon; an unreachable one exits 3
(`DAEMON_UNAVAILABLE`), and a missing or wrong token exits 2 (`USAGE`).
It sends no working directory, because the daemon is on another machine.

**Whole flows run on the device's host.** `run FILE --target-hub…` packs the
file with everything it uses (`maestro-d pack --flow`, below), uploads the
files the daemon lacks, and posts the bundle: a file with a config header
runs as a flow (`POST …/flows {bundle, env}`), a step list step by step
(`POST …/steps {bundle, continueOnError}`, so `--continue-on-error` keeps
its meaning). The flow's `runFlow`, `retry`, hooks, JavaScript and
subflows all run on the daemon, with one request for the run. Its output
files come back beside the flow, where a local run writes them:
`takeScreenshot: x` in `sub/open.yaml` lands in `sub/x.png`. `run -` packs
stdin as a step list or flow sitting in the working directory. `--include
GLOB` adds files a flow opens dynamically. A path that cannot be packed — a
`${VAR}` no `-e` resolves, a missing file, an absolute path, an output path
that leaves the flow's directory — stops the run before any upload, naming
the step and field.

```sh
maestro-d run e2e/testdata/hub/main.yaml --target-hub=192.168.64.1 --device "$UDID"
```

**Files travel with the command.** A one-shot's input files — `addMedia`
files, a `runScript` ending in `.js`, an `assertScreenshot` reference,
`runWebViewScript`/`runBrowserScript`, `uploadFile`, `loadAuthState`, and
`attach`'s `--app-file` — are packed (`maestro-d pack`),
uploaded only when the daemon lacks them (stderr says which), and sent as
`blob:` refs. A bare value counts: `addMedia photo.png` is uploaded like
`--files`. Its output files — `takeScreenshot --path`, `saveAuthState` —
are written on the daemon and fetched back to the path you gave
(`takeScreenshot --path name` writes `name.png`). An `assertScreenshot`
reference you do not have yet is a baseline the run writes and fetches
back; a mismatch fetches `<ref>_diff.png` beside your reference. A flow
file in `runFlow` (and its `else`) or `retry` is packed with its whole
closure into one bundle and sent as a `bundle:` ref, and the outputs its
subflow writes come back relative to that bundle's directory:
`runFlow sub/open.yaml` fetches `takeScreenshot: x` to `sub/x.png`. That daemon treats
a caller from beyond loopback as **remote**: `runShell` and host paths in
steps are refused with `FORBIDDEN` (exit 2) — see
[remote callers](rest.md#remote-callers). For the same reason a targeted
command refuses `--output-dir` and `--user-data-dir` (`USAGE`), and ignores
an `--output-dir` that came only from `MAESTRO_OUTPUT`, with a note on
stderr. It also refuses `--caps` or `APPIUM_CAPS` (`USAGE`) before sending:
a remote caller gets no Appium driver (`--driver appium` and
`--appium-url` are refused by the daemon with `FORBIDDEN`).

Over TCP every step-running request (and `install`) carries a request id, and the CLI
retries a dropped connection with the same id up to 3 times (0.5 s, 1 s,
2 s); the daemon runs each id once. `--verbose` prints each retry.

### Sharing devices: `hub start` and leases

Run `maestro-d-hub devices` on the device host. Every devices subcommand
accepts `--config FILE` and `--json`. It uses the hub's Unix socket when
running and works directly on the config file when the hub is stopped.
[Device config](config.md)

A **hub** is a daemon that shares this machine's devices with clients on the
network. It lends each device to one client at a time, with a **lease**.

```sh
# on the device host, list the device ID in config.json and use mode 0600 for the token files
maestro-d-hub start --token-file ~/.maestro-d/hub-token --peer-token-file ~/.maestro-d/peer-token
# anywhere on the network
export MAESTRO_D_TOKEN=$(cat hub-token)
maestro-d device list --target-hub=192.168.64.1          # LEASE: holder and time left, next reservation, or available
maestro-d lease --device "$UDID" --ttl 2h --target-hub=192.168.64.1   # leasing is always explicit
maestro-d tapOn General --target-hub=192.168.64.1 --device "$UDID"   # sends the lease on file
maestro-d renew --device "$UDID" --target-hub=192.168.64.1           # only renew moves the window's end
maestro-d release --device "$UDID" --target-hub=192.168.64.1
maestro-d reserve --device "$UDID" --start-at 2h --ttl 1h --target-hub=192.168.64.1   # a window that starts later
maestro-d claim --lease "$LEASE" --target-hub=192.168.64.1          # from its start, on any machine
```

| command | |
| --- | --- |
| `maestro-d-hub start [--name NAME] [--listen ADDR] [--advertise HOST:PORT] [--peer HOST[:PORT] …] [--no-mdns] --token T \| --token-file F --peer-token P \| --peer-token-file F [--tls-cert F --tls-key F] [--peer-ca F] [--lease-max [DEVICE=]DURATION …] [--reservation-lead DURATION] [--allow-remote-shell] [--pass-env NAME …] [--config F] [--foreground]` | start this machine’s hub in the background, with no idle timeout. `--name` is its name in the mesh (default: the short host name). `--listen` defaults to `:7433` and, unlike `--http`, binds every interface. Both tokens are required; the `-file` forms read a file only its owner can read, and the detached hub gets them through its environment, never its command line. At launch it advertises itself with mDNS, browses 3 s and dials every hub found plus every `--peer` (`--no-mdns` skips the LAN); it reports ready once every `--peer` answered, or after 10 s, and exits 5 when every `--peer` refused it (`HUB_NAME_TAKEN` for the peer's own name, `HUB_VERSION`). List each shared device's ID in [config.json](config.md). `--lease-max` sets the longest lease window: `DURATION` (`8h`, `90m`, or seconds) for the whole hub (default 8h), `DEVICE=DURATION` (repeatable) for one device by id or name. `--reservation-lead` is how far ahead a reservation may start (default 24h). `--config` names the [device config](config.md) (default `<home>/config.json`). `--foreground` runs it in this process, for a supervisor. See [hub.md](hub.md) |
| `maestro-d-hub devices [--json] [--config FILE]` | List configured devices with their descriptions, discovery and lease state. |
| `maestro-d-hub devices add ID [--description TEXT]` | Share a device, including a device currently offline. |
| `maestro-d-hub devices set ID --description TEXT` | Set the description. An empty string clears it. |
| `maestro-d-hub devices remove ID [--end-leases]` | Remove a device. Use `--end-leases` to end its grants and reservations. |
| `maestro-d-hub stop` | stop the hub: its leases end, its devices are detached and its peer links close (through launchd when the service is installed) |
| `maestro-d-hub status [--json]` | the hub's name, version and listener, and for each peer its address, link state, direction, RTT and device count (`GET /v1/hub`) |
| `maestro-d-hub service install [hub start flags]` / `uninstall [--purge]` / `status` | macOS: run the hub as the LaunchAgent `app.edge.maestro-d.hub` (one per machine; starts at login, restarts after a crash, not after `hub stop`; tokens in `<home>/hub/secrets`, never in the plist). While installed, `hub start` kickstarts it. `--purge` also deletes the tokens |
| `lease [--device ID \| --platform P --kind K --name N --os V --hub H [--boot]] [--ttl DURATION] [--holder H] [--no-attach] [attach flags] [--json]` | lease a device, by id or by selector, and attach it (`--no-attach` only reserves it). `--ttl` is the window, a duration (`2h`, `90m`) or seconds; default 8h, or the device's maximum when that is shorter, and over the maximum is `USAGE` (exit 2) naming it. A selector takes the first free match: ready, neither leased nor attached; with `--boot`, else a shut-down simulator or AVD, which the hub boots and shuts down again when the lease ends. With `--device` while this client already holds a live lease on it, it prints that lease instead. `--json` prints `{lease: {id, device, holder, startsAt, expiresAt, state, ttlSec, maxSec}, device}`. A reservation on file for the device does not stand in for a lease: the hub is asked, and a window that reaches a reservation is `DEVICE_IN_USE` naming `nextStartsAt` |
| `reserve (--device ID \| --platform P --kind K --name N --os V --os-min V --os-max V --hub H) --start-at TIME [--ttl DURATION] [--holder H] [--json]` | reserve a device for a window that starts later (`POST /v1/reservations`). `--start-at` is an RFC 3339 time or a duration from now (`90m`, `2h`), by this machine's clock, which must be within 60 s of the hub's (`USAGE` with `reason: clock-skew` otherwise); at most the hub's lead ahead (24h by default). `--ttl` as for `lease`. A selector takes the first device free for the whole window, ready now. The reservation is recorded in `leases.json`; before its start a command on the device is `LEASE_NOT_STARTED` (exit 5), and from it the command sends it and attaches the device. A hub that predates reservations is `USAGE` "does not take reservations" |
| `claim --lease ID [--no-attach] [attach flags] [--json]` | take a lease by its id, from any machine: a reservation once it has started, or a lease handed over. It records it in `leases.json` and attaches the device (`--no-attach` only records it). A reservation that has not started is `LEASE_NOT_STARTED` (exit 5), and nothing is recorded |
| `renew [--device ID] [--ttl DURATION] [--json]` | renew this client's lease on a device explicitly (without `--device`, every lease it holds on that hub that has started): its end moves to now plus `--ttl` (default the lease's own window). Over the device's maximum is `USAGE`, into the device's next reservation `DEVICE_IN_USE`; with no lease on file, `LEASE_REQUIRED` (exit 5) |
| `release --device ID \| --lease ID \| --all` | end this client's leases on a device, its reservations included (or one lease by id, which the hub is asked about even when it is not on file, or all of them on that hub); the hub detaches the device, and a reservation is cancelled without touching it. Against a daemon without leases it does nothing and exits 0 |

**A mesh of hubs.** Hubs link with each other ([hub.md](hub.md)), and any hub
serves every hub's devices. On a hub every device id is `<localId>@<hub>` and
every lease id `<uuid>@<hub>`; `device list` through any hub lists every hub's
devices (a hub that did not answer is named on stderr as `unreachable`), and a
command for `<udid>@hub-a` sent to hub-b runs on hub-a. An unqualified
`--device` resolves to the one hub that has it. `device catalog`, `device
create` and `device start` take `--hub NAME` to act on another hub's machine.
A hub whose link is down fails its devices' requests `PEER_UNAVAILABLE`
(exit 3).

How leases work:

- **Every command that drives a device needs its lease.** That is attach,
  detach, every one-shot, `run`, `get`, `set`, `eval`, `install`,
  `uninstall` and the uploads. Without it the hub answers `LEASE_REQUIRED`;
  with a lease that has ended, `LEASE_EXPIRED` (both exit 5). `device list`
  and a device's own record stay open to every client. With a reservation
  that has not started, `LEASE_NOT_STARTED` (exit 5), naming its start.
- **Leasing is always explicit.** No command takes a lease on its own. A
  command that names a `--device` this client holds no lease on is sent
  without one, and the hub answers `LEASE_REQUIRED` (exit 5); the message
  says to run `maestro-d lease --device <id>`. `lease` itself fails
  `DEVICE_IN_USE` (exit 5), naming the holder, while another client holds
  the device.
- **leases.json.** The CLI keeps its leases in `<home>/leases.json`
  (`$MAESTRO_D_HOME`, else `~/.maestro-d`), keyed by the hub's address and
  the device id the hub returns, and sends the matching one with every
  request. Before it looks one up it normalizes `--device` through the
  hub's `GET /v1/devices/{id}`; an id the hub does not list is kept as
  given. A client may hold several leases on one device (a lease now and
  reservations later); a command sends the one that started last, else the
  reservation that starts first. An entry the hub reports expired is
  dropped; the next command then sends the device's next lease on file, or
  fails `LEASE_REQUIRED` until `maestro-d lease` takes a new one. A script
  using the JavaScript library in the same home finds these leases too.
  Processes that share the home (parallel shards) update
  it under an advisory lock on `leases.json.lock`, so none loses another's
  lease.
- **A fixed window.** A lease lasts `--ttl` from its grant, used or not;
  using it never extends it. Only `maestro-d renew` moves its end. Each
  device has a maximum window (8 hours, or the hub's `--lease-max`);
  `device list --json` and the lease carry it as `maxSec`.
- **Expiry.** At the end of the window the hub ends the lease and detaches
  the device: a step still running is interrupted (`INTERRUPTED`), later
  commands with the lease fail `LEASE_EXPIRED`, a device the hub booted for
  the lease is shut down, and the device is listed as available again.
- **On a hub every TCP caller is remote**, loopback included — `runShell`
  (unless `--allow-remote-shell`) and host paths are `FORBIDDEN`, and
  sessions import none of the hub's environment except `--pass-env` names.
  Only the hub's unix socket is local; commands over it lease the same way.

```sh
LEASE=$(maestro-d lease --platform ios --kind simulator --ttl 30m --target-hub=hub.lan --json | jq -r .lease.id)
curl -H "Authorization: Bearer $T" -H "X-Maestro-Lease: $LEASE" \
  -X POST http://hub.lan:7433/v1/devices/$UDID/commands/tapOn -d '"General"'
```

## Daemon lifecycle

| command | |
| --- | --- |
| `start [--http ADDR --token T] [--idle-timeout N] [attach flags]` | start the named daemon in the background if it is not running; with `--device` also attach |
| `serve [--http ADDR --token T] [--config F]` | Run the local daemon in the foreground. `--http` must name a loopback address. `--config` or `MAESTRO_D_CONFIG` selects the [device config](config.md), which defaults to `<home>/config.json`. |
| `status` | daemon pid, version, socket, report dir, idle deadline and its devices; exit 3 when not running |
| `ps` | every daemon of this user (running or stale) with the devices it holds |
| `stop [--all] [--timeout N]` | detach every device, shut down the simulators/emulators the daemon booted, exit |
| `gc [--dry-run] [--json] [--include-build-cache]` | run the housekeeping sweep every daemon runs half a minute after it starts and then hourly (old blobs, idle reports, dead daemons' run dirs, orphaned Chrome profiles) without a daemon; `--include-build-cache` also prunes iOS runner builds unused for 30 days. See [What maestro-d keeps on disk](README.md#what-maestro-d-keeps-on-disk) |

```sh
maestro-d start --daemon ci --http 127.0.0.1:7788 --token "$TOKEN"
maestro-d ps
maestro-d stop --all
```

## Devices

| command | |
| --- | --- |
| `device list` (`ls`) `[--location local,mesh,cloud] [--provider P] [--kind K] [--name RE] [--os-min V] [--os-max V] [--available]` | connected devices, simulators (booted or not), running emulators under their AVD names, shut-down AVDs as `avd:<name>`, cloud devices, and on macOS the Mac itself as `mac` (the [computer device](../macos.md)), with attach state and a `LOCATION` column: `local`, `mesh (<machine>)` or `cloud (<provider>@<hub>)` relative to this computer (a `PROVIDER` column appears when a row is in the cloud); on a hub also a `LEASE` column: the holder and the time left, or `available`; with `--json` each entry also carries `location`, `machine`, and on a hub `maxSec` and `addresses`. The flags filter the list: `--name` is a case-insensitive regular expression on the name and id, `--os-min`/`--os-max` bound the OS version (`--os-max 17` admits 17.5), `--available` keeps devices a lease could take now. See [Cloud devices](cloud.md) |
| `device catalog [--platform ios\|android] [--available] [--hub NAME] [--location cloud] [--provider P] [--name RE] [--os-min V] [--os-max V]` | what `device create` can use: iOS runtimes and device types (simctl), Android system images (sdkmanager; `--available` adds downloadable ones) and hardware profiles (avdmanager). With `--location cloud`, the models a cloud provider can launch instead, one row each (PROVIDER, HUB, ID, PLATFORM, NAME, OS, FORM, AVAILABILITY), filtered by the other flags (`--available`: only models available now) and answered in a mesh by the hub that would launch them |
| `device create --platform ios --name N [--type T] [--runtime R] [--no-boot] [--hub NAME]` | create a simulator; defaults are the newest installed runtime and the newest iPhone it supports |
| `device create --platform android --name N [--image I] [--profile P] [--data-size S] [--install-image --accept-licenses] [--no-boot]` | create an AVD; defaults are the newest installed `google_apis` image for the host's ABI, `pixel_8` and a 6G data partition (at most 10G) |
| `device start NAME [--platform ios\|android] [--boot-timeout N] [--hub NAME]` | boot a simulator (name or UDID) or emulator (AVD name or `avd:<name>`); recorded as daemon-owned. Without `--platform`, `avd:` means android and a listed name or UDID picks its platform |
| `device create --provider devicefarm --platform android\|ios [--name MODEL] [--os-version V \| --os-min V --os-max V] [--app FILE\|ARN] [--auxiliary-app …]… [--app-id ID] [--appium-version 2\|3] [--caps FILE] [--idle-release DUR] [--boot-timeout S]` | launch a real phone on AWS Device Farm with this daemon's own AWS credentials (direct mode); `--name` is the model (exact name, catalog id or regular expression), `--idle-release` (Go duration or seconds, default 10m, 60s–150m) releases it after that long without a request. Not on a hub: lease one (`lease --location cloud`). See [Cloud devices](cloud.md) |
| `device stop [ID] [--force] [--orphans [--devicefarm-credentials-file F \| --devicefarm-profile P]]` | shut a device down (detaching first); only daemon-booted ones without `--force`; a cloud device is released and the minutes it used printed. `--orphans` cleans up after dead daemons: it shuts down the simulators and emulators they booted and releases the cloud sessions the cloud ledger holds for them, with this shell's AWS credentials or the file or profile given; each session it releases is printed with its minutes, and one it cannot release is listed with its ARN, kept, and the command exits non-zero |
| `device delete ID\|NAME [--force]` | delete a simulator or AVD (by UDID, name, `avd:<name>` or a running emulator's serial), detaching and stopping it first; only ones `device create` made without `--force` (`DEVICE_EXTERNAL`). A cloud device is released, with its minutes |
| `lease --location cloud --platform ios\|android [--provider P] [--name MODEL] [--os-min V --os-max V] [--hub H] [--ttl D] [--idle-release D] [--appium-version 2\|3] [--attach]` | launch a cloud device for a lease on the hub that holds the provider's credentials (`hub start --devicefarm`), and attach it; the lease's end releases it, and `release` prints the device minutes. See [Cloud devices](cloud.md) |
| `devicefarm sessions [--hub NAME]` | the Device Farm project's remote access sessions: NAME (the device id), STATUS, RESULT, MODEL, MINUTES (the billed figure once the session has COMPLETED) |
| `devicefarm account [--hub NAME]` | the Device Farm account: trial minutes left of the total, the unmetered device slots and the job timeout |
| `devicefarm apps [--hub NAME]` | the project's uploaded apps: NAME (`maestro-d-<content hash>.apk\|.ipa`), TYPE, STATUS, CREATED, REF (the upload ARN) |
| `devicefarm upload FILE [--hub NAME]` | upload an `.apk` or `.ipa` from this machine and print its ARN, for `device create --app ARN`; an identical upload from the last 30 days is reused (`already uploaded (reused)`). Through a remote hub it is refused (`FORBIDDEN`): install the app on a leased cloud device instead |
| `attach --device ID [flags]` | open a driver on a device now rather than on the first command |
| `detach [--device ID \| --all]` | close the driver; the device stays as it is |

Attach flags map one to one onto maestro-runner's `test` flags:
`--platform`, `--driver`, `--app-file`, `--app-id`, `--url`, `--team-id`,
`--wda-bundle-id`, `--appium-url`, `--caps`, `-e/--env`, `--env-file`,
`--artifacts all|none|on-failure`, `--output-dir`, `--discard-reports`,
`--no-app-install`,
`--no-driver-install`, `--driver-start-timeout`, `--wait-for-idle-timeout`,
`--condition-timeout`, `--step-delay`, `--typing-frequency`,
`--command-timeout`, `--new-command-timeout`, `--alert-monitor[=false]`,
`--headed`, `--browser`, `--window-size`, `--user-data-dir`,
`--android-tcp-forward`.

`device create` records what it makes in `<home>/created.json`
(`$MAESTRO_D_HOME`, else `~/.maestro-d`), shared by every daemon of that
home and kept across restarts; `device delete` checks it. A new AVD's
`--data-size` is written to its `config.ini` as `disk.dataPartition.size`.
`--install-image` downloads a missing system image with `sdkmanager --install`
and needs `--accept-licenses`; without it, a missing image fails `USAGE` with
the `sdkmanager` command to run. The Android tools are looked up under
`ANDROID_HOME` / `ANDROID_SDK_ROOT`, then on `PATH`.

```sh
maestro-d device catalog --platform ios
udid=$(maestro-d device create --platform ios --name ci-phone --type "iPhone 16" --runtime iOS-26-5 --json | jq -r .device.id)
maestro-d device create --platform android --name ci-pixel --data-size 8G --no-boot   # → avd:ci-pixel
serial=$(maestro-d device start avd:ci-pixel --json | jq -r .device.id)
maestro-d device delete "$serial"                     # stops the emulator, deletes the AVD
udid=$(maestro-d device start "iPhone 16" --platform ios --json | jq -r .device.id)
maestro-d attach --device "$udid" --app-file build/Edge.app
maestro-d attach --device FAKEPIXEL0002 --app-file app-release.apk --app-id co.edgesecure.app
maestro-d attach --device mock-1 --platform mock      # a fake device for trying things out
```

## Batches, flows and state

| command | |
| --- | --- |
| `run FILE\|-  [-e K=V] [--continue-on-error]` | a file with a config header (`appId:` / `---`) runs as a flow, hooks and flow env included; a bare YAML list runs step by step in the session |
| `get screenshot [-o FILE\|-] [--image jpeg\|png] [--quality N] [--region x,y,w,h]` | PNG, or JPEG on the [computer device](../macos.md) (default `screenshot-<time>.png` or `.jpg`); `--image`, else `-o`'s extension, picks the type; `--region` captures just that rectangle, in screen pixels |
| `get hierarchy [--compact \| --find TEXT \| --raw]` | view hierarchy as the normalized JSON tree; `--compact` flat listing, `--find` matching elements (as `maestro-runner hierarchy`); `--raw` the driver's own XML/JSON |
| `get state` | foreground app, orientation, … |
| `get info` | platform, OS version, device name, app version |
| `get vars` | session variables |
| `stream [--fps N] [--audio] [--duration S] [-o FILE\|-]` | the device's live screen as fragmented MP4, to stdout (refused when stdout is a terminal) or a file, until Ctrl-C, `--duration` or the daemon ends it (exit 0 and a whole file); prints `stream: <codecs> fps=<fps> audio=<bool>` to stderr. See [streaming.md](streaming.md) |
| `get artifacts [-o DIR]` | the session's report files (screenshots, `outputs/`, recordings, report); `-o DIR` downloads them all into `DIR`, keeping their paths |
| `set KEY=VALUE …` | set session variables |
| `eval 'SCRIPT'` | run JavaScript in the session engine and print the value; `output.X` assignments persist |

```sh
printf -- '- launchApp\n- tapOn: Login\n' | maestro-d run -
maestro-d run flows/login.yaml -e USER=alice
maestro-d get hierarchy | jq '.. | .text? // empty'
maestro-d get hierarchy --find "Sign in"
maestro-d stream --fps 30 --duration 10 -o screen.mp4
maestro-d stream | ffplay -
maestro-d set USER=alice PIN=1234
maestro-d eval 'maestro.copiedText'
```

## Apps and files

| command | |
| --- | --- |
| `install FILE [--reinstall] [--platform P]` | install an `.apk`, `.app` or `.ipa` on `--device` without attaching, and print its app id when it can be read (an `.apk`'s needs `aapt2`); with `--target-hub` the file is uploaded first, unless the daemon has it. On a cloud device the app is installed into the running session, which keeps its id (`.apk` or `.ipa`) |
| `uninstall APP_ID` | remove an app by bundle id or package name; a cloud device refuses it (`USAGE`) |
| `pack PATH… [--json]` | print each file's upload unit: sha256, size and `blob:` ref. A directory (an iOS `.app`) is zipped with sorted entries and fixed timestamps, so it always hashes the same |
| `pack --flow FILE… [--base DIR] [-e K=V] [--include GLOB] [--json]` | pack flow files (the first is the root) with their dependency closure into one bundle: `runFlow` files and `else`, `retry` files, the `onFlowStart`/`onFlowComplete` hooks, `runScript` `.js` files, `addMedia`, existing `assertScreenshot` references, the other file inputs, and the root flow's `config.yaml`. `${VAR}` in a path is expanded from `-e`; an unresolvable or absolute path, or an output path with `..`, is `USAGE` naming the step. Prints the manifest `{root, files: {relPath: sha256}}`, relative to the files' common directory, and its sha256. `--base DIR` places the first file as if it sat in `DIR` (the JS library's inline steps) |

```sh
maestro-d install drivers/android/settings_apk-debug.apk --device emulator-5554
maestro-d install build/My.app --device "$UDID" --target-hub=192.168.64.1
maestro-d uninstall io.appium.settings --device emulator-5554
maestro-d pack --flow e2e/testdata/hub/main.yaml --json
maestro-d get artifacts --device "$UDID" --target-hub=192.168.64.1 -o artifacts/
```

## Environment

| variable | |
| --- | --- |
| `MAESTRO_D` | default `--daemon` |
| `MAESTRO_DEVICE` | default `--device` |
| `MAESTRO_D_HOME` | run-file root: sockets, logs, reports (default `~/.maestro-d`) |
| `MAESTRO_D_BIN` | binary to spawn as the daemon (default: this executable) |
| `MAESTRO_D_HUB` | default `--target-hub` (a TCP daemon or hub to talk to instead of the local one) |
| `MAESTRO_D_TOKEN` | default `--token` |
| `MAESTRO_D_CA` | default `--ca` |
| `MAESTRO_D_TLS_CERT`, `MAESTRO_D_TLS_KEY` | default `maestro-d-hub start` `--tls-cert`/`--tls-key` |
| `MAESTRO_D_LISTEN` | default `hub start --listen` |
| `MAESTRO_D_MOCK_DEVICES` | tests only: `mock-1,mock-2` lists those mock-driver devices as ready, so leases and selectors see them (the mock driver attaches any id but lists none) |
| `MAESTRO_D_HOUSEKEEPING_INTERVAL` | how often a running daemon sweeps its home (a Go duration, default `1h`) |
| `MAESTRO_D_ANDROID_TRACE` | `1` makes the Android drivers write a redacted, capped `client.log` into the report directory |
| `MAESTRO_RUNNER_HOME` | install home: `bin/`, `drivers/`, `cache/`, `browsers/` (default: the parent of the binary's `bin/`, i.e. `~/.maestro-d`). Upstream's variable, upstream's name |

## Recipes

**An agent driving one device.** Pick a name and a device once, then every
command is short:

```sh
export MAESTRO_D=task-1234 MAESTRO_DEVICE=FAKEPIXEL0002
maestro-d ps                                  # nobody else on that device?
maestro-d launchApp co.edgesecure.app --clearState
maestro-d tapOn "Get started"
maestro-d get screenshot -o step1.png
…
maestro-d stop
```

**Poll for something.**

```sh
until maestro-d assertVisible "Synced" --timeout 1000 2>/dev/null; do
  maestro-d swipe --direction DOWN
done
```

**Parse results.**

```sh
if ! out=$(maestro-d tapOn Login --json 2>&1); then
  echo "$out" | jq -r '.error.code + ": " + .error.message'
fi
```
