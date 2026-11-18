// Device management in the JavaScript library — catalog(), createDevice(),
// deleteDevice() and startDevice() without a platform — against a mock
// daemon: an HTTP server that records each request and answers as the Go
// daemon does, so no simulator or emulator is touched.

import assert from 'node:assert/strict'
import * as http from 'node:http'
import { after, before, describe, test } from 'node:test'

import { MaestroD, isMaestroError } from '../dist/esm/index.js'

/** @type {{method: string, url: string, body: any}[]} */
const requests = []
/** @type {http.Server} */
let server
/** @type {MaestroD} */
let md

const ok = (extra) => ({ ok: true, exitCode: 0, ...extra })
const fail = (code, message, exitCode, details) => ({ ok: false, exitCode, error: { code, message, details } })

const CATALOG = {
  ios: {
    runtimes: [{ id: 'com.apple.CoreSimulator.SimRuntime.iOS-26-5', name: 'iOS 26.5', version: '26.5', available: true }],
    deviceTypes: [{ id: 'com.apple.CoreSimulator.SimDeviceType.iPhone-16', name: 'iPhone 16', productFamily: 'iPhone' }],
  },
  android: {
    images: [{ id: 'system-images;android-35;google_apis;arm64-v8a', api: 'android-35', tag: 'google_apis', abi: 'arm64-v8a', installed: true }],
    profiles: ['pixel_8'],
  },
}

function answer(req, body) {
  const url = new URL(req.url, 'http://x')
  if (req.method === 'GET' && url.pathname === '/v1/status') {
    return [200, ok({ daemon: { name: 'mock', pid: 1, apiVersion: '1', version: 'test' }, devices: [] })]
  }
  if (req.method === 'GET' && url.pathname === '/v1/catalog') {
    const p = url.searchParams.get('platform')
    if (p === 'bogus') return [400, fail('USAGE', 'platform must be ios or android', 2)]
    return [200, ok(p ? { [p]: CATALOG[p] } : CATALOG)]
  }
  if (req.method === 'POST' && url.pathname === '/v1/devices') {
    if (body.create) {
      if (body.create.dataSize === '12G') return [400, fail('USAGE', 'data size 12G is above the 10G limit', 2)]
      const id = body.platform === 'android' ? `avd:${body.name}` : 'AAAA-1111'
      const booted = body.boot !== false
      return [200, ok({ device: { id: booted && body.platform === 'android' ? 'emulator-5554' : id, platform: body.platform,
        name: body.name, ready: booted, attached: false, state: booted ? 'Booted' : 'Shutdown', createdBy: 'maestro-d' } })]
    }
    if (!body.platform && !body.name.startsWith('avd:') && body.name !== 'AAAA-1111') {
      return [400, fail('USAGE', `no simulator, AVD or device is called "${body.name}"`, 2)]
    }
    const android = body.platform === 'android' || body.name.startsWith('avd:')
    return [200, ok({ device: { id: android ? 'emulator-5554' : body.name, platform: android ? 'android' : 'ios', ready: true, attached: false } })]
  }
  const del = url.pathname.match(/^\/v1\/devices\/([^/]+)$/)
  if (req.method === 'DELETE' && del) {
    const id = decodeURIComponent(del[1])
    if (url.searchParams.get('delete') !== 'true') return [200, ok()]
    if (id === 'external' && url.searchParams.get('force') !== 'true') {
      return [409, fail('DEVICE_EXTERNAL', 'external was not created by maestro-d; use --force to delete it', 5, { device: id })]
    }
    return [200, ok({ device: { id, platform: 'ios', name: id, ready: false, attached: false } })]
  }
  return [400, fail('USAGE', `no route for ${req.method} ${url.pathname}`, 2)]
}

before(async () => {
  server = http.createServer((req, res) => {
    let data = ''
    req.on('data', (c) => (data += c))
    req.on('end', () => {
      const body = data ? JSON.parse(data) : undefined
      if (!req.url.startsWith('/v1/status')) requests.push({ method: req.method, url: req.url, body })
      const [status, payload] = answer(req, body ?? {})
      res.writeHead(status, { 'Content-Type': 'application/json', 'X-Maestro-Api-Version': '1' })
      res.end(JSON.stringify(payload))
    })
  })
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve))
  md = await MaestroD.connect({ url: `127.0.0.1:${server.address().port}` })
})

after(() => {
  md?.close()
  server?.close()
})

function last() {
  return requests[requests.length - 1]
}

async function rejectsWith(fn, code) {
  try {
    await fn()
  } catch (err) {
    assert.ok(isMaestroError(err, code), `expected ${code}, got ${err}`)
    return err
  }
  assert.fail(`expected ${code}`)
}

