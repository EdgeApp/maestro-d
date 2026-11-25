// Screen streams from JavaScript (N1–N3): device.stream() against a real
// daemon driving the mock device (its capture loops an embedded H.264
// clip), its refusals, a stream through a hub under a lease, and the
// transport's edge cases against a scripted server.

import assert from 'node:assert/strict'
import { execFileSync } from 'node:child_process'
import * as fs from 'node:fs'
import * as http from 'node:http'
import * as os from 'node:os'
import * as path from 'node:path'
import { Readable, Writable } from 'node:stream'
import { pipeline } from 'node:stream/promises'
import { after, before, describe, test } from 'node:test'
import { fileURLToPath } from 'node:url'

import { MaestroD, isMaestroError } from '../dist/esm/index.js'
import { Transport, boxSize } from '../dist/esm/transport.js'

const here = path.dirname(fileURLToPath(import.meta.url))
const repoRoot = path.resolve(here, '..', '..', '..')
const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'maestro-d-stream-'))
const bin = process.env.MAESTRO_D_TEST_BIN ?? path.join(tmp, 'maestro-d')
const hubBin = process.env.MAESTRO_D_TEST_HUB_BIN ?? path.join(path.dirname(bin), 'maestro-d-hub')
process.env.MAESTRO_D_HOME = path.join(tmp, 'home')
process.env.MAESTRO_D_BIN = bin
for (const k of ['MAESTRO_DEVICE', 'MAESTRO_D', 'MAESTRO_D_HUB', 'MAESTRO_D_TOKEN', 'MAESTRO_D_CA']) delete process.env[k]

/** @type {MaestroD} */
let daemon

before(async () => {
  if (!process.env.MAESTRO_D_TEST_BIN) execFileSync('go', ['build', '-o', bin, './cmd/maestro-d'], { cwd: repoRoot, stdio: 'inherit' })
  if (!process.env.MAESTRO_D_TEST_BIN && !process.env.MAESTRO_D_TEST_HUB_BIN) {
    execFileSync('go', ['build', '-o', hubBin, './cmd/maestro-d-hub'], { cwd: repoRoot, stdio: 'inherit' })
  }
  daemon = await MaestroD.connect({ name: 'jsstream', idleTimeout: 120 })
})

after(async () => {
  try {
    await daemon?.shutdown()
  } finally {
    fs.rmSync(tmp, { recursive: true, force: true })
  }
})

/** Read a body until `ms` passes or it ends; returns the bytes and whether it ended. */
async function readFor(body, ms, ac) {
  const reader = body.getReader()
  const chunks = []
  const timer = setTimeout(() => ac?.abort(), ms)
  try {
    for (;;) {
      const { done, value } = await reader.read()
      if (done) break
      chunks.push(Buffer.from(value))
    }
  } finally {
    clearTimeout(timer)
  }
  return Buffer.concat(chunks)
}

async function rejects(fn, check) {
  try {
    await fn()
  } catch (err) {
    check(err)
    return err
  }
  assert.fail('expected a rejection')
}

