// Live screen streaming end to end on a real device, driven through this
// repository's JavaScript client (npm/maestro-d): the device's stream at
// several frame rates while the device is driven, then a recording made
// with the startRecording and stopRecording steps.
//
// The repository has no TypeScript test setup outside npm/maestro-d, so the
// script stands alone: Node strips its types (Node 22.18 or later runs a .ts
// file as is; older 22.x needs --experimental-strip-types), or run it with
// `npx tsx`. It needs ffprobe on PATH. Build the binary into the worktree's
// own install and the client first:
//
//   make build INSTALL_DIR=$PWD/.home && (cd npm/maestro-d && npm install && npm run build)
//
//   MAESTRO_RUNNER_HOME=$PWD/.home MAESTRO_D_BIN=$PWD/.home/bin/maestro-d \
//   MAESTRO_D_DEVICE=<device id> \
//   MAESTRO_D_APP=<bundle id to drive, e.g. com.android.settings | com.apple.Preferences> \
//   [MAESTRO_D_TEAM_ID=<WebDriverAgent signing team, for a physical iOS device>] \
//   [MAESTRO_D=<daemon name>] [MAESTRO_D_CLIENT_ROOT=<client package, default npm/maestro-d>] \
//   [MAESTRO_D_ARTIFACT_DIR=<where the streams and the recording go, default a temp dir>] \
//   [MAESTRO_D_FPS_LIST=3,15,60] \
//     node e2e/streaming.e2e.ts
//
// Steps:
//   1. Attach, then launchApp.
//   2. For each fps, open device.stream({fps, audio: true}) for 12 s while
//      running 6 swipes (alternating up and down) and one takeScreenshot,
//      and save the bytes as stream-<fps>.mp4. Between two rates the script
//      waits out the 10 s a capture keeps running after its last viewer, so
//      each rate starts a capture of its own; where the device honours the
//      rate, X-Maestro-Fps must then be the rate asked for.
//   3. Check each file with ffprobe: H.264 at a real size; at least one
//      frame per swipe; an audio track when the stream says it has one; and
//      the observed rate, frames / duration (ffprobe's format duration; the
//      rate within the busiest stretch, (frames - 1) / PTS span, is logged
//      beside it). Where X-Maestro-Fps is a number it must be at most
//      max(request * 1.35, request + 1). There is no lower bound on Android:
//      max_fps is a ceiling and a static screen sends almost nothing. Where
//      it is `device` (iOS), the rate must be between 40 and 66.
//   4. Run startRecording: e2e and stopRecording around 3 swipes, fetch the
//      recording from the report and check it the same way.
//   5. Detach.
// Each step is checked and the script exits 1 at the first failure.
import assert from 'node:assert/strict'
import { execFile } from 'node:child_process'
import { mkdir, mkdtemp, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import path from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'
import { promisify } from 'node:util'

import type {
  MaestroDevice,
  VideoStream
} from '../npm/maestro-d/dist/types/index.js'

interface Client {
  MaestroD: {
    attach: (options: {
      name?: string
      device: string
      appId: string
      teamId?: string
    }) => Promise<MaestroDevice>
  }
}

const run = promisify(execFile)
const repoRoot = path.dirname(path.dirname(fileURLToPath(import.meta.url)))

function required(key: string): string {
  const value = process.env[key]
  if (value == null || value === '') throw new Error(`${key} is required`)
  return value
}
function optional(key: string): string | undefined {
  const value = process.env[key]
  return value == null || value === '' ? undefined : value
}

const device = required('MAESTRO_D_DEVICE')
const appId = required('MAESTRO_D_APP')
const teamId = optional('MAESTRO_D_TEAM_ID')
const daemonName = optional('MAESTRO_D') ?? 'streaming-e2e'
const clientRoot =
  optional('MAESTRO_D_CLIENT_ROOT') ?? path.join(repoRoot, 'npm', 'maestro-d')
const fpsList = (optional('MAESTRO_D_FPS_LIST') ?? '3,15,60')
  .split(',')
  .map(s => Number(s.trim()))
for (const fps of fpsList) {
  if (!Number.isInteger(fps) || fps < 3 || fps > 60) {
    throw new Error(`MAESTRO_D_FPS_LIST: ${String(fps)} is not an integer from 3 to 60`)
  }
}

const streamSeconds = 12
const swipesPerStream = 6
/** A capture keeps running 10 s after its last viewer; wait a bit longer. */
const lingerMs = 11_000

const started = Date.now()
function say(line: string): void {
  const seconds = ((Date.now() - started) / 1000).toFixed(1)
  console.log(`+${seconds.padStart(6)}s ${line}`)
}
async function sleep(ms: number): Promise<void> {
  await new Promise(resolve => setTimeout(resolve, ms))
}

let failures = 0
let passes = 0
/** Runs one checked step and logs it; the first failure ends the run. */
async function step<T>(name: string, body: () => Promise<T>): Promise<T> {
  const t0 = Date.now()
  try {
    const value = await body()
    passes++
    say(`PASS ${name} (${Date.now() - t0} ms)`)
    return value
  } catch (error) {
    failures++
    const message = error instanceof Error ? error.message : String(error)
    say(`FAIL ${name} (${Date.now() - t0} ms): ${message}`)
    throw error
  }
}

interface Probe {
  video: { codec: string; width: number; height: number }
  audio?: { codec: string; sampleRate: number; channels: number }
  frames: number
  /** The file's duration in seconds (ffprobe's format duration). */
  duration: number
  /** frames / duration: the observed rate. */
  fps: number
  /** (frames - 1) / (last PTS - first PTS), or 0 with fewer than 2 frames. */
  spanFps: number
  span: number
}

/** ffprobe's view of a file: its streams, and the video's frame times. */
async function probe(file: string): Promise<Probe> {
  const streams = JSON.parse(
    (
      await run('ffprobe', [
        '-v', 'error',
        '-show_entries', 'stream=codec_type,codec_name,width,height,sample_rate,channels:format=duration',
        '-of', 'json', file
      ])
    ).stdout
  ) as {
    streams: Array<{
      codec_type: string
      codec_name: string
      width?: number
      height?: number
      sample_rate?: string
      channels?: number
    }>
    format?: { duration?: string }
  }
  const v = streams.streams.find(s => s.codec_type === 'video')
  assert(v != null, `${file} has no video stream`)
  const a = streams.streams.find(s => s.codec_type === 'audio')
  const times = (
    await run(
      'ffprobe',
      ['-v', 'error', '-select_streams', 'v:0', '-show_entries', 'packet=pts_time', '-of', 'csv=p=0', file],
      { maxBuffer: 64 << 20 }
    )
  ).stdout
    .split('\n')
    .map(Number.parseFloat)
    .filter(Number.isFinite)
    .sort((x, y) => x - y)
  const span = times.length > 1 ? times[times.length - 1] - times[0] : 0
  const duration = Number(streams.format?.duration ?? 0)
  return {
    video: { codec: v.codec_name, width: v.width ?? 0, height: v.height ?? 0 },
    ...(a == null
      ? {}
      : { audio: { codec: a.codec_name, sampleRate: Number(a.sample_rate), channels: a.channels ?? 0 } }),
    frames: times.length,
    duration,
    fps: duration > 0 ? times.length / duration : 0,
    spanFps: span > 0 ? (times.length - 1) / span : 0,
    span
  }
}

function describe(p: Probe): string {
  const audio = p.audio == null ? 'no audio' : `${p.audio.codec} ${p.audio.sampleRate} Hz ${p.audio.channels} ch`
  return (
    `${p.video.codec} ${p.video.width}x${p.video.height}, ${p.frames} frames in ${p.duration.toFixed(2)} s (${p.fps.toFixed(2)} fps; ` +
    `${p.spanFps.toFixed(2)} fps over the ${p.span.toFixed(2)} s from the first frame to the last), ${audio}`
  )
}

/** Checks a probed file: H.264 at a real size, with at least `minFrames`. */
function checkVideo(p: Probe, minFrames: number): void {
  assert.equal(p.video.codec, 'h264', `video codec ${p.video.codec}`)
  assert(p.video.width > 0 && p.video.height > 0, `video size ${p.video.width}x${p.video.height}`)
  assert(p.frames >= minFrames, `${p.frames} frames, want at least ${minFrames} (one per swipe)`)
}

let swipeCount = 0
/** One swipe, alternating up and down. */
async function swipe(m: MaestroDevice): Promise<void> {
  const up = swipeCount++ % 2 === 0
  await m.swipe(
    up
      ? { start: '50%, 75%', end: '50%, 30%', duration: 400 }
      : { start: '50%, 30%', end: '50%, 75%', duration: 400 }
  )
}

/** Reads a stream's body until it ends, into one buffer. */
async function collect(body: ReadableStream<Uint8Array>): Promise<Buffer> {
  const chunks: Uint8Array[] = []
  const reader = body.getReader()
  for (;;) {
    const { done, value } = await reader.read()
    if (done) break
    chunks.push(value)
  }
  return Buffer.concat(chunks)
}

async function streamAt(m: MaestroDevice, fps: number, artifacts: string): Promise<void> {
  const file = path.join(artifacts, `stream-${fps}.mp4`)
  const stop = new AbortController()
  const stream: VideoStream = await step(`stream({fps: ${fps}, audio: true})`, async () => {
    const s = await m.stream({ fps, audio: true, signal: stop.signal })
    say(`     X-Maestro-Fps ${String(s.fps)}, X-Maestro-Audio ${String(s.audio)}, X-Maestro-Codecs ${s.codecs}`)
    assert(s.codecs.startsWith('avc3.'), `codecs ${s.codecs}`)
    if (s.fps !== 'device') assert.equal(s.fps, fps, 'X-Maestro-Fps is not the rate asked for')
    return s
  })
  const bytes = collect(stream.body)
  const t0 = Date.now()
  try {
    await step(`${swipesPerStream} swipes and a takeScreenshot while streaming at ${fps} fps`, async () => {
      for (let i = 0; i < swipesPerStream; i++) {
        await swipe(m)
        await sleep(600)
        if (i === 2) await m.takeScreenshot({ path: path.join(artifacts, `screenshot-${fps}.png`) })
      }
    })
    const left = streamSeconds * 1000 - (Date.now() - t0)
    if (left > 0) await sleep(left)
  } finally {
    stop.abort()
  }
  const data = await bytes
  await writeFile(file, data)
  say(`     ${file}: ${data.length} bytes`)
  await step(`ffprobe stream-${fps}.mp4`, async () => {
    const p = await probe(file)
    say(`     ${describe(p)}`)
    checkVideo(p, swipesPerStream)
    if (stream.audio) assert(p.audio != null, 'the stream said it had audio; the file has none')
    if (stream.fps === 'device') {
      assert(p.fps >= 40 && p.fps <= 66, `observed ${p.fps.toFixed(2)} fps, want 40 to 66 (device rate)`)
    } else {
      const bound = Math.max(fps * 1.35, fps + 1)
      assert(p.fps <= bound, `observed ${p.fps.toFixed(2)} fps, want at most ${bound.toFixed(2)} for ${fps} requested`)
    }
  })
}

async function main(): Promise<void> {
  const artifacts =
    optional('MAESTRO_D_ARTIFACT_DIR') ??
    (await mkdtemp(path.join(tmpdir(), 'streaming-e2e-')))
  await mkdir(artifacts, { recursive: true })
  say(`device ${device}, app ${appId}, daemon ${daemonName}, fps ${fpsList.join('/')}, artifacts in ${artifacts}`)

  const client = (await import(
    pathToFileURL(path.join(clientRoot, 'dist', 'esm', 'index.js')).href
  )) as Client
  const m = await step('attach', async () =>
    await client.MaestroD.attach({
      name: daemonName,
      device,
      appId,
      ...(teamId == null ? {} : { teamId })
    })
  )
  let failed: unknown
  try {
    await step(`launchApp ${appId}`, async () => {
      await m.launchApp({ appId })
    })

    for (const [i, fps] of fpsList.entries()) {
      if (i > 0) {
        say(`     waiting ${lingerMs / 1000} s for the previous capture to stop`)
        await sleep(lingerMs)
      }
      await streamAt(m, fps, artifacts)
    }

    const rel = await step('startRecording: e2e', async () => {
      const res = await m.command('startRecording', 'e2e')
      assert.equal(typeof res.data, 'string', 'startRecording returned no path')
      say(`     ${String(res.data)}`)
      return res.data as string
    })
    await step('3 swipes while recording', async () => {
      for (let i = 0; i < 3; i++) {
        await swipe(m)
        await sleep(600)
      }
    })
    await step('stopRecording', async () => {
      const res = await m.command('stopRecording')
      assert.equal(res.data, rel, `stopRecording returned ${String(res.data)}`)
    })
    await step(`fetch and ffprobe ${rel}`, async () => {
      const file = path.join(artifacts, 'recording-e2e.mp4')
      await writeFile(file, await m.artifact(rel))
      const p = await probe(file)
      say(`     ${describe(p)}`)
      checkVideo(p, 3)
    })
  } catch (error) {
    failed = error
  }
  await step('detach', async () => {
    await m.detach()
  }).catch((error: unknown) => {
    failed ??= error
  })
  if (failed !== undefined) throw failed
}

main().then(
  () => {
    say(`${passes} steps passed`)
  },
  (error: unknown) => {
    if (failures === 0) {
      say(`FAIL ${error instanceof Error ? error.message : String(error)}`)
    }
    say(`${passes} steps passed, then a failure`)
    process.exitCode = 1
  }
)
