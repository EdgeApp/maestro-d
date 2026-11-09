// HTTP transport over the daemon's unix socket or TCP address. Every
// failure surfaces as a MaestroError: decoded from the body when the daemon
// answered, DAEMON_UNAVAILABLE when it did not, INTERRUPTED when the
// caller's AbortSignal fired.
//
// Over TCP the link can drop, so requests that are safe to send again — a
// read, an attach, and every step-running POST, which carries an
// X-Maestro-Request-Id the daemon answers once per id — are retried with
// the same id after a reset or refused connection, PEER_UNAVAILABLE, or a
// gateway's 502/504.

import * as http from 'node:http'
import * as https from 'node:https'
import * as tls from 'node:tls'

import { MaestroError } from './errors.js'
import type { Envelope } from './types.js'

export const API_VERSION = '1'

/** Header carrying the id of one logical request. */
export const REQUEST_ID_HEADER = 'X-Maestro-Request-Id'

/** Waits before each retry: 0.5 s, then doubling, three retries in all. */
export const RETRY_DELAYS = [500, 1000, 2000]

export interface TransportOptions {
  socketPath?: string
  /** `http://127.0.0.1:7788`, `127.0.0.1:7788` or `https://hub:7433`. */
  url?: string
  token?: string
  /** PEM trust roots added to the system's for an https:// url (a self-signed hub). */
  ca?: string | Buffer
  /** The url is a hub (`hub` / MAESTRO_D_HUB): calls send no host directories. */
  hub?: boolean
  /** Milliseconds before each retry (default RETRY_DELAYS); [] disables retries. */
  retryDelays?: number[]
}

export interface RequestOptions {
  method?: 'GET' | 'POST' | 'PUT' | 'DELETE'
  body?: unknown
  accept?: string
  signal?: AbortSignal
  /** Milliseconds; 0 = none. */
  timeout?: number
  /** Sent as X-Maestro-Request-Id, and kept across retries. */
  requestId?: string
}

export interface RawResponse {
  status: number
  headers: http.IncomingHttpHeaders
  body: Buffer
}

const RETRY_ERRNOS = new Set(['ECONNRESET', 'ECONNREFUSED', 'EPIPE', 'ECONNABORTED', 'ETIMEDOUT', 'UND_ERR_SOCKET'])

/** True for a dropped or refused connection, not a certificate or name failure. */
export function isRetryableError(err: unknown): boolean {
  const e = err as NodeJS.ErrnoException
  if (!e) return false
  if (e.code && RETRY_ERRNOS.has(e.code)) return true
  return /socket hang up|ECONNRESET|connection reset/i.test(e.message ?? '')
}

/**
 * True when a response asks for a retry: PEER_UNAVAILABLE, or a gateway's
 * 502/504 without a maestro-d envelope (the daemon's own DEVICE_ERROR is a
 * 502 too, and is final).
 */
export function isRetryableResponse(status: number, body: string): boolean {
  if (status !== 502 && status !== 503 && status !== 504) return false
  try {
    const parsed = JSON.parse(body) as { error?: { code?: string } }
    if (parsed && parsed.error && parsed.error.code) return parsed.error.code === 'PEER_UNAVAILABLE'
  } catch {
    // not an envelope
  }
  return status !== 503
}

function sleep(ms: number, signal?: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal?.aborted) return reject(MaestroError.of('INTERRUPTED', 'request aborted'))
    const t = setTimeout(() => {
      signal?.removeEventListener('abort', onAbort)
      resolve()
    }, ms)
    function onAbort(): void {
      clearTimeout(t)
      reject(MaestroError.of('INTERRUPTED', 'request aborted'))
    }
    signal?.addEventListener('abort', onAbort, { once: true })
  })
}

export class Transport {
  private readonly agent: http.Agent
  /** The trust roots of an https:// transport given a ca. */
  private readonly ca?: string[]
  private readonly socketPath?: string
  private readonly host: string
  private readonly port: number
  private readonly token?: string
  private readonly secure: boolean
  private readonly retryDelays: number[]
  readonly describe: string
  /** True for a TCP connection (request ids and retries). */
  readonly tcp: boolean
  /** True when connected to a hub: no host directories are sent. */
  readonly hub: boolean

