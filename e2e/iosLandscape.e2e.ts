// iOS landscape end to end: an app whose interface is landscape, driven
// through this repository's JavaScript client (npm/maestro-d).
//
// The repository has no TypeScript test setup outside npm/maestro-d, so the
// script stands alone: Node strips its types (Node 22.18 or later runs a .ts
// file as is; older 22.x needs --experimental-strip-types), or run it with
// `npx tsx`. Build the binary and the client first; `make build` installs the
// binary as ~/.maestro-d/bin/maestro-d:
//
//   make build && (cd npm/maestro-d && npm install && npm run build)
//
//   MAESTRO_D_DEVICE=<device id> MAESTRO_D_APP=<bundle id> \
//   [MAESTRO_D_TEAM_ID=<WebDriverAgent signing team, for a physical device>] \
//   [MAESTRO_D=<daemon name>] [MAESTRO_D_BIN=<binary, else found on PATH>] \
//   [MAESTRO_D_CLIENT_ROOT=<client package, default npm/maestro-d>] \
//   [MAESTRO_D_ELEMENT_ID=<accessibility id of the landscape element>] \
//   [MAESTRO_D_RESET_ID=<id of an element whose tap undoes the element's>] \
//   [MAESTRO_D_EFFECT_ID=<id of the element the tap changes>] \
//   [MAESTRO_D_EFFECT_TEXT=<that element's text after the tap>] \
//   [MAESTRO_D_ARTIFACT_DIR=<where screenshots go, default a temp dir>] \
//     node e2e/iosLandscape.e2e.ts
//
// Without MAESTRO_D_ELEMENT_ID the script picks the first element with an id
// whose whole frame lies right of the portrait width. With RESET, EFFECT_ID
// and EFFECT_TEXT, every tap on the element is preceded by a tap on RESET and
// must turn EFFECT_ID's text into EFFECT_TEXT in the hierarchy; without them
// a tap only has to succeed. For example, a camera app with zoom presets:
// ELEMENT_ID=zoom-preset-3x RESET_ID=zoom-preset-1x EFFECT_ID=zoom-label
// EFFECT_TEXT=3x.
//
// Steps: attach; launchApp; setOrientation LANDSCAPE_LEFT; the window must
// read landscape; assertVisible the element; tapOn it by selector, then by a
// percentage point at its centre; swipe across the long axis; takeScreenshot
// with cropOn the element, which must come back the element's size;
// setOrientation LANDSCAPE_RIGHT and both taps again. The last RESET tap
// leaves the app as it was. Each step is checked and the script exits 1 at
// the first failure.
import assert from 'node:assert/strict'
import { mkdir, mkdtemp, readFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import path from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'

import type {
  Bounds,
  HierarchyNode,
  MaestroDevice
} from '../npm/maestro-d/dist/types/index.js'

interface Client {
  MaestroD: {
    attach: (options: {
      name?: string
      device: string
      platform: 'ios'
      appId: string
      teamId?: string
    }) => Promise<MaestroDevice>
  }
}

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
const daemonName = optional('MAESTRO_D') ?? 'ios-landscape-e2e'
const clientRoot =
  optional('MAESTRO_D_CLIENT_ROOT') ?? path.join(repoRoot, 'npm', 'maestro-d')
const elementId = optional('MAESTRO_D_ELEMENT_ID')
const resetId = optional('MAESTRO_D_RESET_ID')
const effectId = optional('MAESTRO_D_EFFECT_ID')
const effectText = optional('MAESTRO_D_EFFECT_TEXT')
if ((effectId == null) !== (effectText == null)) {
  throw new Error('MAESTRO_D_EFFECT_ID and MAESTRO_D_EFFECT_TEXT go together')
}
if (effectId != null && resetId == null) {
  throw new Error('MAESTRO_D_EFFECT_ID needs MAESTRO_D_RESET_ID')
}

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

function flatten(node: HierarchyNode): HierarchyNode[] {
  return [node, ...(node.children ?? []).flatMap(flatten)]
}
function describe(bounds: Bounds): string {
  return `(${bounds.x},${bounds.y} ${bounds.width}x${bounds.height})`
}

interface Window {
  width: number
  height: number
}
/**
 * The window in the app's interface orientation: the largest frame at the
 * origin in WebDriverAgent's page source, which follows the rotation. The
 * driver's platform info is logged beside it.
 */
async function windowSize(m: MaestroDevice): Promise<Window> {
  let window: Window | undefined
  for (const node of flatten(await m.hierarchy())) {
    const b = node.bounds
    if (b == null || b.x !== 0 || b.y !== 0) continue
    if (window == null || b.width * b.height > window.width * window.height) {
      window = { width: b.width, height: b.height }
    }
  }
  assert(window != null && window.width > 0, 'the hierarchy has no window frame')
  const info = await m.platformInfo()
  say(
    `     window ${window.width}x${window.height} pt; platform info ${String(info.screenWidth)}x${String(info.screenHeight)}`
  )
  return window
}

/** The PNG's pixel size, from its IHDR chunk. */
function pngSize(png: Buffer): Window {
  assert(png.length > 24, 'the screenshot is too short to be a PNG')
  assert.equal(
    png.subarray(1, 4).toString('latin1'),
    'PNG',
    'the screenshot is not a PNG'
  )
  return { width: png.readUInt32BE(16), height: png.readUInt32BE(20) }
}

async function findElement(
  m: MaestroDevice,
  window: Window
): Promise<HierarchyNode & { id: string; bounds: Bounds }> {
  const portraitWidth = Math.min(window.width, window.height)
  const nodes = flatten(await m.hierarchy())
  const inWindow = (b: Bounds): boolean =>
    b.width > 0 &&
    b.height > 0 &&
    b.x + b.width <= window.width &&
    b.y + b.height <= window.height
  const match = nodes.find(
    node =>
      node.id != null &&
      node.bounds != null &&
      (elementId == null
        ? node.bounds.x >= portraitWidth && inWindow(node.bounds)
        : node.id === elementId)
  )
  assert(
    match?.id != null && match.bounds != null,
    elementId == null
      ? `no element with an id lies right of the portrait width ${portraitWidth}`
      : `no element with id ${elementId} in the hierarchy`
  )
  const bounds = match.bounds
  assert(
    bounds.x >= portraitWidth,
    `${match.id} ${describe(bounds)} does not lie right of the portrait width ${portraitWidth}`
  )
  assert(
    inWindow(bounds),
    `${match.id} ${describe(bounds)} is not inside the ${window.width}x${window.height} window`
  )
  return { ...match, id: match.id, bounds }
}

/** Waits up to 5 s for `id` to read `text` (or anything else, when `not`). */
async function waitForText(
  m: MaestroDevice,
  id: string,
  text: string,
  not = false
): Promise<string> {
  let seen = '(absent)'
  for (const until = Date.now() + 5000; ; ) {
    const node = flatten(await m.hierarchy()).find(n => n.id === id)
    seen = node?.text ?? '(absent)'
    if (node != null && (node.text === text) !== not) return seen
    if (Date.now() > until) break
    await sleep(250)
  }
  throw new Error(
    `${id} reads "${seen}", expected ${not ? 'anything but ' : ''}"${text}"`
  )
}

/**
 * Taps the element one way, after a reset when one is configured, and checks
 * the tap's effect in the hierarchy.
 */
async function tapAndCheck(
  m: MaestroDevice,
  label: string,
  tap: () => Promise<unknown>
): Promise<void> {
  if (resetId != null) {
    await step(`${label}: reset with tapOn id=${resetId}`, async () => {
      await m.tapOn({ id: resetId, timeout: 5000 })
      if (effectId != null && effectText != null) {
        const text = await waitForText(m, effectId, effectText, true)
        say(`     ${effectId} reads "${text}"`)
      }
    })
  }
  await step(label, async () => {
    await tap()
    if (effectId != null && effectText != null) {
      const text = await waitForText(m, effectId, effectText)
      say(`     ${effectId} reads "${text}"`)
    }
  })
}

function percentPoint(bounds: Bounds, window: Window): string {
  const x = ((bounds.x + bounds.width / 2) / window.width) * 100
  const y = ((bounds.y + bounds.height / 2) / window.height) * 100
  return `${x.toFixed(1)}%, ${y.toFixed(1)}%`
}

async function landscape(
  m: MaestroDevice,
  orientation: 'LANDSCAPE_LEFT' | 'LANDSCAPE_RIGHT'
): Promise<Window> {
  await step(`setOrientation ${orientation}`, async () => {
    await m.setOrientation({ orientation })
  })
  return await step('the window reads landscape', async () => {
    const window = await windowSize(m)
    assert(
      window.width > window.height,
      `the window is ${window.width}x${window.height}, not landscape`
    )
    return window
  })
}

async function bothTaps(
  m: MaestroDevice,
  orientation: string,
  window: Window
): Promise<HierarchyNode & { id: string; bounds: Bounds }> {
  const element = await step(
    `${orientation}: an element right of the portrait width`,
    async () => {
      const found = await findElement(m, window)
      say(`     ${found.type ?? 'element'} id=${found.id} ${describe(found.bounds)}`)
      return found
    }
  )
  await step(`${orientation}: assertVisible id=${element.id}`, async () => {
    await m.assertVisible({ id: element.id, timeout: 5000 })
  })
  await tapAndCheck(m, `${orientation}: tapOn id=${element.id}`, async () => {
    await m.tapOn({ id: element.id, timeout: 5000 })
  })
  const point = percentPoint(element.bounds, window)
  await tapAndCheck(m, `${orientation}: tapOn point '${point}'`, async () => {
    await m.tapOn({ point })
  })
  return element
}

async function main(): Promise<void> {
  const artifacts =
    optional('MAESTRO_D_ARTIFACT_DIR') ??
    (await mkdtemp(path.join(tmpdir(), 'ios-landscape-')))
  await mkdir(artifacts, { recursive: true })
  say(`app ${appId}, daemon ${daemonName}, artifacts in ${artifacts}`)

  const client = (await import(
    pathToFileURL(path.join(clientRoot, 'dist', 'esm', 'index.js')).href
  )) as Client
  const m = await step('attach', async () =>
    await client.MaestroD.attach({
      name: daemonName,
      device,
      platform: 'ios',
      appId,
      ...(teamId == null ? {} : { teamId })
    })
  )
  try {
    await step(`launchApp ${appId}`, async () => {
      await m.launchApp({ appId })
    })

    const left = await landscape(m, 'LANDSCAPE_LEFT')
    const element = await bothTaps(m, 'LANDSCAPE_LEFT', left)

    await step("swipe '10%, 50%' -> '90%, 50%' across the long axis", async () => {
      await m.swipe({ start: '10%, 50%', end: '90%, 50%', duration: 600 })
    })

    await step(`takeScreenshot cropOn id=${element.id}`, async () => {
      const full = pngSize(await m.screenshot())
      // The framebuffer is portrait whatever the interface orientation.
      const scale =
        Math.max(full.width, full.height) / Math.max(left.width, left.height)
      const file = path.join(artifacts, `crop-${element.id}.png`)
      await m.takeScreenshot({ path: file, cropOn: { id: element.id } })
      const crop = pngSize(await readFile(file))
      say(
        `     crop ${crop.width}x${crop.height} px of a ${full.width}x${full.height} px screen; ` +
          `the element is ${element.bounds.width}x${element.bounds.height} pt at ${scale.toFixed(2)} px/pt`
      )
      const slack = Math.max(4, Math.ceil(scale * 2))
      for (const [axis, got, want] of [
        ['width', crop.width, element.bounds.width * scale],
        ['height', crop.height, element.bounds.height * scale]
      ] as const) {
        assert(
          Math.abs(got - want) <= slack,
          `the crop's ${axis} is ${got} px, the element's ${Math.round(want)} px`
        )
      }
    })

    const right = await landscape(m, 'LANDSCAPE_RIGHT')
    await bothTaps(m, 'LANDSCAPE_RIGHT', right)
    await m.takeScreenshot({ path: path.join(artifacts, 'landscape-right.png') })

    if (resetId != null) {
      await step(`leave the app as found: tapOn id=${resetId}`, async () => {
        await m.tapOn({ id: resetId, timeout: 5000 })
      })
    }
  } finally {
    await m.detach().catch((error: unknown) => {
      say(`detach failed: ${error instanceof Error ? error.message : String(error)}`)
    })
  }
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
