// A fake AWS Device Farm for the JavaScript library's tests, the Node twin
// of pkg/devicefarm/dftest: the awsJson1.1 operations maestro-d calls
// (X-Amz-Target: DeviceFarm_20150623.<Op>), presigned-style upload URLs, and
// one Appium endpoint per running session. A daemon reaches it through
// MAESTRO_D_DEVICEFARM_ENDPOINT; fakeEnv() is the environment that pins the
// AWS credential chain to fake keys and shortens the daemon's waits.

import * as crypto from 'node:crypto'
import * as fs from 'node:fs'
import * as http from 'node:http'
import * as path from 'node:path'

const TARGET = 'DeviceFarm_20150623.'
const ACCOUNT = '123456789012'

export const SEED_DEVICES = [
  { arn: 'arn:aws:devicefarm:us-west-2::device:8A11C0DE000000000000000000000008', name: 'Google Pixel 8', platform: 'ANDROID', os: '14', formFactor: 'PHONE', manufacturer: 'Google', availability: 'HIGHLY_AVAILABLE' },
  { arn: 'arn:aws:devicefarm:us-west-2::device:7A11C0DE000000000000000000000007', name: 'Google Pixel 7', platform: 'ANDROID', os: '13', formFactor: 'PHONE', manufacturer: 'Google', availability: 'AVAILABLE' },
  { arn: 'arn:aws:devicefarm:us-west-2::device:15A9E00000000000000000000000000F', name: 'Apple iPhone 15', platform: 'IOS', os: '17.4', formFactor: 'PHONE', manufacturer: 'Apple', availability: 'HIGHLY_AVAILABLE' },
  { arn: 'arn:aws:devicefarm:us-west-2::device:14A9E00000000000000000000000000E', name: 'Apple iPhone 14', platform: 'IOS', os: '27.0', formFactor: 'PHONE', manufacturer: 'Apple', availability: 'HIGHLY_AVAILABLE' },
  { arn: 'arn:aws:devicefarm:us-west-2::device:12A9E00000000000000000000000000C', name: 'Apple iPhone 12', platform: 'IOS', os: '15.0.2', formFactor: 'PHONE', manufacturer: 'Apple', availability: 'BUSY' },
]

const epoch = (ms) => ms / 1000

/**
 * The environment a daemon under test runs with: the fake's endpoint, fake
 * keys, and every other source of AWS credentials pinned to nothing.
 * @param {string} url the fake's URL
 * @param {string} dir a directory for the empty shared files
 * @param {{withKey?: boolean}} [opts]
 */
export function fakeEnv(url, dir, opts = {}) {
  const env = { ...process.env }
  for (const k of Object.keys(env)) {
    if (k.startsWith('AWS_CONTAINER_CREDENTIALS')) delete env[k]
  }
  for (const k of ['AWS_PROFILE', 'AWS_DEFAULT_PROFILE', 'AWS_ACCESS_KEY_ID', 'AWS_SECRET_ACCESS_KEY', 'AWS_SESSION_TOKEN', 'AWS_ROLE_ARN', 'AWS_WEB_IDENTITY_TOKEN_FILE']) {
    delete env[k]
  }
  const creds = path.join(dir, 'aws-credentials')
  const config = path.join(dir, 'aws-config')
  fs.writeFileSync(creds, '')
  fs.writeFileSync(config, '')
  env.AWS_SHARED_CREDENTIALS_FILE = creds
  env.AWS_CONFIG_FILE = config
  env.AWS_EC2_METADATA_DISABLED = 'true'
  env.MAESTRO_D_DEVICEFARM_ENDPOINT = url
  // Short waits: session polls every 100 ms, idle release from 1 s.
  env.MAESTRO_D_TEST_FAST_CLOUD = '1'
  if (opts.withKey !== false) {
    env.AWS_ACCESS_KEY_ID = 'AKIAFAKEFAKEFAKE0001'
    env.AWS_SECRET_ACCESS_KEY = 'fake-secret-key-for-tests'
  }
  return env
}

