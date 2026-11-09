# JavaScript library

The `maestro-d` npm package is a thin client for the [REST API](rest.md):
one method per YAML command, `MaestroError` for every failure, and the same
daemon the CLI uses, so a script and a shell can share a session.

```js
import { MaestroD } from 'maestro-d'

const m = await MaestroD.attach({ device: '29271FDH200ABP', appId: 'co.edgesecure.app' })
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

On a hub (`info.hub`, from `maestro-d hub start`) devices are
[leased](rest.md#leases-hubs), and leasing is always explicit: the library
never takes a lease on its own. `md.lease({device | select, ttlSec, attach})`
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

`leases.json` locking: the CLI holds an advisory `flock` on
`leases.json.lock` while it updates the file; Node cannot take one, so the
library writes the whole file to a temporary file and renames it into
place. A reader never sees a torn file, but a script and a CLI updating it
at the same instant can lose one update. Cross-process safety is the
CLI's: when several processes share a home and lease at once, lease from
the shell.

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
