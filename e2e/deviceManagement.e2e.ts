// Device management end to end: create, boot, use, stop and delete an iOS
// simulator and an Android AVD through this repository's JavaScript client
// (npm/maestro-d) — catalog(), createDevice(), startDevice() without a
// platform, deviceDelete() — on the machine the daemon runs on.
//
// Like iosLandscape.e2e.ts the script stands alone: Node strips its types
// (Node 22.18 or later runs a .ts file as is; older 22.x needs
// --experimental-strip-types), or run it with `npx tsx`. Build the binary
// and the client first; `make build` installs the binary as
// ~/.maestro-d/bin/maestro-d:
//
//   make build && (cd npm/maestro-d && npm install && npm run build)
//
//   MAESTRO_D_SIM_NAME=<name of the device to create; must not exist> \
//   [MAESTRO_D_PLATFORMS=ios,android (default both, in that order)] \
//   [MAESTRO_D_IOS_RUNTIME=<e.g. iOS-26-5, default the newest>] \
//   [MAESTRO_D_IOS_TYPE=<e.g. "iPhone 16", default the newest iPhone>] \
//   [MAESTRO_D_ANDROID_IMAGE=<system-images;…, default the newest google_apis image for this host>] \
//   [MAESTRO_D_ANDROID_PROFILE=<default pixel_8>] [MAESTRO_D_DATA_SIZE=<default 6G>] \
//   [MAESTRO_D=<daemon name>] [MAESTRO_D_BIN=<binary, else found on PATH>] \
//   [MAESTRO_D_CLIENT_ROOT=<client package, default npm/maestro-d>] \
//     node e2e/deviceManagement.e2e.ts
//
// Per platform: the catalog lists something to create from; createDevice
// with boot: false makes the device, which then lists shut down and
// created by maestro-d; startDevice by its id with no platform boots it;
// attach, launchApp Settings, and assert its first screen; detach;
// stopDevice; deleteDevice; and the device is gone from the listing. Each
// step is checked and the script exits 1 at the first failure. A device it
// created is force-deleted on the way out if a step failed after creating
// it, so a failed run leaves nothing behind.
import assert from 'node:assert/strict'
import path from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'

import type {
  CreateDeviceOptions,
  DeviceInfo,
  MaestroD
} from '../npm/maestro-d/dist/types/index.js'

