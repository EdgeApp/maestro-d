// The computer device end to end: the Mac this script runs on, listed as
// `mac` and driven over its own Screen Sharing, through this repository's
// JavaScript client (npm/maestro-d) and the one-shot CLI. A window of four
// colour quadrants (e2e/testdata/computer/MaestroDObserver) covers the
// screen and records every key and mouse event it receives, so what the
// device sends is checked where it lands.
//
// The script stands alone, as the other scripts here do: Node strips its
// types (Node 22.18 or later runs a .ts file as is). Build the binary into
// the branch's own install and the client first, and put the Mac's
// credentials in that install's home:
//
//   make build INSTALL_DIR=$HOME/.maestro-d-<branch> && (cd npm/maestro-d && npm install && npm run build)
//   # $MAESTRO_D_HOME/computer.json, mode 0600:
//   #   {"vncPassword": "…", "macosUser": "<you>", "macosPassword": "…"}
//
//   MAESTRO_D_BIN=$HOME/.maestro-d-<branch>/bin/maestro-d \
//   MAESTRO_D_HOME=$HOME/.maestro-d-<branch>/run \
//   [MAESTRO_D=<daemon name, default computerDevice-e2e>] \
//   [MAESTRO_D_SCRATCH=<scratch directory, default $TMPDIR/maestro-d-computer-e2e>] \
//   [MAESTRO_D_HUB_PORTS=<two free ports, default 7510,7511>] \
//     node e2e/computer.e2e.ts [--only C1,C3,…]
//
// The Mac must be logged in with Screen Sharing on, and nobody may use its
// keyboard or mouse while the script runs. It never locks the Mac or logs it
// out: the lock-screen and login-window logins are covered by the driver's
// unit tests.
//
// Every action is printed as a `$ ` line followed by what it returned, so
// the output is the scenario's log. The script exits 1 at the end when any
// scenario failed.
import assert from 'node:assert/strict'
import { execFile, spawn, type ChildProcess } from 'node:child_process'
import { chmod, mkdir, readFile, readdir, rm, stat, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import path from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'
import { promisify } from 'node:util'
import { inflateSync } from 'node:zlib'

const run = promisify(execFile)
const repoRoot = path.dirname(path.dirname(fileURLToPath(import.meta.url)))

function required(key: string): string {
  const v = process.env[key]
  if (v == null || v === '') throw new Error(`${key} is required`)
  return v
}
const bin = required('MAESTRO_D_BIN')
const hubBin = process.env.MAESTRO_D_HUB_BIN ?? path.join(path.dirname(bin), 'maestro-d-hub')
const home = required('MAESTRO_D_HOME')
const daemonName = process.env.MAESTRO_D ?? 'computerDevice-e2e'
const clientRoot = process.env.MAESTRO_D_CLIENT_ROOT ?? path.join(repoRoot, 'npm', 'maestro-d')
const scratch = process.env.MAESTRO_D_SCRATCH ?? path.join(tmpdir(), 'maestro-d-computer-e2e')
const [hubPortA, hubPortB] = (process.env.MAESTRO_D_HUB_PORTS ?? '7510,7511').split(',').map(Number)
const onlyArg = process.argv.indexOf('--only')
const only = onlyArg > 0 ? new Set(process.argv[onlyArg + 1].split(',').map(s => s.trim())) : null

const started = Date.now()
function say(line: string): void {
  console.log(`+${((Date.now() - started) / 1000).toFixed(1).padStart(6)}s ${line}`)
}
function show(cmd: string): void {
  console.log(`$ ${cmd}`)
}
async function sleep(ms: number): Promise<void> {
  await new Promise(resolve => setTimeout(resolve, ms))
}

// --- the CLI -----------------------------------------------------------------

interface CLIResult { code: number; stdout: string; stderr: string }

async function cli(args: string[], env: Record<string, string> = {}, label?: string, executable = bin): Promise<CLIResult> {
  show(`${label ?? ''}${path.basename(executable)} ${args.join(' ')}`)
  try {
    const r = await run(executable, args, { env: { ...process.env, MAESTRO_D_HOME: home, ...env }, maxBuffer: 64 << 20 })
    if (r.stdout.trim()) console.log(r.stdout.trimEnd())
    return { code: 0, stdout: r.stdout, stderr: r.stderr }
  } catch (error) {
    const e = error as { code?: number; stdout?: string; stderr?: string }
    const out = `${e.stdout ?? ''}${e.stderr ?? ''}`.trimEnd()
    if (out) console.log(out)
    // A failure's text may be on either stream; callers check stdout.
    return { code: typeof e.code === 'number' ? e.code : 1, stdout: out, stderr: e.stderr ?? '' }
  }
}

async function sh(cmd: string, args: string[]): Promise<string> {
  show(`${cmd} ${args.join(' ')}`)
  const r = await run(cmd, args).catch((e: { stdout?: string }) => ({ stdout: e.stdout ?? '' }))
  if (r.stdout.trim()) console.log(r.stdout.trimEnd())
  return r.stdout
}

/** PIDs of the maestro-d processes serving a daemon name. */
async function daemonPids(name: string): Promise<string[]> {
  const out = await run('pgrep', ['-f', `serve --daemon ${name}`]).catch(() => ({ stdout: '' }))
  return out.stdout.split('\n').map(s => s.trim()).filter(Boolean)
}

/** Open connections from a daemon to Screen Sharing. */
async function vncConnections(name: string): Promise<number> {
  let n = 0
  for (const pid of await daemonPids(name)) {
    const out = await sh('lsof', ['-nP', '-a', '-p', pid, '-iTCP:5900', '-sTCP:ESTABLISHED'])
    n += out.split('\n').filter(l => l.includes(':5900')).length
  }
  return n
}

// --- images ------------------------------------------------------------------

interface RGB { w: number; h: number; px(x: number, y: number): [number, number, number] }

/** Decodes an 8-bit RGB or RGBA, non-interlaced PNG (what maestro-d writes). */
function decodePNG(buf: Buffer): RGB {
  assert.equal(buf.subarray(1, 4).toString('latin1'), 'PNG', 'not a PNG')
  let off = 8
  let w = 0, h = 0, channels = 0
  const idat: Buffer[] = []
  while (off < buf.length) {
    const len = buf.readUInt32BE(off)
    const type = buf.subarray(off + 4, off + 8).toString('latin1')
    const data = buf.subarray(off + 8, off + 8 + len)
    if (type === 'IHDR') {
      w = data.readUInt32BE(0); h = data.readUInt32BE(4)
      assert.equal(data[8], 8, 'bit depth'); channels = data[9] === 6 ? 4 : 3
    } else if (type === 'IDAT') idat.push(data)
    off += 12 + len
  }
  const raw = inflateSync(Buffer.concat(idat))
  const stride = w * channels
  const out = Buffer.alloc(h * stride)
  for (let y = 0; y < h; y++) {
    const f = raw[y * (stride + 1)]
    const line = raw.subarray(y * (stride + 1) + 1, (y + 1) * (stride + 1))
    for (let i = 0; i < stride; i++) {
      const a = i >= channels ? out[y * stride + i - channels] : 0
      const b = y > 0 ? out[(y - 1) * stride + i] : 0
      const c = y > 0 && i >= channels ? out[(y - 1) * stride + i - channels] : 0
      let v = line[i]
      if (f === 1) v += a
      else if (f === 2) v += b
      else if (f === 3) v += (a + b) >> 1
      else if (f === 4) { const p = a + b - c; const pa = Math.abs(p - a), pb = Math.abs(p - b), pc = Math.abs(p - c); v += pa <= pb && pa <= pc ? a : pb <= pc ? b : c }
      out[y * stride + i] = v & 0xff
    }
  }
  return { w, h, px: (x, y) => { const i = y * stride + x * channels; return [out[i], out[i + 1], out[i + 2]] } }
}

/** Decodes any image macOS reads (JPEG included) by converting it to PNG with sips. */
async function decodeImage(buf: Buffer, name: string): Promise<RGB> {
  if (buf.subarray(1, 4).toString('latin1') === 'PNG') return decodePNG(buf)
  const src = path.join(scratch, name)
  await writeFile(src, buf)
  const dst = `${src}.png`
  await run('sips', ['-s', 'format', 'png', src, '--out', dst])
  return decodePNG(await readFile(dst))
}

function kindOf(buf: Buffer): string {
  if (buf.subarray(1, 4).toString('latin1') === 'PNG') return 'png'
  if (buf[0] === 0xff && buf[1] === 0xd8) return 'jpeg'
  return 'unknown'
}

/**
 * The colour a pixel shows. Screen Sharing sends the display's own colour
 * space (a Retina XDR's sRGB red reads about 234,52,35), so colours are told
 * apart by which channels dominate, not by exact values.
 */
function colourOf([r, g, b]: [number, number, number]): string {
  if (r > 200 && g > 200 && b > 200) return 'white'
  if (r > 150 && g < 110 && b < 110) return 'red'
  if (g > 150 && r < 130 && b < 130) return 'green'
  if (b > 150 && r < 110 && g < 130) return 'blue'
  return `other(${r},${g},${b})`
}

/** The observer's quadrants at their centres: red, green, blue, white. */
function checkQuadrants(img: RGB, ox = 0, oy = 0, W = img.w, H = img.h): void {
  const want: Array<[number, number, string]> = [
    [W / 4, H / 4, 'red'], [3 * W / 4, H / 4, 'green'], [W / 4, 3 * H / 4, 'blue'], [3 * W / 4, 3 * H / 4, 'white']
  ]
  for (const [x, y, name] of want) {
    const got = img.px(Math.round(x - ox), Math.round(y - oy))
    assert.equal(colourOf(got), name, `pixel at ${x},${y} is ${got}`)
  }
  console.log('  quadrant centres: red, green, blue, white')
}

/** Every pixel on a grid over the image shows one colour. */
function allOf(img: RGB, name: string): void {
  for (let y = 5; y < img.h; y += Math.max(1, Math.floor(img.h / 10))) {
    for (let x = 5; x < img.w; x += Math.max(1, Math.floor(img.w / 10))) {
      assert.equal(colourOf(img.px(x, y)), name, `pixel at ${x},${y} is ${img.px(x, y)}`)
    }
  }
  console.log(`  ${img.w}x${img.h}, all ${name}`)
}

// --- the observer ------------------------------------------------------------

interface ObsEvent {
  type: string; t: number; flags: string[]; x: number; y: number
  characters?: string; charactersIgnoringModifiers?: string; keyCode?: number; clickCount?: number
  scrollingDeltaX?: number; scrollingDeltaY?: number
}

const observerBin = path.join(scratch, 'MaestroDObserver')
const eventsFile = path.join(scratch, 'events.jsonl')
let observer: ChildProcess | null = null
let screen = { width: 0, height: 0, scale: 1 }

async function startObserver(): Promise<void> {
  show(`xcrun swiftc -O -o ${observerBin} e2e/testdata/computer/MaestroDObserver/main.swift`)
  await run('xcrun', ['swiftc', '-O', '-o', observerBin, path.join(repoRoot, 'e2e/testdata/computer/MaestroDObserver/main.swift')])
  screen = JSON.parse((await run(observerBin, ['--screen-info'])).stdout)
  say(`screen ${screen.width}x${screen.height} points at ${screen.scale}x`)
  observer = spawn(observerBin, ['--out', eventsFile], { stdio: ['ignore', 'pipe', 'inherit'] })
  await new Promise<void>((resolve, reject) => {
    observer!.stdout!.once('data', () => resolve())
    observer!.once('exit', code => reject(new Error(`observer exited ${code}`)))
  })
  // Keys go to the frontmost app: until the observer is it, nothing is sent.
  for (let i = 0; i < 40 && await frontApp() !== 'MaestroDObserver'; i++) await sleep(250)
  await requireObserverFront()
}

/** The frontmost app's name, as Launch Services reports it. */
async function frontApp(): Promise<string> {
  const asn = (await run('lsappinfo', ['front'])).stdout.trim()
  const info = (await run('lsappinfo', ['info', '-only', 'name', asn])).stdout
  return /"LSDisplayName"="([^"]*)"/.exec(info)?.[1] ?? ''
}

