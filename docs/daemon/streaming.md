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
  same; neither retries a stream.
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
- In a daemon session `GET /v1/devices/{id}/artifacts/recordings/<name>.mp4`
  serves the file, to remote and hub callers too.
- `--record` / `--video` record through the same capture session into the
  flow's `recording.mp4`.
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
| `camera-permission` | macOS denied the capture helper Camera access | allow it in System Settings → Privacy & Security → Camera |
| `no-capture-device` | the device's capture source was not found | check the USB connection |
| `ambiguous-device-name` | two devices share a name | rename one |
| `helper-failed` | the capture helper did not start | see the daemon log |
| `android-too-old` | Android below SDK 21 | — |
| `scrcpy-failed` | the Android capture server failed | see the daemon log |
| `capture-lost` | the capture stopped while the device was attached | detach and attach again |
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
| `mock` | an embedded 640×360 H.264 clip with AAC audio, looped at the requested `fps` (`FPS` is a ceiling); used by the tests and the viewer's checks |
| every other driver | `CAPTURE_UNAVAILABLE` `unsupported-platform` for now |
