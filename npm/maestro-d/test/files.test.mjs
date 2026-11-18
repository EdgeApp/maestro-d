// Files on a hub: packing, uploads (deduplicated and resumed), the file
// fields of commands, outputs fetched back, install and artifacts. A real
// daemon with the mock driver listens on TCP; the library reaches it as a
// hub through a proxy that records (and can drop) requests.

import assert from 'node:assert/strict'
import { execFileSync } from 'node:child_process'
import * as fs from 'node:fs'
import * as http from 'node:http'
import * as os from 'node:os'
import * as path from 'node:path'
import { after, before, beforeEach, describe, test } from 'node:test'
import { fileURLToPath } from 'node:url'
import * as zlib from 'node:zlib'

import { MaestroD, buildValue, isMaestroError, pack, upload } from '../dist/esm/index.js'
import { HubFiles, diffScreenshotPath, hash8, withPNG } from '../dist/esm/pack.js'
import { Transport } from '../dist/esm/transport.js'

const here = path.dirname(fileURLToPath(import.meta.url))
const repoRoot = path.resolve(here, '..', '..', '..')
const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'maestro-d-files-'))
const bin = process.env.MAESTRO_D_TEST_BIN ?? path.join(tmp, 'maestro-d')
process.env.MAESTRO_D_HOME = path.join(tmp, 'home')
process.env.MAESTRO_D_BIN = bin
for (const k of ['MAESTRO_DEVICE', 'MAESTRO_D', 'MAESTRO_D_HUB', 'MAESTRO_D_TOKEN', 'MAESTRO_D_CA']) delete process.env[k]

const work = path.join(tmp, 'work')
fs.mkdirSync(work, { recursive: true })
const file = (rel, data) => {
  const p = path.join(work, rel)
  fs.mkdirSync(path.dirname(p), { recursive: true })
  fs.writeFileSync(p, data)
  return p
}

/** A 1x1 PNG of one colour. */
function png(r, g, b, a) {
  const crcTable = Array.from({ length: 256 }, (_, n) => {
    let c = n
    for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1
    return c >>> 0
  })
  const crc = (buf) => {
    let c = 0xffffffff
    for (const x of buf) c = crcTable[(c ^ x) & 0xff] ^ (c >>> 8)
    return (c ^ 0xffffffff) >>> 0
  }
  const chunk = (type, data) => {
    const len = Buffer.alloc(4)
    len.writeUInt32BE(data.length)
    const td = Buffer.concat([Buffer.from(type), data])
    const c = Buffer.alloc(4)
    c.writeUInt32BE(crc(td))
    return Buffer.concat([len, td, c])
  }
  const ihdr = Buffer.alloc(13)
  ihdr.writeUInt32BE(1, 0)
  ihdr.writeUInt32BE(1, 4)
  ihdr[8] = 8
  ihdr[9] = 6
  return Buffer.concat([
    Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]),
    chunk('IHDR', ihdr),
    chunk('IDAT', zlib.deflateSync(Buffer.from([0, r, g, b, a]))),
    chunk('IEND', Buffer.alloc(0)),
  ])
}

/** @type {MaestroD} */ let tcp
/** @type {MaestroD} */ let hub
/** @type {http.Server} */ let proxy
/** @type {{method: string, url: string}[]} */ let seen = []
/** Drop the next PUT before forwarding it. */ let dropNextPut = false
/**
 * Faults for the next requests, in order: each answers the first request
 * of its method instead of the daemon.
 * @type {{method: string, act: (req: http.IncomingMessage, res: http.ServerResponse, port: number) => void}[]}
 */ let faults = []

before(async () => {
  if (!process.env.MAESTRO_D_TEST_BIN) execFileSync('go', ['build', '-o', bin, '.'], { cwd: repoRoot, stdio: 'inherit' })
  tcp = await MaestroD.connect({ name: 'jsfiles', http: '127.0.0.1:0', token: 'files-token', idleTimeout: 120 })
  const port = Number(tcp.info.httpAddr.split(':').pop())
  proxy = http.createServer((req, res) => {
    seen.push({ method: req.method, url: req.url, outputs: req.headers['x-maestro-client-outputs'] })
    if (faults[0]?.method === req.method) return faults.shift().act(req, res, port)
    if (req.method === 'PUT' && dropNextPut) {
      dropNextPut = false
      req.resume()
      return req.socket.destroy()
    }
    const up = http.request({ host: '127.0.0.1', port, method: req.method, path: req.url, headers: req.headers }, (ur) => {
      res.writeHead(ur.statusCode, ur.headers)
      ur.pipe(res)
    })
    req.pipe(up)
  })
  await new Promise((resolve) => proxy.listen(0, '127.0.0.1', resolve))
  hub = await MaestroD.connect({ hub: `127.0.0.1:${proxy.address().port}`, token: 'files-token' })
})

