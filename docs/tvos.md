# Explicit tvOS simulator sessions

The Apple WDA driver supports explicitly selected tvOS simulators. Automatic device discovery retains its existing iOS behavior.

```sh
maestro-d attach --daemon tv-work --platform ios --device "$TV_SIMULATOR_UDID" --app-file "$TV_APP_FILE"
```

`--platform ios` selects the Apple driver. The selected simulator runtime determines the `WebDriverAgentRunner_tvOS` scheme, tvOS destination, and separate tvOS build cache. Install Xcode and the tvOS runtime before attaching. Unsupported simulator platforms fail before building; physical iOS devices retain the existing path.

TV remote keys accepted by `pressKey` are `up`, `down`, `left`, `right` (also `DPAD_*`), `select`/`center`/`DPAD_CENTER`/`enter`, `back`/`menu`, and `playPause`/`play_pause`. Element selection uses WDA's native tvOS support. Flutter's iPhone fallback is not enabled for TV sessions.

For measured changed-line coverage, set `TV_COVERAGE_BASE` to the integration base and run `bash scripts/tvos-coverage.sh unit`, then `build`. Use `coverage/tvos/bin/maestro-d` with `GOCOVERDIR="$PWD/coverage/tvos/e2e"` to start a separate daemon, execute real TV journeys, and stop the daemon normally. Run `bash scripts/tvos-coverage.sh combined`. The main package is instrumented in the executable so Go writes counters at exit; the coverage gate measures the changed driver, CLI, and simulator code. The script rejects absent counters and changed source revisions.
