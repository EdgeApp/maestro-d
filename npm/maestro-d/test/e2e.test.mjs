// End-to-end tests for the JavaScript library against a real daemon
// process driving the mock driver. The Go binary is built from the repo
// root into a temp dir; MAESTRO_D_HOME keeps the run files out of
// ~/.maestro-d.

import assert from 'node:assert/strict'
import { execFileSync } from 'node:child_process'
import * as fs from 'node:fs'
import * as os from 'node:os'
import * as path from 'node:path'
import { after, before, describe, test } from 'node:test'
import { fileURLToPath } from 'node:url'

import { COMMANDS, MaestroD, MaestroError, PACKAGE_BY_TARGET, buildValue, isMaestroError, liveInfo } from '../dist/esm/index.js'

const here = path.dirname(fileURLToPath(import.meta.url))
const repoRoot = path.resolve(here, '..', '..', '..')

const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'maestro-d-js-'))
const bin = process.env.MAESTRO_D_TEST_BIN ?? path.join(tmp, 'maestro-d')
process.env.MAESTRO_D_HOME = path.join(tmp, 'home')
process.env.MAESTRO_D_BIN = bin
delete process.env.MAESTRO_DEVICE
delete process.env.MAESTRO_D

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

  test('startDevice without platform/name is a TypeError', async () => {
    await assert.rejects(() => daemon.startDevice({ platform: 'ios' }), TypeError)
  })
})

describe('commands', () => {
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

  test('assertScreenshot reports a missing baseline as data and creates nothing', async () => {
    const reference = path.join(tmp, 'never-created.png')
    const capture = path.join(tmp, 'capture.png')
    await dev.detach()
    const scoped = await daemon.attach({ ...MOCK, flowDir: tmp })
    const failure = await scoped
      .assertScreenshot({ path: reference, requireBaseline: true, captureOut: capture })
      .then(() => undefined, e => e)
    await scoped.detach()
    dev = await daemon.attach(MOCK)
    assert.ok(failure, 'a missing baseline must fail')
    assert.equal(failure.code, 'COMMAND_FAILED')
    assert.match(failure.message, /BASELINE_MISSING/)
    assert.equal(fs.existsSync(reference), false)
    assert.equal(failure.result.data.code, 'BASELINE_MISSING')
    assert.equal(failure.result.data.capturePath, capture)
    assert.equal(fs.existsSync(capture), true)
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
