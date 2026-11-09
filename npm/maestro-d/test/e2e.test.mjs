// End-to-end tests for the JavaScript library against a real daemon
// process driving the mock driver. The Go binary is built from the repo
// root into a temp dir; MAESTRO_D_HOME keeps the run files out of
// ~/.maestro-d.

import assert from 'node:assert/strict'
import { execFileSync } from 'node:child_process'
import * as fs from 'node:fs'
import * as http from 'node:http'
import * as os from 'node:os'
import * as path from 'node:path'
import { after, before, describe, test } from 'node:test'
import { fileURLToPath } from 'node:url'

import { COMMANDS, MaestroD, MaestroError, PACKAGE_BY_TARGET, buildValue, isMaestroError, liveInfo, uploadPath } from '../dist/esm/index.js'

const here = path.dirname(fileURLToPath(import.meta.url))
const repoRoot = path.resolve(here, '..', '..', '..')

const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'maestro-d-js-'))
const bin = process.env.MAESTRO_D_TEST_BIN ?? path.join(tmp, 'maestro-d')
process.env.MAESTRO_D_HOME = path.join(tmp, 'home')
process.env.MAESTRO_D_BIN = bin
delete process.env.MAESTRO_DEVICE
delete process.env.MAESTRO_D
// A shell set up to target a hub must not reach the local-daemon cases;
// the hub case below sets these for itself.
delete process.env.MAESTRO_D_HUB
delete process.env.MAESTRO_D_TOKEN
delete process.env.MAESTRO_D_CA

const MOCK = { device: 'mock-1', platform: 'mock' }

/** @type {MaestroD} */
let daemon
/** @type {import('../dist/esm/index.js').MaestroDevice} */
let dev

before(async () => {
  if (!process.env.MAESTRO_D_TEST_BIN) {
    execFileSync('go', ['build', '-o', bin, '.'], { cwd: repoRoot, stdio: 'inherit' })
  }
  daemon = await MaestroD.connect({ name: 'jstest', idleTimeout: 120 })
  dev = await daemon.attach(MOCK)
})

after(async () => {
  try {
    await daemon?.shutdown()
  } finally {
    fs.rmSync(tmp, { recursive: true, force: true })
  }
})

async function rejects(fn, check) {
  try {
    await fn()
  } catch (err) {
    check(err)
    return err
  }
  assert.fail('expected a rejection')
}

describe('connect', () => {
  test('spawned the daemon and wrote daemon.json', () => {
    assert.equal(daemon.started, true)
    assert.equal(daemon.name, 'jstest')
    assert.ok(daemon.info.pid > 0)
    assert.equal(liveInfo('jstest').pid, daemon.info.pid)
  })

  test('a second connect reuses the running daemon', async () => {
    const d2 = await MaestroD.connect({ name: 'jstest' })
    assert.equal(d2.started, false)
    assert.equal(d2.info.pid, daemon.info.pid)
    const st = await d2.status()
    assert.equal(st.ok, true)
    assert.equal(st.daemon.name, 'jstest')
    d2.close()
  })

  test('spawn: false rejects with DAEMON_UNAVAILABLE when nothing runs', async () => {
    await rejects(
      () => MaestroD.connect({ name: 'jstest-none', spawn: false }),
      (err) => {
        assert.ok(isMaestroError(err, 'DAEMON_UNAVAILABLE'))
        assert.equal(err.exitCode, 3)
        assert.equal(err.httpStatus, 503)
      },
    )
  })

  test('a bad name is a TypeError', async () => {
    await assert.rejects(() => MaestroD.connect({ name: 'bad name!' }), TypeError)
  })

  test('a missing binary is DAEMON_UNAVAILABLE', async () => {
    await rejects(
      () => MaestroD.connect({ name: 'jstest-nobin', bin: path.join(tmp, 'nope') }),
      (err) => assert.ok(isMaestroError(err, 'DAEMON_UNAVAILABLE')),
    )
  })
})

