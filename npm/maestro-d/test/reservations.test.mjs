// reserve(), claim() and the multi-lease leases.json, against a mock hub:
// an HTTP server that records each request and answers as a hub would.

import assert from 'node:assert/strict'
import * as fs from 'node:fs'
import * as http from 'node:http'
import * as os from 'node:os'
import * as path from 'node:path'
import { after, before, beforeEach, describe, test } from 'node:test'

import { MaestroD, MaestroError, isMaestroError } from '../dist/esm/index.js'
import { currentLease, findLease, leasesPath, loadLeases, recordLease } from '../dist/esm/leasefile.js'
import { isRetryableError } from '../dist/esm/transport.js'

const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'maestro-d-res-'))
process.env.MAESTRO_D_HOME = path.join(tmp, 'home')

/** @type {{method: string, url: string, headers: http.IncomingHttpHeaders, body: any}[]} */
let requests = []
/** The lease GET /v1/leases/{id} answers with. */
let leaseState = 'scheduled'
/** True to answer POST /v1/reservations as a hub that predates it. */
let noRoute = false
/** True to fail the next attaches. */
let failAttach = false

const ok = (extra) => ({ ok: true, exitCode: 0, ...extra })
const LEASE = {
  id: 'res-1',
  device: 'mock-1',
  holder: 'me',
  startsAt: '2099-01-01T00:00:00Z',
  expiresAt: '2099-01-01T01:00:00Z',
  ttlSec: 3600,
}

function handler(req, res) {
  let data = ''
  req.on('data', (c) => (data += c))
  req.on('end', () => {
    const body = data ? JSON.parse(data) : undefined
    const url = new URL(req.url, 'http://x')
    requests.push({ method: req.method, url: req.url, headers: req.headers, body })
    const send = (status, payload) => {
      res.writeHead(status, { 'Content-Type': 'application/json', ...(payload.error ? { 'X-Maestro-Error-Code': payload.error.code } : {}) })
      res.end(JSON.stringify(payload))
    }
    if (url.pathname === '/v1/status') {
      return send(200, ok({ daemon: { name: 'hub', pid: 1, apiVersion: '1', version: 't', hub: 'hub' }, devices: [] }))
    }
    if (url.pathname === '/v1/reservations') {
      if (noRoute) return send(400, { ok: false, exitCode: 2, error: { code: 'USAGE', message: 'no route for POST /v1/reservations' } })
      return send(200, ok({ lease: { ...LEASE, startsAt: body.startAt, state: 'scheduled' } }))
    }
    if (url.pathname.startsWith('/v1/leases/') && req.method === 'GET') {
      const id = decodeURIComponent(url.pathname.split('/').pop())
      return send(200, ok({ lease: { ...LEASE, id, state: leaseState, startsAt: leaseState === 'active' ? '2020-01-01T00:00:00Z' : LEASE.startsAt } }))
    }
    if (url.pathname.startsWith('/v1/leases/') && req.method === 'DELETE') return send(200, ok({ lease: LEASE }))
    if (url.pathname === '/v1/devices/mock-1') return send(200, ok({ device: { id: 'mock-1', platform: 'mock', ready: true, attached: false } }))
    if (url.pathname.endsWith('/attach')) {
      if (failAttach) return send(502, { ok: false, exitCode: 4, error: { code: 'DEVICE_ERROR', message: 'driver died' } })
      return send(200, ok({ device: { id: 'mock-1', platform: 'mock', ready: true, attached: true } }))
    }
    if (url.pathname.includes('/commands/')) {
      const lease = req.headers['x-maestro-lease']
      if (lease === 'later') {
        return send(409, { ok: false, exitCode: 5, error: { code: 'LEASE_NOT_STARTED', message: 'starts later', details: { startsAt: '2099-01-01T00:00:00Z' } } })
      }
      return send(200, ok({ device: 'mock-1', step: 'x', type: 'x', index: 0, durationMs: 0, artifacts: {} }))
    }
    return send(404, { ok: false, exitCode: 4, error: { code: 'DEVICE_NOT_FOUND', message: req.url } })
  })
}

