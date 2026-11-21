// Traffic shaping end to end on real devices, driven through this
// repository's JavaScript client (npm/maestro-d): setTrafficShape and
// resetTrafficShape send the calls the host's network.json configures for
// each device, and the effect is measured with ping from this host. The
// script never names a shaper; the host's config does.
//
// The script stands alone, as the other scripts here do: Node strips its
// types (Node 22.18 or later runs a .ts file as is), or run it with
// `npx tsx`. Build the binary into the worktree's own install and the
// client first:
//
//   make build INSTALL_DIR=$PWD/.home && (cd npm/maestro-d && npm install && npm run build)
//
//   MAESTRO_RUNNER_HOME=$PWD/.home MAESTRO_D_HOME=$PWD/.home/dhome \
//   MAESTRO_D_BIN=$PWD/.home/bin/maestro-d \
//   [MAESTRO_D_IOS=<iPhone UDID> MAESTRO_D_IOS_ADDR=<its IPv4 address> MAESTRO_D_TEAM_ID=<WDA signing team>] \
//   [MAESTRO_D_ANDROID=<adb serial> MAESTRO_D_ANDROID_ADDR=<its IPv4 address>] \
//   [MAESTRO_D=<daemon name>] [MAESTRO_D_CLIENT_ROOT=<client package, default npm/maestro-d>] \
//     node e2e/trafficShape.e2e.ts
//
// $MAESTRO_D_HOME/network.json (mode 0600) must configure both calls for
// each device named, and this host must reach each address (the shaper
// must see that traffic). Per device:
//   0. Attach; a baseline ping (the lowest of 20 round trips).
//   1. setTrafficShape {lossPct: 100}: no ping answers, and takeScreenshot
//      still succeeds (the driver is on USB).
//   2. setTrafficShape {latencyMs: 100}: ping rises by about 200 ms.
//   3. resetTrafficShape: ping back near its baseline.
//   4. setTrafficShape {latencyMs: 100, direction: download}: ping rises by
//      about 100 ms over the lower of the baseline and step 3's reading
//      (one direction only; it follows a reset).
//   5. iOS: setAirplaneMode answers UNSUPPORTED. Android: setAirplaneMode
//      enabled.
//   6. setTrafficShape {latencyMs: 100}, then detach: the session's end
//      resets the shape (and on Android turns airplane mode off), so ping is
//      back near its baseline; on Android airplane_mode_on reads 0, and the
//      ping goes to the address the phone rejoined Wi-Fi with (DHCP may
//      hand out another one after airplane mode).
// Each step is checked and the script exits 1 at the first failure.
import assert from 'node:assert/strict'
import { execFile } from 'node:child_process'
import path from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'
import { promisify } from 'node:util'

import type { MaestroDevice } from '../npm/maestro-d/dist/types/index.js'

interface Client {
  MaestroD: {
    attach: (options: { name?: string; device: string; appId: string; teamId?: string }) => Promise<MaestroDevice>
  }
  isMaestroError: (err: unknown, code?: string) => boolean
}

const run = promisify(execFile)
const repoRoot = path.dirname(path.dirname(fileURLToPath(import.meta.url)))

function optional(key: string): string | undefined {
  const value = process.env[key]
  return value == null || value === '' ? undefined : value
}

interface Target {
  platform: 'ios' | 'android'
  device: string
  addr: string
  appId: string
}

const targets: Target[] = []
for (const [platform, appId] of [['ios', 'com.apple.Preferences'], ['android', 'com.android.settings']] as const) {
  const device = optional(`MAESTRO_D_${platform.toUpperCase()}`)
  if (device == null) continue
  const addr = optional(`MAESTRO_D_${platform.toUpperCase()}_ADDR`)
  if (addr == null) throw new Error(`MAESTRO_D_${platform.toUpperCase()}_ADDR is required with MAESTRO_D_${platform.toUpperCase()}`)
  targets.push({ platform, device, addr, appId })
}
if (targets.length === 0) throw new Error('set MAESTRO_D_IOS and/or MAESTRO_D_ANDROID')
const teamId = optional('MAESTRO_D_TEAM_ID')
const daemonName = optional('MAESTRO_D') ?? 'trafficShape-e2e'
const clientRoot = optional('MAESTRO_D_CLIENT_ROOT') ?? path.join(repoRoot, 'npm', 'maestro-d')