export class FakeDeviceFarm {
  constructor() {
    this.devices = SEED_DEVICES.map((d) => ({ remoteAccessEnabled: true, ...d }))
    this.projects = []
    this.uploads = []
    this.sessions = []
    /** @type {{op: string, method: string, path: string, body: any}[]} */
    this.requests = []
    this.trial = { total: 1000, remaining: 990 }
    this.server = http.createServer((req, res) => this.serve(req, res))
  }

  /** Listen on a random loopback port; resolves with the URL. */
  async start() {
    await new Promise((resolve) => this.server.listen(0, '127.0.0.1', resolve))
    this.url = `http://127.0.0.1:${this.server.address().port}`
    return this.url
  }

  close() {
    this.server.closeAllConnections?.()
    return new Promise((resolve) => this.server.close(() => resolve()))
  }

  /** The AWS operations served, in order. */
  ops() {
    return this.requests.filter((r) => r.op).map((r) => r.op)
  }

  serve(req, res) {
    const chunks = []
    req.on('data', (c) => chunks.push(c))
    req.on('end', () => {
      const raw = Buffer.concat(chunks)
      const url = new URL(req.url, 'http://x')
      const op = (req.headers['x-amz-target'] ?? '').replace(TARGET, '')
      let body
      try {
        body = op && raw.length ? JSON.parse(raw.toString('utf8')) : undefined
      } catch {
        body = undefined
      }
      this.requests.push({ op, method: req.method, path: url.pathname, body })
      if (url.pathname.startsWith('/upload/')) return this.serveUpload(req, res, url, raw)
      if (url.pathname.startsWith('/remote-endpoint/')) return this.serveAppium(req, res, url, raw)
      if (!op) return this.awsError(res, 'UnknownOperationException', 'not a Device Farm operation')
      this.serveAWS(res, op, body ?? {})
    })
  }

  json(res, status, v) {
    res.writeHead(status, { 'Content-Type': 'application/x-amz-json-1.1' })
    res.end(JSON.stringify(v))
  }

  awsError(res, code, message) {
    res.writeHead(400, { 'Content-Type': 'application/x-amz-json-1.1', 'X-Amzn-ErrorType': code })
    res.end(JSON.stringify({ __type: code, message }))
  }

  ensureProject(name) {
    let p = this.projects.find((x) => x.name === name)
    if (!p) {
      p = { arn: `arn:aws:devicefarm:us-west-2:${ACCOUNT}:project:${crypto.randomUUID()}`, name, created: epoch(Date.now()) }
      this.projects.push(p)
    }
    return p
  }

  deviceJSON(d) {
    return { ...d, model: d.name, type: 'PHYSICAL' }
  }

  sessionJSON(s, full) {
    const j = { arn: s.arn, name: s.name, status: s.status, result: s.result, created: s.created, billingMethod: 'METERED' }
    const d = this.devices.find((x) => x.arn === s.deviceArn)
    if (d) j.device = this.deviceJSON(d)
    if (s.started) {
      j.started = s.started
      const m = Math.max(1, Math.ceil((Date.now() / 1000 - s.started) / 60))
      j.deviceMinutes = { total: m, metered: m, unmetered: 0 }
    }
    if (full && s.status === 'RUNNING') {
      j.endpoints = { remoteDriverEndpoint: `${this.url}/remote-endpoint/${s.token}/wd/hub` }
    }
    return j
  }

  uploadJSON(u) {
    const j = { arn: u.arn, name: u.name, type: u.type, status: u.status, created: u.created, contentType: 'application/octet-stream' }
    if (u.status === 'INITIALIZED') j.url = `${this.url}/upload/${u.token}?X-Amz-Signature=${u.token}`
    return j
  }

