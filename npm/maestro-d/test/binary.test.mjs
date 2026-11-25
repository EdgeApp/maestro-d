import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'

import { hubBinary, isMaestroError, PACKAGE_BY_TARGET } from '../dist/esm/index.js'

test('hubBinary resolves the override, platform package and PATH in order', t => {
  const tmp = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), 'maestro-hub-binary-')))
  const cwd = process.cwd()
  const entry = process.argv[1]
  process.argv[1] = path.join(tmp, 'entry.js')
  const old = { path: process.env.PATH, bin: process.env.MAESTRO_D_HUB_BIN }
  t.after(() => {
    process.argv[1] = entry
    process.chdir(cwd)
    process.env.PATH = old.path
    if (old.bin === undefined) delete process.env.MAESTRO_D_HUB_BIN
    else process.env.MAESTRO_D_HUB_BIN = old.bin
    fs.rmSync(tmp, { recursive: true, force: true })
  })
  process.chdir(tmp)
  process.env.PATH = tmp
  delete process.env.MAESTRO_D_HUB_BIN
  const name = 'maestro-d-hub'
  const pkg = PACKAGE_BY_TARGET[`${process.platform}-${process.arch}`]
  const bin = path.join(tmp, name)
  const override = path.join(tmp, 'override')
  assert.throws(() => hubBinary(), e => isMaestroError(e, 'DAEMON_UNAVAILABLE'))
  fs.writeFileSync(bin, '', { mode: 0o755 })
  assert.equal(hubBinary(), bin)
  if (pkg) {
    const dir = path.join(tmp, 'node_modules', pkg)
    fs.mkdirSync(path.join(dir, 'bin'), { recursive: true })
    fs.writeFileSync(path.join(dir, 'package.json'), JSON.stringify({name:pkg}))
    const bundled = path.join(dir, 'bin', name)
    fs.writeFileSync(bundled, '', { mode: 0o755 })
    assert.equal(hubBinary(), bundled)
  }
  process.env.MAESTRO_D_HUB_BIN = override
  assert.throws(() => hubBinary(), /MAESTRO_D_HUB_BIN/)
  fs.writeFileSync(override, '', { mode: 0o755 })
  assert.equal(hubBinary(), override)
  assert.equal(hubBinary(bin), bin)
})