/** Refuses to go on unless the observer has keyboard focus: input sent now
 * would land in another app. */
async function requireObserverFront(): Promise<void> {
  const app = await frontApp()
  if (app !== 'MaestroDObserver') throw new Error(`the frontmost app is "${app}", not the observer; no input was sent`)
}

async function observed(): Promise<ObsEvent[]> {
  const text = await readFile(eventsFile, 'utf8').catch(() => '')
  return text.split('\n').filter(Boolean).map(l => JSON.parse(l) as ObsEvent)
}

/** Runs body and returns the observer's events it caused. */
async function watch(body: () => Promise<unknown>, settle = 600): Promise<ObsEvent[]> {
  await requireObserverFront()
  const before = (await observed()).length
  await body()
  await sleep(settle)
  const ev = (await observed()).slice(before)
  for (const e of ev) {
    const detail = e.characters != null ? ` characters=${JSON.stringify(e.characters)} keyCode=${e.keyCode}` :
      e.scrollingDeltaY != null ? ` dx=${e.scrollingDeltaX} dy=${e.scrollingDeltaY}` :
        e.clickCount != null ? ` clickCount=${e.clickCount}` : ''
    console.log(`  observer: ${e.type}${detail} flags=[${e.flags.join(',')}] at ${e.x.toFixed(1)},${e.y.toFixed(1)}`)
  }
  return ev
}

