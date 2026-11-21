// Cloud devices in the JavaScript library against a real daemon process and
// a fake AWS Device Farm (fakeDeviceFarm.mjs): locations and filters on
// devices(), the cloud catalog, and (from M3) launching, driving and
// releasing cloud devices. No credentials or network.

import assert from 'node:assert/strict'
import { execFile, execFileSync } from 'node:child_process'
import * as fs from 'node:fs'
import * as os from 'node:os'
import * as path from 'node:path'
import { after, before, describe, test } from 'node:test'
import { fileURLToPath } from 'node:url'
import { promisify } from 'node:util'

import { CLIENT_MACHINE_HEADER, MaestroD, isMaestroError, machineName, withQuery } from '../dist/esm/index.js'
import { FakeDeviceFarm, fakeEnv } from './fakeDeviceFarm.mjs'

const here = path.dirname(fileURLToPath(import.meta.url))
const repoRoot = path.resolve(here, '..', '..', '..')

const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'maestro-d-df-'))
const bin = process.env.MAESTRO_D_TEST_BIN ?? path.join(tmp, 'maestro-d')
const fake = new FakeDeviceFarm()
const execFileAsync = promisify(execFile)

/** @type {MaestroD} */
let md

before(async () => {
  if (!process.env.MAESTRO_D_TEST_BIN) {
    execFileSync('go', ['build', '-o', bin, '.'], { cwd: repoRoot, stdio: 'inherit' })
  }
  const url = await fake.start()
  // The daemon this file spawns inherits this environment: the fake's
  // endpoint and keys, every other AWS credential source pinned to nothing.
  const env = fakeEnv(url, tmp)
  for (const k of Object.keys(process.env)) {
    if (!(k in env)) delete process.env[k]
  }
  Object.assign(process.env, env, {
    MAESTRO_D_HOME: path.join(tmp, 'home'),
    MAESTRO_D_BIN: bin,
    MAESTRO_D_MOCK_DEVICES: 'mock-1',
    MAESTRO_D_TEST_MACHINE: 'df-machine',
  })
  for (const k of ['MAESTRO_DEVICE', 'MAESTRO_D', 'MAESTRO_D_HUB', 'MAESTRO_D_TOKEN', 'MAESTRO_D_CA']) delete process.env[k]
  md = await MaestroD.connect({ name: 'jsdftest', idleTimeout: 120 })
})

after(async () => {
  try {
    await md?.shutdown()
  } finally {
    await fake.close()
    fs.rmSync(tmp, { recursive: true, force: true })
  }
})

describe('locations and filters', () => {
  test('every device carries its location and machine', async () => {
    const devs = await md.devices()
    const mock = devs.find((d) => d.id === 'mock-1')
    assert.ok(mock, 'mock-1 listed')
    assert.equal(mock.location, 'local')
    assert.equal(mock.machine, 'df-machine')
    assert.ok(devs.every((d) => ['local', 'mesh', 'cloud'].includes(d.location)))
  })

  test('location filters, as a word or a list', async () => {
    assert.deepEqual((await md.devices({ location: ['local'], platform: 'mock' })).map((d) => d.id), ['mock-1'])
    assert.deepEqual((await md.devices({ location: 'mesh', platform: 'mock' })).map((d) => d.id), [])
    assert.deepEqual((await md.devices({ name: '^MOCK', kind: 'device', available: true })).map((d) => d.id), ['mock-1'])
    assert.deepEqual(await md.devices({ osVersionMin: '1', platform: 'mock' }), [])
  })

  test('the machine header names this computer', async () => {
    assert.equal(CLIENT_MACHINE_HEADER, 'X-Maestro-Client-Machine')
    process.env.MAESTRO_D_TEST_MACHINE = 'elsewhere'
    try {
      assert.equal(machineName(), 'elsewhere')
      const mock = (await md.devices({ platform: 'mock' })).find((d) => d.id === 'mock-1')
      assert.equal(mock.location, 'mesh')
    } finally {
      process.env.MAESTRO_D_TEST_MACHINE = 'df-machine'
    }
    delete process.env.MAESTRO_D_TEST_MACHINE
    assert.equal(machineName(), os.hostname())
    process.env.MAESTRO_D_TEST_MACHINE = 'df-machine'
  })

  test('a bad filter is USAGE', async () => {
    await assert.rejects(md.devices({ name: '([' }), (err) => isMaestroError(err) && err.code === 'USAGE')
    await assert.rejects(md.devices({ location: 'moon' }), (err) => isMaestroError(err) && err.code === 'USAGE')
  })

  test('withQuery encodes filters', () => {
    assert.equal(withQuery('/v1/devices', {}), '/v1/devices')
    assert.equal(withQuery('/v1/devices', { location: ['local', 'cloud'], available: true, name: '', kind: undefined, x: false }),
      '/v1/devices?location=local%2Ccloud&available=true')
  })
})