const started = Date.now()
function say(line: string): void {
  const seconds = ((Date.now() - started) / 1000).toFixed(1)
  console.log(`+${seconds.padStart(6)}s ${line}`)
}
async function sleep(ms: number): Promise<void> {
  await new Promise(resolve => setTimeout(resolve, ms))
}

let passes = 0
let failures = 0
async function step<T>(name: string, body: () => Promise<T>): Promise<T> {
  const t0 = Date.now()
  try {
    const value = await body()
    passes++
    say(`PASS ${name} (${Date.now() - t0} ms)`)
    return value
  } catch (error) {
    failures++
    say(`FAIL ${name} (${Date.now() - t0} ms): ${error instanceof Error ? error.message : String(error)}`)
    throw error
  }
}

interface Ping {
  sent: number
  received: number
  /** The lowest round trip in ms, or Infinity with no answer. */
  min: number
}

const pings = 20

/**
 * 20 pings, 200 ms apart, each waiting up to 2 s. The lowest round trip is
 * the measure: Wi-Fi power saving delays some answers by tens of
 * milliseconds, never all of them.
 */
async function ping(addr: string): Promise<Ping> {
  // -W is the per-reply wait: milliseconds on macOS, seconds on Linux.
  const wait = process.platform === 'darwin' ? '2000' : '2'
  const { stdout } = await run('ping', ['-c', String(pings), '-i', '0.2', '-W', wait, addr]).catch(
    (error: { stdout?: string }) => ({ stdout: error.stdout ?? '' })
  )
  const times = [...stdout.matchAll(/time=([\d.]+) ms/g)].map(m => Number(m[1]))
  return { sent: pings, received: times.length, min: times.length > 0 ? Math.min(...times) : Infinity }
}

function show(p: Ping): string {
  return `${p.received}/${p.sent} answered, lowest ${Number.isFinite(p.min) ? p.min.toFixed(1) : '—'} ms`
}

/** Pings until the lowest round trip is within 50 ms of base, for up to 30 s. */
async function backToBaseline(addr: string, base: number): Promise<Ping> {
  const deadline = Date.now() + 30_000
  for (;;) {
    const p = await ping(addr)
    if (p.received > 0 && p.min < base + 50) return p
    if (Date.now() > deadline) throw new Error(`still ${show(p)}; baseline ${base.toFixed(1)} ms`)
    await sleep(2000)
  }
}

async function adb(serial: string, ...args: string[]): Promise<string> {
  return (await run('adb', ['-s', serial, ...args])).stdout.trim()
}

/** The phone's IPv4 address on wlan0, once it has rejoined Wi-Fi (up to 60 s). */
async function wifiAddress(serial: string): Promise<string> {
  const deadline = Date.now() + 60_000
  for (;;) {
    const out = await adb(serial, 'shell', 'ip', '-4', '-o', 'addr', 'show', 'wlan0').catch(() => '')
    const m = /inet (\d+\.\d+\.\d+\.\d+)\//.exec(out)
    if (m != null) return m[1]
    if (Date.now() > deadline) throw new Error('wlan0 has no IPv4 address 60 s after airplane mode')
    await sleep(2000)
  }
}