async function flagsState(): Promise<{ flags: number; left: boolean; right: boolean }> {
  show('MaestroDObserver --flags-state')
  const s = JSON.parse((await run(observerBin, ['--flags-state'])).stdout)
  console.log(`  ${JSON.stringify(s)}`)
  return s
}

/** Screen pixels to the observer's points (origin bottom-left). */
function toPoints(px: number, py: number): [number, number] {
  return [px / screen.scale, screen.height - py / screen.scale]
}

function nearPoint(e: ObsEvent, px: number, py: number, tol = 2): boolean {
  const [x, y] = toPoints(px, py)
  return Math.abs(e.x - x) <= tol && Math.abs(e.y - y) <= tol
}

// --- the client --------------------------------------------------------------

interface Device {
  id: string
  [command: string]: any // eslint-disable-line @typescript-eslint/no-explicit-any
}
interface Client {
  MaestroD: {
    connect: (opts: { name?: string; bin?: string; hubSocket?: boolean }) => Promise<any> // eslint-disable-line @typescript-eslint/no-explicit-any
  }
  isMaestroError: (err: unknown, code?: string) => boolean
}

let client: Client
let maestro: any // eslint-disable-line @typescript-eslint/no-explicit-any
let W = 0, H = 0

async function call<T>(label: string, body: () => Promise<T>): Promise<T> {
  show(label)
  const v = await body()
  if (v != null && !(v instanceof Buffer)) console.log(`  ${JSON.stringify(v).slice(0, 400)}`)
  if (v instanceof Buffer) console.log(`  ${v.length} bytes (${kindOf(v)})`)
  return v
}

async function callFails(label: string, body: () => Promise<unknown>, code: string, text?: string): Promise<void> {
  show(label)
  try {
    await body()
  } catch (error) {
    const msg = error instanceof Error ? error.message : String(error)
    console.log(`  error ${(error as { code?: string }).code ?? ''}: ${msg}`)
    assert.ok(client.isMaestroError(error, code), `want ${code}, got ${msg}`)
    if (text) assert.ok(msg.includes(text), `want "${text}" in ${msg}`)
    return
  }
  throw new Error(`${label} succeeded; want ${code}`)
}

/** The computer, attached to the run's daemon (attaching it when needed). */
let handle: Device | null = null
async function mac(extra?: Record<string, unknown>): Promise<Device> {
  const list = await maestro.devices()
  const d = list.find((x: { id: string }) => x.id === 'mac')
  if (d?.attached && handle != null && extra == null) return handle
  if (d?.attached) await maestro.detach('mac')
  handle = await call(`attach({device: 'mac'${extra ? `, extra: ${JSON.stringify(extra)}` : ''}})`, async () =>
    await maestro.attach({ device: 'mac', ...(extra ? { extra } : {}) }))
  return handle!
}

/** Detaches the computer from every daemon of the run. */
async function freeMac(): Promise<void> {
  handle = null
  for (const name of [daemonName, 'computerDevice-cli']) {
    await cli(['detach', '--device', 'mac', '--daemon', name])
  }
}

