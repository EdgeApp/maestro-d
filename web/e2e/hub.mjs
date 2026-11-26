// Each fixture owns its config, run directory, API port and dashboard port.
import { execFileSync } from 'node:child_process'
import { randomBytes } from 'node:crypto'
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import path from 'node:path'

export const devices = {
  'mock-1': { name: 'Bench Pixel', platform: 'android', kind: 'device', model: 'Pixel 8', osVersion: '16' },
  'mock-2': { name: 'Lab iPad', platform: 'ios', kind: 'device', model: 'iPad16,3', osVersion: '26.3' },
  'mock-3': { name: 'Prototype', platform: 'ios', kind: 'device', model: 'iPhone99,1', osVersion: '27.0' }
}

export function createHub(name, props = devices, credentials = {}) {
  const home = mkdtempSync(path.join(tmpdir(), 'md-dashboard-'))
  const token = credentials.token ?? randomBytes(20).toString('hex')
  const peerToken = credentials.peerToken ?? randomBytes(20).toString('hex')
  const env = {
    ...process.env,
    MAESTRO_D_HOME: home,
    MAESTRO_D_CONFIG: path.join(home, 'config.json'),
    MAESTRO_D_MOCK_DEVICES: Object.keys(props).join(','),
    MAESTRO_D_MOCK_DEVICE_PROPS: JSON.stringify(props),
    MAESTRO_D_TOKEN: token,
    MAESTRO_D_PEER_TOKEN: peerToken
  }
  writeFileSync(
    env.MAESTRO_D_CONFIG,
    JSON.stringify({ hubDevices: Object.keys(props).map((id) => ({ id })) }),
    { mode: 0o600 }
  )
  const cli = (...args) =>
    execFileSync(process.env.MAESTRO_D_TEST_HUB_BIN ?? 'maestro-d-hub', args, {
      env,
      encoding: 'utf8',
      timeout: 45_000
    })
  let info
  let running = false
  return {
    name,
    token,
    peerToken,
    cli,
    get address() {
      return info.httpAddr
    },
    get web() {
      return `https://127.0.0.1:${new URL('https://' + info.webAddr).port}`
    },
    start(peer) {
      const port = info ? new URL('https://' + info.webAddr).port : '0'
      info = JSON.parse(
        cli(
          'start',
          '--name',
          name,
          '--listen',
          info?.httpAddr ?? '127.0.0.1:0',
          '--web-port',
          port,
          '--no-mdns',
          ...(peer ? ['--peer', peer] : []),
          '--json'
        )
      ).daemon
      running = true
    },
    stop() {
      cli('stop')
      running = false
    },
    crash() {
      process.kill(info.pid, 'SIGKILL')
      running = false
    },
    async request(method, route, body) {
      const response = await fetch('http://' + info.httpAddr + '/v1/' + route, {
        method,
        headers: { authorization: 'Bearer ' + token, 'content-type': 'application/json' },
        body: body === undefined ? undefined : JSON.stringify(body)
      })
      const parsed = await response.json()
      if (!response.ok || !parsed.ok) throw Error(method + ' ' + route + ' failed: ' + JSON.stringify(parsed))
      return parsed
    },
    dispose() {
      try {
        if (running) cli('stop', '--end-leases')
      } finally {
        rmSync(home, { recursive: true, force: true })
      }
    }
  }
}
