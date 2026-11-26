# JavaScript library

The `maestro-d` npm package is a thin client for the [REST API](rest.md):
one method per YAML command, `MaestroError` for every failure, and the same
daemon the CLI uses, so a script and a shell can share a session.

```js
import { MaestroD } from 'maestro-d'

const m = await MaestroD.attach({ device: 'FAKEPIXEL0002', appId: 'co.edgesecure.app' })
await m.launchApp({ clearState: true })
await m.tapOn({ text: 'Get started' })
try {
  await m.assertVisible({ text: 'Create account', timeout: 5_000 })
} catch (e) {
  if (e.code !== 'COMMAND_FAILED') throw e
  console.log('not there', e.result?.artifacts.screenshotAfter)
}
await m.detach()
```

The package's own [README](../../npm/maestro-d/README.md) is the
reference: installation, the `MaestroD` / `MaestroDevice` API, result
and error shapes, `CallOptions` (`signal`, `wait`, `cwd`), events and
recipes. This page covers how it fits with the rest of the fork.

`MaestroD.connect({hub: 'host[:port]' | true, token, ca})` (or
`$MAESTRO_D_HUB`, `$MAESTRO_D_TOKEN`, `$MAESTRO_D_CA`) talks to a daemon
on another machine over TCP or TLS, the counterpart of the CLI's
`--target-hub`: nothing is spawned, no working directory is sent, and the
daemon applies its [remote-caller](rest.md#remote-callers) rules. Over TCP
step-running calls carry request ids and are retried with the same id
after a dropped connection; see
[request ids](rest.md#request-ids-and-retries).

On a hub, files travel with the calls, as they do for the CLI: a command's
input files (`addMedia`, a `runScript` ending in `.js`, an
`assertScreenshot` reference, …) and `attach({appFile})`
are packed by the bundled binary (`maestro-d pack`, so both clients pack
identically; `$MAESTRO_D_BIN` names it when no platform package is
installed), uploaded when the hub lacks them, and sent as `blob:` refs. A
hub attaches no Appium driver for a script: `driver: 'appium'`,
`appiumUrl` and `extra.capsFile` are refused `FORBIDDEN`. A
bare value counts: `device.addMedia('photo.png')`. Output files
(`takeScreenshot({path})`, a missing `assertScreenshot` baseline, a
mismatch's `_diff.png`) are fetched back to the path you gave, resolved
against `cwd` (default `process.cwd()`), so a script that writes a
screenshot and reads it back runs unchanged. `device.installApp(file)`,
`device.uninstallApp(appId)`, `device.artifacts()` and
`device.artifact(path)` are `POST/DELETE …/apps` and `GET …/artifacts`.
Those artifacts are fetchable while the device is attached. The report
directory stays after `detach()` for local readers and is swept 3 days after
its last write; `attach({…, discardReports: true})` deletes it at detach
instead.

`device.screenshot({image, quality, region})` resolves a `Buffer`: PNG, or
JPEG on the [computer device](../macos.md), unless `image` asks for
`'jpeg'` or `'png'`; `quality` is JPEG's 1–100 (default 80) and `region:
{x, y, width, height}` captures just that rectangle in screen pixels
([`GET …/screenshot`](rest.md#get-v1devicesidscreenshot)). With no options a
phone's PNG is byte for byte what it was before.

`device.stream({fps, audio, signal})` resolves a `VideoStream`
(`{fps: number | 'device', audio, codecs, body: ReadableStream<Uint8Array>}`):
the device's live screen as fragmented MP4 from
[`GET …/stream`](rest.md#get-v1devicesidstreamfps15audiofalse), on a
socket of its own (as `events()` is) and with the handle's lease on a hub.
Aborting `signal` ends `body` cleanly, after the last whole fragment (the
body only ever holds whole MP4 boxes, a `moof` with its `mdat`, as
`maestro-d stream -o` writes them). Cancelling `body` (`body.cancel()`,
a `break` out of a `for await` over it, or a `pipeline` that fails)
closes the connection. Each fragment's duration is an estimate, the gap
before it (a live stream cannot know the gap after it yet): a consumer
that feeds Media Source Extensions should retime the fragments as the
viewer does (`makeRetimer` in `pkg/daemon/viewer.html`, described under
[the viewer](streaming.md#the-viewer)), or a still Android screen gives a
discontinuity and the browser drops frames until the next keyframe. A
device without screen capture
rejects with `CAPTURE_UNAVAILABLE` and `details.reason`. It is never
retried. See [streaming.md](streaming.md).

Whole flows and step lists run on the hub as
[flow bundles](rest.md#flow-bundles). `device.flow({file})` packs the file
with its dependency closure (`maestro-d pack --flow`), uploads what the hub
lacks and posts `/flows {bundle, env}`. `run(steps)`, `runYaml(yaml)` and
`flow({yaml})` write the steps or YAML to a temporary root file, pack it
with `--base <cwd>` so its relative references resolve here as they would
locally, and post `/steps {bundle, continueOnError}` or `/flows {bundle}`.
A flow field of a command — `runFlow`'s file or `else`, `retry`'s file —
goes as a `bundle:` ref. Afterwards every file the result lists in
`outputs` is fetched to the same place relative to the bundle's directory:
the flow file's (common) directory, or `cwd` for inline steps. `include`
adds files a flow opens dynamically; a path that cannot be packed rejects
with `USAGE` naming the step, before anything is sent. `packFlow()`,
`uploadBundle()` and `fetchBundleOutputs()` are exported for REST callers.

To edit this machine's device pool, connect through its hub socket:

```js
const hub = await MaestroD.connect({ hubSocket: true })
try {
  const { path, devices, configError } = await hub.hubDevices()
  console.log(path, configError ?? devices)
  const device = await hub.setHubDevice('avd:ci-pixel', { description: 'CI phone' })
  await hub.removeHubDevice(device.id)
} finally {
  await hub.close()
}
```

`hubDevices()` lists configured devices in file order and includes offline
entries with `listed: false`. `setHubDevice(id)` adds a device, keeping any
existing description. Pass `{description: ''}` to clear it.
`removeHubDevice(id, {endLeases: true})` ends that device's lease and
reservations before removal. Over a TCP connection these methods reject
with a `MaestroError` whose code is `FORBIDDEN` and HTTP status is 403.

On a hub (`info.hub`, from `maestro-d-hub start`) devices are
[leased](rest.md#leases-hubs), and leasing is always explicit: the library
never takes a lease on its own. `md.lease({device | select, ttlSec, description, attach})`
normalizes the id through `GET /v1/devices/{id}` (keeping an unlisted one,
which the hub then refuses with `DEVICE_NOT_FOUND`), takes the lease and
returns the device's handle, attached unless `attach: false`; the handle
sends `X-Maestro-Lease` on every request. The
lease is a fixed window (`ttlSec`, default 8 hours or the device's
`maxSec` when that is shorter; over the maximum rejects with `USAGE`):
nothing renews it on its own, and `device.renew({ttlSec})` restarts it
explicitly. `md.lease()` records the lease in `<home>/leases.json`, the
file `maestro-d lease` writes. `md.attach(…)` and `MaestroD.attach(…)` on a
hub use a lease the client holds — one taken in this process, or one in
`leases.json` for that hub and device — and never take one: with none,
they reject with `LEASE_REQUIRED`, whose message names `md.lease()` and
`maestro-d lease --device <id>`. So `e2e/iosLandscape.e2e.ts` runs against a
hub unchanged once `maestro-d lease --device <id>` has run in the same home.
`device.release()` ends a lease, and `md.close()` ends the leases this
process took (a shell's lease from `leases.json` stays); against a daemon
that is not a hub, `release()` does nothing.

`md.shutdown()` asks the hub to stop and keeps this client’s lease records.
Use its local Unix socket for shutdown. A TCP refusal leaves the connection
and its leased devices usable. After restart, a leased device can re-attach
with its original options and retry a command once. Keep any local app
file available at the path used for attach. Emulator handles resolve their
grant by ID before addressing the device, including after a serial change.

**Reservations.** `md.reserve({device | select, startAt, ttlSec, holder, description})`
reserves a device for a window that starts later
([`POST /v1/reservations`](rest.md#post-v1reservations)) and resolves with
the lease, in `state: 'scheduled'`; `startAt` is a `Date` or an RFC 3339
string, and this machine's clock goes with it as `clientTime` (the hub
refuses one more than 60 s off its own). Nothing is attached and nothing
waits for the start: keep `lease.id`, and from the start take the device
with `md.claim(lease.id, {attach})`, in this process or any other, on any
machine. `claim()` returns the device's handle, attached unless
`attach: false`, recorded in `leases.json` and released by `release()` or
`close()` as a `lease()` handle is; before the start it rejects
`LEASE_NOT_STARTED` with `details.startsAt` and holds nothing. A request
under a reservation before its start rejects `LEASE_NOT_STARTED` too, and
the lease stays on file. A hub that predates reservations rejects `USAGE`
"does not take reservations" (`details.reason: 'reservations-unsupported'`).
`leases.json` may hold several leases for one device (a lease now and
reservations later): `attach()` sends the one that started last, else the
reservation that starts first. Every `Lease` carries `startsAt` and
`state`, each `DeviceInfo` on a hub its `reservations`, and `hubStatus()`
the hub's `reservationLeadSec`.

The library uses the local `maestro-d` binary to update `leases.json`.
It holds the same `leases.json.lock` as the CLI from the read through the
atomic rename, so concurrent scripts and CLI commands retain each other's
lease records.

On a hub each `DeviceInfo` also carries `maxSec`, and `machine` and
`addresses`: the owning machine's host name, and its hub's advertised
address plus its non-loopback IPs, so a script can find the devices
attached to its own machine.

Hubs form a mesh ([hub.md](hub.md)): connect to any hub and every hub's
devices are there, with qualified ids (`id` is `<localId>@<hub>`, and
`DeviceInfo` carries `hub` and `localId`; lease ids are `<uuid>@<hub>`).
`md.lease({device: '<udid>@hub-a'})` through hub-b leases hub-a's device and
every call on the handle runs there; an unqualified id resolves to the one
hub that has it. `md.hubStatus()` returns `GET /v1/hub`: the hub's name and
version, and each peer's address, link state, RTT and device count.

Device management maps one to one onto the REST routes: `catalog()` is
`GET /v1/catalog`, `createDevice()` is `POST /v1/devices` with `create`,
`startDevice()` (whose `platform` may be left out for `avd:<name>` and for
a listed name or UDID) is `POST /v1/devices`, and `deleteDevice()` is
`DELETE /v1/devices/{id}?delete=true`. `e2e/deviceManagement.e2e.ts` runs
the whole lifecycle on a real simulator and emulator.

`md.devices(query)` filters the listing as `GET /v1/devices` does and every
entry carries `location` (`local`, `mesh` or `cloud`) relative to this
computer: `md.devices({location: ['local']})`, `md.devices({platform:
'ios', osVersionMin: '17', available: true})`, `md.devices({location:
'cloud', provider: 'devicefarm'})`. `md.catalog({location: 'cloud',
platform: 'android', osVersionMin: '13', available: true})` lists the
models a cloud provider can launch in `cloud`, and `hub` names the hub
that answers. Every request sends `X-Maestro-Client-Machine` with
`machineName()` (the host name). See [Cloud devices](cloud.md).

`md.createDevice({cloud: {provider: 'devicefarm', platform: 'android', name:
'Google Pixel 8', app: '/path/app.apk', appId: 'org.example', idleReleaseSec:
1800}})` launches a cloud device on this computer's own daemon (direct mode)
and resolves with it; `md.attach({device: dev.id})` drives it like any other
device. `md.stopDevice(id)` resolves with `{deviceMinutes}` for a cloud
device, and `md.deleteDevice(id)`'s device carries `deviceMinutes`.
Through a hub, `md.lease({select: {location: 'cloud', platform: 'ios', name:
'Apple iPhone 15'}, ttlSec: 1800, cloud: {idleReleaseSec: 900}, attach: {}})`
launches a cloud device on the hub that holds the credentials and resolves
with its leased handle; `dev.release()` resolves with `{deviceMinutes}`.
`dev.installApp(file)` on a cloud device installs into the running session,
which keeps its id; `dev.uninstallApp()` rejects with `USAGE` there.

`device.installApp({url, headers, appId})` installs a build the device's
own daemon downloads (a hub routes it to the hub the device is plugged
into, which fetches it), with `headers` such as a bearer key sent to `url`
only, never to a redirect's target; it resolves `{appId, cached, etag}`,
`cached` when the server's `ETag` matches the build installed last
([`POST …/apps`](rest.md#post-v1devicesidapps)). The device's daemon keeps
that build, and `clearState` on a physical iPhone or iPad reinstalls it. `md.attach({…, ai:
{jevKey, anthropicKey}})` (and `lease({attach: {ai}})`) gives the
session the keys its AI checks spend when the daemon has no `ai` block of
its own ([config.md](config.md#ai)); the keys are sent, never kept on the
handle or its `info`. Device records carry `capabilities`, the JSON their
host's config.json gives them, untouched. `createDevice({platform:
'android', name, hub})` makes an AVD on another hub's machine, as it does a
simulator.

The commands that answer data resolve a `StepResult` whose `data` is
typed by the exported result types: `keyboardVisible()` gives
`KeyboardVisibleResult` (`{visible}`), `startSampler(kind)` gives
`SamplerStarted` and `stopSampler(kind)` `SamplerResult` (`{kind, unit,
appId, intervalMs, durationMs, count, min, max, mean, samples}`), and
`assertWithAI(assertion)` and `assertNoDefectsWithAI()` give
`AICheckResult` (`{verdict, confidence, model, escalations, observed,
costUsd}`): every verdict resolves, so read `data.verdict` rather than
catching. `setStatusBar({mode: 'demo'})` rejects with `UNSUPPORTED` on a
physical iPhone or iPad; `device.supportsStatusBar()` (or the exported
`supportsStatusBar(info)`) tells ahead, so a rig can mask the status bar
there instead.

`md.devicefarm` reaches Device Farm without a device: `account()` (trial
minutes and unmetered slots), `apps()` (the project's uploads),
`sessions()` (its remote access sessions, each named with its device id)
and `upload(file)` (an `.apk` or `.ipa` from the daemon's machine, reused
when an identical one went up in the last 30 days; through a hub it rejects
with `FORBIDDEN`). Each takes `{hub}` to name the hub that answers.
`e2e/devicefarm.e2e.ts` runs the whole lifecycle on live Device Farm, in
direct mode and through a hub, including an install into the running
session.

## How the package is built

`npm/maestro-d/` holds the library (TypeScript, no runtime
dependencies, ESM + CommonJS). The command methods come from
`src/generated/commands.ts`, which `make gen` writes from the flow parser's
step structs together with the Go table (`pkg/daemon/commands_gen.go`) and
[commands.md](commands.md) — so a new upstream command shows up in all three
after regenerating, and `make gen-check` (run by CI) fails when they drift.

The binary ships as an optional dependency per platform
(`maestro-d-darwin-arm64`, `-darwin-x64`, `-linux-arm64`, `-linux-x64`),
each package being a maestro-runner-style home: `bin/maestro-d`,
`drivers/`, `LICENSE`. `npm/build-maestro-d-npm.sh` cross-compiles and packs
them all:

```sh
VERSION=0.1.0 ./npm/build-maestro-d-npm.sh                       # every platform
VERSION=0.1.0 ./npm/build-maestro-d-npm.sh --targets darwin/arm64  # just this one
```

Output is `npm/dist-maestro-d/<VERSION>/*.tgz`; the script prints the
`npm install` line to try them locally and the `npm publish` order
(platform packages first). Pushing a `maestro-d-v<VERSION>` tag makes
`.github/workflows/maestro-d.yml` do the same and attach the tarballs to a
GitHub release, publishing to npm when the `NPM_TOKEN` secret is set.

## Using a locally built binary

The library spawns the daemon from, in order, `$MAESTRO_D_BIN`, the
platform package's `bin/maestro-d`, then `maestro-d` on `PATH`.
After `make build` in a checkout:

```sh
export MAESTRO_D_BIN=$HOME/.maestro-d/bin/maestro-d
```

or, to keep the npm binary but reuse the drivers and caches of a `make
build` install, `export MAESTRO_RUNNER_HOME=$HOME/.maestro-d`.

## Development

```sh
cd npm/maestro-d
npm install
npm test            # tsc build, then test/e2e.test.mjs against the mock driver
```

The tests build the Go binary from the repository root into a temp
directory (`MAESTRO_D_TEST_BIN` to reuse one), run their own daemon
under a private `MAESTRO_D_HOME`, and cover every exported method —
including error codes, `AbortSignal`, events and shutdown — without a
device. Run them alongside `go test ./pkg/daemon/... ./pkg/cli/...` after
touching the protocol.
