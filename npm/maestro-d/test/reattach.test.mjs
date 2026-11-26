import assert from 'node:assert/strict'
import * as fs from 'node:fs'
import * as http from 'node:http'
import * as os from 'node:os'
import * as path from 'node:path'
import { test } from 'node:test'
import { MaestroD } from '../dist/esm/index.js'
import { loadLeases, recordLease } from '../dist/esm/leasefile.js'

test('a held emulator follows its grant and retries at most once on the verified serial', async (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'js-reattach-'))
  const prior = process.env.MAESTRO_D_HOME
  process.env.MAESTRO_D_HOME = dir
  t.after(() => {
    if (prior === undefined) delete process.env.MAESTRO_D_HOME
    else process.env.MAESTRO_D_HOME = prior
    fs.rmSync(dir, { recursive: true, force: true })
  })
  const original = 'emulator-5554@lab'
  const next = 'emulator-5556@lab'
  const lease = { id: 'original@lab', device: original, state: 'active', ttlSec: 900, expiresAt: new Date(Date.now() + 900000).toISOString() }
  const sibling = { ...lease, id: 'other@lab', device: next }
  const info = (id) => ({ id, platform: 'android', kind: 'emulator', ready: true, attached: true })
  let active = true
  let siblingActive = true
  let failCommand = 0
  let expireOnLookup = false
  const access = []
  const server = http.createServer(async (req, res) => {
    const route = decodeURIComponent(new URL(req.url, 'http://localhost').pathname)
    const chunks = []
    for await (const c of req) chunks.push(c)
    const body = chunks.length ? JSON.parse(Buffer.concat(chunks)) : {}
    const answer = (status, data) => {
      res.writeHead(status, { 'content-type': 'application/json' })
      res.end(JSON.stringify({ ok: status === 200, exitCode: status === 200 ? 0 : 5, ...data }))
    }
    if (route === '/v1/status') return answer(200, { daemon: { name: 'hub', hub: 'lab' }, devices: [] })
    if (route === '/v1/leases') {
      const granted = body.select?.name === 'Replacement' ? { ...lease, id: 'replacement@lab', device: original } : lease
      return answer(200, { lease: granted, device: info(granted.device) })
    }
    if (route === '/v1/leases/replacement@lab' && req.method === 'DELETE') return answer(200, {})
    if (route === '/v1/leases/' + sibling.id) return siblingActive
      ? answer(200, { lease: sibling })
      : answer(409, { error: { code: 'LEASE_EXPIRED', message: 'gone' } })
    if (route === '/v1/leases/' + lease.id) {
      if (req.method === 'DELETE') return answer(200, { lease })
      if (!active) return answer(410, { error: { code: 'LEASE_EXPIRED', message: 'gone' } })
      return answer(200, { lease })
    }
    const match = route.match(/^\/v1\/devices\/([^/]+)(.*)$/)
    if (!match) return answer(404, {})
    access.push({ id: match[1], path: match[2], grant: req.headers['x-maestro-lease'], body })
    if (match[2] === '/detach') return answer(404, { error: { code: 'DEVICE_NOT_ATTACHED', message: 'already closed' } })
    if (match[2] === '/attach') return answer(200, { device: info(match[1]) })
    if (match[2].startsWith('/commands/')) {
      if (expireOnLookup) active = false
      if (failCommand-- > 0) return answer(409, { error: { code: 'DEVICE_NOT_ATTACHED', message: 'detached' } })
      return answer(200, { step: 'tapOn' })
    }
    return answer(200, { device: info(match[1]) })
  })
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve))
  t.after(() => new Promise(resolve => server.close(resolve)))
  const hub = '127.0.0.1:' + server.address().port
  const md = await MaestroD.connect({ hub })
  const d = await md.lease({ select: { platform: 'android' }, attach: { appId: 'fixture.checkout' } })
  recordLease(md.leaseKey, sibling, { appId: 'sibling.app' })
  const window = d.lease.expiresAt
  lease.device = ''
  await assert.rejects(d.tapOn('absent'), e => e.code === 'DEVICE_NOT_FOUND')
  assert.deepEqual(access, [])
  lease.device = next
  const replacement = await md.lease({ select: { name: 'Replacement' }, attach: false })
  assert.equal(replacement.id, original)
  assert.deepEqual(loadLeases().find(r => r.id === d.lease.id)?.attach, { appId: 'fixture.checkout' })
  await replacement.release()
  sibling.device = original
  failCommand = 1
  assert.equal((await d.tapOn('verified')).ok, true)
  assert.deepEqual(access.map(a => a.path), ['/commands/tapOn', '/attach', '/commands/tapOn'])
  assert.deepEqual(access[1].body, { appId: 'fixture.checkout' })
  assert.ok(access.every(a => a.id === next && a.grant === lease.id))
  assert.equal(d.id, next)
  assert.equal(d.lease.expiresAt, window)
  const saved = JSON.parse(fs.readFileSync(path.join(dir, 'leases.json'))).leases.find(r => r.id === lease.id)
  assert.deepEqual([saved.device, saved.originalDevice, saved.id, saved.expiresAt], [next, original, lease.id, window])
  access.length = 0
  const other = await MaestroD.connect({ hub })
  // An older window for the same original target now sorts before the live
  // grant by current serial. Its earlier start must lose across both records.
  recordLease(md.leaseKey, { ...lease, id: 'past@lab', device: original, startsAt: new Date(Date.now() - 60000).toISOString() })
  recordLease(md.leaseKey, { ...lease, id: 'past@lab', device: 'emulator-5550@lab', startsAt: new Date(Date.now() - 60000).toISOString() })
  recordLease(md.leaseKey, { ...lease, startsAt: new Date(Date.now() - 30000).toISOString() })
  const alias = await other.attach({ device: original, appId: 'fixture.checkout' })
  assert.equal(alias.id, next)
  assert.equal(access.length, 1)
  assert.equal(access[0].id, next)
  const swapped = await other.attach({ device: next, appId: 'sibling.app' })
  assert.equal(swapped.id, original)
  assert.equal(access.at(-1).grant, sibling.id)
  siblingActive = false
  await assert.rejects(other.attach({ device: next }), e => e.code === 'LEASE_EXPIRED')
  assert.ok(!loadLeases().some(r => r.id === sibling.id))
  assert.equal(other.transport.leases.get(original), undefined)
  const surviving = await other.attach({ device: next })
  assert.equal(surviving.id, next)
  assert.equal(access.at(-1).grant, lease.id)
  await other.close()
  access.length = 0
  await assert.rejects(d.detach(), e => e.code === 'DEVICE_NOT_ATTACHED')
  assert.deepEqual(access.map(a => [a.id, a.path, a.grant]), [[next, '/detach', lease.id]])
  access.length = 0
  failCommand = 2
  await assert.rejects(d.tapOn('twice'), e => e.code === 'DEVICE_NOT_ATTACHED')
  assert.deepEqual(access.map(a => a.path), ['/commands/tapOn', '/attach', '/commands/tapOn'])
  failCommand = 1
  expireOnLookup = true
  access.length = 0
  await assert.rejects(d.tapOn('expired'), e => e.code === 'LEASE_EXPIRED')
  assert.deepEqual(access.map(a => a.path), ['/commands/tapOn'])
  await md.close()
})