async function exercise(client: Client, t: Target): Promise<void> {
  say(`== ${t.platform} ${t.device} at ${t.addr}`)
  const m = await step(`${t.platform}: attach`, async () =>
    await client.MaestroD.attach({ name: daemonName, device: t.device, appId: t.appId, ...(teamId == null ? {} : { teamId }) })
  )
  let detached = false
  try {
    const base = await step(`${t.platform}: baseline ping`, async () => {
      const p = await ping(t.addr)
      say(`     ${show(p)}`)
      assert(p.received >= pings - 2, `only ${p.received} of ${pings} answered`)
      return p.min
    })

    await step(`${t.platform}: L1 setTrafficShape {lossPct: 100}`, async () => {
      const res = await m.setTrafficShape({ lossPct: 100 })
      say(`     ${String(res.message)}`)
      const p = await ping(t.addr)
      say(`     ${show(p)}`)
      assert.equal(p.received, 0, 'pings still answered under 100% loss')
      const shot = await m.takeScreenshot()
      assert.equal(shot.ok, true, 'takeScreenshot failed under 100% loss')
      say('     takeScreenshot still works (USB)')
    })

    await step(`${t.platform}: L2 setTrafficShape {latencyMs: 100}`, async () => {
      await m.setTrafficShape({ latencyMs: 100 })
      const p = await ping(t.addr)
      const delta = p.min - base
      say(`     ${show(p)}: +${delta.toFixed(1)} ms over the baseline`)
      assert(delta >= 150 && delta <= 300, `ping rose by ${delta.toFixed(1)} ms, want about 200`)
    })

    const unshaped = await step(`${t.platform}: L3 resetTrafficShape`, async () => {
      await m.resetTrafficShape()
      const p = await backToBaseline(t.addr, base)
      say(`     ${show(p)}`)
      return p.min
    })

    // Against the lower of the two unshaped readings, so one taken in a
    // slow Wi-Fi spell cannot hide (or fake) the added delay.
    const floor = Math.min(base, unshaped)
    await step(`${t.platform}: L4 setTrafficShape {latencyMs: 100, direction: download}`, async () => {
      await m.setTrafficShape({ latencyMs: 100, direction: 'download' })
      const p = await ping(t.addr)
      const delta = p.min - floor
      say(`     ${show(p)}: +${delta.toFixed(1)} ms over the baseline`)
      assert(delta >= 60 && delta <= 160, `ping rose by ${delta.toFixed(1)} ms, want about 100 (one direction)`)
    })

    if (t.platform === 'ios') {
      await step('ios: setAirplaneMode answers UNSUPPORTED', async () => {
        try {
          await m.setAirplaneMode('enabled')
        } catch (error) {
          assert(client.isMaestroError(error, 'UNSUPPORTED'), String(error))
          say(`     ${error instanceof Error ? error.message : String(error)}`)
          return
        }
        assert.fail('setAirplaneMode succeeded on iOS')
      })
    } else {
      await step('android: setAirplaneMode enabled', async () => {
        await m.setAirplaneMode('enabled')
        say(`     airplane_mode_on = ${await adb(t.device, 'shell', 'settings', 'get', 'global', 'airplane_mode_on')}`)
      })
    }

    await step(`${t.platform}: L5 setTrafficShape {latencyMs: 100}, then detach`, async () => {
      await m.setTrafficShape({ latencyMs: 100 })
      detached = true
      await m.detach()
      let addr = t.addr
      if (t.platform === 'android') {
        const on = await adb(t.device, 'shell', 'settings', 'get', 'global', 'airplane_mode_on')
        say(`     airplane_mode_on = ${on} after detach`)
        assert.equal(on, '0', 'airplane mode is still on after detach')
        // Leaving airplane mode rejoins Wi-Fi, and DHCP may hand out
        // another address; ping the one the phone has now.
        addr = await wifiAddress(t.device)
        if (addr !== t.addr) say(`     the phone rejoined Wi-Fi at ${addr}`)
      }
      say(`     ${show(await backToBaseline(addr, base))} after detach`)
    })
  } finally {
    if (!detached) await m.detach().catch(() => {})
  }
}

async function main(): Promise<void> {
  say(`daemon ${daemonName}; devices ${targets.map(t => `${t.platform} ${t.device}`).join(', ')}`)
  const client = (await import(pathToFileURL(path.join(clientRoot, 'dist', 'esm', 'index.js')).href)) as Client
  for (const t of targets) await exercise(client, t)
}

main().then(
  () => {
    say(`${passes} steps passed`)
  },
  (error: unknown) => {
    if (failures === 0) say(`FAIL ${error instanceof Error ? error.message : String(error)}`)
    say(`${passes} steps passed, then a failure`)
    process.exitCode = 1
  }
)