describe('the cloud catalog', () => {
  test('models filtered on the daemon', async () => {
    const res = await md.catalog({ location: 'cloud', platform: 'android', osVersionMin: '13', available: true })
    assert.equal(res.ios, undefined)
    assert.equal(res.cloud.length, 1)
    const [cat] = res.cloud
    assert.equal(cat.provider, 'devicefarm')
    assert.deepEqual(cat.models.map((m) => m.name).sort(), ['Google Pixel 7', 'Google Pixel 8'])
    const p8 = cat.models.find((m) => m.name === 'Google Pixel 8')
    assert.equal(p8.id, 'df:android:google-pixel-8:14')
    assert.equal(p8.available, true)
    assert.ok(fake.ops().includes('ListDevices'))
  })

  test('iOS range and name', async () => {
    const res = await md.catalog({ location: 'cloud', platform: 'ios', name: 'iphone 1[45]', osVersionMax: '17' })
    assert.deepEqual(res.cloud[0].models.map((m) => m.id), ['df:ios:apple-iphone-15:17.4'])
  })

  test('status lists the provider once used', async () => {
    const st = await md.status()
    assert.deepEqual(st.cloud, [{ provider: 'devicefarm', held: 0 }])
  })
})

describe('direct mode', () => {
  test('createDevice({cloud}) launches, attach drives it, deleteDevice releases it', async () => {
    const app = path.join(tmp, 'sample.ipa')
    fs.writeFileSync(app, 'PK fake ipa')
    const dev = await md.createDevice({
      cloud: { provider: 'devicefarm', platform: 'ios', name: 'Apple iPhone 15', app, appId: 'org.example', idleReleaseSec: 900 },
    })
    assert.match(dev.id, /^df-[0-9a-f]{8}$/)
    assert.equal(dev.location, 'cloud')
    assert.equal(dev.provider, 'devicefarm')
    assert.equal(dev.idleReleaseSec, 900)
    assert.equal(dev.name, 'Apple iPhone 15')
    const session = fake.sessions.find((s) => s.name === dev.id)
    assert.ok(session, 'the session is named with the device id')
    assert.equal(session.parameters['appium:version'], '2')
    assert.ok(fake.uploads.some((u) => u.type === 'IOS_APP'), 'the app was uploaded')

    const handle = await md.attach({ device: dev.id })
    await handle.tapOn('Button')
    await handle.flow({ yaml: 'appId: org.example\n---\n- tapOn: Button\n' })
    assert.equal(session.caps?.['appium:udid'], undefined)

    const del = await md.deleteDevice(dev.id)
    assert.equal(del.id, dev.id)
    assert.ok(del.deviceMinutes && del.deviceMinutes.total >= 1)
    assert.equal(session.status, 'COMPLETED')
    assert.deepEqual(await md.devices({ location: 'cloud' }), [])
  })

  test('stopDevice resolves with the minutes', async () => {
    const dev = await md.createDevice({ cloud: { platform: 'android', name: 'Google Pixel 8' } })
    const res = await md.stopDevice(dev.id)
    assert.ok(res.deviceMinutes && res.deviceMinutes.total >= 1)
    // A device that is not a cloud device resolves without minutes.
    assert.deepEqual(await md.stopDevice('mock-1', { force: true }), {})
  })

  test('createDevice({cloud}) sends app files absolute', async () => {
    // The daemon runs from another directory than this process now does.
    const dir = fs.mkdtempSync(path.join(tmp, 'rel-'))
    fs.writeFileSync(path.join(dir, 'rel.apk'), 'PK relative apk')
    const cwd = process.cwd()
    process.chdir(dir)
    try {
      const before = fake.uploads.length
      const dev = await md.createDevice({ cloud: { platform: 'android', name: 'Google Pixel 8', app: 'rel.apk', appId: 'org.rel' } })
      assert.equal(fake.uploads.length, before + 1, 'the relative app was uploaded')
      await md.deleteDevice(dev.id)
    } finally {
      process.chdir(cwd)
    }
  })

  test('createDevice({cloud}) needs cloud.platform', async () => {
    await assert.rejects(md.createDevice({ cloud: {} }), TypeError)
    await assert.rejects(md.createDevice({ cloud: { platform: 'android', idleReleaseSec: 99999 } }), (err) => isMaestroError(err) && err.code === 'USAGE')
  })
})

