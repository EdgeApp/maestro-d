// The hub option, request ids and retries, TLS trust roots, and the error
// code tables, against a mock daemon: an HTTP server that records each
// request and can drop a connection or answer as a gateway would.

import assert from 'node:assert/strict'
import { execFileSync } from 'node:child_process'
import * as fs from 'node:fs'
import * as http from 'node:http'
import * as https from 'node:https'
import * as os from 'node:os'
import * as path from 'node:path'
import { after, before, beforeEach, describe, test } from 'node:test'

import { EXIT_CODES, HTTP_STATUS, MaestroD, codeFromStatus, hubURL, isMaestroError } from '../dist/esm/index.js'
import { isRetryableError, isRetryableResponse } from '../dist/esm/transport.js'

/** @type {{method: string, url: string, headers: http.IncomingHttpHeaders, body: any}[]} */
let requests = []
/** What the next step-running requests do: 'drop' | 'peer' | 'gateway' | 'device-error'. */
let plan = []

const ok = (extra) => ({ ok: true, exitCode: 0, ...extra })
const DEVICE = { id: 'mock-1', platform: 'mock', ready: true, attached: true }

function handler(req, res) {
  let data = ''
  req.on('data', (c) => (data += c))
  req.on('end', () => {
    const body = data ? JSON.parse(data) : undefined
    const url = new URL(req.url, 'http://x')
    requests.push({ method: req.method, url: req.url, headers: req.headers, body })
    const send = (status, payload) => {
      res.writeHead(status, { 'Content-Type': 'application/json' })
      res.end(typeof payload === 'string' ? payload : JSON.stringify(payload))
    }
    if (url.pathname === '/v1/events') {
      res.writeHead(200, { 'Content-Type': 'text/event-stream' })
      return res.end('id: 1\nevent: step\ndata: {"type":"step","device":"mock-1","seq":1}\n\n')
    }
    if (url.pathname === '/v1/status') return send(200, ok({ daemon: { name: 'hub', pid: 1, apiVersion: '1', version: 't' }, devices: [] }))
    if (url.pathname.endsWith('/attach')) return send(200, ok({ device: DEVICE }))
    const next = /\/(commands|steps|flows)/.test(url.pathname) ? plan.shift() : undefined
    if (next === 'drop') return req.socket.destroy()
    if (next === 'peer') return send(503, { ok: false, exitCode: 3, error: { code: 'PEER_UNAVAILABLE', message: 'hub-a is down', details: { hub: 'hub-a' } } })
    if (next === 'gateway') return send(502, '<html>bad gateway</html>')
    if (next === 'device-error') return send(502, { ok: false, exitCode: 4, error: { code: 'DEVICE_ERROR', message: 'driver died' } })
    if (url.pathname.endsWith('/steps')) return send(200, ok({ device: 'mock-1', results: [], passed: 0, failed: 0, skipped: 0 }))
    return send(200, ok({ device: 'mock-1', step: 'x', type: 'x', index: 0, durationMs: 0, artifacts: {} }))
  })
}

/** @type {http.Server} */
let server
let addr = ''
const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'maestro-d-hub-'))

before(async () => {
  server = http.createServer(handler)
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve))
  addr = `127.0.0.1:${server.address().port}`
})

after(() => {
  server?.close()
  fs.rmSync(tmp, { recursive: true, force: true })
})

beforeEach(() => {
  requests = []
  plan = []
  delete process.env.MAESTRO_D_HUB
  delete process.env.MAESTRO_D_TOKEN
  delete process.env.MAESTRO_D_CA
})

const stepRequests = () => requests.filter((r) => /\/(commands|steps|flows)/.test(r.url))

