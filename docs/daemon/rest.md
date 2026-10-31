# REST API

The daemon is an HTTP server. The CLI and the JavaScript library are clients
of it; anything that can speak HTTP can drive a device the same way.

## Reaching the daemon

| transport | when | how |
| --- | --- | --- |
| unix socket | always | `~/.maestro-d/run/<name>/daemon.sock` (`$MAESTRO_D_HOME` overrides the root) |
| TCP | daemon started with `--http ADDR` | `http://ADDR`, with `Authorization: Bearer <token>` when `--token` was given |

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
  A missing or wrong token is `401` with a `USAGE` body.

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
  (`INTERRUPTED`, 499 if the response can still be written).

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
| `DEVICE_IN_USE` | 409 | 5 | another daemon holds the device |
| `DEVICE_EXTERNAL` | 409 | 5 | `DELETE` on a device the daemon did not boot (use `force`) |
| `INTERRUPTED` | 499 | 130 | the request was cancelled mid-step |

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
    "idleTimeout": 1800
  },
  "devices": [ … as GET /v1/devices, attached ones only … ],
  "idleShutdownAt": "2026-09-10T10:30:00Z"
}
```

The only route that still answers while the daemon is shutting down.

### `POST /v1/shutdown`

Detaches every device, shuts down the simulators and emulators the daemon
booted, and exits. Answers `{"ok": true}` first; the process is gone a
moment later. No body.

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
`device.stopped`, `daemon.shutdown`. `message` carries free text where
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
    { "id": "00008110-0006098A02FA801E", "platform": "ios", "kind": "device", "name": "iPhone", "ready": true, "attached": false, "owner": "other-daemon" }
  ]
}
```

| field | |
| --- | --- |
| `ready` | booted / online, so it can be attached |
| `attached`, `attachedAt`, `appId`, `driver`, `reportDir` | this daemon's attachment |
| `busy` | a step is running on it right now |
| `bootedBy`, `bootedAt` | the daemon that booted it (it will shut it down at exit) |
| `owner` | another daemon has it attached — attaching here fails with `DEVICE_IN_USE` |

### `POST /v1/devices` — boot a simulator or emulator

```sh
curl -s -H "$H" $D/v1/devices -d '{"platform":"ios","name":"iPhone 16","bootTimeout":120}'
```

`name` is a simulator name or UDID, or an Android AVD name. Waits until the
device is ready and answers `{"ok": true, "device": {…}}` with the booted
device (its `id` is what to attach). The device is recorded as booted by
this daemon.

### `DELETE /v1/devices/{id}`

Detaches if attached, then shuts the simulator/emulator down. Refuses with
`DEVICE_EXTERNAL` unless the daemon booted it; body `{"force": true}` or
`?force=true` overrides. Answers `{"ok": true}`.

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
| `appFile` | `--app-file` | installed on attach unless `noAppInstall` |
| `appId` | `--app-id` | default for `launchApp`, `stopApp`, `clearState`, … |
| `url` | `--url` | web |
| `teamId` | `--team-id` | physical iOS: signs WebDriverAgent |
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
the driver's own error).

### `POST /v1/devices/{id}/detach`

Closes the driver; the device keeps running. `{"ok": true}`, or
`DEVICE_NOT_ATTACHED`.

### `GET /v1/devices/{id}`

One device, same shape as the list entries. `DEVICE_NOT_FOUND` if the host
cannot see it and the daemon does not hold it.

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
| `cwd=DIR` | directory relative paths in the value (`runFlow: file`, `runScript`, `addMedia`) resolve against |

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

`steps` is a JSON list of steps (or `yaml` a string holding the YAML list).
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

Query `wait` and `cwd` as for commands.

### `POST /v1/devices/{id}/flows` — a flow file

```sh
curl -s -H "$H" $D/v1/devices/29271FDH200ABP/flows -d '{
  "file": "/Users/me/edge/maestro/login.yaml",
  "env": { "USER": "alice" }
}'
```

`file` (a path on the daemon's host) or `yaml` (the file's text, with the
`appId:` / `---` header). The flow runs as `maestro-runner test` would —
`onFlowStart`/`onFlowComplete` hooks, the flow's own `env`, `runFlow`
relative to `cwd` or the file's directory — inside the device's session,
so the session's variables are visible to it and `output.*` it sets stay.
The response is one step result of type `runFlow` whose `subSteps` are the
flow's steps.

## Reading state

### `GET /v1/devices/{id}/screenshot`

PNG bytes (`Content-Type: image/png`), or with `?format=json` /
`Accept: application/json` the envelope `{"ok": true, "data": "<base64>"}`.

```sh
curl -s -H "$H" $D/v1/devices/29271FDH200ABP/screenshot -o now.png
```

### Screenshot comparison

`assertScreenshot` takes three options for unattended runs: `mask`, selectors whose
element bounds are left out of the comparison on both images, and which fail the step if
an element is missing or has empty bounds; `requireBaseline`, which fails with
`BASELINE_MISSING` instead of creating a missing reference; and `captureOut`, where the
settled frame is always written. With either of the last two, the result's `data` holds
`code`, `matchPercentage`, `differingPixels`, `totalPixels`, `diffPath` and `capturePath`,
and a screen that changed size is compared on a common canvas, so it has a diff too.
Paths must sit inside the device's `flowDir`; a call's `cwd` does not apply to them.

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

`{"ok": true, "data": {…}}` — `appState`, `foregroundApp` (the bundle or package id
in front), orientation and whatever else the platform reports. The foreground lookup
has a two-second limit; on timeout the two fields are absent and the rest still answer.
`devicelab_ios` reports the state of the attached app rather than whichever app is in front.

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