  serveAWS(res, op, b) {
    switch (op) {
      case 'GetAccountSettings':
        return this.json(res, 200, {
          accountSettings: {
            awsAccountNumber: ACCOUNT,
            trialMinutes: this.trial,
            maxSlots: { '00000000-0000-0000-0000-000000000000': 1 },
            unmeteredDevices: { ANDROID: 0, IOS: 0 },
            unmeteredRemoteAccessDevices: { ANDROID: 0, IOS: 0 },
            maxJobTimeoutMinutes: 150,
          },
        })
      case 'ListProjects':
        return this.json(res, 200, { projects: this.projects })
      case 'CreateProject':
        return this.json(res, 200, { project: this.ensureProject(b.name) })
      case 'ListDevices': {
        const filters = b.filters ?? []
        const ok = (d) =>
          filters.every((f) => {
            const v = { REMOTE_ACCESS_ENABLED: String(d.remoteAccessEnabled).toUpperCase(), PLATFORM: d.platform }[f.attribute]
            return v === undefined || f.values.some((w) => w.toUpperCase() === v)
          })
        return this.json(res, 200, { devices: this.devices.filter(ok).map((d) => this.deviceJSON(d)) })
      }
      case 'CreateUpload': {
        const u = { arn: `arn:aws:devicefarm:us-west-2:${ACCOUNT}:upload:${crypto.randomUUID()}`, projectArn: b.projectArn, name: b.name, type: b.type,
          status: 'INITIALIZED', created: epoch(Date.now()), token: crypto.randomBytes(8).toString('hex') }
        this.uploads.push(u)
        return this.json(res, 200, { upload: this.uploadJSON(u) })
      }
      case 'GetUpload': {
        const u = this.uploads.find((x) => x.arn === b.arn)
        if (!u) return this.awsError(res, 'NotFoundException', `no upload ${b.arn}`)
        return this.json(res, 200, { upload: this.uploadJSON(u) })
      }
      case 'ListUploads':
        return this.json(res, 200, { uploads: this.uploads.filter((u) => u.projectArn === b.arn).map((u) => this.uploadJSON(u)) })
      case 'CreateRemoteAccessSession': {
        const s = { arn: `arn:aws:devicefarm:us-west-2:${ACCOUNT}:session:${crypto.randomUUID()}/${crypto.randomUUID()}/00000`,
          projectArn: b.projectArn, deviceArn: b.deviceArn, appArn: b.appArn, name: b.name, status: 'PENDING', result: 'PENDING',
          created: epoch(Date.now()), token: crypto.randomBytes(8).toString('hex'), parameters: b.configuration?.parameters ?? {},
          installed: [], webdrivers: 0, opened: 0 }
        this.sessions.push(s)
        return this.json(res, 200, { remoteAccessSession: this.sessionJSON(s, true) })
      }
      case 'GetRemoteAccessSession': {
        const s = this.sessions.find((x) => x.arn === b.arn)
        if (!s) return this.awsError(res, 'NotFoundException', `no session ${b.arn}`)
        this.json(res, 200, { remoteAccessSession: this.sessionJSON(s, true) })
        if (s.status === 'PENDING') s.status = 'PREPARING'
        else if (s.status === 'PREPARING') Object.assign(s, { status: 'RUNNING', started: epoch(Date.now()) })
        return
      }
      case 'StopRemoteAccessSession': {
        const s = this.sessions.find((x) => x.arn === b.arn)
        if (!s) return this.awsError(res, 'NotFoundException', `no session ${b.arn}`)
        if (s.status === 'COMPLETED') return this.awsError(res, 'ArgumentException', 'the session has already completed')
        Object.assign(s, { status: 'COMPLETED', result: 'STOPPED' })
        return this.json(res, 200, { remoteAccessSession: this.sessionJSON(s, true) })
      }
      case 'ListRemoteAccessSessions':
        return this.json(res, 200, { remoteAccessSessions: this.sessions.filter((s) => s.projectArn === b.arn).map((s) => this.sessionJSON(s, false)) })
      case 'InstallToRemoteAccessSession': {
        const s = this.sessions.find((x) => x.arn === b.remoteAccessSessionArn)
        const u = this.uploads.find((x) => x.arn === b.appArn)
        if (!s || !u) return this.awsError(res, 'NotFoundException', 'no session or upload')
        s.installed.push(u.arn)
        return this.json(res, 200, { appUpload: this.uploadJSON(u) })
      }
      default:
        return this.awsError(res, 'UnknownOperationException', `the fake does not serve ${op}`)
    }
  }

