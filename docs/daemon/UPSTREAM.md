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
| `pkg/driver/mock/mock.go` | `takeScreenshot` and `assertScreenshot` return the PNG bytes in `result.Data`, as real drivers do (lets the mock e2e tests save and compare a screenshot); a `// maestro-d` block at the end implements `core.Capturer` (an on-demand `capture.Session` over the fake source, `pkg/capture/fake`), `core.ScreenRecorder` through a `capture.Recorder`, and `Close`, so the stream, viewer and recording paths run on mock devices | ~60 |
| `pkg/core/driver.go` | The `core.Capturer` interface (`Capture() (*capture.Session, error)`), marked `// maestro-d`, reached through `core.Unwrap` as `ScreenRecorder` is | 10 |
| `pkg/cli/test.go` | `CreateDriver`'s mock branch returns the driver's `Close` as its cleanup (`// maestro-d`), ending the mock's capture session; a `ScreenCapture` field on `RunConfig`, and one line in `executeFlowsWithMode` setting it when the run records (`--record`/`--video` or a `startRecording` anywhere in the flows, `flowsRecord` in `ios_capture.go`), which covers both the single-device `CreateDriver` call and the parallel `CreateIOSDriver` one, all marked `// maestro-d` | 7 |
| `pkg/cli/ios.go` | Three `// maestro-d` hooks in `CreateIOSDriver`: step 0, `startIOSCapture` (a physical iPhone's screen capture, `ios_capture.go`), right after the simulator check and before app install and WebDriverAgent, because starting capture re-enumerates the device on USB; a deferred `closeUnlessKept` so every early return ends that capture session; and `capt.bind` on the returned cleanup, which hands the capture to the WDA driver and ends WebDriverAgent before the capture. Also `iosDeviceInfo` gains `Model`; `getPhysicalDeviceInfo` fills it from lockdown's `ProductType` (or devicectl's `productType` on its fallback path) and reaches go-ios through `goiosGetDevice` and `goiosGetValues`, and `listPhysicalIOSUDIDs` through `goiosListDevices` (`ios_seams.go`), so tests can answer for usbmuxd and lockdown. All `// maestro-d` | 11 |
| `pkg/driver/wda/driver.go` | The capture fields on `Driver` (session, its error, the `--record` recorder, `captureLost`) and one `d.addCaptureLostHint(result)` call in `Execute`, all marked `// maestro-d`; the methods are in `capture.go` | 13 |
| `pkg/driver/wda/recording.go` | A physical device's `StartScreenRecording`/`StopScreenRecording` record its capture session (`capture.go`) instead of refusing; simulators still use `simctl io recordVideo` | 6 |
| `Makefile` | `BINARY_NAME=maestro-d`; `INSTALL_DIR=$(HOME)/.maestro-d` (its own home, so installing cannot disturb a maestro-runner install); `gen` / `gen-check` targets; `build` replaces the installed binary's inode (a running daemon plus an in-place `cp` gets the next exec SIGKILLed on macOS); `cover-changed` (unit plus e2e-subprocess coverage of the branch's changed lines, through `tools/covchanged`) | ~25 |
| `pkg/driver/wda/client.go` | `CreateSession` sends `bundleId` only when it has one, so the daemon's attach can open a session without launching an app | 4 |
| `pkg/driver/browser/cdp/driver.go` | Downloaded browsers cache in `~/.maestro-d/browsers` rather than `~/.maestro-runner/browsers` (two hard-coded paths; upstream does not route this one through `config.GetHome()`, and it must not — under `go test` that resolves to the package directory) | 3 |
| `README.md` | Replaced wholesale with the fork's own README; upstream's is kept at `docs/maestro-runner.md`. On a rebase, keep ours and re-read theirs for anything new worth carrying over | whole file |
| `.gitignore` | Ignore the `maestro-d` binary, `npm/dist-maestro-d/` and `cover-changed`'s `.cover/`; track `e2e/testdata/` (upstream ignores every `testdata/`) | 9 |
| `pkg/cli/devices.go` | `// maestro-d` lines only. `collectDevices` starts the Android half with `startAndroidEntries(platform, includeShutdown)` (in `devices_props.go`: `androidEntries` then `avdEntries`, so running emulators carry their AVD names and shut-down AVDs list as `avd:<name>`) beside the iOS listing, and returns `append(android(), entries...)`; the upstream `if platform == "" \|\| platform == "android"` block goes. `DeviceEntry` gains `model`. `androidEntries` lists through `listAndroidDevices()` and calls `applyAndroidProps(&entries[len(entries)-1], d)` after each append (the `pkg/device` import goes). `physicalIOSEntries` copies `info.Model`; `simulatorEntries` starts `go simulator.WarmModels()` and sets `Model: simulator.ModelIdentifier(s.DeviceType)` | ~10 |
| `pkg/executor/scripting.go` | The caller policy: a `policy *CallerPolicy` field on `ScriptEngine`, one `// maestro-d` hook call at the end of `ExpandStep` (`se.policyStep(step)`) and one at the start of `ResolvePath` (`se.policyResolve(path)`). Both are no-ops while no daemon call has installed a policy, so `maestro-runner test` is unchanged | 6 |
| `pkg/flow/step.go` | `StepSetTrafficShape` and `StepResetTrafficShape` in the `StepType` constants, in a block of their own, marked `// maestro-d` (the structs are in `trafficshape.go`) | 4 |
| `pkg/flow/parser.go` | The two traffic-shaping step types in `isStepType`, and their two `decodeStep` cases, which hand off to `decodeSetTrafficShape` and `decodeResetTrafficShape` (`trafficshape.go`), marked `// maestro-d` | 8 |
| `pkg/executor/runshell.go` | One `// maestro-d` check at the start of `executeRunShell` (`fr.policyDeniesShell(step)`): a remote caller without `--allow-remote-shell` gets a failed result instead of a shell | 3 |
| `pkg/executor/flow_runner.go` | Two `// maestro-d` touches in `executeAssertScreenshot`: the `validateArtifactPath` check also accepts a path the active caller policy allows (`fr.script.PolicyAllows`: an uploaded blob copy or the session's `outputs/`), and both `core.DiffScreenshotPath(referencePath)` calls (the diff write and the stale-diff removal) go through `fr.script.DiffPath`, which for a remote or client-named call puts the diff under `outputs/`. With no policy both return upstream's answer. For screen capture (`recording.go`): a `rec stepRecording` field on `FlowRunner`, one `case *flow.StartRecordingStep, *flow.StopRecordingStep` in each of `executeStep` and `executeNestedStep` (calling `fr.executeRecordingStep`, which falls back to `fr.driver.Execute` for a driver without capture), a deferred `fr.finishRecording()` after `flowWriter.Start()` in `Run`, and the `--record` defer's check of `StopScreenRecording` going through `recordingSaved` (a recording that ended early is saved, so it is still linked and still follows `--video on-failure`), all marked `// maestro-d`. One `case *flow.SetAirplaneModeStep, *flow.ToggleAirplaneModeStep` in each of `executeStep` and `executeNestedStep` calls `fr.executeAirplaneStep` (`unsupported.go`), which answers UNSUPPORTED on iOS and tvOS and otherwise falls back to `fr.driver.Execute`. For traffic shaping: a `shaper TrafficShaper` field on `FlowRunner` and one `case *flow.SetTrafficShapeStep, *flow.ResetTrafficShapeStep` in each switch calling `fr.executeTrafficShapeStep` (`trafficshape.go`) | 25 |
| `pkg/cli/android.go` | One `// maestro-d` call in `CreateAndroidDriver` after the driver is created, `attachAndroidCapture` (a `// maestro-d` function at the end of the file): builds the device's on-demand `capture.Session` over `pkg/capture/scrcpy` and hands it to the uiautomator2 or devicelab driver through `SetCapture`; the returned cleanup closes the session after the driver's own | 25 |
| `pkg/device/android.go` | `ADBPath()`, marked `// maestro-d`: the adb binary the device runs, for the capture's long-running `adb shell` | 5 |
| `pkg/driver/uiautomator2/recording.go`, `pkg/driver/devicelab/recording.go` | Rewritten (screenrecord, with its three-minute cap and on-device file, is the defect being replaced): `SetCapture`, `Capture` (`core.Capturer`), `StartScreenRecording`/`StopScreenRecording` (`core.ScreenRecorder`) through a `capture.Recorder` on the session, and `recordingStep`, the answer when a recording step reaches the driver without capture | whole file |
| `pkg/driver/uiautomator2/driver.go`, `pkg/driver/devicelab/driver.go` | A `screen capture.DriverCapture` field and its import, and the two `startRecording`/`stopRecording` cases in `Execute` merged into one calling `recordingStep`, all marked `// maestro-d` | 6 |
| `pkg/driver/uiautomator2/commands.go`, `pkg/driver/devicelab/commands.go` | The `startRecording`/`stopRecording` step functions (`screenrecord` on the device) removed, with a `// maestro-d` note: the executor runs those steps through the driver's capture | −40 |
| `pkg/simulator/ios.go`, `types.go` | `simctlDevice` decodes `deviceTypeIdentifier` and `SimulatorDevice` carries it as `DeviceType` (three `// maestro-d` lines), for `simulator.ModelIdentifier` | 3 |
| `pkg/device/android_test.go` | One `// maestro-d` check at the start of `skipIfNoDevice`: the real-device tests (UIAutomator2 start and stop, forwards, install and uninstall on the first adb device) skip unless `MAESTRO_D_ANDROID_DEVICE_TESTS=1`, so `go test` never drives a phone other sessions hold | 7 |

Everything else is new:

| Path | Purpose |
| --- | --- |
| `pkg/flow/steptype_export.go` | Exported wrappers around the unexported step parser: `StepTypes`, `ParseStep`, `BuildStep` |
| `pkg/flow/trafficshape.go` | The `setTrafficShape` and `resetTrafficShape` steps: their structs, value checks and decoder |
| `pkg/netshape/` | Traffic shaping through a host's configured calls: `network.json`, placeholder filling, the HTTP call ([network.md](network.md)) |
| `pkg/executor/trafficshape.go`, `pkg/executor/unsupported.go` | The executor's own steps: the shaping calls through the session's `TrafficShaper`, iOS airplane mode refused, and the UNSUPPORTED/USAGE codes a step's failure carries |
| `pkg/daemon/trafficshape.go` | Each session's shaper, `--network-config`, and `trafficShaping` in device listings |
| `pkg/cli/hierarchy_export.go` | `NormalizeHierarchy`, the `hierarchy` command's cross-driver tree, for `GET …/hierarchy` |
| `pkg/executor/session.go` | `executor.Session` — a long-lived `FlowRunner` that executes one step at a time and keeps variables, report and artifacts between calls; installs each call's caller policy (and its per-call flow directory, for a bundle, restoring the session's afterwards), reports a denial as `FORBIDDEN`, and imports the daemon's environment only when asked (`ImportEnv`, `PassEnv`) |
| `pkg/executor/recording.go` | The `startRecording` / `stopRecording` steps on a driver with screen capture (`core.Capturer`, through `core.Unwrap`): a `capture.Recorder` into `<report>/recordings/<name>.mp4` (a session) or `<flow assets>/recordings/<name>.mp4` (`maestro-d test`), name rules, one recording at a time, finalized at the end of the flow or session |
| `pkg/flutter/capture_unwrap_test.go` | Test: `core.Unwrap` reaches a capturing driver through `flutter.Wrap` |
| `pkg/cli/ios_capture.go` | Step 0 of `CreateIOSDriver`: the conditions for capturing (`ScreenCapture`, not a simulator, macOS, listed by usbmuxd over USB, an iPhone or iPad), opening the device on the process's capture helper, the wait for usbmuxd to list the re-enumerated device, the WebDriverAgent-then-capture cleanup, and `flowsRecord`, the step-tree scan `maestro-d test` uses |
| `pkg/driver/wda/capture.go` | The WDA driver's `core.Capturer` (`SetCapture`, `Capture`), the physical `--record` recorder, and the re-attach hint for network failures after a lost capture |
| `drivers/ios/MaestroDCapture/` | The capture helper app's Swift source and `Info.plist`, built and signed on first use by `pkg/capture/quicktime` |
| `pkg/executor/caller_policy.go` | `CallerPolicy`: the per-request confinement of a remote caller — the typed switch over the path-bearing step fields (`PolicyFields()`), `blob:` and `bundle:` ref parsing and mapping (a materialized bundle becomes one of the call's roots), confinement to the call's roots, bundle-relative output names, the output rewrite under `outputs/` and the `outputs` record, the remote `assertScreenshot` input/baseline rule and its diff path (`DiffPath`, `PolicyAllows`), the `runShell` check and the denial record. Reached only through the hooks in `scripting.go`, `runshell.go` and `flow_runner.go` |
| `pkg/hub/` | The hub mesh: peer links (`link.go`: framed HTTP exchanges over `coder/websocket`, ping and liveness; `flow.go`: per-exchange flow control of streamed bodies), membership (`mesh.go`: dialing, accepting, refusals, join propagation, dedupe, reconnect, `GET /v1/hub`), mDNS once at launch (`discover.go`, `grandcat/zeroconf`; on macOS the browse runs the system's `dns-sd`, `discover_dnssd.go`), the router in front of a hub's handlers (`router.go`: qualified ids, forwarding, fan-out lists, selector leasing, events), id qualification (`rewrite.go`) and bulk links (`bulk.go`). It imports `pkg/daemon`, never the other way round: the daemon sees it only as `daemon.Mesh` |
| `pkg/hub/hubtest/` | Test harnesses for hubs; `cmd/wanproxy` is the TCP proxy that adds latency, bandwidth limits and dropped connections for the WAN end-to-end tests |
| `pkg/capture/` | Screen capture: the per-device `Session` (fan-out, GOP cache, lag handling, on-demand linger), the file `Recorder`, `DriverCapture` (a driver's session and `--record` recording), the fragmented-MP4 muxer (`fmp4/`), H.264 bitstream helpers (`h264/`), the fake source looping embedded fixtures (`fake/`, regenerated by `fake/fixtures/make.sh`), the iOS source (`quicktime/`: the process's capture helper, its build, signing and launch, and the helper socket protocol; a stub answering `not-macos` elsewhere) and the Android source (`scrcpy/`: pushes and verifies the pinned scrcpy-server, forwards its socket, starts it over `adb shell`, parses its video and audio sockets; `scrcpy/fixtures/` are two recorded Pixel 8 socket dumps, re-recorded by `TestRecordFixture`). It never imports `pkg/daemon` and builds on every GOOS |
| `drivers/android/scrcpy-server-v5.0.1`, `drivers/android/LICENSE-scrcpy` | The pinned scrcpy-server jar (the v5.0.1 release asset, sha256 checked before every push) and its Apache-2.0 licence |
| `pkg/daemon/` | Daemon server (REST over a unix socket / optional TCP), protocol types, client, spawn logic, device registry (with `created.json`), device catalog/create/delete (`catalog.go`), the blob store (`blobs.go`), flow bundles (`bundle.go`), app install (`apps.go`), artifacts (`artifacts.go`), screen streams, `device.capture` events and the viewer page (`stream.go`, `viewer.html`), a hub's device leases and `--share-device` pool (`leases.go`), the `Mesh` seam and hub status types (`mesh.go`), injected `Deps` (`deps.go`), generated `commands_gen.go` |
| `pkg/daemon/gen/` | Generator: reflects over the step structs → `commands_gen.go`, `npm/maestro-d/src/generated/commands.ts`, `docs/daemon/commands.md` |
| `pkg/cli/daemon.go`, `daemon_flags.go`, `daemon_wiring.go`, `oneshot.go`, `stream.go` | CLI: `start`/`serve`/`stop`/`status`/`ps`/`device`/`attach`/`detach`/`run`/`get`/`stream`/`set`/`eval`/`commands` and one command per YAML step type |
| `pkg/cli/target.go` | `--target-hub`, `--ca`: a TCP or TLS client for a daemon or hub on another machine |
| `pkg/cli/hub.go`, `hub_service.go`, `lease.go`, `lease_lock_*.go` | `hub start`/`stop`/`status` and the mesh flags; `hub service install`/`uninstall`/`status` (the macOS LaunchAgent); `lease`, `renew` and `release`, and `<home>/leases.json` (under an advisory `flock`; none on Windows) |
| `pkg/cli/pack.go`, `bundle.go`, `remote_files.go`, `install.go` | `pack` and `pack --flow` (a flow's dependency closure as a bundle), the upload of a targeted command's input files, flow files and whole `run` bundles and the fetch of their outputs, `install`/`uninstall` and the daemon's install, uninstall and app-id hooks |
| `pkg/cli/device_manage.go`, `devices_avd.go` | `device catalog`/`create`/`delete`; AVD names and `avd:<name>` entries in device listings |
| `pkg/cli/devices_props.go` | `listAndroidDevices` (`device.ListDevicesWithProps`, swappable in tests), `applyAndroidProps` (a phone's name, model and Android version, an emulator's model and version beside its AVD name) and `startAndroidEntries` (the Android half of `collectDevices`, run beside iOS) |
| `pkg/cli/ios_seams.go` | `goiosListDevices`, `goiosGetDevice`, `goiosGetValues`: the go-ios calls of the physical iOS listing and `getPhysicalDeviceInfo`, swappable in tests |
| `pkg/simulator/models.go` | `ModelIdentifier` and `WarmModels`: a simulator device type's model identifier, from `simctl list devicetypes -j` under a 1 s bound, read again for an unknown type at most every 30 s, a failure retried after 30 s |
| `pkg/execbound/` | `Run`: a command under a time bound, killed at the bound, with a short `WaitDelay` so a killed wrapper's child cannot hold the wait; the adb props read and the simctl device-types read use it |
| `pkg/simulator/catalog.go` | The simctl runtime and device-type catalog and the default runtime and iPhone type |
| `pkg/emulator/catalog.go`, `avdmanager.go`, `avd_serials.go` | The sdkmanager/avdmanager catalog, AVD create/delete, image install, data partition size, and `RunningAVDsBySerial` |
| `pkg/device/props.go` | `ListDevicesWithProps`: `adb devices -l` with transport ids, and each ready device's name, model and Android version, read in one bounded `adb shell` call per connection and cached |
| `tools/covchanged/` | `make cover-changed`'s changed-line coverage report |
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
| `executor.FlowRunner` internals: `executeNestedStep`, `executeRepeat`/`executeRetry`/`executeRunFlow`/`executeSubFlow`, `executeTakeScreenshot`, `executeRunShell`, `captureArtifacts`; `ScriptEngine` | `pkg/executor/session.go` | Executing a single parsed step with the runner's normal retry/optional/gate handling; variable store; `ImportSystemEnv` |
| `executeAssertScreenshot` resolves its reference with `ResolvePath`, checks it with `validateArtifactPath` and names its diff with `core.DiffScreenshotPath` | `pkg/executor/caller_policy.go` (`PolicyAllows`, `DiffPath`) | The two `flow_runner.go` touches; a rewrite of `executeAssertScreenshot` needs them carried over |
| `ScriptEngine.flowDir` (`FlowDir`/`SetFlowDir`) is where relative paths resolve; upstream sets it only inside `executeSubFlow` (and restores it), and `runFlowSteps` runs the hooks outside that | `pkg/executor/session.go` (`beginPolicy`), `pkg/daemon/bundle.go` | A bundled `/flows` or `/steps` call sets the flow directory to the bundle root file's directory for the whole call and restores it, so hooks and step lists resolve inside the bundle; if upstream starts setting `flowDir` elsewhere, check that both still hold |
| `installIOSApp`, `isIOSSimulator`, `device.AndroidDevice.Install`/`Uninstall` | `pkg/cli/install.go` | The installers `attach --app-file` uses, reused by `install` and `POST …/apps` |
| `ScriptEngine.ExpandStep` runs before every step executes (top-level `executeStep`, nested `executeNestedStep`), and `ResolvePath` is where the executor turns a path into a file it reads | `pkg/executor/caller_policy.go` | The two places the caller policy checks paths. A new upstream step type with a path field needs a case in `policyStep`; a new way to run a host command needs a check like `executeRunShell`'s |
| `cli.RunConfig`, `cli.CreateDriver` | `pkg/cli/daemon_wiring.go` (`attachToRunConfig`) | Opening a driver exactly as `maestro-runner test` does |
| `cli.parseHierarchy`, `cli.formatHierarchy` (unexported) | `pkg/cli/hierarchy_export.go`, `get hierarchy` | The normalized tree and the `--compact`/`--find` renderers of the `hierarchy` command |
| `emulator.FindAVDManagerBinary`, `emulator.ListAVDs`, `emulator.IsEmulator`, `simulator.CreateSimulator`, `simulator.DeleteSimulator`, `simulator.FindSimctlBinary` | `pkg/cli/daemon_wiring.go`, `devices_avd.go`, `pkg/emulator/*` (fork files) | Tool lookup, AVD listing and simulator create/delete for `device create`/`delete` |
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