let server
let addr = ''
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
  leaseState = 'scheduled'
  noRoute = false
  fs.rmSync(leasesPath(), { force: true })
})

const leaseRequests = (method) => requests.filter((r) => r.method === method && r.url.startsWith('/v1/leases/'))

describe('reserve()', () => {
  test('sends startAt and this machine’s clock, records the lease, and attaches nothing', async () => {
    const md = await MaestroD.connect({ hub: addr, token: 't' })
    const before = Date.now()
    const lease = await md.reserve({ device: 'mock-1', startAt: new Date('2099-01-01T00:00:00.123Z'), ttlSec: 3600 })
    assert.equal(lease.state, 'scheduled')
    const r = requests.find((x) => x.url === '/v1/reservations')
    assert.equal(r.body.startAt, '2099-01-01T00:00:00Z')
    assert.equal(r.body.device, 'mock-1')
    assert.equal(r.body.ttlSec, 3600)
    const client = Date.parse(r.body.clientTime)
    assert.ok(client >= before - 1000 && client <= Date.now() + 1000, r.body.clientTime)
    assert.match(r.body.clientTime, /^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ$/)
    assert.match(r.headers['x-maestro-request-id'], /^[0-9a-f-]{36}$/)
    assert.equal(requests.filter((x) => x.url.endsWith('/attach')).length, 0)
    assert.equal(findLease(md.leaseKey, 'mock-1')?.id, 'res-1')
    assert.equal(findLease(md.leaseKey, 'mock-1')?.startsAt, '2099-01-01T00:00:00Z')
    // A string start goes as given; a selector goes as select.
    await md.reserve({ select: { platform: 'ios' }, startAt: '2099-02-02T00:00:00Z' })
    const r2 = requests.filter((x) => x.url === '/v1/reservations')[1]
    assert.equal(r2.body.startAt, '2099-02-02T00:00:00Z')
    assert.deepEqual(r2.body.select, { platform: 'ios' })
    await md.close()
  })

  test('a hub that predates reservations is USAGE naming it', async () => {
    noRoute = true
    const md = await MaestroD.connect({ hub: addr, token: 't' })
    await assert.rejects(
      () => md.reserve({ device: 'mock-1', startAt: new Date(Date.now() + 3600_000) }),
      (err) => isMaestroError(err, 'USAGE') && /does not take reservations/.test(err.message) && err.details.reason === 'reservations-unsupported',
    )
    await md.close()
  })

  test('needs a device or a selector, a start, and a hub', async () => {
    const md = await MaestroD.connect({ hub: addr, token: 't' })
    await assert.rejects(() => md.reserve({ startAt: new Date() }), TypeError)
    await assert.rejects(() => md.reserve({ device: 'a', select: {}, startAt: new Date() }), TypeError)
    await assert.rejects(() => md.reserve({ device: 'a' }), TypeError)
    await md.close()
  })
})

