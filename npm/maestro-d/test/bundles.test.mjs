// Flow bundles on a hub: pack --flow through the library, whole flows and
// step lists posted as bundles, inline steps packed from cwd, flow-field
// one-shots sending bundle: refs, and outputs fetched back into the
// bundle's directory. A real daemon with the mock driver listens on TCP;
// the library reaches it as a hub through a proxy that records requests.

import assert from 'node:assert/strict'
import { execFileSync } from 'node:child_process'
import * as fs from 'node:fs'
import * as http from 'node:http'
import * as os from 'node:os'
import * as path from 'node:path'
import { after, before, beforeEach, describe, test } from 'node:test'
import { fileURLToPath } from 'node:url'

import { MaestroD, fetchBundleOutputs, isMaestroError, packFlow } from '../dist/esm/index.js'
import { HubFiles } from '../dist/esm/pack.js'

const here = path.dirname(fileURLToPath(import.meta.url))
const repoRoot = path.resolve(here, '..', '..', '..')
const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'maestro-d-bundles-'))
const bin = process.env.MAESTRO_D_TEST_BIN ?? path.join(tmp, 'maestro-d')
process.env.MAESTRO_D_HOME = path.join(tmp, 'home')
process.env.MAESTRO_D_BIN = bin
for (const k of ['MAESTRO_DEVICE', 'MAESTRO_D', 'MAESTRO_D_HUB', 'MAESTRO_D_TOKEN', 'MAESTRO_D_CA']) delete process.env[k]

const work = path.join(tmp, 'work')
const file = (rel, data) => {
  const p = path.join(work, rel)
  fs.mkdirSync(path.dirname(p), { recursive: true })
  fs.writeFileSync(p, data)
  return p
}
const exists = (rel) => fs.existsSync(path.join(work, rel))

/** @type {MaestroD} */ let tcp
/** @type {MaestroD} */ let hub
/** @type {http.Server} */ let proxy
/** @type {{method: string, url: string, body: string}[]} */ let seen = []

before(async () => {
  if (!process.env.MAESTRO_D_TEST_BIN) execFileSync('go', ['build', '-o', bin, '.'], { cwd: repoRoot, stdio: 'inherit' })
  file('flow/main.yaml', 'appId: x\n---\n- launchApp\n- runFlow: sub/open.yaml\n- runScript: scripts/compute.js\n- assertTrue: ${output.ok}\n- addMedia: [photo.png]\n- takeScreenshot: done\n')
  file('flow/sub/open.yaml', '- tapOn: General\n- takeScreenshot: x\n')
  file('flow/scripts/compute.js', 'output.ok = true')
  file('flow/photo.png', '\x89PNG photo')
  file('flow/else.yaml', '- takeScreenshot: e\n')
  tcp = await MaestroD.connect({ name: 'jsbundles', http: '127.0.0.1:0', token: 'b-token', idleTimeout: 120 })
  const port = Number(tcp.info.httpAddr.split(':').pop())
  proxy = http.createServer((req, res) => {
    const rec = { method: req.method, url: req.url, body: '' }
    seen.push(rec)
    const chunks = []
    req.on('data', (c) => chunks.push(c))
    req.on('end', () => {
      const body = Buffer.concat(chunks)
      if (req.method === 'POST') rec.body = body.toString('utf8')
      const up = http.request({ host: '127.0.0.1', port, method: req.method, path: req.url, headers: req.headers }, (ur) => {
        res.writeHead(ur.statusCode, ur.headers)
        ur.pipe(res)
      })
      up.end(body)
    })
  })
  await new Promise((resolve) => proxy.listen(0, '127.0.0.1', resolve))
  hub = await MaestroD.connect({ hub: `127.0.0.1:${proxy.address().port}`, token: 'b-token' })
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
})

const posts = (re) => seen.filter((r) => r.method === 'POST' && re.test(r.url)).map((r) => JSON.parse(r.body))

describe('packFlow', () => {
  test('packs the closure, with -e and --include, and fails naming the step', async () => {
    file('flow/data/a.csv', '1')
    file('flow/vars.yaml', '- addMedia: ["${PIC}"]\n')
    const b = await packFlow([path.join(work, 'flow', 'main.yaml')], { include: ['data/*.csv'] })
    assert.equal(b.manifest.root, 'main.yaml')
    assert.equal(b.dir, path.join(work, 'flow'))
    assert.deepEqual(Object.keys(b.manifest.files).sort(), ['data/a.csv', 'main.yaml', 'photo.png', 'scripts/compute.js', 'sub/open.yaml'])
    assert.equal(b.manifestUnit.sha256, b.sha256)
    const v = await packFlow([path.join(work, 'flow', 'vars.yaml')], { env: { PIC: 'photo.png' } })
    assert.deepEqual(Object.keys(v.manifest.files).sort(), ['photo.png', 'vars.yaml'])
    file('flow/missing.yaml', '- runFlow: ${MISSING}.yaml\n')
    await assert.rejects(
      () => packFlow([path.join(work, 'flow', 'missing.yaml')]),
      (err) => isMaestroError(err, 'USAGE') && err.message.includes('runFlow: ${MISSING}.yaml'),
    )
    file('flow/abs-out.yaml', '- takeScreenshot: /tmp/x\n')
    await assert.rejects(() => packFlow([path.join(work, 'flow', 'abs-out.yaml')]), (err) => isMaestroError(err, 'USAGE'))
    await assert.rejects(() => packFlow([path.join(work, 'flow', 'main.yaml')], { bin: path.join(work, 'flow', 'photo.png') }), (err) => isMaestroError(err))
  })
})

