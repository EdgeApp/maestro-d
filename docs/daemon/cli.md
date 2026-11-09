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
  --device 00008110-0006098A02FA801E --platform ios --team-id G5LQ7MERPK
```

It spawns the daemon, attaches the iPhone (starting WebDriverAgent), runs the
step and leaves both in place for the next command. If the device is already
attached, attach flags are ignored (a warning goes to stderr when `--app-id`
differs from the one in effect; `detach` and `attach` again to change them).

### Another machine's daemon: `--target-hub`

Every client command — the one-shots, `run`, `device …`, `attach`,
`detach`, `get`, `set`, `eval`, `status`, `install` and `uninstall` — also takes:

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
maestro-d serve --daemon shared --http 0.0.0.0:7433 --token "$T" &
# anywhere on the network
export MAESTRO_D_TOKEN=$T
maestro-d device list --target-hub=192.168.64.1
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

## Daemon lifecycle

| command | |
| --- | --- |
| `start [--http ADDR --token T] [--idle-timeout N] [attach flags]` | start the named daemon in the background if it is not running; with `--device` also attach |
| `serve [--http ADDR --token T] [--tls-cert F --tls-key F] [--pass-env NAME …]` | run the daemon in the foreground (what `start` spawns; useful under a supervisor). `--tls-cert`/`--tls-key` serve HTTPS on `--http`. An `--http` address beyond loopback needs `--token`, and sessions then import none of this process's environment except each `--pass-env NAME` |
| `status` | daemon pid, version, socket, report dir, idle deadline and its devices; exit 3 when not running |
| `ps` | every daemon of this user (running or stale) with the devices it holds |
| `stop [--all] [--timeout N]` | detach every device, shut down the simulators/emulators the daemon booted, exit |

```sh
maestro-d start --daemon ci --http 127.0.0.1:7788 --token "$TOKEN"
maestro-d ps
maestro-d stop --all
```

## Devices

| command | |
| --- | --- |
| `device list` (`ls`) | connected devices, simulators (booted or not), running emulators under their AVD names, and shut-down AVDs as `avd:<name>`, with attach state |
| `device catalog [--platform ios\|android] [--available]` | what `device create` can use: iOS runtimes and device types (simctl), Android system images (sdkmanager; `--available` adds downloadable ones) and hardware profiles (avdmanager) |
| `device create --platform ios --name N [--type T] [--runtime R] [--no-boot]` | create a simulator; defaults are the newest installed runtime and the newest iPhone it supports |
| `device create --platform android --name N [--image I] [--profile P] [--data-size S] [--install-image --accept-licenses] [--no-boot]` | create an AVD; defaults are the newest installed `google_apis` image for the host's ABI, `pixel_8` and a 6G data partition (at most 10G) |
| `device start NAME [--platform ios\|android] [--boot-timeout N]` | boot a simulator (name or UDID) or emulator (AVD name or `avd:<name>`); recorded as daemon-owned. Without `--platform`, `avd:` means android and a listed name or UDID picks its platform |
| `device stop [ID] [--force] [--orphans]` | shut a device down (detaching first); only daemon-booted ones without `--force`; `--orphans` cleans up after dead daemons |
| `device delete ID\|NAME [--force]` | delete a simulator or AVD (by UDID, name, `avd:<name>` or a running emulator's serial), detaching and stopping it first; only ones `device create` made without `--force` (`DEVICE_EXTERNAL`) |
| `attach --device ID [flags]` | open a driver on a device now rather than on the first command |
| `detach [--device ID \| --all]` | close the driver; the device stays as it is |

Attach flags map one to one onto maestro-runner's `test` flags:
`--platform`, `--driver`, `--app-file`, `--app-id`, `--url`, `--team-id`,
`--wda-bundle-id`, `--appium-url`, `--caps`, `-e/--env`, `--env-file`,
`--artifacts all|none|on-failure`, `--output-dir`, `--no-app-install`,
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
maestro-d attach --device 29271FDH200ABP --app-file app-release.apk --app-id co.edgesecure.app
maestro-d attach --device mock-1 --platform mock      # a fake device for trying things out
```

## Batches, flows and state

| command | |
| --- | --- |
| `run FILE\|-  [-e K=V] [--continue-on-error]` | a file with a config header (`appId:` / `---`) runs as a flow, hooks and flow env included; a bare YAML list runs step by step in the session |
| `get screenshot [-o FILE\|-]` | PNG (default `screenshot-<time>.png`) |
| `get hierarchy [--compact \| --find TEXT \| --raw]` | view hierarchy as the normalized JSON tree; `--compact` flat listing, `--find` matching elements (as `maestro-runner hierarchy`); `--raw` the driver's own XML/JSON |
| `get state` | foreground app, orientation, … |
| `get info` | platform, OS version, device name, app version |
| `get vars` | session variables |
| `get artifacts [-o DIR]` | the session's report files (screenshots, `outputs/`, recordings, report); `-o DIR` downloads them all into `DIR`, keeping their paths |
| `set KEY=VALUE …` | set session variables |
| `eval 'SCRIPT'` | run JavaScript in the session engine and print the value; `output.X` assignments persist |

```sh
printf -- '- launchApp\n- tapOn: Login\n' | maestro-d run -
maestro-d run flows/login.yaml -e USER=alice
maestro-d get hierarchy | jq '.. | .text? // empty'
maestro-d get hierarchy --find "Sign in"
maestro-d set USER=alice PIN=1234
maestro-d eval 'maestro.copiedText'
```

## Apps and files

| command | |
| --- | --- |
| `install FILE [--reinstall] [--platform P]` | install an `.apk`, `.app` or `.ipa` on `--device` without attaching, and print its app id when it can be read (an `.apk`'s needs `aapt2`); with `--target-hub` the file is uploaded first, unless the daemon has it |
| `uninstall APP_ID` | remove an app by bundle id or package name |
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
| `MAESTRO_D_TLS_CERT`, `MAESTRO_D_TLS_KEY` | default `serve --tls-cert`/`--tls-key` |
| `MAESTRO_RUNNER_HOME` | install home: `bin/`, `drivers/`, `cache/`, `browsers/` (default: the parent of the binary's `bin/`, i.e. `~/.maestro-d`). Upstream's variable, upstream's name |

## Recipes

**An agent driving one device.** Pick a name and a device once, then every
command is short:

```sh
export MAESTRO_D=task-1234 MAESTRO_DEVICE=29271FDH200ABP
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