interface Client {
  MaestroD: { connect: (options: { name?: string }) => Promise<MaestroD> }
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

const simName = required('MAESTRO_D_SIM_NAME')
const platforms = (optional('MAESTRO_D_PLATFORMS') ?? 'ios,android')
  .split(',')
  .map(p => p.trim())
  .filter(p => p !== '')
for (const p of platforms) {
  if (p !== 'ios' && p !== 'android') throw new Error(`MAESTRO_D_PLATFORMS: unknown platform ${p}`)
}
const daemonName = optional('MAESTRO_D') ?? 'device-management-e2e'
const clientRoot =
  optional('MAESTRO_D_CLIENT_ROOT') ?? path.join(repoRoot, 'npm', 'maestro-d')

const started = Date.now()
function say(line: string): void {
  const seconds = ((Date.now() - started) / 1000).toFixed(1)
  console.log(`+${seconds.padStart(6)}s ${line}`)
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

interface Platform {
  create: CreateDeviceOptions
  appId: string
  /** Text on the app's first screen. */
  firstScreen: string
}

function platformOptions(platform: string): Platform {
  if (platform === 'ios') {
    const create: CreateDeviceOptions = { platform, name: simName, boot: false }
    const runtime = optional('MAESTRO_D_IOS_RUNTIME')
    const type = optional('MAESTRO_D_IOS_TYPE')
    if (runtime != null) create.runtime = runtime
    if (type != null) create.type = type
    return { create, appId: 'com.apple.Preferences', firstScreen: 'General' }
  }
  const create: CreateDeviceOptions = { platform, name: simName, boot: false }
  const image = optional('MAESTRO_D_ANDROID_IMAGE')
  const profile = optional('MAESTRO_D_ANDROID_PROFILE')
  const dataSize = optional('MAESTRO_D_DATA_SIZE')
  if (image != null) create.image = image
  if (profile != null) create.profile = profile
  if (dataSize != null) create.dataSize = dataSize
  return {
    create,
    appId: 'com.android.settings',
    firstScreen: '(Network & internet|Search [Ss]ettings)'
  }
}

async function listed(md: MaestroD, platform: string): Promise<DeviceInfo | undefined> {
  return (await md.devices({ platform })).find(
    d => d.name === simName || d.id === `avd:${simName}`
  )
}

async function lifecycle(md: MaestroD, platform: string): Promise<void> {
  const opts = platformOptions(platform)
  say(`--- ${platform}: ${simName}`)
  await step(`${platform}: catalog() lists something to create from`, async () => {
    const cat = await md.catalog({ platform })
    if (platform === 'ios') {
      assert.ok((cat.ios?.runtimes.length ?? 0) > 0, 'no iOS runtime')
      assert.ok((cat.ios?.deviceTypes ?? []).some(t => t.productFamily === 'iPhone'), 'no iPhone type')
      say(`  runtimes: ${(cat.ios?.runtimes ?? []).map(r => r.name).join(', ')}`)
    } else {
      assert.ok((cat.android?.images ?? []).some(i => i.installed), 'no installed system image')
      assert.ok((cat.android?.profiles ?? []).includes('pixel_8'), 'no pixel_8 profile')
      say(`  images: ${(cat.android?.images ?? []).map(i => i.id).join(', ')}`)
    }
  })
  assert.equal(await listed(md, platform), undefined, `${simName} already exists`)

  let created: string | undefined
  let deleted = false
  try {
    const dev = await step(`${platform}: createDevice(${JSON.stringify(opts.create)})`, async () => {
      const d = await md.createDevice(opts.create)
      assert.equal(d.createdBy, 'maestro-d')
      assert.equal(d.ready, false)
      if (platform === 'android') assert.equal(d.id, `avd:${simName}`)
      say(`  created ${d.id}`)
      return d
    })
    created = dev.id
    await step(`${platform}: devices() lists it shut down, created by maestro-d`, async () => {
      const d = await listed(md, platform)
      assert.ok(d, 'not listed')
      assert.equal(d.id, dev.id)
      assert.equal(d.state, 'Shutdown')
      assert.equal(d.createdBy, 'maestro-d')
    })
    const booted = await step(`${platform}: startDevice({name: '${dev.id}'}) with no platform`, async () => {
      const d = await md.startDevice({ name: dev.id, bootTimeout: 300 })
      assert.equal(d.platform, platform)
      assert.equal(d.ready, true)
      say(`  booted as ${d.id}${d.osVersion ? ` (${d.osVersion})` : ''}`)
      return d
    })
    if (platform === 'android') {
      await step('android: the running emulator carries its AVD name, listed once', async () => {
        const all = (await md.devices({ platform })).filter(
          d => d.name === simName || d.id === `avd:${simName}`
        )
        assert.equal(all.length, 1, JSON.stringify(all))
        assert.equal(all[0]?.id, booted.id)
      })
    }
    const m = await step(`${platform}: attach and launchApp ${opts.appId}`, async () => {
      const device = await md.attach({ device: booted.id, platform, appId: opts.appId })
      await device.launchApp(opts.appId)
      return device
    })
    await step(`${platform}: assertVisible ${opts.firstScreen}`, async () => {
      await m.assertVisible({ text: opts.firstScreen, timeout: 20_000 })
    })
    await step(`${platform}: detach`, async () => {
      await m.detach()
    })
    await step(`${platform}: stopDevice(${booted.id})`, async () => {
      await md.stopDevice(booted.id)
    })
    await step(`${platform}: deleteDevice(${dev.id})`, async () => {
      const d = await md.deleteDevice(dev.id)
      assert.equal(d.name, simName)
      deleted = true
    })
    await step(`${platform}: the device is gone`, async () => {
      assert.equal(await listed(md, platform), undefined)
    })
  } finally {
    if (created != null && !deleted) {
      say(`cleanup: force-deleting ${created}`)
      await md.deleteDevice(created, { force: true }).catch((error: unknown) => {
        say(`cleanup failed: ${error instanceof Error ? error.message : String(error)}`)
      })
    }
  }
}

async function main(): Promise<void> {
  const client = (await import(
    pathToFileURL(path.join(clientRoot, 'dist', 'esm', 'index.js')).href
  )) as Client
  const md = await client.MaestroD.connect({ name: daemonName })
  say(`daemon ${daemonName} (pid ${md.info?.pid ?? '?'}); platforms ${platforms.join(', ')}`)
  try {
    for (const platform of platforms) await lifecycle(md, platform)
  } finally {
    md.close()
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