// --- scenarios ---------------------------------------------------------------

const scenarios: Array<[string, () => Promise<void>]> = []
function scenario(id: string, body: () => Promise<void>): void { scenarios.push([id, body]) }

scenario('C1', async () => {
  const list = await call('devices()', async () => await maestro.devices({ platform: 'macos' }))
  const d = list.find((x: { id: string }) => x.id === 'mac')
  assert.ok(d, 'mac not listed')
  assert.equal(d.platform, 'macos'); assert.equal(d.kind, 'computer'); assert.equal(d.state, 'ready'); assert.ok(d.ready)
  const host = (await run('scutil', ['--get', 'ComputerName'])).stdout.trim()
  const model = (await run('sysctl', ['-n', 'hw.model'])).stdout.trim()
  const os = (await run('sw_vers', ['-productVersion'])).stdout.trim()
  assert.equal(d.name, host); assert.equal(d.model, model); assert.equal(d.osVersion, os)
  const all = await cli(['device', 'list', '--json', '--daemon', daemonName])
  assert.ok(/"id": "mac"[\s\S]*?"kind": "computer"/.test(all.stdout), 'the unfiltered listing lacks mac')
  await cli(['device', 'list', '--platform', 'macos', '--daemon', daemonName])
  const empty = path.join(scratch, 'emptyhome')
  await mkdir(empty, { recursive: true })
  const r = await cli(['device', 'list', '--json', '--platform', 'macos', '--daemon', 'computerDevice-empty'], { MAESTRO_D_HOME: empty }, 'MAESTRO_D_HOME=<scratch>/emptyhome ')
  assert.ok(r.stdout.includes('credentials-missing'), 'no config should list credentials-missing')
  await cli(['stop', '--daemon', 'computerDevice-empty'], { MAESTRO_D_HOME: empty }, 'MAESTRO_D_HOME=<scratch>/emptyhome ')
})

scenario('C2', async () => {
  await freeMac()
  const bad = path.join(scratch, 'badhome')
  await rm(bad, { recursive: true, force: true })
  await mkdir(bad, { recursive: true, mode: 0o700 })
  // Only the real user name: the account login never uses the VNC password,
  // so the copy holds no secret, and it is removed when the scenario ends.
  const cfg = JSON.parse(await readFile(path.join(home, 'computer.json'), 'utf8'))
  await writeFile(path.join(bad, 'computer.json'), JSON.stringify({ vncPassword: 'not-the-vnc-password', macosUser: cfg.macosUser, macosPassword: 'wrong-pass' }), { mode: 0o600 })
  console.log('  <scratch>/badhome/computer.json: the real macosUser, macosPassword "wrong-pass", a placeholder vncPassword')
  const env = { MAESTRO_D_HOME: bad }
  try {
    let r = await cli(['attach', '--device', 'mac', '--daemon', 'computerDevice-bad'], env, 'MAESTRO_D_HOME=<scratch>/badhome ')
    assert.notEqual(r.code, 0)
    assert.ok(r.stdout.includes('DEVICE_ERROR') && r.stdout.includes('rejected the account login'), 'want DEVICE_ERROR with the server reason')
    assert.equal(await vncConnections('computerDevice-bad'), 0)
    await chmod(path.join(bad, 'computer.json'), 0o644)
    r = await cli(['attach', '--device', 'mac', '--json', '--daemon', 'computerDevice-bad'], env, 'MAESTRO_D_HOME=<scratch>/badhome ')
    assert.ok(r.stdout.includes('"code": "USAGE"') && r.stdout.includes('readable by others'), 'want the code USAGE for mode 0644')
  } finally {
    await cli(['stop', '--daemon', 'computerDevice-bad'], env, 'MAESTRO_D_HOME=<scratch>/badhome ')
    await rm(bad, { recursive: true, force: true })
  }
})

scenario('C3', async () => {
  const m = await mac()
  const st = await call('device.platformInfo()', async () => await m.platformInfo())
  W = st.screenWidth; H = st.screenHeight
  assert.equal(W, screen.width * screen.scale); assert.equal(H, screen.height * screen.scale)
  const shot = await call('device.screenshot()', async () => await m.screenshot())
  checkQuadrants(await decodeImage(shot, 'c3'))
})

scenario('C4', async () => {
  const m = await mac()
  await m.screenshot() // the first full frame is the slow one
  let t0 = Date.now()
  const jpeg = await call('device.screenshot()', async () => await m.screenshot())
  const jpegMs = Date.now() - t0
  t0 = Date.now()
  const png = await call(`device.screenshot({image: 'png'})`, async () => await m.screenshot({ image: 'png' }))
  const pngMs = Date.now() - t0
  console.log(`  JPEG ${jpegMs} ms, ${jpeg.length} bytes; PNG ${pngMs} ms, ${png.length} bytes`)
  assert.equal(kindOf(jpeg), 'jpeg', 'the computer default is JPEG')
  assert.equal(kindOf(png), 'png')
  for (const [buf, name] of [[jpeg, 'c4.jpg'], [png, 'c4.png']] as const) {
    const img = await decodeImage(buf, name)
    assert.equal(img.w, W); assert.equal(img.h, H)
    checkQuadrants(img)
  }
  assert.ok(jpegMs < 600, `JPEG took ${jpegMs} ms`)
  await writeFile(path.join(scratch, 'screenshot-full.jpg'), jpeg)
})