describe('connect({hub})', () => {
  test('sends the token, no cwd, and a request id on every step-running call', async () => {
    const md = await MaestroD.connect({ hub: addr, token: 'tk' })
    assert.equal(md.hub, true)
    assert.equal(md.name, `http://${addr}`)
    const dev = await md.attach({ device: 'mock-1' })
    await dev.tapOn('Login')
    await dev.run([{ tapOn: 'A' }])
    await dev.runYaml('- tapOn: B\n')
    await dev.flow({ yaml: 'appId: x\n---\n- tapOn: C\n' })
    const flowFile = path.join(tmp, 'flow.yaml')
    fs.writeFileSync(flowFile, 'appId: x\n---\n- tapOn: D\n')
    await dev.flow({ file: flowFile })
    for (const r of requests) assert.equal(r.headers.authorization, 'Bearer tk')
    const steps = stepRequests()
    assert.equal(steps.length, 5)
    assert.ok(!steps[0].url.includes('cwd='), steps[0].url)
    for (const r of steps.slice(1)) assert.equal(r.body.cwd, undefined)
    assert.equal(steps[4].body.file, undefined)
    assert.match(steps[4].body.yaml, /tapOn: D/)
    const ids = steps.map((r) => r.headers['x-maestro-request-id'])
    assert.equal(new Set(ids).size, 5)
    for (const id of ids) assert.match(id, /^[0-9a-f-]{36}$/)
    await assert.rejects(() => dev.flow({ file: path.join(tmp, 'missing.yaml') }), (err) => isMaestroError(err, 'USAGE'))
    md.close()
  })

  test('MAESTRO_D_HUB and MAESTRO_D_TOKEN target a hub when no option does', async () => {
    process.env.MAESTRO_D_HUB = addr
    process.env.MAESTRO_D_TOKEN = 'envtok'
    const md = await MaestroD.connect({ name: 'ignored-once-targeted' })
    assert.equal(md.hub, true)
    const dev = await md.attach({ device: 'mock-1' })
    await dev.tapOn('X')
    assert.equal(requests.at(-1).headers.authorization, 'Bearer envtok')
    assert.ok(!requests.at(-1).url.includes('cwd='))
    md.close()
  })

  test('an explicit url wins over MAESTRO_D_HUB and keeps cwd', async () => {
    process.env.MAESTRO_D_HUB = '203.0.113.1:1'
    const md = await MaestroD.connect({ url: `http://${addr}` })
    assert.equal(md.hub, false)
    const dev = await md.attach({ device: 'mock-1' })
    await dev.tapOn('X')
    await dev.run(['back'])
    assert.ok(stepRequests()[0].url.includes('cwd='))
    assert.equal(stepRequests()[1].body.cwd, process.cwd())
    md.close()
  })

  test('hubURL', () => {
    assert.equal(hubURL(true), 'http://localhost:7433')
    assert.equal(hubURL('h'), 'http://h:7433')
    assert.equal(hubURL('https://h:9'), 'https://h:9')
    assert.equal(hubURL('[::1]:9'), 'http://[::1]:9')
    assert.equal(hubURL('::1'), 'http://[::1]:7433')
    assert.throws(() => hubURL('ftp://h'), TypeError)
    assert.throws(() => hubURL('h/x'), TypeError)
  })

  test('a missing ca file is USAGE', async () => {
    await assert.rejects(() => MaestroD.connect({ hub: addr, ca: path.join(tmp, 'nope.pem') }), (err) => isMaestroError(err, 'USAGE'))
  })
})

describe('retry with the same request id', () => {
  test('after a dropped connection, PEER_UNAVAILABLE and a gateway error page', async () => {
    const md = await MaestroD.connect({ hub: addr })
    const dev = await md.attach({ device: 'mock-1' })
    plan = ['drop', 'peer', 'gateway']
    const res = await dev.tapOn('Login')
    assert.equal(res.ok, true)
    const steps = stepRequests()
    assert.equal(steps.length, 4)
    assert.equal(new Set(steps.map((r) => r.headers['x-maestro-request-id'])).size, 1)
    md.close()
  })

  test("a daemon's own DEVICE_ERROR is final", async () => {
    const md = await MaestroD.connect({ hub: addr })
    const dev = await md.attach({ device: 'mock-1' })
    plan = ['device-error']
    await assert.rejects(() => dev.tapOn('X'), (err) => isMaestroError(err, 'DEVICE_ERROR'))
    assert.equal(stepRequests().length, 1)
    md.close()
  })

  test('gives up after three retries', async () => {
    const md = await MaestroD.connect({ hub: addr })
    const dev = await md.attach({ device: 'mock-1' })
    plan = ['peer', 'peer', 'peer', 'peer']
    await assert.rejects(() => dev.tapOn('X'), (err) => isMaestroError(err, 'PEER_UNAVAILABLE') && err.exitCode === 3)
    assert.equal(stepRequests().length, 4)
    md.close()
  })

  test('classification', () => {
    assert.equal(isRetryableError(Object.assign(new Error('x'), { code: 'ECONNRESET' })), true)
    assert.equal(isRetryableError(Object.assign(new Error('x'), { code: 'ECONNREFUSED' })), true)
    assert.equal(isRetryableError(new Error('socket hang up')), true)
    assert.equal(isRetryableError(Object.assign(new Error('x'), { code: 'DEPTH_ZERO_SELF_SIGNED_CERT' })), false)
    assert.equal(isRetryableError(undefined), false)
    assert.equal(isRetryableResponse(504, ''), true)
    assert.equal(isRetryableResponse(503, 'busy'), false)
    assert.equal(isRetryableResponse(200, ''), false)
  })
})