describe('claim()', () => {
  test('a reservation that has not started is LEASE_NOT_STARTED, and nothing is held', async () => {
    const md = await MaestroD.connect({ hub: addr, token: 't' })
    await assert.rejects(
      () => md.claim('res-1'),
      (err) => isMaestroError(err, 'LEASE_NOT_STARTED') && err.details.startsAt === LEASE.startsAt,
    )
    assert.equal(requests.filter((x) => x.url.endsWith('/attach')).length, 0)
    assert.equal(findLease(md.leaseKey, 'mock-1'), undefined)
    await assert.rejects(() => md.claim(''), TypeError)
    await md.close()
  })

  test('an active lease is attached once, sent with requests, and released by close()', async () => {
    leaseState = 'active'
    const md = await MaestroD.connect({ hub: addr, token: 't' })
    const dev = await md.claim('res-1', { attach: { platform: 'mock' } })
    const attaches = requests.filter((x) => x.url.endsWith('/attach'))
    assert.equal(attaches.length, 1)
    assert.equal(attaches[0].body.platform, 'mock')
    assert.equal(attaches[0].headers['x-maestro-lease'], 'res-1')
    await dev.tapOn('x')
    assert.equal(requests.at(-1).headers['x-maestro-lease'], 'res-1')
    assert.equal(findLease(md.leaseKey, 'mock-1')?.id, 'res-1')
    await md.close()
    assert.equal(leaseRequests('DELETE').length, 1)
    assert.equal(findLease(md.leaseKey, 'mock-1'), undefined)
  })

  test('a failed attach puts back the lease the device had in this process', async () => {
    leaseState = 'active'
    const md = await MaestroD.connect({ hub: addr, token: 't' })
    const dev = await md.claim('res-1', { attach: false })
    failAttach = true
    try {
      await assert.rejects(() => md.claim('res-2', { attach: {} }), (err) => isMaestroError(err, 'DEVICE_ERROR'))
    } finally {
      failAttach = false
    }
    await dev.tapOn('x')
    assert.equal(requests.at(-1).headers['x-maestro-lease'], 'res-1')
    await md.close()
  })

  test('a request refused under one of a device\'s leases goes once more with the other on file', async () => {
    const md = await MaestroD.connect({ hub: addr, token: 't' })
    // As if this clock ran ahead: the reservation looks started, the lease
    // it follows has not ended at the hub.
    // (The reservation was recorded while pending, so the file holds both.)
    fs.mkdirSync(path.dirname(leasesPath()), { recursive: true })
    fs.writeFileSync(
      leasesPath(),
      JSON.stringify({
        leases: [
          { hub: md.leaseKey, device: 'mock-1', id: 'now', startsAt: '2020-01-01T00:00:00Z', expiresAt: '2099-01-01T00:00:00Z', ttlSec: 60 },
          { hub: md.leaseKey, device: 'mock-1', id: 'later', startsAt: '2021-01-01T00:00:00Z', expiresAt: '2099-01-01T01:00:00Z', ttlSec: 60 },
        ],
      }),
    )
    const dev = await md.attach({ device: 'mock-1' })
    assert.equal(requests.filter((x) => x.url.endsWith('/attach')).at(-1).headers['x-maestro-lease'], 'later')
    await dev.tapOn('x')
    const sent = requests.filter((x) => x.url.includes('/commands/')).map((x) => x.headers['x-maestro-lease'])
    assert.deepEqual(sent, ['later', 'now'])
    await dev.tapOn('y')
    assert.equal(requests.at(-1).headers['x-maestro-lease'], 'now')
    await md.close()
  })

  test('attach: false only takes the lease; release() ends it', async () => {
    leaseState = 'active'
    const md = await MaestroD.connect({ hub: addr, token: 't' })
    const dev = await md.claim('res-1', { attach: false })
    assert.equal(requests.filter((x) => x.url.endsWith('/attach')).length, 0)
    assert.equal(dev.info.attached, false)
    await dev.release()
    assert.equal(leaseRequests('DELETE').length, 1)
    await md.close()
  })
})

