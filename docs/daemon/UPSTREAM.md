# Tracking upstream maestro-runner

maestro-d is a fork of
[devicelab-dev/maestro-runner](https://github.com/devicelab-dev/maestro-runner).
The Go module path (`github.com/devicelab-dev/maestro-runner`) is deliberately
unchanged so that rebasing is a plain `git rebase` with no import rewrites.
The upstream release this tree is based on is recorded in
`pkg/cli/daemon_wiring.go` as `UpstreamVersion` and printed by
`maestro-d --version`.

## Branches

| Branch | Holds |
| --- | --- |
| `master` | The fork. This is the default branch: build from it, branch from it, release from it. |
| `main` | Upstream maestro-runner, unmodified. It is only ever moved to a newer upstream release, never committed to. |

`master` is `main` plus the fork's commits, so `git log main..master` is
exactly what the fork adds.

## Design rule

Everything the daemon adds lives in **new files**. Upstream files are touched
only where there is no other way to register the commands, and each touch is
a few lines marked with a `// maestro-d` comment or a self-contained
block. The daemon consumes upstream through a handful of seams — the step
parser, the flow runner, `CreateDriver`/`RunConfig`, device discovery — and
never copies their code.

## Upstream files modified

| File | Change | Lines |
| --- | --- | --- |
| `pkg/cli/cli.go` | `app.Commands = append(app.Commands, daemonCommands()...)`; app name, usage text and version printer say `maestro-d` | ~15 |
| `pkg/driver/mock/mock.go` | `takeScreenshot` returns the PNG bytes in `result.Data`, as real drivers do (lets the mock e2e test save a screenshot) | 6 |
| `Makefile` | `BINARY_NAME=maestro-d`; `INSTALL_DIR=$(HOME)/.maestro-d` (its own home, so installing cannot disturb a maestro-runner install); `gen` / `gen-check` targets; `build` replaces the installed binary's inode (a running daemon plus an in-place `cp` gets the next exec SIGKILLed on macOS) | ~16 |
| `pkg/driver/wda/client.go` | `CreateSession` sends `bundleId` only when it has one, so the daemon's attach can open a session without launching an app | 4 |
| `pkg/driver/browser/cdp/driver.go` | Downloaded browsers cache in `~/.maestro-d/browsers` rather than `~/.maestro-runner/browsers` (two hard-coded paths; upstream does not route this one through `config.GetHome()`, and it must not — under `go test` that resolves to the package directory) | 3 |
| `README.md` | Replaced wholesale with the fork's own README; upstream's is kept at `docs/maestro-runner.md`. On a rebase, keep ours and re-read theirs for anything new worth carrying over | whole file |
| `.gitignore` | Ignore the `maestro-d` binary and `npm/dist-maestro-d/` | 6 |

Everything else is new:

| Path | Purpose |
| --- | --- |
| `pkg/flow/steptype_export.go` | Exported wrappers around the unexported step parser: `StepTypes`, `ParseStep`, `BuildStep` |
| `pkg/cli/hierarchy_export.go` | `NormalizeHierarchy`, the `hierarchy` command's cross-driver tree, for `GET …/hierarchy` |
| `pkg/executor/session.go` | `executor.Session` — a long-lived `FlowRunner` that executes one step at a time and keeps variables, report and artifacts between calls |
| `pkg/daemon/` | Daemon server (REST over a unix socket / optional TCP), protocol types, client, spawn logic, device registry, generated `commands_gen.go` |
| `pkg/daemon/gen/` | Generator: reflects over the step structs → `commands_gen.go`, `npm/maestro-d/src/generated/commands.ts`, `docs/daemon/commands.md` |
| `pkg/cli/daemon.go`, `daemon_flags.go`, `daemon_wiring.go`, `oneshot.go` | CLI: `start`/`serve`/`stop`/`status`/`ps`/`device`/`attach`/`detach`/`run`/`get`/`set`/`eval`/`commands` and one command per YAML step type |
| `pkg/cli/daemon_e2e_test.go` | Binary-level test against the mock driver |
| `npm/maestro-d/` | The JavaScript library |
| `npm/build-maestro-d-npm.sh` | Cross-compiles the binary into per-platform npm packages and packs the library (`npm/build-npm.sh` is upstream's, for maestro-runner's own package) |
| `.github/workflows/maestro-d.yml` | The fork's checks and `maestro-d-v*` release workflow (`ci.yml` is upstream's) |
| `docs/daemon/` | These docs |

## Upstream seams the daemon depends on

If an upstream change breaks the build, it is almost certainly one of these.

| Seam | Used by | What we rely on |
| --- | --- | --- |
| `flow.parseStep`, `flow.parseSteps`, `flow.isStepType` (unexported) | `pkg/flow/steptype_export.go` | One YAML node → `flow.Step`; the registry of step type names (the export file keeps its own ordered list, `stepTypeOrder`, filtered through `isStepType`) |
| `executor.FlowRunner` internals: `executeNestedStep`, `executeRepeat`/`executeRetry`/`executeRunFlow`/`executeSubFlow`, `executeTakeScreenshot`, `captureArtifacts`; `ScriptEngine` | `pkg/executor/session.go` | Executing a single parsed step with the runner's normal retry/optional/gate handling; variable store; `ImportSystemEnv` |
| `cli.RunConfig`, `cli.CreateDriver` | `pkg/cli/daemon_wiring.go` (`attachToRunConfig`) | Opening a driver exactly as `maestro-runner test` does |
| `cli.parseHierarchy`, `cli.formatHierarchy` (unexported) | `pkg/cli/hierarchy_export.go`, `get hierarchy` | The normalized tree and the `--compact`/`--find` renderers of the `hierarchy` command |
| `cli.collectDevices`, `cli.buildDeviceReport`, `cli.buildAppReport`, `cli.resolveDriverName`, `cli.parseArtifactMode`, `cli.loadCapabilities` | `pkg/cli/daemon_wiring.go` | Device listing and report metadata |
| `cli.GlobalFlags` | `pkg/cli/daemon.go` (`inheritedArgs`) | Honouring `--device`/`--platform` given before the command name |
| `logger.Init/Close` | `pkg/cli/daemon.go` (`serve`) | The daemon's log file |
| `report.Command` | `pkg/daemon/protocol.go` | Sub-step results for `runFlow`/`repeat`/`retry` |
| `mock.Driver` | tests | The mock driver must keep accepting `Platform: "mock"` |

## Rebasing on a new upstream release

```sh
git fetch devicelab                     # git@github.com:devicelab-dev/maestro-runner.git
git branch -f main v1.1.27              # main is upstream, unmodified
git rebase main master                  # or cherry-pick the fork commits onto the tag
make gen                                # regenerate command tables if step structs changed
go test ./...                           # includes the e2e test in pkg/cli
```

Then update `UpstreamVersion` in `pkg/cli/daemon_wiring.go` and the
`maestro-runner` version in `npm/maestro-d/package.json`'s description.
New step types show up in the generated files automatically; a new field on
`RunConfig` that should be settable at attach time needs a line in
`daemon.AttachConfig` and `attachToRunConfig`.