describe('bundles on a hub', () => {
  test('flow({file}) runs the bundle and fetches outputs beside the flow', async () => {
    const dev = await hub.attach({ device: 'mock-1', platform: 'mock' })
    const res = await dev.flow({ file: path.join(work, 'flow', 'main.yaml'), env: { A: 'b' } })
    assert.equal(res.ok, true)
    assert.deepEqual([...res.outputs].sort(), ['outputs/done.png', 'outputs/sub/x.png'])
    assert.ok(exists('flow/done.png'))
    assert.ok(exists('flow/sub/x.png'))
    const [body] = posts(/\/flows/)
    assert.equal(body.bundle.root, 'main.yaml')
    assert.deepEqual(body.env, { A: 'b' })
    assert.equal(body.yaml, undefined)
    assert.equal(body.cwd, undefined)
    // Every file was uploaded once; a second run uploads nothing.
    assert.equal(seen.filter((r) => r.method === 'PUT').length, 4)
    seen = []
    await dev.flow({ file: 'main.yaml', cwd: path.join(work, 'flow') })
    assert.equal(seen.filter((r) => r.method === 'PUT').length, 0)
    // The daemon materialized bundles/<sha>/sub/open.yaml.
    const arts = await dev.artifacts()
    assert.ok(arts.some((a) => /^bundles\/[0-9a-f]{64}\/sub\/open\.yaml$/.test(a.path)), JSON.stringify(arts))
  })

  test('flow({yaml}) stands in cwd', async () => {
    const dev = await hub.attach({ device: 'mock-1', platform: 'mock' })
    await dev.flow({ yaml: 'appId: x\n---\n- runFlow: sub/open.yaml\n- takeScreenshot: inline-flow\n', cwd: path.join(work, 'flow') })
    assert.ok(exists('flow/inline-flow.png'))
  })

  test('run(steps) packs from cwd and writes outputs back into cwd', async () => {
    const dev = await hub.attach({ device: 'mock-1', platform: 'mock' })
    const cwd = path.join(work, 'flow')
    const res = await dev.run([{ addMedia: ['photo.png'] }, { runFlow: 'sub/open.yaml' }, { takeScreenshot: 'shot' }], { cwd })
    assert.equal(res.passed, 3)
    assert.ok(exists('flow/shot.png'))
    const [body] = posts(/\/steps/)
    assert.equal(body.bundle.root, '.maestro-d-inline.yaml')
    assert.equal(body.continueOnError, false)
    assert.equal(body.steps, undefined)
    // runYaml, with continueOnError: the failure is reported, the rest ran,
    // and the outputs still come back.
    await assert.rejects(
      () => dev.runYaml('- assertTrue: ${false}\n- takeScreenshot: after-failure\n', { cwd, continueOnError: true }),
      (err) => isMaestroError(err) && err.result.passed === 1,
    )
    assert.ok(exists('flow/after-failure.png'))
    // A step that cannot be packed fails before any request.
    seen = []
    await assert.rejects(() => dev.run([{ addMedia: ['nope.png'] }], { cwd }), (err) => isMaestroError(err, 'USAGE'))
    assert.equal(posts(/\/steps/).length, 0)
  })

  test('flow-field one-shots send bundle refs and fetch the subflow outputs', async () => {
    const dev = await hub.attach({ device: 'mock-1', platform: 'mock' })
    fs.rmSync(path.join(work, 'flow', 'sub', 'x.png'), { force: true })
    await dev.runFlow('sub/open.yaml', { cwd: path.join(work, 'flow') })
    assert.ok(exists('flow/sub/x.png'))
    const [ref] = posts(/\/commands\/runFlow/)
    // Alone, the subflow is its bundle root: sub/ is the common directory.
    assert.match(ref, /^bundle:sha256:[0-9a-f]{64}\/open\.yaml$/)
    // file and else in one bundle; else's output comes back.
    seen = []
    await dev.runFlow({ file: 'sub/open.yaml', when: { true: '${false}' }, else: 'else.yaml', env: { N: '1' } }, { cwd: path.join(work, 'flow') })
    const [body] = posts(/\/commands\/runFlow/)
    assert.match(body.else, /^bundle:sha256:[0-9a-f]{64}\/else\.yaml$/)
    assert.equal(body.file.split('/')[0], body.else.split('/')[0])
    assert.ok(exists('flow/e.png'))
    await dev.command('retry', { file: path.join(work, 'flow', 'sub', 'open.yaml') })
    // Inline else steps pass through.
    const h = new HubFiles(undefined, '', work)
    assert.deepEqual(await h.rewrite('runFlow', { else: [{ tapOn: 'X' }] }), { else: [{ tapOn: 'X' }] })
    assert.equal(await h.rewrite('runFlow', 'bundle:sha256:x/y'), 'bundle:sha256:x/y')
  })

  test('fetchBundleOutputs skips entries that leave the directory', async () => {
    const calls = []
    const fake = { bytes: async (url) => (calls.push(url), Buffer.from('x')) }
    const dir = path.join(work, 'fetch')
    await fetchBundleOutputs(fake, '/d', dir, ['outputs/../escape.png', 'outputs/', 'other/x', 'outputs/a//b', 'outputs/ok/x.png'])
    assert.deepEqual(calls, ['/d/artifacts/outputs/ok/x.png'])
    assert.ok(fs.existsSync(path.join(dir, 'ok', 'x.png')))
    await fetchBundleOutputs(fake, '/d', dir, undefined)
  })
})

describe('local daemon', () => {
  test('flow({yaml}) and run(steps) still go inline with cwd', async () => {
    const dev = await tcp.attach({ device: 'mock-2', platform: 'mock' })
    await dev.flow({ yaml: 'appId: x\n---\n- tapOn: X\n' })
    await dev.run([{ tapOn: 'Y' }])
  })
})