after(async () => {
  try {
    hub?.close?.()
    proxy?.close()
    await tcp?.shutdown()
  } finally {
    fs.rmSync(tmp, { recursive: true, force: true })
  }
})

beforeEach(() => {
  seen = []
  faults = []
})

/**
 * Forward only the first `bytes` of a PUT to the daemon (which keeps them
 * as a partial upload), then answer the client `status` and `body`: the
 * link to the device's hub dropped mid-upload.
 */
function cutPut(bytes, status, body) {
  return (req, res, port) => {
    const headers = { ...req.headers, 'content-length': String(bytes) }
    const up = http.request({ host: '127.0.0.1', port, method: 'PUT', path: req.url, headers }, (ur) => {
      ur.resume()
      ur.on('end', () => {
        res.writeHead(status, { 'Content-Type': typeof body === 'string' ? 'text/plain' : 'application/json' })
        res.end(typeof body === 'string' ? body : JSON.stringify(body))
      })
    })
    let left = bytes
    req.on('data', (c) => {
      if (left > 0) up.write(c.subarray(0, left))
      left -= c.length
      if (left <= 0 && !up.writableEnded) up.end()
    })
    req.on('end', () => up.writableEnded || up.end())
  }
}

const puts = () => seen.filter((r) => r.method === 'PUT').map((r) => r.url)

describe('pack', () => {
  test('a file is one unit; a directory zips the same every time', async () => {
    const f = file('p/photo.png', png(1, 2, 3, 255))
    file('p/My.app/Info.plist', 'plist')
    file('p/My.app/x/y', 'y')
    const [u, d] = await pack([f, path.join(work, 'p', 'My.app')])
    assert.equal(u.name, 'photo.png')
    assert.match(u.ref, /^blob:sha256:[0-9a-f]{64}\/photo\.png$/)
    assert.equal(d.dir, true)
    const later = new Date(Date.now() + 3600_000)
    fs.utimesSync(path.join(work, 'p', 'My.app', 'Info.plist'), later, later)
    const [d2] = await pack([path.join(work, 'p', 'My.app')])
    assert.equal(d2.sha256, d.sha256)
    await assert.rejects(() => pack([path.join(work, 'missing')]), (err) => isMaestroError(err, 'USAGE'))
    await assert.rejects(() => pack([f], { bin: path.join(work, 'p', 'photo.png') }), (err) => isMaestroError(err))
  })

  test('a bare addMedia value fills files, alone or with options', () => {
    assert.equal(buildValue('addMedia', 'a.png', {}), 'a.png')
    assert.deepEqual(buildValue('addMedia', 'a.png', { optional: true }), { files: ['a.png'], optional: true })
    assert.deepEqual(buildValue('addMedia', ['a.png', 'b.png'], { label: 'x' }), { files: ['a.png', 'b.png'], label: 'x' })
    assert.throws(() => buildValue('assertVisible', ['x'], { optional: true }), TypeError)
  })

  test('helpers', () => {
    assert.equal(withPNG('a'), 'a.png')
    assert.equal(withPNG('a.jpg'), 'a.jpg')
    assert.equal(diffScreenshotPath('dir/ref.png'), 'dir/ref_diff.png')
    assert.equal(diffScreenshotPath('ref'), 'ref_diff.png')
    assert.equal(hash8('x').length, 8)
  })
})