describe('devices', () => {
  test('attach returned a handle on mock-1', async () => {
    assert.equal(dev.id, 'mock-1')
    assert.equal(dev.info.platform, 'mock')
    assert.equal(dev.info.attached, true)
    assert.ok(dev.info.reportDir)
  })

  test('devices() lists the attached mock', async () => {
    const list = await daemon.devices()
    const m = list.find((d) => d.id === 'mock-1')
    assert.ok(m)
    assert.equal(m.attached, true)
    assert.equal((await daemon.device('mock-1')).id, 'mock-1')
  })

  test('attach without a device id picks the only attached one', async () => {
    const d = await daemon.attach()
    assert.equal(d.id, 'mock-1')
  })

  test('an unknown device is DEVICE_NOT_FOUND', async () => {
    await rejects(
      () => daemon.device('no-such-device'),
      (err) => {
        assert.ok(isMaestroError(err))
        assert.equal(err.code, 'DEVICE_NOT_FOUND')
        assert.equal(err.httpStatus, 404)
      },
    )
  })

  test('a device attached by another daemon is DEVICE_IN_USE', async () => {
    const other = await MaestroD.connect({ name: 'jstest2', idleTimeout: 120 })
    try {
      await rejects(
        () => other.attach(MOCK),
        (err) => {
          assert.ok(isMaestroError(err, 'DEVICE_IN_USE'))
          assert.equal(err.exitCode, 5)
          assert.equal(err.httpStatus, 409)
        },
      )
    } finally {
      await other.shutdown()
    }
  })

  test('startDevice without a name is a TypeError', async () => {
    await assert.rejects(() => daemon.startDevice({ platform: 'ios' }), TypeError)
  })
})

describe('commands', () => {
  // These tests exercise serialization and daemon routing. Physical lock-state
  // verification belongs to the WDA driver tests, not the mock driver.
  for (const key of ['LOCK', 'UNLOCK']) {
    test(`TypeScript client routes ${key} through pressKey`, async () => {
      const result = await dev.pressKey({ key })
      assert.equal(result.ok, true)
      assert.equal(result.type, 'pressKey')
      assert.ok(result.step.includes(key))
    })

    test(`REST routes ${key} through pressKey`, async () => {
      const response = await new Promise((resolve, reject) => {
        const req = http.request({
          socketPath: daemon.info.socketPath,
          path: '/v1/devices/mock-1/commands/pressKey',
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
        }, (res) => {
          let body = ''
          res.setEncoding('utf8')
          res.on('data', chunk => { body += chunk })
          res.on('error', reject)
          res.on('end', () => {
            try { resolve({ status: res.statusCode, body: JSON.parse(body) }) }
            catch (error) { reject(error) }
          })
        })
        req.on('error', reject)
        req.setTimeout(10000, () => req.destroy(new Error('REST command timed out')))
        req.end(JSON.stringify({ key }))
      })
      assert.equal(response.status, 200)
      assert.equal(response.body.ok, true)
      assert.equal(response.body.type, 'pressKey')
      assert.ok(response.body.step.includes(key))
    })
  }

  test('every generated command has a method', () => {
    for (const name of Object.keys(COMMANDS)) {
      assert.equal(typeof dev[name], 'function', name)
    }
  })

  test('scalar value', async () => {
    const r = await dev.tapOn('Login')
    assert.equal(r.ok, true)
    assert.equal(r.device, 'mock-1')
    assert.equal(r.type, 'tapOn')
    assert.match(r.step, /Login/)
    assert.ok(r.durationMs >= 0)
  })

  test('object value', async () => {
    const r = await dev.tapOn({ id: 'submit', index: 1 })
    assert.match(r.step, /submit/)
  })

  test('scalar value plus fields in opts', async () => {
    const r = await dev.tapOn('Login', { index: 2, label: 'second login' })
    assert.equal(r.ok, true)
  })

  // The documented form is one object of the command's fields, and it is
  // sent unchanged: a call and the REST body are the same thing written
  // twice.
  test('an object of fields is the REST body', () => {
    const body = { text: 'Login', timeout: 5000, optional: true }
    assert.deepEqual(buildValue('tapOn', body, {}), body)
    assert.deepEqual(buildValue('swipe', { direction: 'UP', duration: 400 }, {}), {
      direction: 'UP',
      duration: 400,
    })
    // opts merge into that same object.
    assert.deepEqual(buildValue('tapOn', { text: 'Login' }, { timeout: 5000, optional: true }), body)
    assert.deepEqual(buildValue('tapOn', 'Login', { timeout: 5000, optional: true }), body)
  })

  // The YAML shorthand goes to the daemon as written rather than being
  // expanded here: the parser accepts spellings the map form has no field
  // for, so only it can decide what a bare value means.
  test('a bare value is passed through as the YAML shorthand', () => {
    assert.equal(buildValue('tapOn', 'Login', {}), 'Login')
    assert.equal(buildValue('launchApp', 'co.edgesecure.app', {}), 'co.edgesecure.app')
  })

  // TypeScript rejects an unknown field at compile time; from plain
  // JavaScript the daemon rejects it, with the same USAGE code the CLI uses.
  test('an unknown field is USAGE, not a silently wrong step', async () => {
    await rejects(
      () => dev.tapOn('Login', { txt: 'Login' }),
      (err) => {
        assert.ok(isMaestroError(err, 'USAGE'), String(err))
        assert.match(err.message, /tapOn has no field "txt"/)
        return true
      },
    )
  })

  test('value-less commands take no arguments', async () => {
    const r = await dev.back()
    assert.equal(r.type, 'back')
    assert.throws(() => buildValue('back', 'x', {}), TypeError)
  })

  test('a scalar for a command without a scalar shorthand is a TypeError', () => {
    assert.throws(() => buildValue('setLocation', 'x', {}), TypeError)
    assert.throws(() => buildValue('setLocation', 'x', { latitude: 1 }), TypeError)
  })

  test('an array value with fields is a TypeError', () => {
    assert.throws(() => buildValue('repeat', [], { times: 2 }), TypeError)
  })

  test('buildValue shapes the YAML value', () => {
    assert.equal(buildValue('tapOn', 'Login', {}), 'Login')
    assert.deepEqual(buildValue('tapOn', 'Login', { index: 1 }), { text: 'Login', index: 1 })
    assert.deepEqual(buildValue('tapOn', { id: 'a' }, { optional: true }), { id: 'a', optional: true })
    assert.deepEqual(buildValue('tapOn', undefined, { id: 'a' }), { id: 'a' })
    assert.equal(buildValue('back', undefined, {}), null)
    assert.deepEqual(buildValue('inputText', 'hi', {}), 'hi')
    assert.deepEqual(buildValue('swipe', undefined, { direction: 'UP' }), { direction: 'UP' })
  })

  test('a failed step rejects with COMMAND_FAILED carrying the result', async () => {
    const err = await rejects(
      () => dev.assertTrue('false'),
      (err) => {
        assert.ok(err instanceof MaestroError)
        assert.equal(err.code, 'COMMAND_FAILED')
        assert.equal(err.exitCode, 1)
        assert.equal(err.httpStatus, 422)
        assert.equal(err.step, 'assertTrue')
        assert.match(err.message, /assertTrue/)
        assert.equal(err.result?.ok, false)
        assert.equal(err.result?.type, 'assertTrue')
        assert.ok(err.details?.reportDir)
      },
    )
    const json = err.toJSON()
    assert.equal(json.ok, false)
    assert.equal(json.exitCode, 1)
    assert.equal(json.error.code, 'COMMAND_FAILED')
    assert.equal(JSON.parse(JSON.stringify(err)).error.code, 'COMMAND_FAILED')
  })

  test('failed optional steps resolve (ok:true) with the error attached', async () => {
    const r = await dev.assertTrue('false', { optional: true })
    assert.equal(r.ok, true)
    assert.equal(r.optional, true)
    assert.equal(r.error?.code, 'COMMAND_FAILED')
  })

  test('an unknown command is USAGE', async () => {
    await rejects(
      () => dev.command('noSuchCommand', 'x'),
      (err) => {
        assert.ok(isMaestroError(err, 'USAGE'))
        assert.equal(err.exitCode, 2)
        assert.equal(err.httpStatus, 400)
      },
    )
  })

  test('platform-scoped steps are skipped on other platforms', async () => {
    const r = await dev.tapOn('Login', { platform: 'ios' })
    assert.equal(r.ok, true)
    assert.equal(r.skipped, true)
  })
})

