// Cloud devices end to end on live AWS Device Farm, through this
// repository's JavaScript client (npm/maestro-d): list the cloud catalog,
// launch a real phone with an app, attach, drive it with commands, a
// screenshot and a flow, and release it, reporting the device minutes.
//
// Like deviceManagement.e2e.ts the script stands alone: Node strips its
// types (Node 22.18 or later runs a .ts file as is; older 22.x needs
// --experimental-strip-types), or run it with `npx tsx`. Build the binary
// and the client into a separate install first (never over the shared
// ~/.maestro-d):
//
//   make build INSTALL_DIR=$HOME/.maestro-d-deviceFarm && (cd npm/maestro-d && npm install && npm run build)
//
//   MAESTRO_D_BIN=$HOME/.maestro-d-deviceFarm/bin/maestro-d MAESTRO_D_HOME=$HOME/.maestro-d-deviceFarm/run \
//   AWS_ACCESS_KEY_ID=… AWS_SECRET_ACCESS_KEY=… (or AWS_SHARED_CREDENTIALS_FILE=…) \
//   MAESTRO_D_DF_MODE=direct \
//   MAESTRO_D_DF_PLATFORM=android|ios MAESTRO_D_DF_NAME=<model, e.g. "Google Pixel 8"> [MAESTRO_D_DF_OS=<version>] \
//   MAESTRO_D_DF_APP=<path to .apk or .ipa> MAESTRO_D_DF_APP_ID=<bundle id or package> \
//   [MAESTRO_D=<daemon>] [MAESTRO_D_ARTIFACT_DIR=<dir for the screenshot>] \
//   [MAESTRO_D_CLIENT_ROOT=<client package, default npm/maestro-d>] \
//     node e2e/devicefarm.e2e.ts
//
// In direct mode the AWS credentials are this script's own daemon's. The
// steps depend on the platform: Android drives WikipediaSample.apk
// (org.wikipedia.alpha) and e2e/fixtures/devicefarm/android-search.yaml;
// iOS drives BStackSampleApp.ipa (com.browserstack.Sample-iOS) and
// e2e/fixtures/devicefarm/ios-text.yaml (both are public sample apps:
// https://www.browserstack.com/app-automate/sample-apps/<android|ios>/<file>).
// Each step is checked and the script exits 1 at the first failure. The
// device is released in a finally, so a failed run leaves nothing billing.
import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'

import type { DeviceInfo, MaestroD, MaestroDevice } from '../npm/maestro-d/dist/types/index.js'

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

const mode = required('MAESTRO_D_DF_MODE')
const platform = required('MAESTRO_D_DF_PLATFORM')
const modelName = required('MAESTRO_D_DF_NAME')
const osVersion = optional('MAESTRO_D_DF_OS')
const app = path.resolve(required('MAESTRO_D_DF_APP'))
const appId = required('MAESTRO_D_DF_APP_ID')
const artifactDir = optional('MAESTRO_D_ARTIFACT_DIR')
const clientRoot = path.resolve(repoRoot, optional('MAESTRO_D_CLIENT_ROOT') ?? 'npm/maestro-d')
if (mode !== 'direct') throw new Error(`MAESTRO_D_DF_MODE ${mode}: only direct is supported`)

// The steps each platform's sample app takes.
const steps: Record<string, { flow: string; drive: (d: MaestroDevice) => Promise<void> }> = {
  android: {
    flow: 'e2e/fixtures/devicefarm/android-search.yaml',
    drive: async (d) => {
      // Android 14 and later warn first that the app is old.
      await d.tapOn({ text: 'OK', optional: true })
      await d.assertVisible({ id: 'org.wikipedia.alpha:id/search_container' })
      await d.tapOn({ id: 'org.wikipedia.alpha:id/search_container' })
      await d.back()
    },
  },
  ios: {
    flow: 'e2e/fixtures/devicefarm/ios-text.yaml',
    drive: async (d) => {
      await d.assertVisible('Text Button')
      await d.tapOn('Text Button')
      await d.assertVisible({ id: 'Text Input' })
    },
  },
}

function log(msg: string): void {
  console.log(`${new Date().toISOString()} ${msg}`)
}

async function main(): Promise<void> {
  const plan = steps[platform]
  if (!plan) throw new Error(`MAESTRO_D_DF_PLATFORM ${platform} is not android or ios`)
  const { MaestroD } = (await import(pathToFileURL(path.join(clientRoot, 'dist/esm/index.js')).href)) as Client
  const md = await MaestroD.connect({ name: optional('MAESTRO_D') })

  const cat = await md.catalog({ location: 'cloud', platform, name: modelName, available: true })
  const models = cat.cloud?.[0]?.models ?? []
  assert.ok(models.length > 0, `the cloud catalog lists ${modelName}`)
  log(`catalog: ${models.length} ${platform} model(s) match ${JSON.stringify(modelName)}: ${models.slice(0, 3).map((m) => `${m.name} ${m.osVersion}`).join(', ')}`)

  let dev: DeviceInfo | undefined
  try {
    const started = Date.now()
    dev = await md.createDevice({ cloud: { platform, name: `^${modelName}$`, osVersion, app, appId, idleReleaseSec: 900 } })
    log(`launched ${dev.id}: ${dev.name} ${dev.osVersion} in ${Math.round((Date.now() - started) / 1000)} s; location ${dev.location}, provider ${dev.provider}, expires ${dev.expiresAt}`)
    assert.equal(dev.location, 'cloud')

    const d = await md.attach({ device: dev.id })
    log(`attached ${dev.id}`)
    await d.launchApp({ appId })
    log('launchApp ok')
    await plan.drive(d)
    log('commands ok')
    const shotPath = path.join(artifactDir ?? '.', `devicefarm-${platform}-screen.png`)
    await d.takeScreenshot(shotPath)
    assert.ok(fs.statSync(shotPath).size > 0, 'the screenshot is written')
    log(`takeScreenshot ok → ${path.basename(shotPath)} (${fs.statSync(shotPath).size} bytes)`)
    const res = await d.flow({ file: path.join(repoRoot, plan.flow) })
    assert.ok(res.ok, `flow ${plan.flow}`)
    log(`flow ${plan.flow} ok in ${res.durationMs} ms`)
  } finally {
    if (dev) {
      const del = await md.deleteDevice(dev.id)
      log(`released ${dev.id}: ${JSON.stringify(del.deviceMinutes)} device minutes`)
    }
  }
  const left = await md.devices({ location: 'cloud' })
  assert.equal(left.length, 0, 'no cloud device left')
  log('PASS')
}

main().catch((err) => {
  console.error(err)
  process.exit(1)
})
