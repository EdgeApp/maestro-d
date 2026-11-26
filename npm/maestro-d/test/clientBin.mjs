import { execFileSync } from 'node:child_process'
import * as path from 'node:path'
import { fileURLToPath } from 'node:url'

export function testClientBin(tmp) {
  if (process.env.MAESTRO_D_TEST_BIN) return process.env.MAESTRO_D_TEST_BIN
  const bin = path.join(tmp, 'maestro-d')
  execFileSync('go', ['build', '-o', bin, './cmd/maestro-d'], {
    cwd: fileURLToPath(new URL('../../../', import.meta.url)),
    stdio: 'inherit',
  })
  return bin
}