describe('variables and scripts', () => {
  test('setVars / vars / eval share the device JS scope', async () => {
    const vars = await dev.setVars({ USER: 'alice', N: '2' })
    assert.equal(vars.USER, 'alice')
    assert.equal((await dev.vars()).N, '2')
    assert.equal(await dev.eval('USER + N'), 'alice2')
    const r = await dev.assertTrue("${USER == 'alice'}")
    assert.equal(r.ok, true)
  })

  test('evalScript sets output', async () => {
    await dev.evalScript("${output.greeting = 'hi ' + USER}")
    assert.equal(await dev.eval('output.greeting'), 'hi alice')
  })
})

describe('batches and flows', () => {
  test('run() stops at the first failure', async () => {
    const err = await rejects(
      () => dev.run([{ tapOn: 'A' }, { assertTrue: 'false' }, { tapOn: 'B' }]),
      (err) => {
        assert.ok(isMaestroError(err, 'COMMAND_FAILED'))
        assert.equal(err.result?.results.length, 2)
      },
    )
    assert.equal(err.result.passed, 1)
    assert.equal(err.result.failed, 1)
  })

  test('run() with continueOnError runs everything and still rejects', async () => {
    await rejects(
      () => dev.run([{ tapOn: 'A' }, { assertTrue: 'false' }, 'back'], { continueOnError: true }),
      (err) => {
        assert.equal(err.code, 'COMMAND_FAILED')
        assert.equal(err.result?.results.length, 3)
        assert.equal(err.result?.passed, 2)
      },
    )
  })

  test('run() resolves with the per-step results', async () => {
    const r = await dev.run([{ tapOn: 'A' }, 'back'])
    assert.equal(r.ok, true)
    assert.equal(r.passed, 2)
    assert.equal(r.results[1].type, 'back')
  })

  test('runYaml() parses a YAML list', async () => {
    const r = await dev.runYaml('- tapOn: A\n- back\n')
    assert.equal(r.passed, 2)
  })

  test('flow() runs a flow file with env', async () => {
    const file = path.join(tmp, 'flow.yaml')
    fs.writeFileSync(file, 'appId: com.example\n---\n- launchApp\n- tapOn: ${WHO}\n- assertTrue: ${WHO == "carol"}\n')
    const r = await dev.flow({ file, env: { WHO: 'carol' } })
    assert.equal(r.ok, true)
    assert.ok(r.subSteps.length >= 3)
  })

  test('flow() with inline yaml', async () => {
    const r = await dev.flow({ yaml: 'appId: com.example\n---\n- tapOn: X\n' })
    assert.equal(r.ok, true)
  })

  test('repeat with inline commands', async () => {
    const r = await dev.repeat({ times: 3, commands: [{ tapOn: 'A' }] })
    assert.equal(r.ok, true)
    assert.equal(r.subSteps?.length, 3)
  })

  test('runFlow with a file and env', async () => {
    const file = path.join(tmp, 'sub.yaml')
    fs.writeFileSync(file, '- assertTrue: ${NAME == "dave"}\n')
    const r = await dev.runFlow({ file, env: { NAME: 'dave' } })
    assert.equal(r.ok, true)
  })
})