scenario('C5', async () => {
  const m = await mac()
  let t0 = Date.now()
  const red = await call('device.screenshot({region: {x: 0, y: 0, width: 800, height: 600}})', async () =>
    await m.screenshot({ region: { x: 0, y: 0, width: 800, height: 600 } }))
  const redMs = Date.now() - t0
  allOf(await decodeImage(red, 'c5-red.jpg'), 'red')
  t0 = Date.now()
  const white = await call(`device.screenshot({image: 'png', region: {x: W-800, y: H-600, width: 800, height: 600}})`, async () =>
    await m.screenshot({ image: 'png', region: { x: W - 800, y: H - 600, width: 800, height: 600 } }))
  const whiteMs = Date.now() - t0
  allOf(decodePNG(white), 'white')
  console.log(`  800x600 regions: ${redMs} ms (JPEG), ${whiteMs} ms (PNG)`)
  assert.ok(redMs < 200 && whiteMs < 200, 'a region took 200 ms or more')
  // Straddling the centre, a region shows all four quadrants around the
  // label: visibly that part of the screen and nothing else.
  const centre = await call(`device.screenshot({image: 'png', region: {x: W/2-400, y: H/2-300, width: 800, height: 600}})`, async () =>
    await m.screenshot({ image: 'png', region: { x: W / 2 - 400, y: H / 2 - 300, width: 800, height: 600 } }))
  const ci = decodePNG(centre)
  assert.equal(ci.w, 800); assert.equal(ci.h, 600)
  checkQuadrants(ci)
  await writeFile(path.join(scratch, 'screenshot-region.png'), centre)
  await callFails(`device.screenshot({region: {x: W-10, y: 0, width: 20, height: 20}})`, async () =>
    await m.screenshot({ region: { x: W - 10, y: 0, width: 20, height: 20 } }), 'USAGE', `${W}x${H}`)
  const r = await cli(['get', 'screenshot', '--device', 'mac', '--region', '0,0,800,600', '-o', path.join(scratch, 'r.jpg'), '--daemon', daemonName])
  assert.equal(r.code, 0)
  const cliShot = await readFile(path.join(scratch, 'r.jpg'))
  assert.equal(kindOf(cliShot), 'jpeg')
  allOf(await decodeImage(cliShot, 'c5-cli.jpg'), 'red')
})

scenario('C6', async () => {
  const m = await mac()
  const ev = await watch(async () => await call(`inputText('Hello, World! é\\n')`, async () => await m.inputText('Hello, World! é\n')))
  const typed = ev.filter(e => e.type === 'keyDown').map(e => e.characters).join('')
  assert.equal(typed, 'Hello, World! é\r')
  assert.equal(ev.filter(e => e.type === 'keyUp').length, ev.filter(e => e.type === 'keyDown').length)
})

scenario('C7', async () => {
  const m = await mac()
  let ev = await watch(async () => await call(`pressKey('Meta+Shift+K')`, async () => await m.pressKey('Meta+Shift+K')))
  const k = ev.find(e => e.type === 'keyDown' && (e.charactersIgnoringModifiers ?? '').toLowerCase() === 'k')
  assert.ok(k && k.flags.includes('command') && k.flags.includes('shift'), 'Meta+Shift+K')
  ev = await watch(async () => await call(`pressKey('Control+E')`, async () => await m.pressKey('Control+E')))
  assert.ok(ev.some(e => e.type === 'keyDown' && e.flags.includes('control') && e.flags.length === 1), 'Control+E')
  // Screen Sharing gives VNC clients no Option key: Alt arrives as Command.
  ev = await watch(async () => await callFails(`pressKey('Alt+E')`, async () => await m.pressKey('Alt+E'), 'COMMAND_FAILED', 'no Option key'))
  assert.equal(ev.length, 0, 'Alt+E sent events')
  ev = await watch(async () => await call(`pressKey('Enter')`, async () => await m.pressKey('Enter')))
  assert.ok(ev.some(e => e.type === 'keyDown' && e.keyCode === 36), 'Enter')
  ev = await watch(async () => await call('eraseText({characters: 3})', async () => await m.eraseText({ characters: 3 })))
  assert.equal(ev.filter(e => e.type === 'keyDown' && e.keyCode === 51).length, 3)
  assert.equal((await flagsState()).flags, 0)
})

scenario('C8', async () => {
  const m = await mac()
  let ev = await watch(async () => await call(`tapOn({point: '25%, 25%'})`, async () => await m.tapOn({ point: '25%, 25%' })))
  assert.ok(ev.some(e => e.type === 'leftMouseDown' && nearPoint(e, W / 4, H / 4)), 'tap at 25%,25%')
  ev = await watch(async () => await call('tapOnPoint({x: 2592, y: 1676})', async () => await m.tapOnPoint({ x: 2592, y: 1676 })))
  assert.ok(ev.some(e => e.type === 'leftMouseDown' && nearPoint(e, 2592, 1676)), 'tap at 2592,1676')
  ev = await watch(async () => await call(`moveMouse('50%, 50%')`, async () => await m.moveMouse('50%, 50%')))
  const moved = ev.filter(e => e.type === 'mouseMoved')
  assert.ok(moved.length > 0 && nearPoint(moved[moved.length - 1], W / 2, H / 2), 'moved to the centre')
})