  constructor(opts: TransportOptions) {
    this.secure = false
    if (opts.socketPath) {
      this.socketPath = opts.socketPath
      this.host = 'maestro-d'
      this.port = 0
      this.describe = opts.socketPath
      this.tcp = false
    } else if (opts.url) {
      const u = new URL(opts.url.includes('://') ? opts.url : `http://${opts.url}`)
      if (u.protocol !== 'http:' && u.protocol !== 'https:') {
        throw new TypeError(`unsupported url scheme ${u.protocol} (use http:// or https://)`)
      }
      this.secure = u.protocol === 'https:'
      this.host = u.hostname.replace(/^\[(.*)\]$/, '$1')
      this.port = Number(u.port || (this.secure ? 443 : 80))
      this.describe = u.origin
      this.tcp = true
    } else {
      throw new TypeError('Transport needs a socketPath or url')
    }
    this.hub = opts.hub === true
    this.retryDelays = this.tcp ? (opts.retryDelays ?? RETRY_DELAYS) : []
    // The daemon serialises steps per device; keep a few idle sockets so a
    // long step and an event stream do not fight over one connection.
    if (this.secure) {
      const agentOpts: https.AgentOptions = { keepAlive: true, maxSockets: 8 }
      if (opts.ca) {
        this.ca = [...tls.rootCertificates, opts.ca.toString()]
        agentOpts.ca = this.ca
      }
      this.agent = new https.Agent(agentOpts)
    } else {
      this.agent = new http.Agent({ keepAlive: true, maxSockets: 8 })
    }
    if (opts.token) this.token = opts.token
  }

  /** Close idle keep-alive sockets. */
  close(): void {
    this.agent.destroy()
  }

  private request(reqOpts: http.RequestOptions, cb: (res: http.IncomingMessage) => void): http.ClientRequest {
    return this.secure ? https.request(reqOpts, cb) : http.request(reqOpts, cb)
  }

  private baseOptions(method: string, headers: http.OutgoingHttpHeaders, signal?: AbortSignal): http.RequestOptions {
    const reqOpts: http.RequestOptions = { method, headers, agent: this.agent, host: this.host, port: this.port }
    if (this.socketPath) {
      reqOpts.socketPath = this.socketPath
      delete reqOpts.port
    }
    if (signal) reqOpts.signal = signal
    return reqOpts
  }

  /** A request that may be sent again: it has an id, only reads, or attaches. */
  private retryable(path: string, opts: RequestOptions): boolean {
    const method = opts.method ?? 'GET'
    return opts.requestId !== undefined || method === 'GET' || (method === 'POST' && /\/attach(\?|$)/.test(path))
  }

  /** Perform a request and return the raw response, retrying a dropped link. */
  async raw(path: string, opts: RequestOptions = {}): Promise<RawResponse> {
    const delays = this.retryable(path, opts) ? this.retryDelays : []
    for (let attempt = 0; ; attempt++) {
      let res: RawResponse
      try {
        res = await this.once(path, opts)
      } catch (err) {
        if (opts.signal?.aborted || attempt >= delays.length || !isRetryableError(err)) {
          throw this.transportError(err, opts.signal)
        }
        await sleep(delays[attempt] as number, opts.signal)
        continue
      }
      if (attempt < delays.length && isRetryableResponse(res.status, res.body.toString('utf8'))) {
        await sleep(delays[attempt] as number, opts.signal)
        continue
      }
      return res
    }
  }

  private once(path: string, opts: RequestOptions): Promise<RawResponse> {
    const method = opts.method ?? 'GET'
    const headers: http.OutgoingHttpHeaders = {
      'X-Maestro-Api-Version': API_VERSION,
      Accept: opts.accept ?? 'application/json',
    }
    let payload: Buffer | undefined
    if (opts.body !== undefined) {
      payload = Buffer.from(JSON.stringify(opts.body), 'utf8')
      headers['Content-Type'] = 'application/json'
      headers['Content-Length'] = payload.length
    }
    if (this.token) headers.Authorization = `Bearer ${this.token}`
    if (opts.requestId) headers[REQUEST_ID_HEADER] = opts.requestId

    const reqOpts = { ...this.baseOptions(method, headers, opts.signal), path }
    if (opts.timeout) reqOpts.timeout = opts.timeout

    return new Promise<RawResponse>((resolve, reject) => {
      const req = this.request(reqOpts, (res) => {
        const chunks: Buffer[] = []
        res.on('data', (c: Buffer) => chunks.push(c))
        res.on('end', () => resolve({ status: res.statusCode ?? 0, headers: res.headers, body: Buffer.concat(chunks) }))
        res.on('error', reject)
        res.on('aborted', () => reject(Object.assign(new Error('socket hang up'), { code: 'ECONNRESET' })))
      })
      req.on('error', reject)
      if (opts.timeout) {
        req.on('timeout', () => req.destroy(new Error(`no response within ${opts.timeout}ms`)))
      }
      if (payload) req.write(payload)
      req.end()
    })
  }

