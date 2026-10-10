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

## Daemon lifecycle

| command | |
| --- | --- |
| `start [--http ADDR --token T] [--idle-timeout N] [attach flags]` | start the named daemon in the background if it is not running; with `--device` also attach |
| `serve` | run the daemon in the foreground (what `start` spawns; useful under a supervisor) |
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
| `device list` (`ls`) | connected devices, simulators and emulators (booted or not) with attach state |
| `device start NAME --platform ios\|android [--boot-timeout N]` | boot a simulator (name or UDID) or emulator (AVD name); recorded as daemon-owned |
| `device stop [ID] [--force] [--orphans]` | shut a device down (detaching first); only daemon-booted ones without `--force`; `--orphans` cleans up after dead daemons |
| `attach --device ID [flags]` | open a driver on a device now rather than on the first command |
| `detach [--device ID \| --all]` | close the driver; the device stays as it is |

Attach flags map one to one onto maestro-runner's `test` flags:
`--platform`, `--driver`, `--app-file`, `--app-id`, `--url`, `--team-id`,
`--wda-bundle-id`, `--appium-url`, `--caps`, `-e/--env`, `--env-file`,
`--artifacts all|none|on-failure`, `--output-dir`, `--no-app-install`,
`--no-driver-install`, `--driver-start-timeout`, `--wait-for-idle-timeout`,
`--condition-timeout`, `--step-delay`, `--typing-frequency`,
`--command-timeout`, `--new-command-timeout`, `--alert-monitor[=false]`,
`--headed`, `--browser`, `--window-size`, `--follow-window`,
`--user-data-dir`, `--android-tcp-forward`.

On the web, `--window-size` pins the page to an emulated viewport of that
size whatever the window does. Add `--follow-window` to size the real window
instead, so the page reflows when the window is resized by hand; taps by
percentage and visibility checks then use the window's current size.

```sh
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

## Environment

| variable | |
| --- | --- |
| `MAESTRO_D` | default `--daemon` |
| `MAESTRO_DEVICE` | default `--device` |
| `MAESTRO_D_HOME` | run-file root: sockets, logs, reports (default `~/.maestro-d`) |
| `MAESTRO_D_BIN` | binary to spawn as the daemon (default: this executable) |
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
