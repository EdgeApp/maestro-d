// The client side of the device steps and fields a test and procurement
// need: installApp from a URL, the attach `ai` keys, a device's
// capabilities, supportsStatusBar, the data setStatusBar, keyboardVisible,
// the samplers and the AI checks answer, and createDevice's hub. Against a
// mock daemon: an HTTP server that records each request.

import assert from 'node:assert/strict'
import * as http from 'node:http'
import { after, before, beforeEach, describe, test } from 'node:test'

import { MaestroD, isMaestroError, supportsStatusBar } from '../dist/esm/index.js'

/** @type {{method: string, url: string, headers: http.IncomingHttpHeaders, body: any}[]} */
let requests = []
/** Answers by command name: the step result's data, or an error body. */
let answers = {}
let installAnswer = {}

const ok = (extra) => ({ ok: true, exitCode: 0, ...extra })
const CAPS = { teamstation: { class: 'ipad', features: ['camera'] }, anything: [1, 2] }
const DEVICE = { id: 'mock-1', platform: 'android', kind: 'device', ready: true, attached: true, capabilities: CAPS }

function handler(req, res) {
  let data = ''
  req.on('data', (c) => (data += c))
  req.on('end', () => {
    const body = data ? JSON.parse(data) : undefined
    const url = new URL(req.url, 'http://x')
    requests.push({ method: req.method, url: req.url, headers: req.headers, body })
    const send = (status, payload) => {
      res.writeHead(status, { 'Content-Type': 'application/json' })
      res.end(JSON.stringify(payload))
    }
    if (url.pathname === '/v1/status') {
      return send(200, ok({ daemon: { name: 'hub', pid: 1, apiVersion: '1', version: 't', hub: 'hub' }, devices: [] }))
    }
    if (url.pathname === '/v1/devices' && req.method === 'GET') return send(200, ok({ devices: [DEVICE] }))
    if (url.pathname === '/v1/devices' && req.method === 'POST') {
      return send(200, ok({ device: { id: `new@${body.hub ?? 'hub'}`, platform: body.platform, ready: true, attached: false } }))
    }
    if (url.pathname === '/v1/leases' && req.method === 'POST') {
      return send(200, ok({ lease: { id: 'lease-1', device: 'mock-1', expiresAt: '2099-01-01T00:00:00Z', ttlSec: 600 }, device: DEVICE }))
    }
    if (url.pathname === '/v1/devices/mock-1' && req.method === 'GET') return send(200, ok({ device: DEVICE }))
    if (url.pathname.startsWith('/v1/leases/')) return send(200, ok({}))
    if (url.pathname.endsWith('/attach')) return send(200, ok({ device: DEVICE }))
    if (url.pathname.endsWith('/apps')) return send(200, ok({ device: 'mock-1', ...installAnswer }))
    const m = /\/commands\/([^/]+)$/.exec(url.pathname)
    if (m) {
      const a = answers[m[1]]
      if (a?.error) return send(a.status ?? 422, { ok: false, exitCode: a.exitCode ?? 2, error: a.error })
      return send(200, ok({ device: 'mock-1', type: m[1], index: 0, success: true, durationMs: 1, artifacts: {}, data: a?.data }))
    }
    return send(404, { ok: false, exitCode: 2, error: { code: 'USAGE', message: `no route ${url.pathname}` } })
  })
}

/** @type {http.Server} */
let server
let addr = ''

before(async () => {
  server = http.createServer(handler)
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve))
  addr = `127.0.0.1:${server.address().port}`
})

after(() => server?.close())

beforeEach(() => {
  requests = []
  answers = {}
  installAnswer = {}
})

const connect = () => MaestroD.connect({ hub: addr, token: 'tk' })