scenario('C9', async () => {
  const m = await mac()
  let ev = await watch(async () => await call(`clickOn({point: '50%, 50%', button: 'right'})`, async () => await m.clickOn({ point: '50%, 50%', button: 'right' })))
  assert.ok(ev.some(e => e.type === 'rightMouseDown') && ev.some(e => e.type === 'rightMouseUp'), 'right click')
  ev = await watch(async () => await call('clickOn({count: 2})', async () => await m.clickOn({ count: 2 })))
  assert.deepEqual(ev.filter(e => e.type === 'leftMouseDown').map(e => e.clickCount), [1, 2], 'double click')
  ev = await watch(async () => await call(`tapOn({point: '40%, 40%', repeat: 2})`, async () => await m.tapOn({ point: '40%, 40%', repeat: 2 })))
  assert.ok(ev.some(e => e.type === 'leftMouseDown' && e.clickCount === 2), 'tap repeat 2 is a double click')
  ev = await watch(async () => await call(`tapOn({point: '60%, 60%', longPress: true})`, async () => await m.tapOn({ point: '60%, 60%', longPress: true })))
  const down = ev.find(e => e.type === 'leftMouseDown'), up = ev.find(e => e.type === 'leftMouseUp')
  assert.ok(down && up && up.t - down.t >= 0.9, 'long press held about 1 s')
  ev = await watch(async () => await call(`swipe({start: '30%, 30%', end: '70%, 70%'})`, async () => await m.swipe({ start: '30%, 30%', end: '70%, 70%' })))
  const iDown = ev.findIndex(e => e.type === 'leftMouseDown'), iUp = ev.findIndex(e => e.type === 'leftMouseUp')
  assert.ok(iDown >= 0 && iUp > iDown && ev.slice(iDown, iUp).some(e => e.type === 'leftMouseDragged'), 'drag')
  ev = await watch(async () => await call(`scrollAt({point: '50%, 50%', y: 3})`, async () => await m.scrollAt({ point: '50%, 50%', y: 3 })))
  const wheel = ev.filter(e => e.type === 'scrollWheel')
  assert.ok(wheel.length > 0 && wheel.some(e => (e.scrollingDeltaY ?? 0) !== 0), 'scrollAt y')
  console.log(`  scrollAt y: 3 gives scrollingDeltaY ${wheel.map(e => e.scrollingDeltaY).join(',')}`)
  ev = await watch(async () => await call('scroll()', async () => await m.scroll()))
  assert.ok(ev.some(e => e.type === 'scrollWheel'), 'scroll')
  const s = await flagsState()
  assert.ok(!s.left && !s.right, 'a button is still down')
})

scenario('C10', async () => {
  const m = await mac()
  await callFails(`tapOn('Login')`, async () => await m.tapOn('Login'), 'COMMAND_FAILED', 'no UI hierarchy; use a point')
  await callFails('hierarchy()', async () => await m.hierarchy(), 'DEVICE_ERROR')
  await callFails(`startRecording('r')`, async () => await m.startRecording('r'), 'COMMAND_FAILED', 'not supported')
  await callFails('stream()', async () => await m.stream(), 'CAPTURE_UNAVAILABLE')
  await callFails('setTrafficShape({lossPct: 100})', async () => await m.setTrafficShape({ lossPct: 100 }), 'UNSUPPORTED')
})

scenario('C11', async () => {
  const m = await mac()
  const shotPath = path.join(scratch, 'c11-shot.png')
  await call(`takeScreenshot('c11-shot')`, async () => await m.takeScreenshot('c11-shot'))
  const files = await call('device.artifacts()', async () => await m.artifacts())
  assert.ok(files.some((f: { path: string }) => f.path.endsWith('-c11-shot.png')), 'the screenshot is not in the report')
  const img = decodePNG(await readFile(shotPath))
  assert.equal(img.w, W); assert.equal(img.h, H)
  await call(`assertScreenshot({path: 'c11-shot.png'})`, async () => await m.assertScreenshot({ path: 'c11-shot.png' }))
})

scenario('C12', async () => {
  await freeMac()
  const m = await mac({ idleTimeoutSec: 20 })
  const t0 = Date.now()
  const events: Array<{ type: string; message?: string; at: number }> = []
  const ac = new AbortController()
  const watching = (async () => {
    try {
      for await (const e of maestro.events({ device: 'mac', signal: ac.signal })) {
        events.push({ type: e.type, message: e.message, at: (Date.now() - t0) / 1000 })
      }
    } catch { /* aborted */ }
  })()
  await sleep(10_000)
  await call('device.screenshot() at t=10 s', async () => await m.screenshot())
  await sleep(35_000)
  // The stream replays the daemon's history first; the idle detach is the
  // one event that carries the message "idle".
  const detached = events.find(e => e.type === 'device.detached' && e.message === 'idle')
  console.log(`  device events: ${JSON.stringify(events.filter(e => e.type.startsWith('device.')))}`)
  assert.ok(detached, 'no device.detached event with message idle')
  assert.ok(detached!.at >= 30 && detached!.at <= 41, `detached at t=${detached!.at} s; want 30-41 s`)
  await callFails('device.screenshot() at t=45 s', async () => await m.screenshot(), 'DEVICE_NOT_ATTACHED')
  assert.equal(await vncConnections(daemonName), 0)
  ac.abort()
  await watching
  handle = null
})

