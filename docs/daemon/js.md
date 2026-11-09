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