describe('installApp({url})', () => {
  test('posts the url, headers and app id without uploading, and reads cached and etag', async () => {
    const md = await connect()
    const dev = await md.attach({ device: 'mock-1' })
    installAnswer = { appId: 'app.teamstation.develop', cached: true, etag: '"abc"' }
    const res = await dev.installApp({
      url: 'https://mason.example/api/v2/builds/7/download',
      headers: { Authorization: 'Bearer secret' },
      appId: 'app.teamstation.develop',
    })
    assert.deepEqual(res, { appId: 'app.teamstation.develop', cached: true, etag: '"abc"' })
    const posts = requests.filter((r) => r.url.endsWith('/apps'))
    assert.equal(posts.length, 1)
    assert.deepEqual(posts[0].body, {
      url: 'https://mason.example/api/v2/builds/7/download',
      headers: { Authorization: 'Bearer secret' },
      appId: 'app.teamstation.develop',
    })
    assert.match(posts[0].headers['x-maestro-request-id'], /^[0-9a-f-]{36}$/)
    // Nothing was uploaded: no blob route was asked.
    assert.equal(requests.filter((r) => r.url.includes('/blobs/')).length, 0)
    md.close()
  })

  test('a fresh install answers what the daemon says, reinstall and platform included', async () => {
    const md = await connect()
    const dev = await md.attach({ device: 'mock-1' })
    installAnswer = { etag: '"e2"' }
    assert.deepEqual(await dev.installApp({ url: 'https://x.example/a.apk', reinstall: true, platform: 'android' }), { etag: '"e2"' })
    const post = requests.find((r) => r.url.endsWith('/apps'))
    assert.deepEqual(post.body, { url: 'https://x.example/a.apk', reinstall: true, platform: 'android' })
    installAnswer = {}
    assert.deepEqual(await dev.installApp({ url: 'https://x.example/a.apk' }), {})
    await assert.rejects(() => dev.installApp({ url: '' }), TypeError)
    md.close()
  })
})

describe('attach ai keys', () => {
  test('attach sends ai, and no returned object keeps it', async () => {
    const md = await connect()
    const ai = { jevKey: 'jev-SECRET', anthropicKey: 'sk-ant-SECRET' }
    const opts = { device: 'mock-1', ai }
    const dev = await md.attach(opts)
    const att = requests.find((r) => r.url.endsWith('/attach'))
    assert.deepEqual(att.body.ai, ai)
    assert.equal(att.body.device, undefined)
    assert.ok(!JSON.stringify(dev.info).includes('SECRET'))
    assert.ok(!JSON.stringify(Object.entries(dev)).includes('SECRET'))
    assert.ok(!JSON.stringify(Object.entries(md)).includes('SECRET'))
    md.close()
  })

  test('a lease with attach sends ai in the lease body', async () => {
    const md = await connect()
    const dev = await md.lease({ device: 'mock-1', attach: { ai: { anthropicKey: 'sk-ant-SECRET' } } })
    const lease = requests.find((r) => r.url === '/v1/leases' && r.method === 'POST')
    assert.deepEqual(lease.body.attach.ai, { anthropicKey: 'sk-ant-SECRET' })
    assert.ok(!JSON.stringify(dev.info).includes('SECRET'))
    await dev.release()
    md.close()
  })
})

describe('capabilities', () => {
  test('a device listing and an attach carry capabilities untouched', async () => {
    const md = await connect()
    const [d] = await md.devices()
    assert.deepEqual(d.capabilities, CAPS)
    const dev = await md.attach({ device: 'mock-1' })
    assert.deepEqual(dev.info.capabilities, CAPS)
    md.close()
  })
})

describe('supportsStatusBar', () => {
  const table = [
    [{ platform: 'ios', kind: 'simulator' }, true],
    [{ platform: 'tvos', kind: 'simulator' }, true],
    [{ platform: 'ios', kind: 'device' }, false],
    [{ platform: 'ios' }, false],
    [{ platform: 'tvos', kind: 'tv' }, false],
    [{ platform: 'android', kind: 'emulator' }, true],
    [{ platform: 'android', kind: 'device' }, true],
    [{ platform: 'Android' }, true],
    [{ platform: 'android', kind: 'cloud' }, false],
    [{ platform: 'web' }, false],
    [{ platform: 'macos', kind: 'computer' }, false],
    [{ platform: 'mock' }, false],
  ]
  for (const [info, want] of table) {
    test(`${info.platform}/${info.kind ?? '-'} → ${want}`, () => {
      assert.equal(supportsStatusBar(info), want)
      assert.equal(supportsStatusBar({ info }), want)
    })
  }

  test('a device handle answers for its own record', async () => {
    const md = await connect()
    const dev = await md.attach({ device: 'mock-1' })
    assert.equal(dev.supportsStatusBar(), true)
    dev.info = { ...dev.info, platform: 'ios', kind: 'device' }
    assert.equal(dev.supportsStatusBar(), false)
    md.close()
  })
})