  /**
   * JSON request. A non-2xx response is decoded into a MaestroError; when
   * the body is itself a result envelope (a failed step) it is attached as
   * `result`.
   */
  async json<T extends Envelope>(path: string, opts: RequestOptions = {}): Promise<T> {
    const res = await this.raw(path, opts)
    const text = res.body.toString('utf8')
    let parsed: T | undefined
    if (text.trim() !== '') {
      try {
        parsed = JSON.parse(text) as T
      } catch (err) {
        if (res.status < 300) {
          throw MaestroError.of('INTERNAL', `decode ${opts.method ?? 'GET'} ${path}: ${(err as Error).message}`)
        }
      }
    }
    if (res.status >= 300) {
      const result = parsed && ('step' in parsed || 'results' in parsed) ? parsed : undefined
      throw MaestroError.fromResponse(res.status, text, result as never)
    }
    if (parsed === undefined) {
      throw MaestroError.of('INTERNAL', `empty response from ${opts.method ?? 'GET'} ${path}`)
    }
    return parsed
  }

  /** Binary request (screenshots, raw hierarchies). */
  async bytes(path: string, opts: RequestOptions = {}): Promise<Buffer> {
    const res = await this.raw(path, { accept: '*/*', ...opts })
    if (res.status >= 300) {
      throw MaestroError.fromResponse(res.status, res.body.toString('utf8'))
    }
    return res.body
  }

  /**
   * Open a Server-Sent Events stream and yield each event's parsed `data`.
   * Ends when the signal fires or the daemon closes the stream.
   */
  async *sse<T>(path: string, signal?: AbortSignal): AsyncGenerator<T, void, undefined> {
    const headers: http.OutgoingHttpHeaders = {
      'X-Maestro-Api-Version': API_VERSION,
      Accept: 'text/event-stream',
    }
    if (this.token) headers.Authorization = `Bearer ${this.token}`
    // An event stream holds its socket for as long as it runs, so it does
    // not take one of the pool's: a socket of its own, with the same trust
    // roots.
    const reqOpts: https.RequestOptions = { ...this.baseOptions('GET', headers, signal), path, agent: false }
    if (this.ca) reqOpts.ca = this.ca

    const res = await new Promise<http.IncomingMessage>((resolve, reject) => {
      const req = this.request(reqOpts, resolve)
      req.on('error', (err) => reject(this.transportError(err, signal)))
      req.end()
    })
    if ((res.statusCode ?? 0) >= 300) {
      const chunks: Buffer[] = []
      for await (const c of res) chunks.push(c as Buffer)
      throw MaestroError.fromResponse(res.statusCode ?? 0, Buffer.concat(chunks).toString('utf8'))
    }

    let buffered = ''
    let data: string[] = []
    try {
      for await (const chunk of res) {
        buffered += (chunk as Buffer).toString('utf8')
        let nl: number
        while ((nl = buffered.indexOf('\n')) >= 0) {
          const line = buffered.slice(0, nl).replace(/\r$/, '')
          buffered = buffered.slice(nl + 1)
          if (line === '') {
            if (data.length > 0) {
              const text = data.join('\n')
              data = []
              try {
                yield JSON.parse(text) as T
              } catch {
                // malformed event; skip
              }
            }
          } else if (line.startsWith('data:')) {
            data.push(line.slice(5).trim())
          }
        }
      }
    } catch (err) {
      if (signal?.aborted) return
      throw this.transportError(err, signal)
    } finally {
      res.destroy()
    }
  }

  private transportError(err: unknown, signal?: AbortSignal): MaestroError {
    if (err instanceof MaestroError) return err
    const e = err as NodeJS.ErrnoException
    if (signal?.aborted || e.name === 'AbortError' || e.code === 'ABORT_ERR') {
      return MaestroError.of('INTERRUPTED', 'request aborted', { cause: err })
    }
    return MaestroError.of('DAEMON_UNAVAILABLE', `daemon ${this.describe}: ${e.message ?? String(err)}`, { cause: err })
  }
}