describe('leases.json with several leases per device', () => {
  test('the lease that started last, else the reservation that starts first', () => {
    const now = Date.parse('2026-11-23T12:00:00Z')
    const at = (h) => new Date(now + h * 3600_000).toISOString().replace(/\.\d{3}Z$/, 'Z')
    const recs = [
      { hub: 'h', id: 'old', device: 'd' },
      { hub: 'h', id: 'ended', device: 'd', startsAt: at(-2), expiresAt: at(-1) },
      { hub: 'h', id: 'later', device: 'd', startsAt: at(3) },
      { hub: 'h', id: 'soon', device: 'd', startsAt: at(1) },
    ]
    assert.equal(currentLease(recs, 'd', now)?.id, 'ended')
    assert.equal(currentLease(recs.slice(0, 1), 'd', now)?.id, 'old')
    assert.equal(currentLease(recs.slice(2), 'd', now)?.id, 'soon')
    assert.equal(currentLease([recs[2], recs[0]], 'd', now)?.id, 'old')
    assert.equal(currentLease(recs, 'x', now), undefined)
    assert.equal(currentLease(recs, 'd', now + 1.5 * 3600_000)?.id, 'soon')
  })

  test('recordLease replaces by id and keeps the device’s other leases; the CLI’s file reads back', () => {
    const future = new Date(Date.now() + 3600_000).toISOString().replace(/\.\d{3}Z$/, 'Z')
    recordLease('h', { id: 'a', device: 'd', expiresAt: future, ttlSec: 60 })
    recordLease('h', { id: 'b', device: 'd', startsAt: '2099-01-01T00:00:00Z', expiresAt: '2099-01-01T01:00:00Z', ttlSec: 3600 })
    recordLease('h', { id: 'a', device: 'd', expiresAt: future, ttlSec: 120 })
    const recs = loadLeases()
    assert.equal(recs.length, 2)
    assert.equal(recs.find((r) => r.id === 'a').ttlSec, 120)
    assert.equal(findLease('h', 'd')?.id, 'a')
    // The file as the Go CLI writes it (pkg/cli/lease.go leaseRecord).
    fs.writeFileSync(
      leasesPath(),
      JSON.stringify({ leases: [{ hub: 'h', device: 'g', id: 'go-1', startsAt: '2099-03-03T00:00:00Z', expiresAt: '2099-03-03T01:00:00Z', ttlSec: 3600 }] }),
    )
    const g = findLease('h', 'g')
    assert.equal(g?.id, 'go-1')
    assert.equal(g?.startsAt, '2099-03-03T00:00:00Z')
    // A started lease replaces the device's other started entries and keeps its reservations.
    recordLease('h', { id: 'old', device: 'g', startsAt: '2020-01-01T00:00:00Z', expiresAt: '2020-01-01T01:00:00Z', ttlSec: 3600 })
    recordLease('h', { id: 'new', device: 'g', startsAt: '2021-01-01T00:00:00Z', expiresAt: future, ttlSec: 3600 })
    const ids = loadLeases().filter((r) => r.device === 'g').map((r) => r.id).sort()
    assert.deepEqual(ids, ['go-1', 'new'])
  })

  test('static attach keeps connection options out of the driver request and the saved grant', async () => {
    const hub = `http://${addr}`
    recordLease(hub, { ...LEASE, startsAt: '2020-01-01T00:00:00Z' })
    const driver = { platform: 'mock', appId: 'example.app', noAppInstall: true }
    await MaestroD.attach({ url: hub, token: 'secret', log() {}, device: 'mock-1', ...driver })
    const req = requests.find(r => r.url.endsWith('/attach'))
    assert.equal(req.headers.authorization, 'Bearer secret')
    assert.deepEqual(req.body, driver)
    assert.deepEqual(findLease(hub, 'mock-1').attach, driver)
  })

  test('attach() sends the started lease on file, not a reservation; LEASE_NOT_STARTED keeps the record', async () => {
    const md = await MaestroD.connect({ hub: addr, token: 't' })
    recordLease(md.leaseKey, { id: 'now', device: 'mock-1', startsAt: '2020-01-01T00:00:00Z', expiresAt: '2099-01-01T00:00:00Z', ttlSec: 60 })
    recordLease(md.leaseKey, { id: 'later', device: 'mock-1', startsAt: '2099-01-01T00:00:00Z', expiresAt: '2099-01-01T01:00:00Z', ttlSec: 60 })
    const dev = await md.attach({ device: 'mock-1' })
    assert.equal(requests.filter((x) => x.url.endsWith('/attach')).at(-1).headers['x-maestro-lease'], 'now')
    await md.close()
    // Only the reservation on file: it is sent, and LEASE_NOT_STARTED drops nothing.
    fs.rmSync(leasesPath(), { force: true })
    const md2 = await MaestroD.connect({ hub: addr, token: 't' })
    recordLease(md2.leaseKey, { id: 'later', device: 'mock-1', startsAt: '2099-01-01T00:00:00Z', expiresAt: '2099-01-01T01:00:00Z', ttlSec: 60 })
    const dev2 = await md2.attach({ device: 'mock-1' })
    await assert.rejects(
      () => dev2.tapOn('x'),
      (err) => isMaestroError(err, 'LEASE_NOT_STARTED'),
    )
    assert.equal(findLease(md2.leaseKey, 'mock-1')?.id, 'later')
    assert.equal(isRetryableError(MaestroError.of('LEASE_NOT_STARTED', 'starts later')), false)
    await md2.close()
    void dev
  })
})