describe('error codes', () => {
  // The same table as pkg/daemon TestCodes_Table.
  const TABLE = {
    COMMAND_FAILED: [1, 422], USAGE: [2, 400], DAEMON_UNAVAILABLE: [3, 503], DEVICE_ERROR: [4, 502], BUSY: [5, 409],
    DEVICE_EXTERNAL: [5, 409], DEVICE_IN_USE: [5, 409], DEVICE_NOT_FOUND: [4, 404], DEVICE_NOT_ATTACHED: [2, 404],
    INTERRUPTED: [130, 499], INTERNAL: [4, 500], FORBIDDEN: [2, 403], PEER_UNAVAILABLE: [3, 503], HUB_NAME_TAKEN: [5, 409],
    HUB_VERSION: [5, 409], LEASE_REQUIRED: [5, 409], LEASE_EXPIRED: [5, 409],
  }
  test('EXIT_CODES and HTTP_STATUS agree with the daemon for every code', () => {
    assert.deepEqual(Object.keys(EXIT_CODES).sort(), Object.keys(TABLE).sort())
    assert.deepEqual(Object.keys(HTTP_STATUS).sort(), Object.keys(TABLE).sort())
    for (const [code, [exit, status]] of Object.entries(TABLE)) {
      assert.equal(EXIT_CODES[code], exit, code)
      assert.equal(HTTP_STATUS[code], status, code)
    }
  })
  test('codeFromStatus', () => {
    const want = { 422: 'COMMAND_FAILED', 400: 'USAGE', 401: 'USAGE', 403: 'FORBIDDEN', 503: 'DAEMON_UNAVAILABLE', 502: 'DEVICE_ERROR',
      409: 'BUSY', 404: 'DEVICE_NOT_FOUND', 418: 'INTERNAL' }
    for (const [status, code] of Object.entries(want)) assert.equal(codeFromStatus(Number(status)), code, status)
  })
})

// A self-signed certificate for 127.0.0.1, made with openssl when it is
// installed.
function selfSigned() {
  try {
    const key = path.join(tmp, 'key.pem')
    const cert = path.join(tmp, 'cert.pem')
    execFileSync('openssl', ['req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-subj', '/CN=127.0.0.1', '-addext',
      'subjectAltName=IP:127.0.0.1', '-keyout', key, '-out', cert, '-days', '1'], { stdio: 'ignore' })
    return { key: fs.readFileSync(key), cert: fs.readFileSync(cert), certPath: cert }
  } catch {
    return undefined
  }
}

describe('TLS', () => {
  const pair = selfSigned()
  test('an https hub with a ca trust root, and without one', { skip: !pair && 'openssl is not installed' }, async () => {
    const tlsServer = https.createServer({ key: pair.key, cert: pair.cert }, handler)
    await new Promise((resolve) => tlsServer.listen(0, '127.0.0.1', resolve))
    const hub = `https://127.0.0.1:${tlsServer.address().port}`
    try {
      const md = await MaestroD.connect({ hub, ca: pair.certPath })
      const dev = await md.attach({ device: 'mock-1' })
      await dev.tapOn('X')
      // The event stream trusts the same ca as every other call.
      const events = []
      for await (const ev of md.events()) {
        events.push(ev)
        break
      }
      assert.equal(events[0]?.device, 'mock-1')
      md.close()
      process.env.MAESTRO_D_CA = pair.certPath
      const viaEnv = await MaestroD.connect({ hub })
      viaEnv.close()
      delete process.env.MAESTRO_D_CA
      const pem = await MaestroD.connect({ hub, ca: pair.cert.toString() })
      pem.close()
      await assert.rejects(() => MaestroD.connect({ hub }), (err) => isMaestroError(err, 'DAEMON_UNAVAILABLE') && err.exitCode === 3)
    } finally {
      tlsServer.close()
    }
  })
})
