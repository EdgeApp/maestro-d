// Where a daemon records itself (mirrors pkg/daemon/runfile.go):
//
//   ~/.maestro-d/run/<name>/daemon.json   pid, socket path, version
//   ~/.maestro-d/run/<name>/daemon.sock   unix socket
//   ~/.maestro-d/run/<name>/startup.log   stdout/stderr of `serve`
//
// MAESTRO_D_HOME overrides ~/.maestro-d.

import * as fs from 'node:fs'
import * as os from 'node:os'
import * as path from 'node:path'

import type { DaemonInfo } from './types.js'

export const DEFAULT_NAME = 'default'
export const ENV_NAME = 'MAESTRO_D'
export const ENV_HOME = 'MAESTRO_D_HOME'
export const ENV_DEVICE = 'MAESTRO_DEVICE'
export const ENV_BIN = 'MAESTRO_D_BIN'

const NAME_RE = /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/

/** Daemon name from an explicit value, `$MAESTRO_D`, or 'default'. */
export function resolveName(name?: string): string {
  return name || process.env[ENV_NAME] || DEFAULT_NAME
}

export function validateName(name: string): void {
  if (!NAME_RE.test(name)) {
    throw new TypeError(`invalid daemon name ${JSON.stringify(name)}: use letters, digits, '.', '_' or '-'`)
  }
}

export function home(): string {
  return process.env[ENV_HOME] || path.join(os.homedir(), '.maestro-d')
}

export function runDir(name: string): string {
  return path.join(home(), 'run', name)
}

export function infoPath(name: string): string {
  return path.join(runDir(name), 'daemon.json')
}

export function startupLogPath(name: string): string {
  return path.join(runDir(name), 'startup.log')
}

/** True when a process with this pid exists (EPERM counts as alive). */
export function pidAlive(pid: number): boolean {
  if (!(pid > 0)) return false
  try {
    process.kill(pid, 0)
    return true
  } catch (err) {
    return (err as NodeJS.ErrnoException).code === 'EPERM'
  }
}

/** daemon.json for the name, or undefined when there is none. */
export function readInfo(name: string): DaemonInfo | undefined {
  return readRecord(infoPath(name))
}

/** The local hub's recorded socket may be outside its run directory. */
export function readHubInfo(): DaemonInfo | undefined {
  return readRecord(path.join(home(), 'hub', 'run', 'daemon.json'))
}

function readRecord(file: string): DaemonInfo | undefined {
  let data: string
  try {
    data = fs.readFileSync(file, 'utf8')
  } catch {
    return undefined
  }
  try {
    return JSON.parse(data) as DaemonInfo
  } catch {
    return undefined
  }
}

/** daemon.json when its process is alive; stale files are removed. */
export function liveInfo(name: string): DaemonInfo | undefined {
  const info = readInfo(name)
  if (info === undefined) return undefined
  if (!pidAlive(info.pid)) {
    removeRunFiles(name)
    return undefined
  }
  return info
}

export function removeRunFiles(name: string): void {
  for (const f of ['daemon.sock', 'daemon.json', 'devices.json']) {
    try {
      fs.unlinkSync(path.join(runDir(name), f))
    } catch {
      // already gone
    }
  }
}

/** Last `n` lines of a file, for error messages. */
export function tailFile(file: string, n: number): string {
  let data: string
  try {
    data = fs.readFileSync(file, 'utf8')
  } catch {
    return ''
  }
  const lines = data.replace(/\n+$/, '').split('\n')
  return `--- ${file} ---\n${lines.slice(-n).join('\n')}`
}