describe('device.stream()', () => {
  test('N1: fps, codecs and fragmented MP4; aborting ends the body cleanly', async () => {
    const dev = await daemon.attach({ device: 'mock-1', platform: 'mock' })
    const ac = new AbortController()
    const s = await dev.stream({ fps: 15, signal: ac.signal })
    assert.equal(s.fps, 15)
    assert.equal(s.audio, false)
    assert.match(s.codecs, /^avc3\.[0-9a-f]{6}$/)
    const bytes = await readFor(s.body, 2000, ac)
    assert.equal(bytes.subarray(4, 8).toString(), 'ftyp')
    assert.ok(bytes.includes(Buffer.from('moof')) && bytes.includes(Buffer.from('mdat')))
    assert.ok(bytes.length > 2000, `${bytes.length} bytes in 2 s`)
    // Commands run beside a stream.
    const s2ac = new AbortController()
    const s2 = await dev.stream({ fps: 30, audio: true, signal: s2ac.signal })
    assert.equal((await dev.tapOn('Beside')).ok, true)
    s2ac.abort()
    await readFor(s2.body, 5000)
    // An aborted signal stops it before it starts.
    await rejects(
      () => dev.stream({ signal: AbortSignal.abort() }),
      (err) => assert.ok(isMaestroError(err, 'INTERRUPTED'), String(err)),
    )
  })

  test('audio from a fresh capture', async () => {
    const dev = await daemon.attach({ device: 'mock-2', platform: 'mock' })
    const ac = new AbortController()
    const s = await dev.stream({ audio: true, signal: ac.signal })
    assert.equal(s.audio, true)
    assert.match(s.codecs, /^avc3\.[0-9a-f]{6},mp4a\.40\.2$/)
    ac.abort()
    await readFor(s.body, 5000)
  })

  test('N2: a bad fps is USAGE; a detached device is DEVICE_NOT_ATTACHED', async () => {
    const dev = await daemon.attach({ device: 'mock-3', platform: 'mock' })
    await rejects(
      () => dev.stream({ fps: 1 }),
      (err) => {
        assert.ok(isMaestroError(err, 'USAGE'), String(err))
        assert.match(err.message, /3 to 60/)
      },
    )
    await dev.detach()
    await rejects(
      () => dev.stream(),
      (err) => assert.ok(isMaestroError(err, 'DEVICE_NOT_ATTACHED'), String(err)),
    )
  })

  test('detaching ends a running stream', async () => {
    const dev = await daemon.attach({ device: 'mock-4', platform: 'mock' })
    const s = await dev.stream()
    setTimeout(() => dev.detach().catch(() => undefined), 500)
    const bytes = await readFor(s.body, 10_000)
    assert.equal(bytes.subarray(4, 8).toString(), 'ftyp')
  })
})

describe('streams on a hub', () => {
  /** @type {string} */
  let addr
  const token = 'stream-token'
  const hubName = 'jsstreamhub'

  before(() => {
    const out = execFileSync(hubBin, ['start', '--name', hubName, '--no-mdns', '--listen', '127.0.0.1:0',
      '--token', token, '--peer-token', 'stream-peer', '--share-device', 'mock-s1', '--json'], { env: { ...process.env, MAESTRO_D_MOCK_DEVICES: 'mock-s1' } })
    addr = JSON.parse(out.toString()).daemon.httpAddr
  })

  after(() => {
    execFileSync(hubBin, ['stop'])
  })

  test('N3: a leased device streams through the hub; without the lease, LEASE_REQUIRED', async () => {
    const md = await MaestroD.connect({ hub: addr, token })
    const dev = await md.lease({ device: 'mock-s1' })
    const ac = new AbortController()
    const s = await dev.stream({ fps: 20, signal: ac.signal })
    assert.equal(s.fps, 20)
    const bytes = await readFor(s.body, 1500, ac)
    assert.equal(bytes.subarray(4, 8).toString(), 'ftyp')
    assert.ok(bytes.includes(Buffer.from('mdat')))
    // The same request without the lease header is refused.
    const bare = new Transport({ url: addr, token })
    await rejects(
      () => bare.stream('/v1/devices/mock-s1/stream'),
      (err) => assert.ok(isMaestroError(err, 'LEASE_REQUIRED'), String(err)),
    )
    bare.close()
    await md.close()
  })
})

/** A box of `type` with `n` payload bytes; `large` uses the 64-bit size. */
function box(type, n, large = false) {
  const hdr = large ? 16 : 8
  const b = Buffer.alloc(hdr + n, 0x2a)
  if (large) {
    b.writeUInt32BE(1, 0)
    b.writeBigUInt64BE(BigInt(hdr + n), 8)
  } else {
    b.writeUInt32BE(hdr + n, 0)
  }
  b.write(type, 4, 'latin1')
  return b
}

/** The whole boxes the 'fragments' server sends before its cut-off fragment. */
const fragmentBoxes = [box('ftyp', 8), box('moov', 24), box('moof', 16), box('mdat', 32), box('free', 8, true), box('moof', 16), box('mdat', 64)]

/** The top-level box types in b; fails on a box that does not fit. */
function boxTypes(b) {
  const types = []
  for (let off = 0; off < b.length; ) {
    const n = boxSize(b.subarray(off, off + 16))
    assert.ok(n !== undefined && off + n <= b.length, `a partial box at ${off} of ${b.length}`)
    types.push(b.toString('latin1', off + 4, off + 8))
    off += n
  }
  return types
}