scenario('C13', async () => {
  await freeMac()
  const logPath = path.join(home, 'run', 'computerDevice-cli', 'daemon.log')
  const logStart = (await readFile(logPath, 'utf8').catch(() => '')).length // the daemon keeps its log across runs
  const cliClient = await client.MaestroD.connect({ name: 'computerDevice-cli', bin })
  await call(`connect({name: 'computerDevice-cli'}).attach({device: 'mac', extra: {idleTimeoutSec: 20}})`, async () =>
    await cliClient.attach({ device: 'mac', extra: { idleTimeoutSec: 20 } }))
  const tap = async (): Promise<CLIResult> => await cli(['tapOnPoint', '--device', 'mac', '--platform', 'macos', '--x', '100', '--y', '100', '--daemon', 'computerDevice-cli'])
  const clicked = (ev: ObsEvent[]): boolean => ev.some(e => e.type === 'leftMouseDown' && nearPoint(e, 100, 100)) && ev.some(e => e.type === 'leftMouseUp')
  let r!: CLIResult
  assert.ok(clicked(await watch(async () => { r = await tap() })) && r.code === 0, 'the first one-shot click did not arrive')
  say('waiting 35 s, past the 20 s timeout and the 10 s sweep')
  await sleep(35_000)
  // This one re-attaches on its own; its click must still arrive.
  assert.ok(clicked(await watch(async () => { r = await tap() })) && r.code === 0, 'the click after the re-attach did not arrive')
  const log = (await readFile(logPath, 'utf8')).slice(logStart)
  const attaches = log.split('\n').filter(l => l.includes('Attached device mac'))
  const idle = log.split('\n').filter(l => l.includes('Detaching mac: no call'))
  console.log(`  daemon log: ${attaches.length} attaches, ${idle.length} idle detach`)
  assert.equal(attaches.length, 2); assert.equal(idle.length, 1)
  await cli(['stop', '--daemon', 'computerDevice-cli'])
})