describe('device state', () => {
  test('screenshot() is a PNG buffer', async () => {
    const png = await dev.screenshot()
    assert.ok(Buffer.isBuffer(png))
    assert.equal(png.subarray(1, 4).toString(), 'PNG')
  })

  test('takeScreenshot writes into the report dir', async () => {
    const r = await dev.takeScreenshot(path.join(tmp, 'shot'))
    assert.equal(r.ok, true)
  })

  test('hierarchy(), state() and platformInfo()', async () => {
    const h = await dev.hierarchy()
    assert.equal(h.type, 'View')
    assert.equal(h.children[0].text, 'Mock Element')
    const raw = await dev.hierarchyRaw()
    assert.ok(raw.length > 0)
    const st = await dev.state()
    assert.equal(typeof st, 'object')
    const info = await dev.platformInfo()
    assert.equal(typeof info, 'object')
  })

  test('refresh() re-reads device info', async () => {
    const info = await dev.refresh()
    assert.equal(info.id, 'mock-1')
    assert.equal(info.attached, true)
  })
})

describe('events', () => {
  test('a step shows up on the SSE stream', async () => {
    const ac = new AbortController()
    const seen = []
    const reader = (async () => {
      for await (const ev of daemon.events({ signal: ac.signal })) {
        seen.push(ev)
        if (ev.type === 'step' && ev.step?.includes('EventProbe')) break
      }
    })()
    await new Promise((r) => setTimeout(r, 100))
    await dev.tapOn('EventProbe')
    await Promise.race([reader, new Promise((_, rej) => setTimeout(() => rej(new Error('no event in 5s')), 5000))])
    ac.abort()
    const ev = seen.find((e) => e.type === 'step' && e.step?.includes('EventProbe'))
    assert.ok(ev)
    assert.equal(ev.device, 'mock-1')
    assert.equal(ev.passed, true)
    assert.ok(ev.seq > 0)
  })

  test('device.events() filters by device', async () => {
    const ac = new AbortController()
    const p = (async () => {
      for await (const ev of dev.events({ signal: ac.signal })) {
        if (ev.type === 'step') return ev
      }
      return undefined
    })()
    await new Promise((r) => setTimeout(r, 100))
    await dev.tapOn('Filtered')
    const ev = await p
    ac.abort()
    assert.equal(ev?.device, 'mock-1')
  })
})

describe('interruption', () => {
  test('aborting a running step rejects with INTERRUPTED', async () => {
    const ac = new AbortController()
    setTimeout(() => ac.abort(), 300)
    await rejects(
      () => dev.repeat({ while: { visible: 'Login' }, commands: [{ tapOn: 'Login' }] }, { signal: ac.signal }),
      (err) => {
        assert.ok(isMaestroError(err, 'INTERRUPTED'), String(err))
        assert.equal(err.exitCode, 130)
      },
    )
    // The device is usable again afterwards.
    const r = await dev.tapOn('AfterAbort')
    assert.equal(r.ok, true)
  })
})