describe('transport.stream()', () => {
  /** What the next request gets: 'slow' | 'cut' | 'expired' | 'unavailable' | 'fragments' | 'endless'. */
  let mode = 'slow'
  /** Resolves when the server sees the connection of an 'endless' stream close. */
  let closed = Promise.resolve()
  /** @type {http.IncomingHttpHeaders[]} */
  const seen = []
  /** @type {http.Server} */
  let server
  let url = ''

  before(async () => {
    server = http.createServer((req, res) => {
      seen.push(req.headers)
      if (mode === 'slow') return setTimeout(() => res.destroy(), 2000)
      if (mode === 'expired') {
        res.writeHead(409, { 'Content-Type': 'application/json', 'X-Maestro-Error-Code': 'LEASE_EXPIRED' })
        return res.end(JSON.stringify({ ok: false, exitCode: 5, error: { code: 'LEASE_EXPIRED', message: 'lease ended' } }))
      }
      if (mode === 'unavailable') {
        res.writeHead(503, { 'Content-Type': 'application/json', 'X-Maestro-Error-Code': 'CAPTURE_UNAVAILABLE' })
        return res.end(JSON.stringify({ ok: false, exitCode: 4, error: { code: 'CAPTURE_UNAVAILABLE', message: 'no capture', details: { reason: 'unsupported-platform' } } }))
      }
      res.writeHead(200, { 'Content-Type': 'video/mp4', 'X-Maestro-Fps': 'device', 'X-Maestro-Audio': 'true', 'X-Maestro-Codecs': 'avc3.640028,ipcm' })
      if (mode === 'endless') {
        // The init segment, then a fragment every 10 ms until the client
        // closes the connection.
        res.write(Buffer.concat(fragmentBoxes.slice(0, 2)))
        const timer = setInterval(() => res.write(Buffer.concat([box('moof', 16), box('mdat', 1024)])), 10)
        closed = new Promise((resolve) => req.socket.once('close', () => resolve(clearInterval(timer))))
        return
      }
      if (mode === 'fragments') {
        // Whole fragments in 5-byte pieces, then a third fragment cut off in
        // its mdat; the connection stays open until the client aborts.
        const bytes = Buffer.concat([...fragmentBoxes, box('moof', 16), box('mdat', 40).subarray(0, 20)])
        for (let i = 0; i < bytes.length; i += 5) res.write(bytes.subarray(i, i + 5))
        return
      }
      res.write(Buffer.from('0000000c66747970', 'hex'))
      setTimeout(() => res.socket.destroy(), 100)
    })
    await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve))
    url = `127.0.0.1:${server.address().port}`
  })

  after(() => server.close())

  test('sends the token, API version and lease; INTERRUPTED when aborted before an answer', async () => {
    const t = new Transport({ url, token: 'tk' })
    t.leases.set('dev-1', 'L1')
    mode = 'slow'
    const ac = new AbortController()
    setTimeout(() => ac.abort(), 100)
    await rejects(
      () => t.stream('/v1/devices/dev-1/stream', { signal: ac.signal }),
      (err) => assert.ok(isMaestroError(err, 'INTERRUPTED'), String(err)),
    )
    const h = seen.at(-1)
    assert.equal(h.authorization, 'Bearer tk')
    assert.equal(h['x-maestro-api-version'], '1')
    assert.equal(h['x-maestro-lease'], 'L1')
    assert.equal(h.accept, 'video/mp4')
    t.close()
  })

  test('a stream cut off mid-body errors its body with DAEMON_UNAVAILABLE; device fps is "device"', async () => {
    mode = 'cut'
    const t = new Transport({ url })
    const res = await t.stream('/v1/devices/dev-2/stream')
    assert.equal(res.headers['x-maestro-fps'], 'device')
    await rejects(
      () => readFor(res.body, 5000),
      (err) => assert.ok(isMaestroError(err, 'DAEMON_UNAVAILABLE'), String(err)),
    )
    t.close()
  })

  test('Review 1.harness-review.3: aborting mid-fragment ends the body after the last whole moof+mdat', async () => {
    mode = 'fragments'
    const t = new Transport({ url })
    const ac = new AbortController()
    const res = await t.stream('/v1/devices/dev-5/stream', { signal: ac.signal })
    const bytes = await readFor(res.body, 500, ac)
    assert.deepEqual(boxTypes(bytes), ['ftyp', 'moov', 'moof', 'mdat', 'free', 'moof', 'mdat'])
    assert.ok(bytes.equals(Buffer.concat(fragmentBoxes)))
    t.close()
  })

  /** Waits for the server to see the connection close, or fails after 5 s. */
  async function serverSeesClose(what) {
    let timer
    const late = new Promise((_, reject) => (timer = setTimeout(() => reject(new Error(`${what}: the connection stayed open`)), 5000)))
    try {
      await Promise.race([closed, late])
    } finally {
      clearTimeout(timer)
    }
  }

  test('Review 2.harness-review.2: cancelling the body closes the connection', async () => {
    mode = 'endless'
    const t = new Transport({ url })
    const res = await t.stream('/v1/devices/dev-6/stream')
    const reader = res.body.getReader()
    const first = await reader.read()
    assert.equal(Buffer.from(first.value).subarray(4, 8).toString(), 'ftyp')
    reader.releaseLock()
    await res.body.cancel()
    await serverSeesClose('body.cancel()')
    t.close()
  })

  test('Review 2.harness-review.2: a break out of for await closes the connection', async () => {
    mode = 'endless'
    const t = new Transport({ url })
    const res = await t.stream('/v1/devices/dev-7/stream')
    let n = 0
    for await (const chunk of res.body) {
      assert.ok(chunk.length > 0)
      if (++n === 3) break
    }
    await serverSeesClose('break')
    t.close()
  })

  test('Review 2.harness-review.2: a pipeline that fails on its write side closes the connection', async () => {
    mode = 'endless'
    const t = new Transport({ url })
    const res = await t.stream('/v1/devices/dev-8/stream')
    let writes = 0
    const disk = new Writable({
      write(_chunk, _enc, cb) {
        cb(++writes === 2 ? new Error('disk full') : undefined)
      },
    })
    await assert.rejects(pipeline(Readable.fromWeb(res.body), disk), /disk full/)
    await serverSeesClose('pipeline')
    t.close()
  })

  test('boxSize reads 32- and 64-bit sizes and refuses headers that never complete', () => {
    assert.equal(boxSize(Buffer.alloc(7)), undefined)
    assert.equal(boxSize(box('mdat', 4)), 12)
    assert.equal(boxSize(box('mdat', 4, true)), 20)
    assert.equal(boxSize(box('mdat', 4, true).subarray(0, 12)), undefined)
    assert.equal(boxSize(Buffer.from('000000006d646174', 'hex')), undefined)
    const huge = box('mdat', 0, true)
    huge.writeBigUInt64BE(2n ** 41n, 8)
    assert.equal(boxSize(huge), undefined)
  })

  test('CAPTURE_UNAVAILABLE carries its reason; LEASE_EXPIRED forgets the lease', async () => {
    const t = new Transport({ url })
    mode = 'unavailable'
    await rejects(
      () => t.stream('/v1/devices/dev-3/stream'),
      (err) => {
        assert.ok(isMaestroError(err, 'CAPTURE_UNAVAILABLE'), String(err))
        assert.equal(err.details.reason, 'unsupported-platform')
        assert.equal(err.exitCode, 4)
        assert.equal(err.httpStatus, 503)
      },
    )
    mode = 'expired'
    t.leases.set('dev-3', 'L3')
    let told = ''
    t.onLeaseExpired = (device, lease) => (told = `${device}=${lease}`)
    await rejects(
      () => t.stream('/v1/devices/dev-3/stream'),
      (err) => assert.ok(isMaestroError(err, 'LEASE_EXPIRED'), String(err)),
    )
    assert.equal(t.leases.get('dev-3'), undefined)
    assert.equal(told, 'dev-3=L3')
    // Nothing listens: DAEMON_UNAVAILABLE.
    const dead = new Transport({ url: '127.0.0.1:1' })
    await rejects(
      () => dead.stream('/v1/devices/dev-3/stream'),
      (err) => assert.ok(isMaestroError(err, 'DAEMON_UNAVAILABLE'), String(err)),
    )
    t.close()
  })
})