scenario('C14', async () => {
  await freeMac()
  const tokens = path.join(scratch, 'hubs')
  await mkdir(tokens, { recursive: true })
  for (const f of ['token', 'peer']) {
    await writeFile(path.join(tokens, f), `${Math.random().toString(36).slice(2)}${Date.now()}\n`, { mode: 0o600 })
  }
  const homeA = home
  const homeB = path.join(scratch, 'hub-b-home')
  const configA = path.join(tokens, 'config.json')
  const mock = 'mock-computer-control'
  await writeFile(configA, JSON.stringify({ hubDevices: [{ id: 'mac' }, { id: mock }] }), { mode: 0o600 })
  const localHub = (hubHome: string, args: string[]) => cli(args, {
    MAESTRO_D_HOME: hubHome, MAESTRO_D_MOCK_DEVICES: hubHome === homeA ? mock : ''
  }, undefined, hubBin)
  const hub = (name: string, port: number) => ['start', '--web-port', '0', '--name', name, '--listen', `127.0.0.1:${port}`, '--no-mdns',
    '--token-file', path.join(tokens, 'token'), '--peer-token-file', path.join(tokens, 'peer')]
  const a = await localHub(homeA, [...hub('computerdevice-a', hubPortA), '--config', configA])
  assert.equal(a.code, 0, a.stdout)
  let startedB = false
  try {
    const b = await localHub(homeB, [...hub('computerdevice-b', hubPortB), '--peer', `127.0.0.1:${hubPortA}`])
    assert.equal(b.code, 0, b.stdout)
    startedB = true
    for (let i = 0; i < 20; i++) {
      const st = await localHub(homeA, ['status'])
      if (st.stdout.includes('computerdevice-b')) break
      await sleep(500)
    }
    const local = await client.MaestroD.connect({ hubSocket: true })
    try {
      const computer = await local.lease({ device: 'mac' })
      const screenshot = await computer.screenshot({ region: { x: 0, y: 0, width: 400, height: 300 } })
      await writeFile(path.join(scratch, 'c14-under-lease.jpg'), screenshot)
      allOf(await decodeImage(screenshot, 'c14.jpg'), 'red')
      await computer.release()
      try {
        const selected = await local.lease({ select: { osVersion: '26' }, attach: false })
        assert.ok(!selected.id.startsWith('mac@'), 'the OS 26 lease got mac')
        await selected.release()
      } catch (err) {
        if (!client.isMaestroError(err, 'DEVICE_NOT_FOUND')) throw err
      }
    } finally {
      await local.close()
    }
    // Hub B over REST with the client token (the CLI's --target-hub does
    // not send MAESTRO_D_TOKEN to a plain http:// hub on origin/master
    // either; that is outside this test).
    const token = (await readFile(path.join(tokens, 'token'), 'utf8')).trim()
    const rest = async (method: string, p: string, body?: unknown): Promise<{ status: number; text: string }> => {
      show(`curl -X ${method} http://127.0.0.1:${hubPortB}${p} -H 'Authorization: Bearer <token>'${body ? ` -d '${JSON.stringify(body)}'` : ''}`)
      const res = await fetch(`http://127.0.0.1:${hubPortB}${p}`, {
        method, headers: { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json', 'X-Maestro-Api-Version': '1' },
        body: body ? JSON.stringify(body) : undefined
      })
      const text = await res.text()
      console.log(`  ${res.status} ${text.slice(0, 300)}`)
      return { status: res.status, text }
    }
    const list = await rest('GET', '/v1/devices')
    assert.equal(list.status, 200)
    assert.ok(list.text.includes(`${mock}@computerdevice-a`) && !list.text.includes('"mac@'), 'hub B lists the computer, or not hub A at all')
    const lease = await rest('POST', '/v1/leases', { device: 'mac@computerdevice-a' })
    assert.ok(lease.status >= 400 && lease.text.includes('DEVICE_NOT_FOUND'), 'hub B leased the computer')
  } finally {
    if (startedB) await localHub(homeB, ['stop'])
    await localHub(homeA, ['stop'])
  }
})

scenario('C15', async () => {
  await mac()
  const r = await cli(['attach', '--device', 'mac', '--daemon', 'computerDevice-other'])
  assert.ok(r.code !== 0 && r.stdout.includes('DEVICE_IN_USE') && r.stdout.includes(daemonName), 'want DEVICE_IN_USE naming the holder')
  await cli(['stop', '--daemon', 'computerDevice-other'])
})

scenario('C16', async () => {
  await mac()
  await call(`detach('mac')`, async () => await maestro.detach('mac'))
  handle = null
  const s = await flagsState()
  assert.ok(s.flags === 0 && !s.left && !s.right, 'something is still held')
  assert.equal(await vncConnections(daemonName), 0)
  const r = await cli(['device', 'list', '--json', '--platform', 'macos', '--daemon', daemonName])
  assert.ok(r.stdout.includes('"ready"'), 'mac not ready again')
  // Neither password anywhere the daemons keep files, or in this run's logs.
  const cfg = JSON.parse(await readFile(path.join(home, 'computer.json'), 'utf8'))
  let vnc = 0, macos = 0
  const scan = async (dir: string): Promise<void> => {
    for (const ent of await readdir(dir, { withFileTypes: true }).catch(() => [])) {
      const p = path.join(dir, ent.name)
      if (ent.isDirectory()) await scan(p)
      else if (ent.isFile() && p !== path.join(home, 'computer.json')) {
        const st = await stat(p)
        if (st.size > 64 << 20) continue
        const data = await readFile(p)
        if (data.includes(cfg.vncPassword)) vnc++
        if (data.includes(cfg.macosPassword)) macos++
      }
    }
  }
  show('scan $MAESTRO_D_HOME (except computer.json) and the scratch directory for each password')
  await scan(home)
  await scan(scratch)
  console.log(`  files holding the VNC password: ${vnc}; the macOS password: ${macos}`)
  assert.equal(vnc + macos, 0)
})

scenario('C17', async () => {
  await freeMac()
  await sh('sh', ['-c', 'ioreg -n Root -d1 | grep -o \'"CGSSessionScreenIsLocked"=[A-Za-z]*\' || echo "CGSSessionScreenIsLocked absent"'])
  const ev = await watch(async () => await mac())
  const keys = ev.filter(e => e.type === 'keyDown' || e.type === 'keyUp')
  const flags = ev.filter(e => e.type === 'flagsChanged')
  assert.equal(keys.length, 0, 'attach typed keys on the desktop')
  assert.ok(flags.length === 2 && flags[0].flags.includes('shift') && flags[1].flags.length === 0, 'want exactly one Shift press and release')
  const log = await readFile(path.join(home, 'run', daemonName, 'daemon.log'), 'utf8')
  const checks = log.split('\n').filter(l => l.includes('screen check'))
  console.log(`  daemon log: ${checks[checks.length - 1]}`)
  assert.ok(checks[checks.length - 1]?.endsWith('screen check: desktop'))
})

scenario('C18', async () => {
  // Input sent right after an attach arrives: Screen Sharing drops key
  // presses and clicks for about 3 s after a login, and attach waits that
  // out. A one-shot on a detached computer attaches and types at once.
  await freeMac()
  let ev = await watch(async () => await cli(['inputText', 'xyz', '--device', 'mac', '--platform', 'macos', '--daemon', 'computerDevice-cli']))
  assert.equal(ev.filter(e => e.type === 'keyDown').map(e => e.characters).join(''), 'xyz', 'the one-shot inputText lost key presses')
  await cli(['stop', '--daemon', 'computerDevice-cli'])
  const m = await mac()
  ev = await watch(async () => await call(`tapOn({point: '30%, 30%'}) right after attach`, async () => await m.tapOn({ point: '30%, 30%' })))
  assert.ok(ev.some(e => e.type === 'leftMouseDown' && nearPoint(e, 0.3 * W, 0.3 * H)), 'the first click after attach did not arrive')
})

// --- main --------------------------------------------------------------------

async function main(): Promise<void> {
  await mkdir(scratch, { recursive: true })
  // The client sends its working directory with each call; flow paths
  // (takeScreenshot, assertScreenshot) resolve there, so this run's files
  // land in scratch and never in the checkout.
  process.chdir(scratch)
  say(`daemon ${daemonName}, home ${home}, scratch ${scratch}`)
  client = (await import(pathToFileURL(path.join(clientRoot, 'dist', 'esm', 'index.js')).href)) as Client
  process.env.MAESTRO_D_HOME = home
  // A daemon left from an earlier run may hold an older binary.
  for (const name of [daemonName, 'computerDevice-cli', 'computerDevice-other']) await cli(['stop', '--daemon', name])
  maestro = await client.MaestroD.connect({ name: daemonName, bin })
  const caffeinate = spawn('caffeinate', ['-d', '-u'], { stdio: 'ignore' })
  let failures = 0
  try {
    await startObserver()
    for (const [id, body] of scenarios) {
      if (only && !only.has(id)) continue
      console.log(`\n=== ${id}`)
      const t0 = Date.now()
      try {
        await body()
        say(`PASS ${id} (${Date.now() - t0} ms)`)
      } catch (error) {
        failures++
        say(`FAIL ${id}: ${error instanceof Error ? error.message : String(error)}`)
      }
    }
  } finally {
    await cli(['detach', '--device', 'mac', '--daemon', daemonName])
    observer?.kill()
    caffeinate.kill()
  }
  say(failures === 0 ? 'all scenarios passed' : `${failures} scenario(s) failed`)
  process.exit(failures === 0 ? 0 : 1)
}

await main()