test('daemon lifecycle calls keep the intended grant when sibling serials swap', async (t) => {
  const home = fs.mkdtempSync(path.join(os.tmpdir(), 'js-lifecycle-'))
  const prior = process.env.MAESTRO_D_HOME
  process.env.MAESTRO_D_HOME = home
  t.after(() => {
    if (prior === undefined) delete process.env.MAESTRO_D_HOME
    else process.env.MAESTRO_D_HOME = prior
    fs.rmSync(home, { recursive: true, force: true })
  })
  const a = 'emulator-5554@lab'
  const b = 'emulator-5556@lab'
  const grants = {
    first: { id: 'first', device: a, state: 'active' },
    second: { id: 'second', device: b, state: 'active' },
  }
  const requests = []
  const info = id => ({ id, platform: 'android', ready: true, attached: true })
  let expired = false
  const server = http.createServer(async (req, res) => {
    const chunks = []
    for await (const chunk of req) chunks.push(chunk)
    const body = chunks.length ? JSON.parse(Buffer.concat(chunks)) : {}
    const url = new URL(req.url, 'http://localhost')
    const route = decodeURIComponent(url.pathname)
    const send = (status, payload) => {
      res.writeHead(status, { 'content-type': 'application/json' })
      res.end(JSON.stringify({ ok: status === 200, ...payload }))
    }
    if (route === '/v1/status') return send(200, { daemon: { name: 'hub', hub: 'lab' } })
    if (route === '/v1/leases') {
      const lease = grants[body.select.name]
      return send(200, { lease, device: info(lease.device) })
    }
    if (route.startsWith('/v1/leases/')) {
      const lease = grants[route.split('/').at(-1)]
      if (req.method === 'DELETE') return send(200, {})
      if (expired && lease.id === 'first') return send(409, { error: { code: 'LEASE_EXPIRED', message: 'expired' } })
      return send(200, { lease })
    }
    const [, , , id, action] = route.split('/')
    if (req.method !== 'GET') requests.push([req.method, id, action ?? url.search, req.headers['x-maestro-lease']])
    send(200, { device: info(id) })
  })
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve))
  t.after(() => new Promise(resolve => server.close(resolve)))
  const md = await MaestroD.connect({ hub: '127.0.0.1:' + server.address().port })
  const first = await md.lease({ select: { name: 'first' } })
  const sibling = await md.lease({ select: { name: 'second' } })
  grants.first.device = b
  grants.second.device = a
  await sibling.refresh()
  assert.equal(first.id, sibling.id)
  for (const method of ['deleteDevice', 'stopDevice', 'detach']) {
    await assert.rejects(md[method](first.id), e => e.code === 'USAGE')
  }
  assert.deepEqual(requests, [])
  grants.first.device = ''
  await assert.rejects(md.stopDevice(first), e => e.code === 'DEVICE_NOT_FOUND')
  grants.first.device = b
  grants.first.state = 'scheduled'
  await assert.rejects(md.deleteDevice(first), e => e.code === 'LEASE_NOT_STARTED')
  grants.first.state = 'active'
  // A concurrently cached sibling credential cannot override the grant
  // pinned to this request.
  md.transport.leases.set(b, 'second')
  await md.deleteDevice(first)
  await md.stopDevice(first)
  await md.detach(first)
  assert.deepEqual(requests.splice(0), [
    ['DELETE', b, '?delete=true', 'first'],
    ['DELETE', b, '', 'first'],
    ['POST', b, 'detach', 'first'],
  ])
  // An expired lookup must drop only that grant. Retrying the surviving
  // sibling's unambiguous string then follows its verified new serial.
  expired = true
  await assert.rejects(md.deleteDevice(first), e => e.code === 'LEASE_EXPIRED')
  assert.deepEqual(requests, [])
  assert.ok(!loadLeases().some(r => r.id === 'first'))
  await md.stopDevice(b)
  assert.deepEqual(requests, [['DELETE', a, '', 'second']])
  await md.close()
})
