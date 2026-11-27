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
// In direct mode the AWS credentials are this script's own daemon's. In hub
// mode (MAESTRO_D_DF_MODE=hub MAESTRO_D_HUB=<host:port> MAESTRO_D_TOKEN=<client
// token>) the script gets no AWS variables: maestro-d-hub start --devicefarm
// holds them, the device is leased (lease({select: {location: 'cloud'}}))
// rather than created, and the app is installed after the lease, uploaded
// under it. The
// steps depend on the platform: Android drives WikipediaSample.apk
// (org.wikipedia.alpha) and e2e/fixtures/devicefarm/android-search.yaml;
// iOS drives BStackSampleApp.ipa (com.browserstack.Sample-iOS) and
// e2e/fixtures/devicefarm/ios-text.yaml (both are public sample apps:
// https://www.browserstack.com/app-automate/sample-apps/<android|ios>/<file>).
// With MAESTRO_D_DF_SECOND_APP=<.apk or .ipa> MAESTRO_D_DF_SECOND_APP_ID=<id>
// MAESTRO_D_DF_SECOND_TEXT=<text on its first screen>, the script then
// installs that app into the running session (installApp), checks the
// app.installed event and that the device kept its id, launches it and
// asserts the text (LocalSample.apk: com.example.android.basicnetworking;
// LocalSample.ipa: com.browserstack.SampleLocalApp).
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
// In hub mode the app is optional: without it the script drives the
// platform's Settings app, which every device has.
const appPath = mode === 'hub' ? optional('MAESTRO_D_DF_APP') : required('MAESTRO_D_DF_APP')
const app = appPath ? path.resolve(appPath) : undefined
const appId = app ? required('MAESTRO_D_DF_APP_ID') : platform === 'ios' ? 'com.apple.Preferences' : 'com.android.settings'
const secondPath = optional('MAESTRO_D_DF_SECOND_APP')
const second = secondPath
  ? { app: path.resolve(secondPath), appId: required('MAESTRO_D_DF_SECOND_APP_ID'), text: required('MAESTRO_D_DF_SECOND_TEXT') }
  : undefined
const artifactDir = optional('MAESTRO_D_ARTIFACT_DIR')
const clientRoot = path.resolve(repoRoot, optional('MAESTRO_D_CLIENT_ROOT') ?? 'npm/maestro-d')
if (mode !== 'direct' && mode !== 'hub') throw new Error(`MAESTRO_D_DF_MODE ${mode}: direct or hub`)

// The steps each platform's sample app takes.
const settings: Record<string, { flow: string; drive: (d: MaestroDevice) => Promise<void> }> = {
  android: {
    flow: 'e2e/fixtures/devicefarm/android-settings.yaml',
    drive: async (d) => {
      await d.assertVisible('Network.*')
    },
  },
  ios: {
    flow: 'e2e/fixtures/devicefarm/ios-settings.yaml',
    drive: async (d) => {
      await d.assertVisible('General')
    },
  },
}
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
  const plan = app ? steps[platform] : settings[platform]
  if (!plan) throw new Error(`MAESTRO_D_DF_PLATFORM ${platform} is not android or ios`)
  const { MaestroD } = (await import(pathToFileURL(path.join(clientRoot, 'dist/esm/index.js')).href)) as Client
  const md = mode === 'hub'
    ? await MaestroD.connect({ hub: required('MAESTRO_D_HUB'), token: required('MAESTRO_D_TOKEN') } as { name?: string })
    : await MaestroD.connect({ name: optional('MAESTRO_D') })

  const cat = await md.catalog({ location: 'cloud', platform, name: modelName, available: true })
  const models = cat.cloud?.[0]?.models ?? []
  assert.ok(models.length > 0, `the cloud catalog lists ${modelName}`)
  log(`catalog: ${models.length} ${platform} model(s) match ${JSON.stringify(modelName)}: ${models.slice(0, 3).map((m) => `${m.name} ${m.osVersion}`).join(', ')}`)

  let dev: DeviceInfo | undefined
  let leased: MaestroDevice | undefined
  try {
    const started = Date.now()
    let d: MaestroDevice
    if (mode === 'hub') {
      leased = d = await md.lease({ select: { location: 'cloud', platform, name: modelName, osVersion }, ttlSec: 1800,
        cloud: { idleReleaseSec: 900, appId }, attach: {} })
      dev = (await md.devices({ location: 'cloud' })).find((x) => x.id === d.id)
      assert.ok(dev, 'the leased cloud device is listed')
      log(`leased ${dev.id} on hub ${dev.hub}: ${dev.name} ${dev.osVersion} in ${Math.round((Date.now() - started) / 1000)} s; location ${dev.location}, provider ${dev.provider}`)
      if (app) {
        const inst = await d.installApp(app)
        log(`installApp ${path.basename(app)} ok (${JSON.stringify(inst)})`)
      }
    } else {
      dev = await md.createDevice({ cloud: { platform, name: modelName, osVersion, app, appId, idleReleaseSec: 900 } })
      log(`launched ${dev.id}: ${dev.name} ${dev.osVersion} in ${Math.round((Date.now() - started) / 1000)} s; location ${dev.location}, provider ${dev.provider}, expires ${dev.expiresAt}`)
      d = await md.attach({ device: dev.id })
      log(`attached ${dev.id}`)
    }
    assert.equal(dev.location, 'cloud')

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

    if (second) {
      // Install a second app into the running session: the device keeps its
      // id, and the executor session reopens on it.
      const events = d.events({ signal: AbortSignal.timeout(600_000) })[Symbol.asyncIterator]()
      const installStarted = Date.now()
      const inst = await d.installApp(second.app)
      log(`installApp ${path.basename(second.app)} ok in ${Math.round((Date.now() - installStarted) / 1000)} s (${JSON.stringify(inst)})`)
      for (;;) {
        const { value, done } = await events.next()
        assert.ok(!done, 'an app.installed event')
        if (value.type === 'app.installed' && value.message === second.appId) {
          log(`event app.installed ${value.message} on ${value.device}`)
          break
        }
      }
      await events.return?.()
      const after = (await md.devices({ location: 'cloud' })).find((x) => x.id === dev?.id)
      assert.ok(after, `${dev.id} is still listed after the install`)
      await d.launchApp({ appId: second.appId })
      await d.assertVisible(second.text)
      log(`launchApp ${second.appId} and assertVisible ${JSON.stringify(second.text)} ok on the same device ${after.id}`)
      const installedShot = path.join(artifactDir ?? '.', `devicefarm-${platform}-installed.png`)
      await d.takeScreenshot(installedShot)
      log(`takeScreenshot ok → ${path.basename(installedShot)} (${fs.statSync(installedShot).size} bytes)`)
    }
  } finally {
    if (leased) {
      const rel = await leased.release()
      log(`released the lease on ${leased.id}: ${JSON.stringify(rel.deviceMinutes)} device minutes`)
    } else if (dev) {
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
