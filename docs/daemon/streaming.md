# Screen streaming and recording

maestro-d streams an attached device's screen as fragmented MP4 while the
device stays fully controllable: steps, screenshots and flows run beside a
stream. One per-device capture session fans out to every viewer and
recorder. The host never decodes or re-encodes: the device's own H.264
samples (and its audio) are re-wrapped by a pure-Go muxer.

- [The stream API](#the-stream-api): REST, CLI, JavaScript
- [The viewer](#the-viewer): a page that plays a stream in Chrome
- [Recording](#recording): `startRecording` / `stopRecording`, `--record`
- [Errors](#errors): `CAPTURE_UNAVAILABLE` and its reasons
- [Platforms](#platforms)
- [Android](#android): scrcpy-server, audio, limits
- [iPhones and iPads](#iphones-and-ipads): the capture helper, Camera
  approval, USB re-enumeration, one capture-enabled process per Mac

## The stream API

All three surfaces sit on one route, `GET /v1/devices/{id}/stream`:

```sh
# REST: until the client leaves
curl -sN -H "Authorization: Bearer $T" -H 'X-Maestro-Api-Version: 1' \
  "http://127.0.0.1:7788/v1/devices/$ID/stream?fps=30&audio=true" -o screen.mp4

# CLI: stdout (refused when it is a terminal) or a file
maestro-d stream --device $ID --fps 30 --duration 10 -o screen.mp4
maestro-d stream --device $ID | ffplay -
```

```js
const video = await device.stream({ fps: 30, audio: true, signal })
// video.fps (number, or 'device'), video.audio, video.codecs, video.body (ReadableStream)
```

- **`fps`** is the requested frame rate, 3 to 60, default 15. Anything else
  is `USAGE`. It is a ceiling where the device honours it; where the device
  picks its own rate the stream says `X-Maestro-Fps: device`.
- **`audio=true`** asks for the device's audio. `X-Maestro-Audio` says
  whether the stream has an audio track.
- **Shared capture.** A device's capture runs once for all its viewers. The
  first viewer starts it at its `fps`; later viewers join at the running
  rate (`X-Maestro-Fps` reports it) and start where they join: the frames
  since the latest keyframe come first, a millisecond apart, so the
  picture is whole at once, and audio starts with the next live packet. An
  on-demand capture keeps running for 10 s after its last viewer leaves, so
  a quick reconnect does not restart it.
- **The body** is an init segment (`ftyp`, `moov`), then one `moof`+`mdat`
  fragment per frame and per audio packet, each flushed as it is made.
  A fragment's duration is an estimate, the gap before it (clamped to
  1 ms–1 s): the gap after it is not known yet. A Media Source Extensions
  consumer retimes it as [the viewer](#the-viewer) does.
  Video is `avc3`: parameter sets travel in-band, so a rotation changes
  the size mid-stream without a new init segment. `X-Maestro-Codecs` lists
  the tracks for `MediaSource.isTypeSupported`, e.g.
  `avc3.640028,mp4a.40.2`.
- **No step lock.** A stream never takes the device's step lock, so steps
  and screenshots run while it plays. It holds the daemon's idle timer.
- **The end.** The body ends when the client leaves, the device is
  detached, the daemon stops, a hub lease ends, the client reads too slowly
  to keep up (its queue of 512 packets fills) or stops reading (a write
  that cannot drain within 10 s ends it, "client too slow"), or the
  capture fails. The daemon log names the reason. `maestro-d stream` exits 0 on Ctrl-C,
  `--duration` or a daemon-ended stream, and its file always ends on a
  whole fragment, so it plays. The Go client (`Client.Stream`) does the
  same. After a hub restart, the CLI can reattach the held lease with its saved
  settings and retry the initial handshake once. A running stream ends on
  disconnect.
- **Hubs.** A stream passes through the hub mesh like any device route,
  with the device's lease, on a bulk link of its own that does not count
  against the four transfers per peer ([hub.md](hub.md#routing)). It
  never runs on a peer's control link: a stream to a peer the hub cannot
  dial (behind NAT, or its bulk dial failed) fails `PEER_UNAVAILABLE`
  with `details.reason: "peer-not-dialable"`; stream from that peer's hub
  directly.

## The viewer

`GET /v1/viewer` is a static page that plays a device's stream. Open it in
Google Chrome, Safari or Edge (a browser with H.264 in Media Source
Extensions; Playwright's open-source Chromium has none):

```
http://127.0.0.1:7788/v1/viewer#device=<id>&token=<token>&fps=15
https://hub-a.example:7433/v1/viewer#device=<id>@hub-b&token=<token>&lease=<lease id>&audio=true
```

- **Parameters** come from the URL fragment, which the browser never sends
  to a server, so the token stays out of every log: `device`, `token`,
  `lease` (a hub lease, sent as `X-Maestro-Lease`), `fps` and `audio=true`.
- **No token for the page.** `GET /v1/viewer` is the one route a TCP
  listener (a daemon's or a hub's) serves without the bearer token, at
  exactly that path. The page carries no device data; the stream it then
  fetches needs the token as every route does.
- **Playback.** The page checks the stream's codecs with
  `MediaSource.isTypeSupported` and appends it to a `SourceBuffer`, where
  each fragment plays at its own time, so playback keeps real time. It
  holds each track's newest fragment until the next one arrives and writes
  the real gap between them as its duration (one frame of delay). The
  daemon can only estimate a duration from the gap before a sample, and a
  sample that starts more than twice the previous one's duration later is
  a discontinuity to Media Source Extensions, after which the browser
  drops frames until the next keyframe, seconds away on Android. With real
  durations an Android screen that is not changing (no frames for seconds)
  plays as one long frame, with no hole and no freeze. Playback runs about
  half a second behind the newest frame (Chrome resumes after running dry
  only with that much buffered): more than a second behind, it plays at
  1.5× until it is within 0.6 s; more than 3 s behind (after a reconnect,
  or a still screen's single long frame), it seeks.
  When the stream's audio cannot play in a browser (iOS sends LPCM, `ipcm`
  or `fpcm`) it asks again without audio.
- **Audio.** The video starts muted, with `autoplay` and `playsinline`,
  which Chrome allows without a click. When the stream has audio the
  browser can play, an **Unmute** button turns it on.
- **The status line** reads
  `playing · <W>×<H> · <fps> fps · <frames> frames · <t> s · <muted|audio on|no audio>`,
  or `error: <reason>` (the `CAPTURE_UNAVAILABLE` reason, or the error
  code) when the stream is refused, and `error: playback failed: …` when
  the browser cannot play it (a decode error, or an append its buffer
  refuses).
- **Reconnects.** After a stream ends, is refused or fails to play the
  page tries again every 3 s. It cancels the old request first, so a
  reconnect never leaves a connection open.

## Recording

On a device with screen capture the executor records the
`startRecording` and `stopRecording` steps itself, from the device's
capture session, and the file lands with the report:

| where | file | the step's `data` |
| --- | --- | --- |
| a daemon session (CLI one-shot, REST, JS) | `<report dir>/recordings/<name>.mp4` | `recordings/<name>.mp4` |
| `maestro-d test` | `<report>/assets/<flow>/recordings/<name>.mp4` | `assets/<flow>/recordings/<name>.mp4` |

- `<name>` is a file name: a directory in it is dropped, `.mp4` is added
  when missing, and an empty name (`maestro-d startRecording` with no
  value) is `recording`.
- A recording never replaces an earlier one. When its name is already
  taken in the session (a second `startRecording` with no name, or
  `/sdcard/a/x.mp4` after `/sdcard/b/x.mp4`) or a file of that name is
  already there, it gets a numeric suffix: `recording-2.mp4`, `x-2.mp4`,
  and so on. The step's `data` names the file it wrote.
- One recording runs at a time: a second `startRecording` fails the step
  with "a recording is already running (<name>)".
- `stopRecording` finalizes the file; its `path` is ignored. A recording
  left running is finalized when the flow ends, the session ends or the
  device is detached.
- A recording that ended before its stop (it fell behind the stream, or
  the capture failed) is still saved, up to where it ended:
  `stopRecording` succeeds with a warning in its message ("Saved
  recording <path> (warning: capture: the recording ended early: …)"),
  and `--record` links the file in the report and logs the warning.
- In a daemon session `GET /v1/devices/{id}/artifacts/recordings/<name>.mp4`
  serves the file, to remote and hub callers too.
- `--record` / `--video` record through the same capture session into the
  flow's `recording.mp4`.
- **Android changed.** `startRecording` used to run `screenrecord` on the
  device and leave the file there (`/sdcard/recording.mp4`, or the step's
  `path`), and `--record` stopped at screenrecord's three-minute cap.
  Both now record on the host, as above, for as long as the flow runs,
  with the device's audio where it has any. A flow that pulled
  `/sdcard/recording.mp4` afterwards should read the step's `data`
  instead.
- The files are fragmented MP4 finalized with the recording's duration
  (`mehd`) and a keyframe index (`mfra`), so the HTML report's `<video>`
  knows its length and can seek. Unlike a live stream, each sample is
  written with its real duration (the recorder holds it until its
  track's next sample), so a still screen is one long frame.

Drivers without screen capture keep their own handling of these steps.

## Errors

`CAPTURE_UNAVAILABLE` (exit 4, HTTP 503) means the stream could not start;
`details.reason` says why. `X-Maestro-Error-Code: CAPTURE_UNAVAILABLE`
tells it from `DAEMON_UNAVAILABLE`'s 503.

```json
{
  "ok": false,
  "exitCode": 4,
  "error": {
    "code": "CAPTURE_UNAVAILABLE",
    "message": "screen capture unavailable (unsupported-platform): device web-viewer (cdp driver) has no screen capture",
    "details": { "reason": "unsupported-platform" }
  }
}
```

| `details.reason` | meaning | what to do |
| --- | --- | --- |
| `unsupported-platform` | the driver has no screen capture (web, tvOS, Appium) | — |
| `simulator` | an iOS simulator | — |
| `not-macos` | iOS capture needs a macOS host | run the daemon on the Mac the device is plugged into |
| `camera-permission` | macOS denied the capture helper Camera access, or nobody approved the prompt within 120 s | [Camera approval](#camera-approval) |
| `no-capture-device` | the device's capture source was not found: it is not connected over USB (Wi-Fi or a CoreDevice network link only), or it did not appear within 10 s | [troubleshooting](#troubleshooting-ios-capture) |
| `ambiguous-device-name` | two iOS devices on the Mac share a name, and macOS does not say which capture device has which UDID ([finding the capture source](#the-capture-helper)) | rename one |
| `helper-failed` | the capture helper did not build, start or connect, or the device did not start capturing within 15 s | [troubleshooting](#troubleshooting-ios-capture) |
| `android-too-old` | Android below SDK 21 | — |
| `scrcpy-failed` | the Android capture server failed (its last log line is in the message) | see the daemon log's `[scrcpy <serial>]` lines |
| `capture-lost` | the capture stopped while the device was attached (on iOS the helper exited or the device's connection broke; the device re-enumerated, so WebDriverAgent is gone too) | detach and attach again |
| `no-frames` | no frames arrived within 10 s (a screen that is off, a capture that never started) | wake the device, then retry |

Through a hub, a stream to a peer the hub cannot open a bulk link to fails
`PEER_UNAVAILABLE` (exit 3, HTTP 503) with `details.reason:
"peer-not-dialable"` and `details.hub` ([hub.md](hub.md#routing)); stream
from that peer's hub directly, or give that hub an address the other can
reach (`--advertise`).

The `device.capture` event on `GET /v1/events` follows a device's capture:
`message` is `capturing`, `unavailable` or `lost`, `error` the reason. An
on-demand capture stopping because nobody watches is not published.

## Platforms

| driver | capture |
| --- | --- |
| `uiautomator2`, `devicelab` (Android devices and emulators) | [scrcpy-server](#android), on demand: H.264 at up to the requested `fps`, AAC audio on Android 11 and later |
| `mock` | an embedded 640×360 H.264 clip with AAC audio, looped at the requested `fps` (`FPS` is a ceiling); used by the tests and the viewer's checks |
| `wda`, a physical iPhone or iPad on a macOS host, over USB | Apple's QuickTime screen-capture channel through the capture helper ([below](#iphones-and-ipads)): the device's own H.264 at about 60 fps (`X-Maestro-Fps: device`; `fps` is advisory) and LPCM audio as the device delivers it (48 kHz, 2 channels, 16-bit integer as `ipcm` or 32-bit float as `fpcm`). Always on while the device is attached |
| `wda`, an iOS simulator | `CAPTURE_UNAVAILABLE` `simulator`; `--record` still records through `simctl io recordVideo`, and the recording steps stay unsupported |
| `wda` on tvOS; `devicelab` (iOS simulators), `appium`, `cdp` (web) | `CAPTURE_UNAVAILABLE` `unsupported-platform` |
| `computer` (the Mac, [macos.md](../macos.md)) | `CAPTURE_UNAVAILABLE` `unsupported-platform`: Screen Sharing sends VNC clients pixels, never video |
| every other driver | `CAPTURE_UNAVAILABLE` `unsupported-platform` for now |

Measured on an M1 Max MacBook Pro (macOS 26) with an iPhone 13
Pro on iOS 26.6.1 and a Pixel 8 on Android 16, streaming at `fps=15`
while steps ran (the Pixel with the Clock stopwatch running, so its
screen changed all the time):

| | iPhone 13 Pro, QuickTime channel | Pixel 8, scrcpy |
| --- | --- | --- |
| video | H.264 (`avc3` on the wire), 1170×2532, about 60 fps whatever `fps` asks, about 2 keyframes a second | H.264 (`avc3`), 2400×1080 in landscape, at most `fps`, a keyframe about every 4 s while the screen changes and none while it is still |
| audio | LPCM 48 kHz, 2 ch, 16-bit integer (`ipcm`) or 32-bit float (`fpcm`) | AAC-LC 48 kHz, 2 ch, about 0.13 Mb/s |
| bitrate | about 3.2 (static screen) to 3.8 Mb/s (swiping) | video about 0.2 (mostly static screen) to 1.2 Mb/s (swiping) |
| host CPU | the capture helper about 4.6% of one core, maestro-d about 1.0% (attached with no viewer: 4.9% and 0.6%) | maestro-d about 0.7% of one core (attached with no viewer: 0.1%); no helper |
| device CPU | `airplayd` about 8% of one core (none without capture); Instruments' total load about 66% summed over the six cores, against 46% with no capture and WebDriverAgent stopped | the scrcpy server (`app_process`) about 30% of one core, the hardware encoder's service (`samsung.hardware.media.c2@1.2-service`) 11%, `media.swcodec` (AAC) 10%, `audioserver` 2%, `surfaceflinger` 42% against 32% without capture; `top`'s whole-device load 231% of 900% against 160% without capture |

## Android

Android devices and emulators capture through
[scrcpy](https://github.com/Genymobile/scrcpy)'s server, pinned at 5.0.1
and shipped as `drivers/android/scrcpy-server-v5.0.1` (Apache-2.0,
`drivers/android/LICENSE-scrcpy`). Nothing is installed on the device
beyond that jar:

- **Start.** The first viewer or recorder starts the capture. maestro-d
  checks the jar's sha256 against the pinned one, pushes it to
  `/data/local/tmp/maestro-d-scrcpy-server.jar` when the device's copy
  differs (checked once per device per daemon, and again after a capture
  on that device fails, so a jar the device lost is pushed again),
  forwards a free local port to the server's socket, and starts the
  server over `adb shell`. Its output goes to the daemon log, each line
  prefixed `[scrcpy <serial>]`.
- **Stop.** The capture stops 10 s after its last viewer leaves (the shell
  and the forward go with it), and when the device is detached. The jar
  stays on the device for the next start.
- **Video.** H.264 from the device's encoder. Keyframes are seconds
  apart (about every 4 s on a Pixel 8 while the screen changes, none while
  it is still); a viewer or recording that joins between them still starts
  at once, because the frames since the last keyframe arrive first, a
  millisecond apart. `fps` is the encoder's `max_fps`, a ceiling: a static
  screen sends almost nothing, so a stream of an idle screen has few
  frames. After the screen stops changing the encoder repeats its last
  frame a few times, every 100 ms or, below 10 fps, once per frame
  interval (`repeat-previous-frame-after`), so the repeats stay under the
  ceiling too. A rotation changes the stream's size in-band.
- **Audio.** AAC, 48 kHz stereo, from Android 11 (SDK 30). On Android 13
  and later the capture always takes the device's audio, from the
  `playback` source with the device's speaker still playing. On Android 11
  and 12 it takes audio only when the viewer that starts the capture (or a
  recording) asks for it; a later viewer asking for audio then gets
  `X-Maestro-Audio: false`. Below Android 11 there is no audio.
- **Recording** (`--record`, `startRecording`) runs on the host through
  the same capture: no three-minute cap, audio where the device has it.

**Limits.**

- **`FLAG_SECURE`.** Windows that set it (banking and DRM screens) capture
  as black on Android 12 and later.
- **Android 11 unlock.** Audio capture on Android 11 needs the screen
  unlocked when the capture starts; otherwise the device disables audio
  and the stream has none (the daemon log says so).
- **Android 11–12 speaker.** Audio capture silences the device's speaker
  while it runs. That is why, on those versions, audio is captured only
  when the first viewer or a recording asks for it.
- **Android 13+ opt-out.** The `playback` source keeps the speaker
  playing, but captures only apps that allow their playback to be
  captured; an app can opt out, and its sound is then missing.
- **Old devices.** Below Android 5.0 (SDK 21) capture answers
  `CAPTURE_UNAVAILABLE` `android-too-old`.
- **Failures.** A server that does not start, or stops, answers
  `scrcpy-failed`; the message carries the server's last log line.

## iPhones and iPads

On a macOS host, attaching a physical iPhone or iPad over USB starts its
screen capture before anything else touches the device, and keeps it
running until the device is detached. Frames nobody watches are dropped.
Streams, the viewer, `--record` and the recording steps all read that
one capture.

### The capture helper

The capture runs in `MaestroDCapture.app` (bundle id
`app.edge.maestro-d.capture`; macOS names it **MaestroDCapture**), a small
Swift app whose source ships in `drivers/ios/MaestroDCapture/`. maestro-d builds
it on first use with `xcrun swiftc` (Xcode's command-line tools), signs
it, and keeps it in `<home>/cache/capture-helper/<key>/` (the newest two
builds are kept; the key covers the source, `Info.plist`, the compiler
version and the signing identity). It is launched with `open -n -g -j`,
so it runs in the background with no Dock icon, and talks to maestro-d
over a unix socket in `$TMPDIR/maestro-d-cap-<uid>/` (mode 0700).

Each maestro-d process (a daemon, or one `maestro-d test` run) runs at
most one helper, shared by all of its iOS devices. It starts with the
process's first iPhone attach and exits with the process; detaching a
device stops only that device's capture.

**Finding the device's capture source.** The helper looks among the
Mac's external capture devices whose model is `iOS Device`, every 250 ms
for up to 10 s. It would take one whose unique ID, without dashes, is the
UDID (the device's USB serial number), but macOS does not give one: an
iPhone's capture device has a UUID of its own as its unique ID, which no
CoreMediaIO device property, IOKit USB entry or lockdown value of the
device repeats, and which no UUID derivation of the UDID gives. So the
helper matches by name: the one capture device named like the device
(Settings → General → About → Name), and the daemon logs
`capture <udid>: capturing (match: name)`. Two USB iOS devices with the
same name on one Mac both answer `ambiguous-device-name` after 10 s; give
each its own name.

**Signing.** The helper is signed with `MAESTRO_D_CAPTURE_SIGN_IDENTITY`
when set, else the first `Apple Development:` identity
`security find-identity -p codesigning -v` lists, else ad-hoc. macOS
remembers the Camera approval per signing identity: with an Apple
Development identity a rebuilt helper (a new maestro-d version, a new
Xcode) keeps the approval; an ad-hoc signed helper asks again after every
rebuild.

### Camera approval

macOS treats screen capture of a USB iPhone as a camera, so the helper
needs Camera access. The first time a Mac's helper starts, macOS shows
**"MaestroDCapture" would like to access the camera**. Someone at the
Mac has to click **Allow**, once per Mac (and per signing identity, see
above):

- While the prompt is open, the attach waits up to 120 s and the daemon
  logs "approve camera access for MaestroDCapture on this Mac". After
  that the device attaches without capture (`camera-permission`); the
  helper keeps waiting, and the next attach captures once access is
  allowed.
- The approval cannot be granted ahead of time by an MDM profile: Camera
  is one of the privacy services a PPPC profile can only deny, never
  allow.
- Denied: allow it in System Settings → Privacy & Security → Camera →
  MaestroDCapture. To be asked again, reset the decision with
  `tccutil reset Camera app.edge.maestro-d.capture` and attach again.
- A Mac reached only over SSH, with nobody at its screen, cannot answer
  the prompt; approve it once from the screen (or Screen Sharing).

### USB re-enumeration and the attach order

Turning the capture channel on makes every USB iOS device on the Mac
switch to its capture USB configuration, which re-enumerates it and drops
every usbmux connection to it (WebDriverAgent's, Xcode's, other tools').
Turning it off again (the helper exiting) does the same. So the attach
order is:

1. Start the capture: launch the helper if the process has none, open
   the device, and wait until it captures (15 s, plus up to 120 s while
   the helper is built and launched or the Camera prompt is open).
2. When the device re-enumerated (the helper found its capture device),
   wait until usbmuxd lists it again, and at least 1.5 s.
3. Install the app, build and start WebDriverAgent, as before.

Detaching stops WebDriverAgent first, then the device's capture.

A capture that fails never fails the attach: the device attaches, the
`device.capture` event says `unavailable` with the reason, and streams
answer `CAPTURE_UNAVAILABLE`. When the capture is lost later (the helper
was killed, the cable was pulled) the device has re-enumerated, so
WebDriverAgent's connection is gone too: `device.capture` says `lost`,
steps that fail with a network error add "the iPhone re-enumerated when
screen capture stopped; detach and attach again", and there is no
automatic recovery. Detach and attach again.

Capture is skipped, and `Capture`/streams answer why, for a simulator
(`simulator`), a TV (`unsupported-platform`), a device usbmuxd lists only
over Wi-Fi or one reachable only through CoreDevice
(`no-capture-device`, "not connected over USB"), and a host that is not
macOS (`not-macos`). `maestro-d test` captures only when the run records
(`--record`/`--video`, or a `startRecording` step anywhere in its flows,
or a sub-flow file it cannot see ahead); the one-shot `screenshot` and
`hierarchy` commands never capture.

### One capture-enabled process per Mac

The capture channel is a switch for the whole Mac. When the first helper
on the Mac starts, and when the last one exits, every USB iOS device on
the Mac re-enumerates, and every other process using one (another daemon,
a `maestro-d test` run, the upstream one-shots, Xcode) loses its
connection. The helper's start logs "screen capture switches every USB
iOS device on this Mac into capture mode; other processes using those
devices lose their connections now".

So run **one capture-enabled maestro-d process per Mac**: the hub's
daemon, or one daemon, and attach every iOS device there. Within one
daemon the switch happens once, during its first iOS attach, before any
of its WebDriverAgent connections exist; later attaches find it on.
Restarting that daemon within 10 s is fine: the next helper launch waits
out launchd's relaunch throttle and retries once.

### Audio in browsers

iOS sends LPCM as the device delivers it: usually 16-bit integer, which
MP4 carries as `ipcm`, and on some attaches 32-bit float, carried as
`fpcm` (ISO/IEC 23003-5; FFmpeg reads it as `pcm_f32le`). The float
attaches deliver each channel's samples one after the other
(non-interleaved); MP4 wants them interleaved, so the host reorders them,
copying every sample bit for bit. It never converts a sample, so the
format can differ from one attach to the next; the daemon log's
`capture <udid>: audio lpcm …` line names the one an attach got. FFmpeg plays both, and VLC and QuickTime Player play `ipcm`;
browsers play neither. The viewer therefore asks an iPhone's
stream without audio, and the HTML report's `<video>` plays an iPhone
recording's video and ignores its audio track.

### Troubleshooting iOS capture

The daemon log (`<home>/run/<daemon>/daemon.log`)
has a `capture <udid>:` line for each step, and the helper's own
statuses.

| reason | look for | fix |
| --- | --- | --- |
| `camera-permission` | "approve camera access" | approve the prompt at the Mac's screen; if it was denied, System Settings → Privacy & Security → Camera, or `tccutil reset Camera app.edge.maestro-d.capture` |
| `helper-failed` "build the capture helper" | `swiftc:` or `codesign:` errors | install Xcode's command-line tools; unlock the login keychain that holds the signing identity, or set `MAESTRO_D_CAPTURE_SIGN_IDENTITY=-` |
| `helper-failed` "did not connect" | "capture helper relaunched within 10 s" | another helper is being torn down; attach again after a few seconds |
| `helper-failed` "did not start capturing within 15 s" | `capture <udid>: device found` without `capturing` | unlock the device and wake its screen; the capture sends nothing while the display is off |
| `no-capture-device` "not connected over USB" | — | connect the device with a cable; Wi-Fi and CoreDevice network links cannot capture |
| `no-capture-device` "did not appear within 10 s" | — | unlock the device, trust the Mac, reconnect the cable |
| `ambiguous-device-name` | — | give each device a unique name (Settings → General → About → Name) |
| `capture-lost` | "capture helper pid … exited" | detach and attach again; another process starting or stopping capture on the Mac also causes it ([one per Mac](#one-capture-enabled-process-per-mac)) |
| `no-frames` (stream) | — | wake the device's screen |
