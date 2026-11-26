import assert from 'node:assert/strict'
import * as fs from 'node:fs'
import * as http from 'node:http'
import * as os from 'node:os'
import * as path from 'node:path'
import { test } from 'node:test'
import { MaestroD } from '../dist/esm/index.js'

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
  const info = (id) => ({ id, platform: 'android', kind: 'emulator', ready: true, attached: true })
  let active = true
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
    if (route === '/v1/leases') return answer(200, { lease, device: info(original) })
    if (route === '/v1/leases/' + lease.id) {
      if (req.method === 'DELETE') return answer(200, { lease })
      if (!active) return answer(410, { error: { code: 'LEASE_EXPIRED', message: 'gone' } })
      return answer(200, { lease })
    }
    const match = route.match(/^\/v1\/devices\/([^/]+)(.*)$/)
    if (!match) return answer(404, {})
    access.push({ id: match[1], path: match[2], grant: req.headers['x-maestro-lease'], body })
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
  const window = d.lease.expiresAt
  lease.device = ''
  await assert.rejects(d.tapOn('absent'), e => e.code === 'DEVICE_NOT_FOUND')
  assert.deepEqual(access, [])
  lease.device = next
  failCommand = 1
  assert.equal((await d.tapOn('verified')).ok, true)
  assert.deepEqual(access.map(a => a.path), ['/commands/tapOn', '/attach', '/commands/tapOn'])
  assert.deepEqual(access[1].body, { appId: 'fixture.checkout' })
  assert.ok(access.every(a => a.id === next && a.grant === lease.id))
  assert.equal(d.id, next)
  assert.equal(d.lease.expiresAt, window)
  const saved = JSON.parse(fs.readFileSync(path.join(dir, 'leases.json'))).leases[0]
  assert.deepEqual([saved.device, saved.originalDevice, saved.id, saved.expiresAt], [next, original, lease.id, window])
  access.length = 0
  const other = await MaestroD.connect({ hub })
  const alias = await other.attach({ device: original, appId: 'fixture.checkout' })
  assert.equal(alias.id, next)
  assert.equal(access.length, 1)
  assert.equal(access[0].id, next)
  await other.close()
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
