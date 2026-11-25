// Locating the maestro-d Go binary.
//
// Order: $MAESTRO_D_BIN → the per-platform npm package installed as an
// optional dependency → `maestro-d` on $PATH. Nothing is downloaded at
// install time; npm picks the platform package from the lockfile.

import * as fs from 'node:fs'
import { createRequire } from 'node:module'
import * as path from 'node:path'

import { MaestroError } from './errors.js'
import { ENV_BIN } from './runfiles.js'

/** npm uses Node's process.arch names, so x64 rather than Go's amd64. */
export const PACKAGE_BY_TARGET: Record<string, string> = {
  'darwin-arm64': 'maestro-d-darwin-arm64',
  'darwin-x64': 'maestro-d-darwin-x64',
  'linux-arm64': 'maestro-d-linux-arm64',
  'linux-x64': 'maestro-d-linux-x64',
}

export function target(): string {
  return `${process.platform}-${process.arch}`
}

/** realpath, or the path itself when it cannot be resolved. */
function realPath(p: string): string {
  try {
    return fs.realpathSync(p)
  } catch {
    return p
  }
}

function isExecutable(p: string): boolean {
  try {
    fs.accessSync(p, fs.constants.X_OK)
    return fs.statSync(p).isFile()
  } catch {
    return false
  }
}

/** The bundled platform package's binary, or undefined when not installed. */
export function bundledBinary(name = 'maestro-d'): string | undefined {
  const pkg = PACKAGE_BY_TARGET[target()]
  if (!pkg) return undefined
  // Resolve the package's manifest rather than a main entry (it has none).
  // A registry install puts it beside maestro-d in the consumer's
  // node_modules, which is reachable from the cwd and from the entry script;
  // both are searched so `npm link` and monorepos work too.
  //
  // The entry has to be the real path: `npm i -g` and `npx` run bin/ through
  // a symlink outside the package (…/bin/maestro-d → …/lib/node_modules/
  // maestro-d/bin/maestro-d.js), and resolving from the link's own
  // directory never reaches the package's node_modules.
  const entry = process.argv[1]
  const roots = [process.cwd()]
  if (entry != null) {
    const resolved = path.resolve(entry)
    roots.push(path.dirname(realPath(resolved)))
  }
  for (const from of roots) {
    try {
      const resolver = createRequire(path.join(from, 'noop.js'))
      const manifest = resolver.resolve(`${pkg}/package.json`)
      const bin = path.join(path.dirname(manifest), 'bin', name)
      if (isExecutable(bin)) return bin
    } catch {
      // not here
    }
  }
  return undefined
}

/** First `maestro-d` on $PATH, or undefined. */
export function pathBinary(name = 'maestro-d'): string | undefined {
  const dirs = (process.env.PATH ?? '').split(path.delimiter).filter(Boolean)
  for (const dir of dirs) {
    const p = path.join(dir, name)
    if (isExecutable(p) && (!process.argv[1] || realPath(p) !== realPath(process.argv[1]))) return p
  }
  return undefined
}

/**
 * Absolute path of the binary to run. Throws DAEMON_UNAVAILABLE with an
 * actionable message when none can be found.
 */
export function binaryPath(explicit?: string): string {
  return resolveBinary('maestro-d', ENV_BIN, explicit)
}

/** Absolute path of the hub executable distributed beside the client. */
export function hubBinary(explicit?: string): string {
  return resolveBinary('maestro-d-hub', 'MAESTRO_D_HUB_BIN', explicit)
}

function resolveBinary(name: string, envName: string, explicit?: string): string {
  const chosen = explicit || process.env[envName]
  if (chosen) {
    if (!isExecutable(chosen)) {
      throw MaestroError.of('DAEMON_UNAVAILABLE', `${explicit ? 'bin' : envName} ${chosen} is not an executable file`)
    }
    return chosen
  }
  const bundled = bundledBinary(name)
  if (bundled) return bundled
  const onPath = pathBinary(name)
  if (onPath) return onPath
  const pkg = PACKAGE_BY_TARGET[target()]
  throw MaestroError.of(
    'DAEMON_UNAVAILABLE',
    pkg
      ? `${name} binary not found: install ${pkg} (it is an optional dependency of maestro-d; ` +
          `installs with --no-optional skip it), put ${name} on PATH, or set ${envName}`
      : `${name} has no prebuilt binary for ${target()}; build it from source ` +
          `(go build -o ${name} ./cmd/${name}) and set ${envName} or put it on PATH`,
  )
}