describe('hub', () => {
  test('connect({hub}) drives a TCP daemon; a lost answer is retried and runs once', async () => {
    const tcp = await MaestroD.connect({ name: 'jstesthub', http: '127.0.0.1:0', token: 'hub-token', idleTimeout: 120 })
    const target = tcp.info.httpAddr
    assert.ok(target, 'the daemon reports its TCP address')
    // A proxy that forwards everything, but loses the answer to the first
    // step-running request: the daemon ran it, the client never hears.
    let lost = 0
    const seen = []
    const proxy = http.createServer((req, res) => {
      const chunks = []
      req.on('data', (c) => chunks.push(c))
      req.on('end', () => {
        const up = http.request({ host: '127.0.0.1', port: Number(target.split(':').pop()), method: req.method, path: req.url, headers: req.headers }, (ur) => {
          const body = []
          ur.on('data', (c) => body.push(c))
          ur.on('end', () => {
            if (req.url.includes('/commands/') && lost === 0) {
              lost++
              seen.push(req.headers['x-maestro-request-id'])
              return req.socket.destroy()
            }
            if (req.url.includes('/commands/')) seen.push(req.headers['x-maestro-request-id'], ur.headers['x-maestro-replayed'])
            res.writeHead(ur.statusCode, ur.headers)
            res.end(Buffer.concat(body))
          })
        })
        up.end(Buffer.concat(chunks))
      })
    })
    await new Promise((resolve) => proxy.listen(0, '127.0.0.1', resolve))
    process.env.MAESTRO_D_HUB = `127.0.0.1:${proxy.address().port}`
    process.env.MAESTRO_D_TOKEN = 'hub-token'
    try {
      const hub = await MaestroD.connect()
      assert.equal(hub.hub, true)
      const hdev = await hub.attach({ device: 'mock-hub', platform: 'mock' })
      const r = await hdev.evalScript('${output.n = (output.n || 0) + 1}')
      assert.equal(r.ok, true)
      assert.equal(lost, 1)
      assert.equal(seen[0], seen[1], 'the retry carried the same request id')
      assert.equal(seen[2], 'true', 'the daemon answered the retry from its cache')
      assert.equal(await hdev.eval('output.n'), 1, 'and ran the step once')
      // A later call gets a fresh id and runs normally.
      assert.equal((await hdev.tapOn('Hub')).ok, true)
      assert.notEqual(seen[3], seen[0])
      await hdev.detach()
      hub.close()
    } finally {
      delete process.env.MAESTRO_D_HUB
      delete process.env.MAESTRO_D_TOKEN
      proxy.close()
      await tcp.shutdown()
    }
    // Wrong token: 401 → USAGE.
    await rejects(
      () => MaestroD.connect({ url: `http://${target}`, token: 'nope' }),
      (err) => assert.ok(isMaestroError(err), String(err)),
    )
  })
})

