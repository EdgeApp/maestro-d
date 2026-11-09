// Hub mesh end to end: one device, driven through each of two linked hubs
// in turn with this repository's JavaScript client (npm/maestro-d). The
// device belongs to one hub; through the other, every call is forwarded to
// it. Run it from any machine that reaches both hubs (the plan runs it in a
// Linux container).
//
// Like iosLandscape.e2e.ts the script stands alone: Node strips its types
// (Node 22.18 or later runs a .ts file as is; older 22.x needs
// --experimental-strip-types), or run it with `npx tsx`. Build the client
// first (cd npm/maestro-d && npm install && npm run build); the library
// packs the flow with the maestro-d binary (MAESTRO_D_BIN, else PATH).
//
//   MAESTRO_D_HUB_A=<host:port of one hub> MAESTRO_D_HUB_B=<host:port of the other> \
//   MAESTRO_D_TOKEN=<the hubs' client token> \
//   MAESTRO_D_DEVICE=<qualified device id, <localId>@<hub>> \
//   MAESTRO_D_FLOW=<a flow file that runs on the device, e.g. e2e/testdata/hub/main.yaml> \
//   [MAESTRO_D_PHASE=<a label for the log, e.g. hub-a-ios>] \
//   [MAESTRO_D_APP=<app id, default com.apple.Preferences>] \
//   [MAESTRO_D_TAP=<text to tap, default General>] \
//   [MAESTRO_D_ASSERT=<text visible after the tap, default About>] \
//   [MAESTRO_D_CLIENT_ROOT=<client package, default npm/maestro-d>] \
//     node e2e/hubMesh.e2e.ts
//
// For each hub in turn (A, then B): hubStatus() shows the other hub linked;
// devices() lists the device by its qualified id, with its owner's machine
// and maximum lease window; lease({device}) leases it explicitly through
// that hub and attaches it (no client takes a lease on its own); launchApp,
// tapOn and assertVisible run on the owner; flow({file}) ships the flow's
// bundle and runs it there in one request; renew() restarts the lease's
// fixed window; release() ends it. Each step is checked and the script
// exits 1 at the first failure; close() releases what it holds.
import assert from 'node:assert/strict'
import path from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'

import type { MaestroD } from '../npm/maestro-d/dist/types/index.js'

interface Client {
  MaestroD: { connect: (options: { hub: string; token?: string }) => Promise<MaestroD> }
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

const hubs = [required('MAESTRO_D_HUB_A'), required('MAESTRO_D_HUB_B')]
const device = required('MAESTRO_D_DEVICE')
const flowFile = path.resolve(required('MAESTRO_D_FLOW'))
const phase = optional('MAESTRO_D_PHASE') ?? 'mesh'
const appId = optional('MAESTRO_D_APP') ?? 'com.apple.Preferences'
const tapText = optional('MAESTRO_D_TAP') ?? 'General'
const assertText = optional('MAESTRO_D_ASSERT') ?? 'About'
const token = optional('MAESTRO_D_TOKEN')
const clientRoot =
  optional('MAESTRO_D_CLIENT_ROOT') ?? path.join(repoRoot, 'npm', 'maestro-d')
if (!device.includes('@')) throw new Error('MAESTRO_D_DEVICE must be qualified: <localId>@<hub>')
const owner = device.slice(device.lastIndexOf('@') + 1)

const started = Date.now()
function say(line: string): void {
  const seconds = ((Date.now() - started) / 1000).toFixed(1)
  console.log(`+${seconds.padStart(6)}s [${phase}] ${line}`)
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

async function throughHub(client: Client, hub: string): Promise<void> {
  const md = await client.MaestroD.connect({ hub, token })
  try {
    const status = await step(`${hub}: hubStatus`, async () => {
      const st = await md.hubStatus()
      const linked = st.peers.filter(p => p.state === 'linked')
      assert.ok(linked.length > 0, `hub ${st.name} has no linked peer: ${JSON.stringify(st.peers)}`)
      return st
    })
    const route = status.name === owner ? 'served by its owner' : `forwarded to ${owner}`
    say(`${hub} is hub ${status.name}; peers ${status.peers.map(p => `${p.name ?? '?'} ${p.state} ${p.rttMs ?? '-'}ms`).join(', ')}; ${device} is ${route}`)
    await step(`${hub}: devices() lists ${device}`, async () => {
      const found = (await md.devices()).find(d => d.id === device)
      assert.ok(found, `${device} is not in ${status.name}'s list`)
      assert.equal(found.hub, owner)
      assert.ok(found.machine, `${device} names no machine`)
      assert.ok((found.maxSec ?? 0) > 0, `${device} has no maxSec`)
      say(`${device} is on ${found.machine} (${(found.addresses ?? []).join(', ')}); lease max ${found.maxSec}s`)
    })
    const dev = await step(`${hub}: lease ${device}`, async () => {
      const d = await md.lease({ device })
      assert.equal(d.id, device)
      assert.ok(d.lease?.id.endsWith(`@${owner}`), `lease id ${d.lease?.id ?? '?'}`)
      return d
    })
    await step(`${hub}: launchApp ${appId}`, async () => {
      assert.equal((await dev.launchApp(appId)).ok, true)
    })
    await step(`${hub}: tapOn ${tapText}`, async () => {
      assert.equal((await dev.tapOn(tapText)).ok, true)
    })
    await step(`${hub}: assertVisible ${assertText}`, async () => {
      assert.equal((await dev.assertVisible(assertText)).ok, true)
    })
    await step(`${hub}: flow ${path.basename(flowFile)}`, async () => {
      const res = await dev.flow({ file: flowFile })
      assert.equal(res.ok, true, JSON.stringify(res.error))
    })
    await step(`${hub}: renew`, async () => {
      const lease = await dev.renew()
      assert.ok(lease && Date.parse(lease.expiresAt) > Date.now(), `renewed lease ${JSON.stringify(lease)}`)
    })
    await step(`${hub}: release`, async () => {
      await dev.release()
    })
  } finally {
    await md.close()
  }
}

async function main(): Promise<void> {
  const client = (await import(
    pathToFileURL(path.join(clientRoot, 'dist', 'esm', 'index.js')).href
  )) as Client
  say(`device ${device} (owner ${owner}), hubs ${hubs.join(' then ')}`)
  for (const hub of hubs) await throughHub(client, hub)
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