describe('catalog()', () => {
  test('lists both platforms by default', async () => {
    const cat = await md.catalog()
    assert.equal(last().url, '/v1/catalog')
    assert.equal(cat.ios.runtimes[0].version, '26.5')
    assert.equal(cat.android.profiles[0], 'pixel_8')
  })

  test('passes platform and available as the query', async () => {
    const cat = await md.catalog({ platform: 'android', available: true })
    assert.equal(last().url, '/v1/catalog?platform=android&available=true')
    assert.equal(cat.ios, undefined)
    assert.equal(cat.android.images[0].installed, true)
  })

  test('an unknown platform rejects with USAGE', async () => {
    await rejectsWith(() => md.catalog({ platform: 'bogus' }), 'USAGE')
  })
})

describe('createDevice()', () => {
  test('an iOS simulator: create holds the type and runtime, boot is left to the daemon', async () => {
    const dev = await md.createDevice({ platform: 'ios', name: 'sim', type: 'iPhone 16', runtime: 'iOS-26-5' })
    assert.deepEqual(last(), {
      method: 'POST',
      url: '/v1/devices',
      body: { platform: 'ios', name: 'sim', create: { name: 'sim', type: 'iPhone 16', runtime: 'iOS-26-5' } },
    })
    assert.equal(dev.id, 'AAAA-1111')
    assert.equal(dev.createdBy, 'maestro-d')
  })

  test('an AVD with every Android option and boot: false', async () => {
    const dev = await md.createDevice({
      platform: 'android',
      name: 'pix',
      image: 'system-images;android-35;google_apis;arm64-v8a',
      profile: 'pixel_8',
      dataSize: '8G',
      installImage: true,
      acceptLicenses: true,
      boot: false,
      bootTimeout: 300,
    })
    assert.deepEqual(last().body, {
      platform: 'android',
      name: 'pix',
      boot: false,
      bootTimeout: 300,
      create: {
        name: 'pix',
        image: 'system-images;android-35;google_apis;arm64-v8a',
        profile: 'pixel_8',
        dataSize: '8G',
        installImage: true,
        acceptLicenses: true,
      },
    })
    assert.equal(dev.id, 'avd:pix')
    assert.equal(dev.ready, false)
  })

  test("the daemon's USAGE refusal rejects", async () => {
    await rejectsWith(() => md.createDevice({ platform: 'android', name: 'pix', dataSize: '12G' }), 'USAGE')
  })

  test('platform and name are required', async () => {
    await assert.rejects(() => md.createDevice({ platform: 'ios' }), TypeError)
    await assert.rejects(() => md.createDevice({ name: 'x' }), TypeError)
  })
})

describe('deleteDevice()', () => {
  test('sends delete=true and resolves with the deleted device', async () => {
    const dev = await md.deleteDevice('AAAA-1111')
    assert.equal(last().method, 'DELETE')
    assert.equal(last().url, '/v1/devices/AAAA-1111?delete=true')
    assert.equal(dev.id, 'AAAA-1111')
  })

  test('an external device is DEVICE_EXTERNAL without force', async () => {
    const err = await rejectsWith(() => md.deleteDevice('external'), 'DEVICE_EXTERNAL')
    assert.equal(err.exitCode, 5)
    assert.equal(err.details.device, 'external')
  })

  test('force=true deletes it', async () => {
    await md.deleteDevice('external', { force: true })
    assert.equal(last().url, '/v1/devices/external?delete=true&force=true')
  })

  test('avd:<name> is escaped in the path', async () => {
    await md.deleteDevice('avd:pix')
    assert.equal(last().url, '/v1/devices/avd%3Apix?delete=true')
  })

  test('an id is required', async () => {
    await assert.rejects(() => md.deleteDevice(''), TypeError)
  })
})

describe('startDevice() without a platform', () => {
  test('avd:<name> boots and returns the serial', async () => {
    const dev = await md.startDevice({ name: 'avd:pix' })
    assert.deepEqual(last().body, { name: 'avd:pix' })
    assert.equal(dev.id, 'emulator-5554')
  })

  test('a simulator UDID', async () => {
    const dev = await md.startDevice({ name: 'AAAA-1111' })
    assert.equal(dev.platform, 'ios')
  })

  test('an unknown name is USAGE', async () => {
    await rejectsWith(() => md.startDevice({ name: 'nothing' }), 'USAGE')
  })
})

describe('stopDevice() and ping() beside them', () => {
  test('stopDevice sends force in the body, without delete', async () => {
    await md.stopDevice('AAAA-1111', { force: true })
    assert.deepEqual(last(), { method: 'DELETE', url: '/v1/devices/AAAA-1111', body: { force: true } })
    await md.stopDevice('AAAA-1111')
    assert.deepEqual(last().body, { force: false })
  })

  test('ping resolves against a live daemon', async () => {
    await md.ping(1000)
  })
})