  serveUpload(req, res, url, raw) {
    const u = this.uploads.find((x) => url.pathname === `/upload/${x.token}`)
    if (!u || req.method !== 'PUT') {
      res.writeHead(404)
      return res.end()
    }
    u.size = raw.length
    u.status = 'SUCCEEDED'
    res.writeHead(200)
    res.end()
  }

  // A minimal WebDriver endpoint: sessions, the screen, one button.
  serveAppium(req, res, url, raw) {
    const m = url.pathname.match(/^\/remote-endpoint\/([^/]+)\/wd\/hub(\/.*)?$/)
    const s = m && this.sessions.find((x) => x.token === m[1] && x.status === 'RUNNING')
    const reply = (status, value) => {
      res.writeHead(status, { 'Content-Type': 'application/json' })
      res.end(JSON.stringify({ value }))
    }
    if (!s) return reply(404, { error: 'invalid session id', message: 'no such session' })
    const rest = m[2] ?? '/'
    const ios = this.devices.find((d) => d.arn === s.deviceArn)?.platform === 'IOS'
    if (req.method === 'POST' && rest === '/session') {
      s.webdrivers++
      s.opened++
      s.caps = JSON.parse(raw.toString('utf8') || '{}')?.capabilities?.alwaysMatch
      return reply(200, { sessionId: crypto.randomUUID(), capabilities: { platformName: ios ? 'iOS' : 'Android' } })
    }
    if (rest === '/status') return reply(200, { ready: true, message: 'fake' })
    const p = rest.replace(/^\/session\/[^/]+/, '')
    if (req.method === 'DELETE' && p === '') {
      s.webdrivers = Math.max(0, s.webdrivers - 1)
      return reply(200, null)
    }
    if (p === '/window/rect') return reply(200, { x: 0, y: 0, width: 390, height: 844 })
    if (p === '/screenshot') return reply(200, ONE_PIXEL_PNG)
    if (p === '/orientation') return reply(200, 'PORTRAIT')
    if (p === '/source') {
      return reply(200, ios
        ? '<?xml version="1.0" encoding="UTF-8"?><XCUIElementTypeApplication type="XCUIElementTypeApplication" name="App" label="App" enabled="true" visible="true" x="0" y="0" width="390" height="844"><XCUIElementTypeButton type="XCUIElementTypeButton" name="Button" label="Button" enabled="true" visible="true" x="20" y="100" width="200" height="44"/></XCUIElementTypeApplication>'
        : '<?xml version="1.0" encoding="UTF-8"?><hierarchy rotation="0"><android.widget.FrameLayout index="0" bounds="[0,0][1080,2400]" text="" resource-id="" class="android.widget.FrameLayout" package="org.example" enabled="true" displayed="true"><android.widget.Button index="0" text="Button" resource-id="org.example:id/button" class="android.widget.Button" package="org.example" bounds="[100,200][500,300]" enabled="true" clickable="true" displayed="true"/></android.widget.FrameLayout></hierarchy>')
    }
    if (p === '/element' || p === '/elements') {
      const el = { 'element-6066-11e4-a52e-4f735466cecf': 'el-1' }
      return reply(200, p === '/elements' ? [el] : el)
    }
    if (/^\/element\/[^/]+\/rect$/.test(p)) return reply(200, { x: 20, y: 100, width: 200, height: 44 })
    if (/^\/element\/[^/]+\/(text|attribute\/.*)$/.test(p)) return reply(200, 'Button')
    if (/^\/element\/[^/]+\/displayed$/.test(p)) return reply(200, true)
    // The daemon waits on this after an install; the fake installs at once.
    if (p === '/execute/sync' && raw.toString('utf8').includes('mobile: isAppInstalled')) {
      s.installChecks = (s.installChecks ?? 0) + 1
      return reply(200, true)
    }
    return reply(200, null)
  }
}

// A 1×1 PNG, base64.
const ONE_PIXEL_PNG = 'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=='