describe('hub mode', () => {
  const homes = { a: path.join(tmp, 'hub-a'), b: path.join(tmp, 'hub-b') }
  /** @type {{a?: string, b?: string}} */
  const addrs = {}
  const hubEnv = (name) => ({ ...process.env, MAESTRO_D_HOME: homes[name], MAESTRO_D_TEST_MACHINE: `machine-${name}` })
  // Asynchronously: hub-b's start check calls the fake, which runs in this
  // process's event loop.
  const start = async (name, extra) => {
    const { stdout } = await execFileAsync(bin, ['hub', 'start', '--daemon', `jsdf-${name}`, '--name', `hub-${name}`, '--listen', '127.0.0.1:0',
      '--no-mdns', '--token', 'jsdf-tok', '--peer-token', 'jsdf-peer', '--json', ...extra], { env: hubEnv(name) })
    return JSON.parse(stdout).daemon.httpAddr
  }
  /** @type {MaestroD} */
  let hubClient

  before(async () => {
    addrs.a = await start('a', [])
    addrs.b = await start('b', ['--peer', addrs.a, '--devicefarm', '--cloud-max-devices', '1'])
    hubClient = await MaestroD.connect({ hub: `http://${addrs.a}`, token: 'jsdf-tok' })
    // Wait for the link: a learns from b's hello that b launches cloud devices.
    for (let i = 0; i < 100; i++) {
      const st = await hubClient.hubStatus()
      if (st.peers.some((p) => p.state === 'linked' && p.cloud?.includes('devicefarm'))) return
      await new Promise((r) => setTimeout(r, 100))
    }
    throw new Error('the hubs did not link')
  })

  after(async () => {
    for (const name of ['b', 'a']) {
      await execFileAsync(bin, ['hub', 'stop', '--daemon', `jsdf-${name}`], { env: hubEnv(name) }).catch(() => undefined)
    }
  })

  test('lease({select: {location: cloud}}) launches through the hub that has credentials', async () => {
    const dev = await hubClient.lease({ select: { location: 'cloud', platform: 'android', name: 'Google Pixel 8' }, ttlSec: 900,
      cloud: { idleReleaseSec: 600 }, attach: {} })
    assert.match(dev.id, /^df-[0-9a-f]{8}@hub-b$/)
    await dev.tapOn('Button')
    const listed = await hubClient.devices({ location: 'cloud' })
    assert.deepEqual(listed.map((d) => [d.id, d.hub, d.provider]), [[dev.id, 'hub-b', 'devicefarm']])
    const res = await dev.release()
    assert.ok(res.deviceMinutes && res.deviceMinutes.total >= 1, 'release resolves with the minutes')
    assert.deepEqual(await hubClient.devices({ location: 'cloud' }), [])
    // A device that is not a cloud device releases with nothing to report.
    const mock = await hubClient.lease({ select: { platform: 'mock' }, attach: false }).catch(() => undefined)
    if (mock) assert.deepEqual(await mock.release(), {})
  })

  test('the cloud catalog through a hub names the hub that answers', async () => {
    const res = await hubClient.catalog({ location: 'cloud', platform: 'ios', hub: 'hub-b' })
    assert.equal(res.cloud[0].hub, 'hub-b')
  })
})