describe('leases', () => {
  /** @type {string} */
  let addr
  const token = 'lease-token'
  const hubName = 'jsleases'

  before(() => {
    const out = execFileSync(bin, ['hub', 'start', '--daemon', hubName, '--listen', '127.0.0.1:0', '--token', token, '--json'], {
      env: { ...process.env, MAESTRO_D_MOCK_DEVICES: 'mock-h1,mock-h2' },
    })
    addr = JSON.parse(out.toString()).daemon.httpAddr
  })

  after(() => {
    execFileSync(bin, ['hub', 'stop', '--daemon', hubName])
  })

  const connectHub = () => MaestroD.connect({ hub: addr, token })

  test('lease() attaches by default; a second client gets DEVICE_IN_USE; close() releases', async () => {
    const a = await connectHub()
    assert.equal(a.leasing, true)
    const d = await a.lease({ device: 'mock-h1', holder: 'alice' })
    assert.equal(d.info.attached, true)
    assert.equal(d.lease.device, 'mock-h1')
    // The default window is 8 hours, the hub's maximum.
    assert.equal(d.lease.ttlSec, 28800)
    assert.equal(d.lease.maxSec, 28800)
    assert.equal((await d.tapOn('Leased')).ok, true)
    // md.lease() records the lease in leases.json, as `maestro-d lease` does.
    const onFile = JSON.parse(fs.readFileSync(path.join(process.env.MAESTRO_D_HOME, 'leases.json'), 'utf8')).leases
    assert.ok(onFile.some((r) => r.hub === `http://${addr}` && r.device === 'mock-h1' && r.id === d.lease.id), JSON.stringify(onFile))
    const listed = (await a.devices({ platform: 'mock' })).find((x) => x.id === 'mock-h1')
    assert.equal(listed.lease.holder, 'alice')
    assert.equal(listed.available, undefined)
    assert.equal(listed.maxSec, 28800)
    assert.equal((await a.devices({ platform: 'mock' })).find((x) => x.id === 'mock-h2').available, true)

    const b = await connectHub()
    await rejects(
      () => b.lease({ device: 'mock-h1' }),
      (err) => assert.ok(isMaestroError(err, 'DEVICE_IN_USE'), String(err)),
    )
    // Without the lease the hub refuses to run anything on the device. (b
    // shares this process's home, so b.attach() would find a's lease in
    // leases.json: one home is one client. Send the request bare.)
    await rejects(
      () => b.transport.json('/v1/devices/mock-h1/attach', { method: 'POST', body: { platform: 'mock' } }),
      (err) => assert.ok(isMaestroError(err, 'LEASE_REQUIRED') && /alice/.test(err.message), String(err)),
    )
    await a.close()
    const gone = JSON.parse(fs.readFileSync(path.join(process.env.MAESTRO_D_HOME, 'leases.json'), 'utf8')).leases
    assert.ok(!gone.some((r) => r.device === 'mock-h1'), 'close() dropped the entry')
    const free = (await b.devices({ platform: 'mock' })).find((x) => x.id === 'mock-h1')
    assert.equal(free.lease, undefined)
    assert.equal(free.attached, false)
    await b.close()
  })

  test('attach: false only reserves; a selector picks the first free device', async () => {
    const md = await connectHub()
    const first = await md.lease({ select: { platform: 'mock' }, attach: false })
    assert.equal(first.id, 'mock-h1')
    assert.equal(first.info.attached, false)
    await rejects(
      () => first.tapOn('x'),
      (err) => assert.ok(isMaestroError(err, 'DEVICE_NOT_ATTACHED'), String(err)),
    )
    const second = await md.lease({ select: { platform: 'mock' } })
    assert.equal(second.id, 'mock-h2')
    await rejects(
      () => md.lease({ select: { platform: 'mock' } }),
      (err) => assert.ok(isMaestroError(err, 'DEVICE_IN_USE'), String(err)),
    )
    await rejects(
      () => md.lease({ select: { platform: 'android', name: 'none' } }),
      (err) => assert.ok(isMaestroError(err, 'DEVICE_NOT_FOUND') && /platform=android/.test(err.message), String(err)),
    )
    await first.release()
    await first.release() // a second release does nothing
    await md.close()
  })

  test('no auto-renew: a lease ends at its window, used or idle; only renew() moves it', async () => {
    const md = await connectHub()
    const d = await md.lease({ device: 'mock-h1', ttlSec: 3 })
    const end = d.lease.expiresAt
    assert.equal((await d.tapOn('Mine')).ok, true)
    assert.equal(d.lease.expiresAt, end, 'use does not move the window')
    // Idle past the window (the hub sweeps every second): expired.
    await new Promise((resolve) => setTimeout(resolve, 4500))
    await rejects(
      () => d.tapOn('Late'),
      (err) => assert.ok(isMaestroError(err, 'LEASE_EXPIRED'), String(err)),
    )
    // An explicit renew moves it, by the ttlSec asked; over the maximum is USAGE.
    const e = await md.lease({ device: 'mock-h1', ttlSec: 3 })
    const renewed = await e.renew({ ttlSec: 30 })
    assert.equal(renewed.id, e.lease.id)
    assert.equal(renewed.ttlSec, 30)
    assert.ok(Date.parse(renewed.expiresAt) > Date.parse(end))
    await new Promise((resolve) => setTimeout(resolve, 4500))
    assert.equal((await e.tapOn('Renewed')).ok, true)
    await rejects(
      () => e.renew({ ttlSec: 28801 }),
      (err) => assert.ok(isMaestroError(err, 'USAGE') && /maximum/.test(err.message), String(err)),
    )
    assert.equal((await e.renew()).ttlSec, 30, 'renew() defaults to the lease window')
    await rejects(
      () => md.lease({ device: 'mock-h2', ttlSec: 28801 }),
      (err) => assert.ok(isMaestroError(err, 'USAGE') && /8h0m0s/.test(err.message), String(err)),
    )
    await e.release()
    assert.equal(await e.renew(), undefined, 'nothing to renew once released')
    await md.close()
  })

  test('a lease the hub ended is forgotten on renew', async () => {
    const md = await connectHub()
    const d = await md.lease({ device: 'mock-h2', ttlSec: 600, attach: false })
    const other = await connectHub()
    await other.transport.json(`/v1/leases/${encodeURIComponent(d.lease.id)}`, { method: 'DELETE' })
    await rejects(
      () => d.renew(),
      (err) => assert.ok(isMaestroError(err, 'LEASE_EXPIRED'), String(err)),
    )
    assert.equal(d.lease, undefined)
    await md.close()
    await other.close()
  })

  test('attach() on a hub never takes a lease: LEASE_REQUIRED without one; a lease from leases.json is used', async () => {
    const md = await connectHub()
    await rejects(
      () => md.attach({ device: 'mock-h1', platform: 'mock' }),
      (err) =>
        assert.ok(
          isMaestroError(err, 'LEASE_REQUIRED') && /md\.lease\(\)/.test(err.message) && /maestro-d lease --device/.test(err.message),
          String(err),
        ),
    )
    const free = (await md.devices({ platform: 'mock' })).find((x) => x.id === 'mock-h1')
    assert.equal(free.lease, undefined, 'no lease was taken')
    // A lease taken in the shell, in the same home, is found and used.
    const out = execFileSync(bin, ['lease', '--device', 'mock-h1', '--no-attach', '--target-hub=' + addr, '--token', token, '--json'])
    const shell = JSON.parse(out.toString()).lease
    const d = await md.attach({ device: 'mock-h1', platform: 'mock' })
    assert.equal(d.lease.id, shell.id)
    assert.equal(d.info.attached, true)
    assert.equal((await d.tapOn('ShellLease')).ok, true)
    // detach() and close() leave a shell's lease alone.
    await d.detach()
    await md.close()
    const md2 = await connectHub()
    const held = (await md2.devices({ platform: 'mock' })).find((x) => x.id === 'mock-h1')
    assert.ok(held.lease, 'the shell lease outlived close()')
    // A lease the hub reports expired leaves leases.json.
    await md2.transport.json(`/v1/leases/${encodeURIComponent(shell.id)}`, { method: 'DELETE' })
    const again = await rejects(
      () => md2.attach({ device: 'mock-h1', platform: 'mock' }),
      (err) => assert.ok(isMaestroError(err, 'LEASE_EXPIRED'), String(err)),
    )
    assert.ok(again)
    const left = JSON.parse(fs.readFileSync(path.join(process.env.MAESTRO_D_HOME, 'leases.json'), 'utf8')).leases
    assert.ok(!left.some((r) => r.id === shell.id), JSON.stringify(left))
    await md2.close()
  })

  test("a blob upload's body-less HEAD refusal keeps its code: LEASE_REQUIRED, then LEASE_EXPIRED and the entry dropped", async () => {
    const file = path.join(tmp, 'lease-blob.bin')
    fs.writeFileSync(file, 'blob under a lease')
    const md = await connectHub()
    await rejects(
      () => uploadPath(md.transport, '/v1/devices/mock-h1', file),
      (err) => assert.ok(isMaestroError(err, 'LEASE_REQUIRED'), String(err)),
    )
    const out = execFileSync(bin, ['lease', '--device', 'mock-h1', '--no-attach', '--target-hub=' + addr, '--token', token, '--json'])
    const shell = JSON.parse(out.toString()).lease
    const d = await md.attach({ device: 'mock-h1', platform: 'mock' })
    assert.equal(d.lease.id, shell.id)
    const base = `/v1/devices/${encodeURIComponent(d.id)}`
    await uploadPath(md.transport, base, file)
    await md.transport.json(`/v1/leases/${encodeURIComponent(shell.id)}`, { method: 'DELETE' })
    await rejects(
      () => uploadPath(md.transport, base, file),
      (err) => assert.ok(isMaestroError(err, 'LEASE_EXPIRED'), String(err)),
    )
    const left = JSON.parse(fs.readFileSync(path.join(process.env.MAESTRO_D_HOME, 'leases.json'), 'utf8')).leases
    assert.ok(!left.some((r) => r.id === shell.id), JSON.stringify(left))
    await md.close()
  })

  test('an id the hub does not list is DEVICE_NOT_FOUND, and nothing is leased', async () => {
    const md = await connectHub()
    await rejects(
      () => md.lease({ device: 'mock-unlisted', attach: { platform: 'mock' } }),
      (err) => assert.ok(isMaestroError(err, 'DEVICE_NOT_FOUND') && /not listed/.test(err.message), String(err)),
    )
    const { leases } = await md.transport.json('/v1/leases')
    assert.ok(!leases.some((l) => /mock-unlisted/.test(l.device)), JSON.stringify(leases))
    await md.close()
  })

  test('an attach that uploads a file follows the lease; a failed one releases it', async () => {
    const md = await connectHub()
    const app = path.join(tmp, 'lease-app.apk')
    fs.writeFileSync(app, 'PK apk')
    const d = await md.lease({ device: 'mock-h2', attach: { platform: 'mock', appFile: app, noAppInstall: true } })
    assert.equal(d.info.attached, true)
    await d.release()
    await rejects(
      () => md.lease({ device: 'mock-h2', attach: { platform: 'mock', appFile: path.join(tmp, 'missing.apk') } }),
      (err) => assert.ok(err instanceof Error),
    )
    const listed = (await md.devices({ platform: 'mock' })).find((x) => x.id === 'mock-h2')
    assert.equal(listed.lease, undefined, 'the failed attach released its lease')
    await md.close()
  })

  test('lease() needs a device or a selector, and a hub', async () => {
    const md = await connectHub()
    await assert.rejects(() => md.lease({}), TypeError)
    await assert.rejects(() => md.lease({ device: 'a', select: {} }), TypeError)
    await md.close()
    await rejects(
      () => daemon.lease({ device: 'mock-1' }),
      (err) => assert.ok(isMaestroError(err, 'USAGE') && /not a hub/.test(err.message), String(err)),
    )
    // On a plain daemon release() does nothing and succeeds.
    await dev.release()
    assert.equal(dev.info.attached, true)
  })
})

