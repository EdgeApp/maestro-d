# maestro-d

Drive a phone, simulator, emulator or browser from JavaScript one Maestro
command at a time — no YAML flow files, no re-launching the driver between
steps.

```js
import { MaestroD } from 'maestro-d'

const m = await MaestroD.attach({ device: '29271FDH200ABP', appId: 'co.edgesecure.app' })
await m.launchApp({ clearState: true })
await m.tapOn({ text: 'Get Started' })
await m.inputText({ text: 'alice@example.com' })
await m.assertVisible({ text: 'Welcome', timeout: 10_000 })
const png = await m.screenshot()
```

Every command from the Maestro YAML vocabulary (`tapOn`, `assertVisible`,
`swipe`, `runFlow`, …) is a method with the same name, taking an object of
that command's fields — the same object the REST API takes. Behind the scenes the first call starts a background
**daemon** (`maestro-d serve`) that keeps the device driver open; every
later call — from this process, a later `node` run, or the `maestro-d`
CLI — is one HTTP request to it. The daemon is
[maestro-runner](https://github.com/devicelab-dev/maestro-runner) with a
server bolted on, so each step runs through the exact same code as a flow
file would.

- **Same daemon, three doors.** The JS library, the `maestro-d` CLI and
  the REST API share a session, so a script can drop to the shell and back.
- **Errors are `MaestroError`s.** A failed step rejects with a code
  (`COMMAND_FAILED`, `DEVICE_ERROR`, …), the CLI exit code, the HTTP status,
  and the full step result. Programming mistakes (wrong argument shapes) are
  `TypeError`s.
- **No runtime dependencies.** Node ≥ 18, CommonJS and ESM, TypeScript
  types for every command's fields.

## Install

```sh
npm install maestro-d
```

The Go binary comes from a per-platform optional dependency
(`maestro-d-darwin-arm64`, `-darwin-x64`, `-linux-arm64`, `-linux-x64`).
If you install with `--no-optional`, or run on another platform, put
`maestro-d` on `PATH` or point `MAESTRO_D_BIN` at it.

The platform package carries the device drivers and is the binary's home
(caches land inside it, as with maestro-runner's own npm package). To reuse
a `make build` install instead, set `MAESTRO_RUNNER_HOME=$HOME/.maestro-d`
— or point `MAESTRO_D_BIN` at `~/.maestro-d/bin/maestro-d`.

Device prerequisites are maestro-runner's: `adb` for Android, Xcode for iOS
(`teamId` for a physical iPhone; an attach without one uses the daemon's
`MAESTRO_TEAM_ID` / `DEVELOPMENT_TEAM`, which a daemon `connect()` spawns
inherits from your environment), a browser for web.

## Concepts

**Daemon.** One background process per *name* (`--daemon`, `$MAESTRO_D`,
default `default`). A daemon owns one or more attached devices and exits
after 30 minutes idle (configurable) or on `shutdown()`. Different processes
should use different daemon names rather than sharing one; a device can only
be attached to one daemon at a time (`DEVICE_IN_USE` otherwise).

**Device.** A `MaestroDevice` is a handle on a device the daemon has a driver
open on. `attach()` opens the driver (installing the app if `appFile` is
given); `detach()` closes it and leaves the device alone; `stop()` shuts the
simulator/emulator down.

**Step.** Each method call is one step in the device's session, with the same
variables (`${NAME}`), `output` object and JavaScript engine as a flow. Steps
on one device run one at a time; steps on different devices run in parallel.

## API

### `MaestroD`

```ts
static connect(opts?: ConnectOptions): Promise<MaestroD>
static attach(opts?: ConnectOptions & DeviceAttachOptions): Promise<MaestroDevice>
```

`connect()` finds the named daemon, spawning it when it is not running.

| option | meaning |
| --- | --- |
| `name` | daemon name (default `$MAESTRO_D`, else `default`) |
| `spawn: false` | reject with `DAEMON_UNAVAILABLE` instead of starting one |
| `idleTimeout` | seconds idle before the spawned daemon exits (0 = never; default 1800) |
| `http`, `token` | also listen on TCP (`127.0.0.1:7788`) with a bearer token |
| `url`, `token` | connect to an existing TCP listener instead of the unix socket (nothing is spawned) |
| `hub`, `token`, `ca` | connect to the daemon or hub on another machine: `'host[:port]'` (port 7433 by default), `'https://host[:port]'`, or `true` for `localhost:7433`. Nothing is spawned, `name` is ignored, and no working directory is sent (see below). `ca` is PEM text or a file path for a self-signed certificate. Defaults: `$MAESTRO_D_HUB`, `$MAESTRO_D_TOKEN`, `$MAESTRO_D_CA` |
| `bin` | path to the binary (else `$MAESTRO_D_BIN`, the platform package, `PATH`) |
| `startTimeout` | ms to wait for the daemon to come up (default 20000) |
| `log` | `(line) => void` for "Starting daemon …" progress |
| `signal` | `AbortSignal` that aborts every request through this handle |

Instance:

| member | |
| --- | --- |
| `name`, `info`, `started` | daemon name (the hub's URL for `hub`), its `daemon.json`, whether this call spawned it |
| `hub` | true when connected with `hub` / `$MAESTRO_D_HUB` |
| `status()` | daemon info plus the devices it has attached or booted |
| `devices({platform?})` | connected devices, booted simulators/emulators, attached sessions |
| `device(id)` | one device record (`DEVICE_NOT_FOUND`) |
| `catalog({platform?, available?})` | what `createDevice()` can use: `{ios: {runtimes, deviceTypes}, android: {images, profiles}}` |
| `createDevice({platform, name, type?, runtime?, image?, profile?, dataSize?, installImage?, acceptLicenses?, boot?, bootTimeout?})` | create a simulator or AVD and boot it (unless `boot: false`); recorded as created by maestro-d |
| `startDevice({platform?, name, bootTimeout?})` | boot a simulator/AVD by name, UDID or `avd:<name>`; `platform` may be left out for a listed device; the daemon shuts it down on exit |
| `stopDevice(id, {force?})` | shut a device down; devices the daemon did not boot need `force` (`DEVICE_EXTERNAL`) |
| `deleteDevice(id, {force?})` | delete a simulator or AVD (stopping it first); ones maestro-d did not create need `force` (`DEVICE_EXTERNAL`) |
| `attach(opts)` | open a driver and return a `MaestroDevice` (below); on a hub it needs a lease the client holds — from `lease()` or `maestro-d lease` (see [Leases](#leases-on-a-hub)) |
| `lease({device \| select, ttlSec?, holder?, attach?})` | on a hub: lease a device and return its handle, attached unless `attach: false` (see [Leases](#leases-on-a-hub)) |
| `leasing` | true when the daemon is a hub (`info.hub`) and leases its devices |
| `detach(id)` | close the driver on a device |
| `events({device?, after?, signal})` | `AsyncIterable<DaemonEvent>` of step/device events (Server-Sent Events) |
| `shutdown({wait?})` | detach everything, stop what the daemon booted, exit |
| `close()` | release the leases this process took with `lease()`, then drop idle connections; the daemon keeps running. Returns a promise |

### `daemon.attach(opts)` → `MaestroDevice`

`device` defaults to `$MAESTRO_DEVICE`, then to the only attached device;
otherwise `USAGE`. Attaching to an already-attached device returns a handle
without reconnecting, so a second process can pick up a session.

The rest are maestro-runner's run configuration, camel-cased: `platform`
(inferred from the id when omitted), `driver`, `appFile`, `appId`, `url`,
`teamId`, `wdaBundleId`, `appiumUrl`, `env`, `artifacts`
(`none | on-failure | all`), `noAppInstall`, `noDriverInstall`,
`driverStartTimeout` (s), `waitForIdleTimeout`, `conditionTimeout`,
`stepDelay`, `commandTimeout` (ms), `typingFrequency`, and `extra` for
driver-specific keys (`headed`, `browser`, `windowSize`, `userDataDir`, …).

### `MaestroDevice`

#### Command methods

One per YAML command, typed from the daemon's command table
(`COMMANDS`). The signature is

```ts
tapOn(value?: string | TapOnParams, opts?: TapOnParams & CallOptions): Promise<StepResult>
```

**Pass an object of the command's fields.** It is the same object the REST
route takes, so a call and a request are the same thing written twice:

```js
await dev.tapOn({ text: 'Login', timeout: 5000, optional: true })
// POST /v1/devices/{id}/commands/tapOn
// {"text": "Login", "timeout": 5000, "optional": true}

await dev.launchApp({ appId: 'co.edgesecure.app', clearState: true })
await dev.swipe({ direction: 'UP', duration: 400 })
await dev.assertVisible({ id: 'balance', timeout: 10_000 })
```

Every command accepts `optional`, `label`, `timeout` and `platform` (skip
the step on other platforms); the rest are per command, typed as
`<Command>Params` and listed in the [command reference][commands].

- **The YAML shorthand** works too, as it does in a flow file and on the
  CLI: `tapOn('Login')` is `- tapOn: Login`, `launchApp('co.edgesecure.app')`,
  `swipe('UP')`, `setDarkMode('dark')`. Which field it fills differs per
  command — `text` for `tapOn`, `appId` for `launchApp`, `path` for
  `takeScreenshot`, `condition` for `assertTrue` — so the object form is the
  one to reach for when it is not obvious. It is sent as written, not
  expanded client-side, because the parser accepts spellings the map form
  has no field for (`setDarkMode: dark` beside `{ enabled: true }`).
- **`opts`** adds fields to either form and carries the per-call `wait`,
  `cwd` and `signal` (see below).
- **Value-less** commands take nothing: `back()`, `hideKeyboard()`,
  `waitForAnimationToEnd()`.
- **Compound** commands take inline steps:
  `repeat({ times: 3, commands: [{ tapOn: { text: 'Next' } }] })`,
  `runFlow({ file: 'login.yaml', env: { USER: 'alice' } })`.

Wrong shapes throw `TypeError` before anything is sent. TypeScript rejects a
field the command does not have; from plain JavaScript the daemon does, with
`MaestroError` code `USAGE`. `command(name, value?, opts?)` is the untyped
form for commands the library does not know yet.

[commands]: https://github.com/EdgeApp/maestro-d/blob/master/docs/daemon/commands.md

#### Batches

| | |
| --- | --- |
| `run(steps, {continueOnError?})` | run a list of step objects (`[{ tapOn: 'A' }, 'back']`) in one request; stops at the first failure unless `continueOnError`; rejects with the partial `StepsResult` in `error.result` |
| `runYaml(yaml, opts)` | the same for a YAML list of steps |
| `flow({file \| yaml, env?})` | run a whole flow file, header included, as one compound step |

#### Inspection (not recorded as steps)

`screenshot()` → PNG `Buffer`; `hierarchy()` → the normalized tree
(`HierarchyNode`: `type`, `id`, `text`, `bounds`, `children`, the same on
every platform) / `hierarchyRaw()` → the driver's own XML/JSON (`Buffer`); `state()`; `platformInfo()`; `vars()` / `setVars({NAME: value})`;
`eval(script)` runs JavaScript in the session's engine; `refresh()` re-reads
the device record; `events({after?, signal})` streams this device's events.

#### Apps and artifacts

| | |
| --- | --- |
| `installApp(file, {reinstall?, platform?})` | install an `.apk`, `.app` or `.ipa` without attaching; resolves `{appId}` when the daemon can read it. On a hub the file is uploaded first, unless the hub has it |
| `uninstallApp(appId)` | remove an app by bundle id or package name |
| `artifacts()` | the session's report files: `{path, size, modTime}[]` |
| `artifact(path)` | one of them as a `Buffer` (`outputs/…`, `report.json`, …) |

#### Lifecycle

`detach()` closes the driver; `stop({force?})` also shuts the device down.
On a hub, `lease` is the handle's lease; `renew({ttlSec?})` restarts its
fixed window now (default the lease's own window; nothing renews it on its
own), and `release()` ends it — the hub detaches the device. `detach()`
leaves the lease held. `release()` on a handle without a lease (a plain
daemon) does nothing.

#### `CallOptions`

| option | |
| --- | --- |
| `wait: false` | return `BUSY` instead of queueing behind a step already running on this device |
| `cwd` | directory relative paths in the step resolve against (default `process.cwd()`); not sent to a hub |
| `signal` | `AbortSignal`; the step is cancelled on the device and the call rejects with `INTERRUPTED` |

### Results

Every successful call resolves with the daemon's JSON envelope; for a step:

```ts
interface StepResult {
  ok: true
  device: string
  step: string        // "tapOn: text=\"Login\""
  type: string        // "tapOn"
  durationMs: number
  message?: string
  skipped?: boolean   // platform did not match
  optional?: boolean
  error?: ErrorBody   // a failed *optional* step resolves with ok:true and this set
  element?: ElementInfo
  data?: unknown      // copyTextFrom text, extractTextWithAI, …
  artifacts: { screenshotBefore?, screenshotAfter?, viewHierarchy? }
  subSteps?: SubStep[]
  reportDir: string   // where artifacts for this session live
  outputs?: string[]  // on a hub: files the step's output fields wrote (outputs/…), already fetched back
}
```

### `MaestroError`

Every failure the daemon reports rejects with a `MaestroError`:

```ts
class MaestroError extends Error {
  code: ErrorCode        // 'COMMAND_FAILED' | 'USAGE' | …
  exitCode: number       // what the CLI exits with
  httpStatus: number     // what the REST API answers with
  reason: string         // the daemon's message, without the step prefix
  step?: string          // the step description, for step failures
  details?: Record<string, unknown>  // reportDir, screenshot, hierarchy, errorType, …
  result?: StepResult | StepsResult  // the full response for a failed step or batch
  toJSON(): { ok: false, exitCode, error: { code, message, step?, details? } }
}
```

| code | exit | HTTP | meaning |
| --- | --- | --- | --- |
| `COMMAND_FAILED` | 1 | 422 | the step ran and failed (element not found, assertion false, …) |
| `USAGE` | 2 | 400 | bad command, field or argument |
| `DEVICE_NOT_ATTACHED` | 2 | 404 | attach first |
| `DAEMON_UNAVAILABLE` | 3 | 503 | no daemon, not answering, shutting down |
| `DEVICE_ERROR` | 4 | 502 | the driver failed (device disconnected, driver crashed) |
| `DEVICE_NOT_FOUND` | 4 | 404 | no such device id |
| `INTERNAL` | 4 | 500 | daemon bug |
| `BUSY` | 5 | 409 | `wait: false` and the device is running a step |
| `DEVICE_IN_USE` | 5 | 409 | another daemon has the device |
| `DEVICE_EXTERNAL` | 5 | 409 | `stopDevice` on a device this daemon did not boot, or `deleteDevice` on one maestro-d did not create |
| `INTERRUPTED` | 130 | 499 | the `signal` fired |
| `FORBIDDEN` | 2 | 403 | a remote caller asked for the daemon's host: `runShell`, a host path, `cwd`, a web attach |
| `PEER_UNAVAILABLE` | 3 | 503 | hub mesh: the device's hub is unreachable (retried automatically) |
| `HUB_NAME_TAKEN`, `HUB_VERSION` | 5 | 409 | hub mesh: a joining hub's name is taken, or its API version differs |
| `LEASE_REQUIRED`, `LEASE_EXPIRED` | 5 | 409 | hub: the device's lease is missing or expired |

`isMaestroError(err, code?)` narrows in a `catch`. `EXIT_CODES` and
`HTTP_STATUS` export the table, and `codeFromStatus(status)` the code for
a response that has no envelope (`401` is `USAGE`, `403` `FORBIDDEN`).

### Environment

| variable | |
| --- | --- |
| `MAESTRO_D` | default daemon name |
| `MAESTRO_DEVICE` | default device id for `attach()` |
| `MAESTRO_D_BIN` | path to the Go binary |
| `MAESTRO_D_HOME` | run-file root (default `~/.maestro-d`); sockets, logs and reports live under `run/<name>/` |
| `MAESTRO_D_HUB` | connect to this hub (`host[:port]`, `https://host[:port]`) when neither `hub` nor `url` is given |
| `MAESTRO_D_TOKEN` | bearer token for `hub` / `url` when `token` is not given |
| `MAESTRO_D_CA` | PEM file of trust roots for an https hub, when `ca` is not given |

### Another machine: `hub`

```js
const md = await MaestroD.connect({ hub: '192.168.64.1', token: process.env.T })
const dev = await md.attach({ device: '99FA590E-…' })
await dev.launchApp('com.apple.Preferences')
```

Precedence: an explicit `hub` or `url` option, then `$MAESTRO_D_HUB`, then
the local daemon — so a script that passes only `name`, such as
`e2e/iosLandscape.e2e.ts`, follows `MAESTRO_D_HUB` unchanged. Against a
hub, commands send no `?cwd=`, and flows run whole on the hub:
`flow({file})` packs the file with everything it uses (`maestro-d pack
--flow`) and posts the bundle, and `run()`/`runYaml()`/`flow({yaml})` pack
their steps as if they sat in `cwd`. A flow's outputs come back beside it
(for inline steps, into `cwd`); a `runFlow`/`retry` file in a command is
sent as a `bundle:` ref the same way.
The hub treats the script as a remote caller: `runShell` and host paths in
steps are refused with `FORBIDDEN`.

Files travel with the calls instead. A command's input files (`addMedia`,
`runScript('x.js')`, an `assertScreenshot` reference, `uploadFile`, …) and
`attach({appFile})` are packed by the maestro-d binary
(`$MAESTRO_D_BIN`, the platform package, or `PATH`), uploaded when the hub
lacks them, and sent as `blob:sha256:<hex>/<basename>` refs; bare values
count (`addMedia('photo.png')`). A hub attaches no Appium driver:
`driver: 'appium'`, `appiumUrl` and `extra.capsFile` are `FORBIDDEN`. Output files — `takeScreenshot({path})`,
an `assertScreenshot` baseline the hub writes because you have none yet, a
mismatch's `_diff.png` — are fetched back to the path you gave, relative
to `cwd` or absolute. `pack(paths)`, `upload(...)`, `packFlow(roots)`,
`uploadBundle(...)` and `fetchBundleOutputs(...)` are exported for callers
of the REST API.

### Leases on a hub

A hub (`maestro-d hub start`) lends each device to one client at a time;
`info.hub` names it. Every request for a device must carry that device's
lease (`LEASE_REQUIRED`, `LEASE_EXPIRED` otherwise). Taking a lease is
always explicit; once taken, the library sends it for you.

```js
const md = await MaestroD.connect({ hub: 'hub.lan', token })
const dev = await md.lease({ select: { platform: 'ios', kind: 'simulator' }, ttlSec: 1800 })
await dev.launchApp('com.apple.Preferences')   // the lease rides on every call
await dev.renew({ ttlSec: 1800 })              // only an explicit renew moves its end
await md.close()                               // releases it
```

- `lease({device | select, ttlSec, holder, attach})` normalizes `device`
  through `GET /v1/devices/{id}` (an id the hub does not list is leased as
  given), takes the lease, and returns the device's handle, **attached**:
  `attach` defaults to `{}` (the attach options of `attach()`), and
  `attach: false` only reserves the device. `select` takes the first free
  device matching `{platform, kind, name, osVersion, hub, boot}`. The lease
  is a fixed window of `ttlSec` (default 8 hours, or the device's `maxSec`
  when that is shorter; over the maximum rejects with `USAGE`). Using it
  never extends it and nothing renews it on its own: at the end of the
  window the hub detaches the device (a running step rejects with
  `INTERRUPTED`) and later calls reject with `LEASE_EXPIRED`. It is
  recorded in `<home>/leases.json`, like `maestro-d lease`, until
  `release()` or `md.close()`.
- `attach({device, …})` on a hub uses a lease this client holds — taken in
  this process, or found in `leases.json` (from `maestro-d lease` in the
  same home) — and never takes one; without one it rejects with
  `LEASE_REQUIRED`, naming `md.lease()` and `maestro-d lease --device <id>`.
  So a script written for a local daemon — `e2e/iosLandscape.e2e.ts` — runs
  on a hub after `maestro-d lease --device <id>` in the shell.
- `leases.json` is written through a temporary file and a rename (Node
  cannot take the CLI's `flock`), so a script and a CLI updating it at the
  same instant can lose one update; when several processes share a home and
  lease at once, lease from the shell.
- Device entries on a hub carry `maxSec`, the device's longest lease
  window.
- Another client's device rejects with `DEVICE_IN_USE`; a selector with no
  match with `DEVICE_NOT_FOUND`. On a daemon that is not a hub, `lease()`
  rejects with `USAGE` and `attach()` works as before.

Over TCP (`hub` or `url`) every command, batch and flow carries an
`X-Maestro-Request-Id`; a dropped connection, `PEER_UNAVAILABLE` or a
gateway's 502/504 is retried up to 3 times with the same id, and the
daemon runs each id once.

## Recipes

**Two devices in one script.** Steps on different devices run in parallel.

```js
const d = await MaestroD.connect({ name: 'pair-test' })
const [android, ios] = await Promise.all([
  d.attach({ device: '29271FDH200ABP', appId: 'co.edgesecure.app' }),
  d.attach({ device: '00008110-0006098A02FA801E', appId: 'co.edgesecure.app', teamId: 'G5LQ7MERPK' }),
])
await Promise.all([android.launchApp(), ios.launchApp()])
```

**Handle a failing step.**

```js
try {
  await m.assertVisible('Balance')
} catch (err) {
  if (isMaestroError(err, 'COMMAND_FAILED')) {
    console.error(err.message, err.details.screenshot)
    process.exit(err.exitCode)
  }
  throw err
}
```

**Time out a step.**

```js
await m.extendedWaitUntil({ visible: 'Done', timeout: 60_000 }, { signal: AbortSignal.timeout(90_000) })
```

**Watch events while a batch runs.**

```js
const ac = new AbortController()
;(async () => {
  for await (const ev of m.events({ signal: ac.signal })) console.log(ev.type, ev.step, ev.passed)
})()
await m.run(steps, { continueOnError: true })
ac.abort()
```

**Simulator lifecycle.**

```js
const sim = await d.startDevice({ platform: 'ios', name: 'iPhone 16' })
const m = await d.attach({ device: sim.id, appFile: 'build/Edge.app' })
// …
await d.shutdown() // also shuts the simulator down, since the daemon booted it
```

**Create and delete a device.**

```js
const cat = await d.catalog({ platform: 'android' })            // images, profiles
const avd = await d.createDevice({ platform: 'android', name: 'ci-pixel', dataSize: '8G', boot: false })
// avd.id === 'avd:ci-pixel'
const emu = await d.startDevice({ name: avd.id })                // no platform needed; emu.id is the serial
// …
await d.deleteDevice(emu.id)                                     // stops the emulator, deletes the AVD
```

## CLI

The package also installs the `maestro-d` command:

```sh
npx maestro-d launchApp co.edgesecure.app --device 29271FDH200ABP
npx maestro-d tapOn Login
npx maestro-d commands           # every one-shot command
npx maestro-d tapOn --help       # a command's fields
npx maestro-d status
npx maestro-d stop
```

See the [maestro-d repository](https://github.com/EdgeApp/maestro-d)
for the CLI and REST documentation.

## Development

```sh
npm install
npm test        # builds, then runs test/ against a daemon driving the mock driver
```

The tests build the Go binary from the repository root; set
`MAESTRO_D_TEST_BIN` to use one you already built.

## License

Apache-2.0, like maestro-runner.
