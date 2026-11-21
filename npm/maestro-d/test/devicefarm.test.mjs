// Cloud devices in the JavaScript library against a real daemon process and
// a fake AWS Device Farm (fakeDeviceFarm.mjs): locations and filters on
// devices(), the cloud catalog, and (from M3) launching, driving and
// releasing cloud devices. No credentials or network.

import assert from 'node:assert/strict'
import { execFileSync } from 'node:child_process'
import * as fs from 'node:fs'
import * as os from 'node:os'
import * as path from 'node:path'
import { after, before, describe, test } from 'node:test'
import { fileURLToPath } from 'node:url'

import { CLIENT_MACHINE_HEADER, MaestroD, isMaestroError, machineName, withQuery } from '../dist/esm/index.js'
import { FakeDeviceFarm, fakeEnv } from './fakeDeviceFarm.mjs'

const here = path.dirname(fileURLToPath(import.meta.url))
const repoRoot = path.resolve(here, '..', '..', '..')

const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'maestro-d-df-'))
const bin = process.env.MAESTRO_D_TEST_BIN ?? path.join(tmp, 'maestro-d')
const fake = new FakeDeviceFarm()

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