describe('detach and shutdown', () => {
  test('detach() releases the device', async () => {
    await dev.detach()
    // Mock devices only exist while attached, so the list is empty now.
    assert.equal((await daemon.devices()).filter((d) => d.attached).length, 0)
    await rejects(
      () => dev.tapOn('x'),
      (err) => assert.ok(isMaestroError(err, 'DEVICE_NOT_ATTACHED'), String(err)),
    )
    dev = await daemon.attach(MOCK)
    assert.equal(dev.info.attached, true)
  })

  test('shutdown() stops the process and removes the run files', async () => {
    const pid = daemon.info.pid
    await daemon.shutdown()
    assert.equal(liveInfo('jstest'), undefined)
    assert.throws(() => process.kill(pid, 0))
    await rejects(
      () => MaestroD.connect({ name: 'jstest', spawn: false }),
      (err) => assert.ok(isMaestroError(err, 'DAEMON_UNAVAILABLE')),
    )
    daemon = undefined
  })
})

describe('binary resolution', () => {
  // `npm i -g` (and `npx`) put the bin on PATH as a symlink that lives
  // outside the package — <prefix>/bin/maestro-d → <prefix>/lib/
  // node_modules/maestro-d/bin/maestro-d.js — so the platform
  // package can only be found by resolving from the link's target.
  test('the wrapper finds the bundled binary through a global-install symlink', () => {
    const pkgRoot = path.resolve(here, '..')
    const prefix = fs.mkdtempSync(path.join(os.tmpdir(), 'maestro-d-global-'))
    const installed = path.join(prefix, 'lib', 'node_modules', 'maestro-d')
    fs.mkdirSync(installed, { recursive: true })
    fs.cpSync(path.join(pkgRoot, 'dist'), path.join(installed, 'dist'), { recursive: true })
    fs.cpSync(path.join(pkgRoot, 'bin'), path.join(installed, 'bin'), { recursive: true })

    // A stub standing in for the platform package's Go binary.
    const platform = path.join(installed, 'node_modules', PACKAGE_BY_TARGET[`${process.platform}-${process.arch}`], 'bin')
    fs.mkdirSync(platform, { recursive: true })
    fs.writeFileSync(path.join(platform, '..', 'package.json'), JSON.stringify({ name: PACKAGE_BY_TARGET[`${process.platform}-${process.arch}`], version: '0.0.0' }))
    const stub = path.join(platform, 'maestro-d')
    fs.writeFileSync(stub, '#!/bin/sh\necho BUNDLED-STUB "$@"\n')
    fs.chmodSync(stub, 0o755)

    fs.mkdirSync(path.join(prefix, 'bin'))
    const link = path.join(prefix, 'bin', 'maestro-d')
    fs.symlinkSync(path.join(installed, 'bin', 'maestro-d.js'), link)

    // No MAESTRO_D_BIN, and nothing named maestro-d on PATH, so
    // only the bundled package can answer. cwd is elsewhere on purpose.
    const env = { ...process.env, PATH: [path.dirname(process.execPath), '/usr/bin', '/bin'].join(path.delimiter) }
    delete env.MAESTRO_D_BIN
    const out = execFileSync(link, ['--version'], { cwd: os.tmpdir(), env, encoding: 'utf8' })
    assert.match(out, /^BUNDLED-STUB --version/)
    fs.rmSync(prefix, { recursive: true, force: true })
  })
})