describe('device step data', () => {
  test('setStatusBar sends its fields; on a physical iOS device it throws UNSUPPORTED', async () => {
    const md = await connect()
    const dev = await md.attach({ device: 'mock-1' })
    await dev.setStatusBar({ mode: 'demo', time: '9:41', battery: 100 })
    const cmd = requests.find((r) => r.url.endsWith('/commands/setStatusBar'))
    assert.deepEqual(cmd.body, { mode: 'demo', time: '9:41', battery: 100 })
    await dev.setStatusBar('live')
    assert.equal(requests.filter((r) => r.url.endsWith('/commands/setStatusBar')).at(-1).body, 'live')
    answers.setStatusBar = { status: 501, exitCode: 6, error: { code: 'UNSUPPORTED', message: 'status bar override on a physical iOS device' } }
    await assert.rejects(
      () => dev.setStatusBar('demo'),
      (err) => isMaestroError(err, 'UNSUPPORTED') && err.message.includes('status bar override on a physical iOS device'),
    )
    md.close()
  })

  test('keyboardVisible, the samplers and the AI checks return their data', async () => {
    const md = await connect()
    const dev = await md.attach({ device: 'mock-1' })
    answers.keyboardVisible = { data: { visible: true } }
    assert.deepEqual((await dev.keyboardVisible()).data, { visible: true })
    answers.startSampler = { data: { kind: 'cpu', appId: 'app.x', intervalMs: 500 } }
    assert.deepEqual((await dev.startSampler({ kind: 'cpu', intervalMs: 500 })).data, { kind: 'cpu', appId: 'app.x', intervalMs: 500 })
    const stopped = {
      kind: 'cpu', unit: 'percent', appId: 'app.x', intervalMs: 500, durationMs: 1000,
      count: 2, min: 1, max: 3, mean: 2, samples: [{ atMs: 0, value: 1 }, { atMs: 500, value: 3 }],
    }
    answers.stopSampler = { data: stopped }
    assert.deepEqual((await dev.stopSampler('cpu')).data, stopped)
    assert.equal(requests.filter((r) => r.url.endsWith('/commands/stopSampler'))[0].body, 'cpu')
    const verdict = {
      verdict: 'fail', confidence: 0.9, model: 'claude-haiku-5-5', observed: 'a sign-in form', costUsd: 0.002,
      escalations: [{ model: 'jev-1.14.0', confidence: 0.5, costUsd: 0.0001 }, { model: 'claude-haiku-5-5', verdict: 'fail', confidence: 0.9, costUsd: 0.0019 }],
    }
    answers.assertWithAI = { data: verdict }
    const res = await dev.assertWithAI({ assertion: 'A baseball field', budgetUsd: 0.1, thresholds: { jevPass: 1, haiku: 1.01 } })
    // A fail is a successful step: the caller decides what it means.
    assert.deepEqual(res.data, verdict)
    assert.deepEqual(requests.find((r) => r.url.endsWith('/commands/assertWithAI')).body, {
      assertion: 'A baseball field', budgetUsd: 0.1, thresholds: { jevPass: 1, haiku: 1.01 },
    })
    answers.assertNoDefectsWithAI = { data: { ...verdict, verdict: 'pass' } }
    assert.equal((await dev.assertNoDefectsWithAI()).data.verdict, 'pass')
    md.close()
  })
})

describe('createDevice hub', () => {
  test('an Android create names its hub beside the spec, as iOS does', async () => {
    const md = await connect()
    const a = await md.createDevice({ platform: 'android', name: 'ci-pixel', profile: 'pixel_8', hub: 'kefir', boot: false })
    assert.equal(a.id, 'new@kefir')
    const ios = await md.createDevice({ platform: 'ios', name: 'sim', hub: 'milk' })
    assert.equal(ios.id, 'new@milk')
    const posts = requests.filter((r) => r.url === '/v1/devices' && r.method === 'POST')
    assert.deepEqual(posts[0].body, { platform: 'android', name: 'ci-pixel', hub: 'kefir', boot: false, create: { name: 'ci-pixel', profile: 'pixel_8' } })
    assert.equal(posts[1].body.hub, 'milk')
    assert.equal(posts[1].body.create.hub, undefined)
    await md.createDevice({ platform: 'android', name: 'local' })
    assert.equal(requests.filter((r) => r.url === '/v1/devices' && r.method === 'POST')[2].body.hub, undefined)
    await md.startDevice({ name: 'avd:ci-pixel', hub: 'kefir' })
    assert.equal(requests.filter((r) => r.url === '/v1/devices' && r.method === 'POST')[3].body.hub, 'kefir')
    md.close()
  })
})