describe('commands on a hub', () => {
  /** @type {import('../dist/esm/index.js').MaestroDevice} */ let dev
  before(async () => {
    dev = await hub.attach({ device: 'mock-1', platform: 'mock' })
  })

  test('input files are uploaded once and sent as blob refs; inline JavaScript is not', async () => {
    file('photo.png', png(9, 9, 9, 255))
    file('b.png', png(8, 8, 8, 255))
    file('x.js', 'output.fromFile = "yes"')
    await dev.addMedia('photo.png', { cwd: work })
    assert.equal(puts().length, 1)
    seen = []
    await dev.addMedia(['photo.png', 'b.png'], { cwd: work })
    assert.equal(puts().length, 1, 'photo.png is on the hub already')
    await dev.runScript('x.js', { cwd: work })
    assert.equal(await dev.eval('output.fromFile'), 'yes')
    seen = []
    await dev.runScript('output.inline = true')
    assert.equal(puts().length, 0)
    await dev.runScript({ file: 'x.js' }, { cwd: work })
    const cmd = seen.find((r) => r.url.includes('/commands/'))
    assert.equal(cmd.outputs, '1', 'a hub client names its outputs')
  })

  test('outputs come back to the caller path, relative or absolute, with .png added', async () => {
    const r = await dev.takeScreenshot('shots/name', { cwd: work })
    assert.equal(r.outputs.length, 1)
    assert.match(r.outputs[0], /^outputs\/[0-9a-f]{8}\/name\.png$/)
    assert.ok(fs.existsSync(path.join(work, 'shots', 'name.png')))
    const abs = path.join(tmp, 'abs', 'shot.png')
    await dev.takeScreenshot({ path: abs })
    assert.ok(fs.existsSync(abs))
  })

  test('assertScreenshot: a missing reference is a baseline; a mismatch fetches its diff', async () => {
    await dev.assertScreenshot('login', { cwd: work })
    const ref = path.join(work, 'login.png')
    assert.ok(fs.existsSync(ref), 'baseline fetched')
    seen = []
    assert.equal((await dev.assertScreenshot('login', { cwd: work })).ok, true)
    assert.equal(puts().length, 1, 'the existing reference was packed and compared')
    fs.writeFileSync(ref, png(255, 0, 0, 255))
    await assert.rejects(
      () => dev.assertScreenshot({ path: 'login', thresholdPercentage: 95 }, { cwd: work }),
      (err) => isMaestroError(err, 'COMMAND_FAILED') && err.result.outputs.length === 1,
    )
    assert.ok(fs.existsSync(path.join(work, 'login_diff.png')), 'diff beside the reference')
  })

  test('install, uninstall, artifacts', async () => {
    const apk = file('app.apk', 'PK\x03\x04 apk')
    assert.deepEqual(await dev.installApp(apk, { platform: 'mock', reinstall: true }), {})
    seen = []
    await dev.installApp('app.apk', { cwd: work, platform: 'mock' })
    assert.equal(puts().length, 0, 'the second install skips the upload')
    await dev.uninstallApp('com.example')
    await assert.rejects(() => dev.installApp(''), TypeError)
    await assert.rejects(() => dev.uninstallApp(''), TypeError)
    const list = await dev.artifacts()
    const out = list.find((a) => a.path.startsWith('outputs/'))
    assert.ok(out, JSON.stringify(list))
    assert.ok((await dev.artifact(out.path)).length > 0)
    await assert.rejects(() => dev.artifact(''), TypeError)
    await assert.rejects(() => dev.artifact('../../etc/hosts'), (err) => isMaestroError(err))
  })

  test('attach on a hub uploads appFile but not an Appium capsFile', async () => {
    file('caps.json', '{}')
    file('other.apk', 'PK\x03\x04 other')
    seen = []
    // This test's daemon is a loopback serve, so the host path is its own;
    // a real hub refuses any capsFile as FORBIDDEN (covered in Go).
    const d1 = await hub.attach({ device: 'mock-2', platform: 'mock', extra: { capsFile: path.join(work, 'caps.json') } })
    assert.equal(puts().length, 0, 'the capabilities file is not uploaded')
    await d1.detach()
    seen = []
    const d2 = await hub.attach({ device: 'mock-2', platform: 'mock', appFile: path.join(work, 'other.apk') })
    assert.equal(puts().length, 1)
    await d2.detach()
  })

  test('an upload resumes after a dropped PUT', async () => {
    const big = file('big.bin', Buffer.alloc(200_000, 7))
    const [unit] = await pack([big])
    const t = new Transport({ url: `127.0.0.1:${proxy.address().port}`, token: 'files-token', hub: true })
    dropNextPut = true
    assert.equal(await upload(t, '/v1/devices/mock-1', unit), true)
    assert.equal(puts().length, 2, 'the dropped PUT was sent again')
    assert.equal(await upload(t, '/v1/devices/mock-1', unit), false, 'now on the hub')
    const bad = { ...unit, sha256: 'f'.repeat(64) }
    await assert.rejects(() => upload(t, '/v1/devices/mock-1', bad), (err) => isMaestroError(err, 'USAGE'))
    await assert.rejects(() => upload(t, '/v1/devices/mock-1', { ...unit, sha256: 'nothex' }), (err) => isMaestroError(err, 'FORBIDDEN'))
    t.close()
  })

  test('an upload resumes after the link to the device\'s hub drops mid-PUT', async () => {
    const big = file('peer.bin', Buffer.alloc(300_000, 9))
    const [unit] = await pack([big])
    const t = new Transport({ url: `127.0.0.1:${proxy.address().port}`, token: 'files-token', hub: true, retryDelays: [1, 1, 1] })
    const peerDown = { ok: false, error: { code: 'PEER_UNAVAILABLE', message: 'hub hub-b: link closed' } }
    faults = [
      { method: 'PUT', act: cutPut(100_000, 503, peerDown) },
      { method: 'HEAD', act: (_req, res) => res.writeHead(503).end() },
      { method: 'PUT', act: cutPut(50_000, 504, 'upstream timed out') },
    ]
    const ranges = []
    const listener = (req) => req.method === 'PUT' && ranges.push(req.headers['content-range'])
    proxy.on('request', listener)
    try {
      assert.equal(await upload(t, '/v1/devices/mock-1', unit), true)
    } finally {
      proxy.off('request', listener)
    }
    assert.equal(faults.length, 0, 'every fault was hit')
    assert.deepEqual(ranges, ['bytes 0-299999/300000', 'bytes 100000-299999/300000', 'bytes 150000-299999/300000'])
    assert.equal(await upload(t, '/v1/devices/mock-1', unit), false, 'now on the hub')
    t.close()
  })

  test('a daemon\'s own DEVICE_ERROR ends an upload at once', async () => {
    const [unit] = await pack([file('final.bin', 'final bytes')])
    const t = new Transport({ url: `127.0.0.1:${proxy.address().port}`, token: 'files-token', hub: true, retryDelays: [1, 1, 1] })
    const deviceErr = { ok: false, error: { code: 'DEVICE_ERROR', message: 'disk full' } }
    faults = [{ method: 'PUT', act: cutPut(0, 502, deviceErr) }]
    await assert.rejects(() => upload(t, '/v1/devices/mock-1', unit), (err) => isMaestroError(err, 'DEVICE_ERROR'))
    assert.equal(puts().length, 1)
    t.close()
  })

  test('a HEAD refusal has no body: its code comes from X-Maestro-Error-Code', async () => {
    const [unit] = await pack([file('head.bin', 'head bytes')])
    const t = new Transport({ url: `127.0.0.1:${proxy.address().port}`, token: 'files-token', hub: true, retryDelays: [] })
    const told = []
    t.onLeaseExpired = (device, lease) => told.push(`${device}=${lease}`)
    const head = (status, code) => ({ method: 'HEAD', act: (_req, res) => res.writeHead(status, code ? { 'X-Maestro-Error-Code': code } : {}).end() })
    t.leases.set('mock-1', 'gone')
    faults = [head(409, 'LEASE_EXPIRED')]
    await assert.rejects(() => upload(t, '/v1/devices/mock-1', unit), (err) => isMaestroError(err, 'LEASE_EXPIRED'))
    assert.equal(t.leases.get('mock-1'), undefined, 'the expired lease was forgotten')
    assert.deepEqual(told, ['mock-1=gone'])
    for (const [status, code, want] of [
      [409, 'LEASE_REQUIRED', 'LEASE_REQUIRED'],
      [409, 'DEVICE_IN_USE', 'DEVICE_IN_USE'],
      [409, 'NOT_A_CODE', 'BUSY'],
      [404, 'DEVICE_NOT_FOUND', 'DEVICE_NOT_FOUND'],
      [403, 'FORBIDDEN', 'FORBIDDEN'],
    ]) {
      faults = [head(status, code)]
      await assert.rejects(() => upload(t, '/v1/devices/mock-1', unit), (err) => isMaestroError(err, want), `${status} ${code}`)
    }
    assert.equal(puts().length, 0, 'nothing was uploaded')
    t.close()
  })

  test('HubFiles leaves fields alone that name no files', async () => {
    const t = new Transport({ url: `127.0.0.1:${proxy.address().port}`, token: 'files-token', hub: true })
    const h = new HubFiles(t, '/v1/devices/mock-1', work)
    assert.equal(await h.rewrite('tapOn', 'Login'), 'Login')
    assert.equal(await h.rewrite('notACommand', 'x'), 'x')
    assert.equal(await h.rewrite('addMedia', null), null)
    assert.deepEqual(await h.rewrite('addMedia', { files: [3] }), { files: [3] })
    assert.equal(await h.rewrite('takeScreenshot', true), true)
    assert.equal(await h.rewrite('runScript', 'blob:sha256:x/y.js'), 'blob:sha256:x/y.js')
    await h.fetchOutputs(undefined)
    await h.fetchOutputs(['outputs/unknown.png'])
    t.close()
  })
})

describe('a local daemon', () => {
  test('installApp sends the host path', async () => {
    const local = await MaestroD.connect({ name: 'jsfiles' })
    const dev = await local.attach({ device: 'mock-1', platform: 'mock' })
    assert.deepEqual(await dev.installApp(path.join(work, 'app.apk'), { platform: 'mock' }), {})
  })
})
