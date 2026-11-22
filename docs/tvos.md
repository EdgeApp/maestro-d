# tvOS sessions

The Apple WDA driver runs tvOS simulators and physical Apple TVs.

## Physical Apple TVs

An Apple TV pairs with a Mac over the network, never over USB, so maestro-d
finds it through `xcrun devicectl list devices`. Pair it once in Xcode
(Window › Devices and Simulators) and turn on developer mode on the TV. Every
paired TV is then listed as platform `ios`, kind `tv`:

```sh
maestro-d device list
# 00000000000000000000000000000000000000a1  ios  tv  connected  Lab TV 26.3
```

A TV that is not paired, not in developer mode, or out of reach (off, or on
another network) is not ready, and is listed only with `--all` (a daemon's
listing always includes it). A listing gives devicectl 1.5 s
and reuses its answer, or its failure, for 5 s; one caller asks devicectl at
a time while the others take the last answer, which stands in for a minute
while devicectl fails. So a stuck CoreDevice costs a listing its TVs, not
the whole listing.
`attach --device <udid> --team-id <team>` reads `devicectl list devices` to
tell a TV from a phone (only for a device usbmuxd does not list; when
devicectl fails there, attach fails with its error rather than guessing),
builds WebDriverAgent's tvOS target
for that TV once (cached per tvOS version and team), installs and starts it
over CoreDevice, and reaches it at the TV's CoreDevice tunnel address.
`install` sends an `.ipa` or `.app` with `devicectl device install app`; the
package must be signed for the TV (an ad-hoc profile that lists it, or a
development profile). `get screenshot`, element queries and assertions go
through WebDriverAgent. TVs have no touch screen: move focus with
`pressKey` (`up`, `down`, `left`, `right`, `select`, `menu`, `home`,
`playPause`); WebDriverAgent sends them as Siri Remote presses. Touchpad
swipes on a physical TV need the Companion pairing below.

A TV can pair with several Macs, so a hub shares one only when `--share-tv`
names it (by id or name) or its `config.json`'s `hubDevices` does (by id);
name each TV on exactly one hub. A hub shares only the devices it names, TVs or not
(`--share-device`, `--share-tv`, `hubDevices`). An unnamed TV is private to the
machine: no remote route lists, reads, stops or leases it. A shared TV is
leased like any device. A selector picks a TV only when it asks for
`--kind tv` or names the TV, so `lease --platform ios` still means a phone or
tablet:

```sh
maestro-d hub start --share-tv "Lab TV" …
maestro-d lease --kind tv --target-hub=hub.lan
maestro-d lease --name "Lab TV" --target-hub=hub.lan
```

WebDriverAgent's first build signs with the login keychain, which a hub
started over SSH cannot use (`errSecInternalComponent`). Run that hub as the
launchd service (`hub service install`, docs/daemon/hub.md), or build once
from a GUI session.

## Simulators

```sh
maestro-d attach --daemon tv-work --platform ios --device "$TV_SIMULATOR_UDID" --app-file "$TV_APP_FILE"
```

`--platform ios` selects the Apple driver. The selected simulator runtime determines the `WebDriverAgentRunner_tvOS` scheme, tvOS destination, and separate tvOS build cache. Install Xcode and the tvOS runtime before attaching. Unsupported simulator platforms fail before building; physical iOS devices retain the existing path.

TV remote keys accepted by `pressKey` are `up`, `down`, `left`, `right` (also `DPAD_*`), `select`/`center`/`DPAD_CENTER`/`enter`, `back`/`menu`, and `playPause`/`play_pause`. Element selection uses WDA's native tvOS support. Flutter's iPhone fallback is not enabled for TV sessions.

For measured changed-line coverage, set `TV_COVERAGE_BASE` to the integration base and run `bash scripts/tvos-coverage.sh unit`, then `build`. From the repository root, use `coverage/tvos/bin/maestro-d` with both `MAESTRO_RUNNER_HOME="$PWD"` (the bundled driver assets) and `GOCOVERDIR="$PWD/coverage/tvos/e2e"` to start a separate daemon, execute real TV journeys, and stop the daemon normally. Run `bash scripts/tvos-coverage.sh combined`. The main package is instrumented in the executable so Go writes counters at exit; the coverage gate measures the changed driver, CLI, and simulator code. The script rejects absent counters and changed source revisions.

## Siri Remote touch swipes

`swipe` on a TV sends a touchpad gesture. Directions are the direction the finger
moves on the remote. Optional `start` and `end` are percentages of the **remote
trackpad**, not screen coordinates. Duration defaults to 100 ms and accepts
1–10000 ms. Slow movements can be recognized as pans instead of discrete swipes.

```ts
await tv.swipe({ direction: 'DOWN', duration: 100 })
await tv.swipe({ start: '50%,20%', end: '50%,80%', duration: 100 })
```

Simulators use a small macOS helper compiled with the selected Xcode and cached
under Maestro's cache directory. It sends began, changed and ended touch events
through SimulatorKit's dedicated Siri Remote trackpad service. This uses private
Apple simulator APIs; an incompatible Xcode fails the command. iOS touchscreen
swipes continue to use WebDriverAgent.

Physical Apple TVs use the paired Companion protocol through `pyatv`. Xcode
pairing/signing is still required for WDA launch, assertions and screenshots.
Install the optional Python dependencies into a virtual environment, then pair:

```sh
python3 -m venv "$HOME/.maestro-tv-python"
"$HOME/.maestro-tv-python/bin/pip" install -r drivers/tvos/requirements.txt
"$HOME/.maestro-tv-python/bin/python" drivers/tvos/pair.py \
  "$HOME/.maestro-tv-remotes.json" "$TV_DEVICE_UDID" "$TV_ADDRESS"
export MAESTRO_TV_REMOTE_CONFIG="$HOME/.maestro-tv-remotes.json"
export MAESTRO_TV_PYTHON="$HOME/.maestro-tv-python/bin/python"
```

Start a fresh daemon with these variables. The configuration maps each explicitly
paired device's Xcode identifier to its address, Companion identifier, port and
credentials. Matching devices select the tvOS WDA scheme and Companion remote
buttons as well as swipes. Protect the configuration like a password; never commit
or attach it to a report. Neither credentials nor pairing codes are passed in
swipe process arguments or emitted in gesture errors.

## Native gesture regression test

`tests/fixtures/tv-swipes/SwipeProbe.xcodeproj` builds a minimal tvOS app whose
label changes only when UIKit recognizes a swipe. Build it for the selected TV,
install it, then run `tests/tvos-swipes.ts` with the task's Maestro daemon and
`TV_DEVICE_UDID`. Physical builds require your signing team; pass the built app's
bundle identifier in `TV_PROBE_APP_ID` if overriding it. The test checks four
directions and a coordinate swipe through the same API on both kinds of target.
Set `TV_COLD_START=1` to restart the named test daemon and verify the first gesture
on a fresh paired connection. Use a task-owned daemon name for this option.

Physical gestures are centered on Companion's virtual pad and use half the
requested displacement to keep the complete gesture within that pad. Coordinates
are remote gestures, not screen locations. The paired connection stays open until
the device session is detached or the daemon shuts down. A failed or cancelled
command closes the connection so the next command can reconnect.

Use 100 ms for discrete swipes. Longer gestures may be recognized as pans by tvOS.
Screen anchors, absolute screen pixels, speed and distance options are rejected
for TV swipes; use a direction or percentage endpoints and a duration.
