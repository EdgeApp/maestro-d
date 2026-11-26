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

import { randomUUID } from 'node:crypto'
import * as fs from 'node:fs'
import * as os from 'node:os'
import * as http from 'node:http'
import * as https from 'node:https'
import * as tls from 'node:tls'
import { Readable, Transform, type TransformCallback } from 'node:stream'

import { codeFromHeaders, MaestroError } from './errors.js'
import type { Envelope } from './types.js'

export const API_VERSION = '1'

/** Header carrying the id of one logical request. */
export const REQUEST_ID_HEADER = 'X-Maestro-Request-Id'

/** Sent to a hub: the client names its output files and fetches them back from outputs/. */
export const CLIENT_OUTPUTS_HEADER = 'X-Maestro-Client-Outputs'

/** Header naming the caller's computer, so a daemon or hub marks devices on it `local`. */
export const CLIENT_MACHINE_HEADER = 'X-Maestro-Client-Machine'

/**
 * This computer's name as maestro-d reports it: the host name, or
 * MAESTRO_D_TEST_MACHINE when that is set (tests run several "machines"
 * on one host).
 */
export function machineName(): string {
  return process.env.MAESTRO_D_TEST_MACHINE || os.hostname()
}

/** Header carrying the lease of the device a request is for (a hub's leased devices). */
export const LEASE_HEADER = 'X-Maestro-Lease'

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
  method?: 'GET' | 'HEAD' | 'POST' | 'PUT' | 'DELETE'
  body?: unknown
  accept?: string
  signal?: AbortSignal
  /** Explicit credential for this request. Null sends it without a lease. */
  lease?: string | null
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

/** A streamed response: its headers, and its body as a web stream. */
export interface StreamResponse {
  headers: http.IncomingHttpHeaders
  body: ReadableStream<Uint8Array>
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

/** The device id of a /v1/devices/{id}… path. */
function pathDevice(path: string): string | undefined {
  const m = /^\/v1\/devices\/([^/?]+)/.exec(path)
  if (!m) return undefined
  try {
    return decodeURIComponent(m[1] as string)
  } catch {
    return undefined
  }
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
  /** Milliseconds before each retry of a request, or of a blob upload; [] over a unix socket. */
  readonly retryDelays: number[]
  readonly describe: string
  /** True for a TCP connection (request ids and retries). */
  readonly tcp: boolean
  /** True when connected to a hub: no host directories are sent. */
  readonly hub: boolean
  /**
   * Device id → the lease sent with every request for that device
   * (X-Maestro-Lease).
   */
  readonly leases = new Map<string, string>()
  /** Told when the daemon answers LEASE_EXPIRED to a request that carried a lease; the lease is forgotten first. */
  onLeaseExpired?: (device: string, lease: string) => void
  /**
   * Names another lease this client holds on a device besides `failed`
   * (undefined for none): a request refused LEASE_NOT_STARTED or
   * LEASE_EXPIRED under `failed` is sent once more with it.
   */
  otherLease?: (device: string, failed: string) => string | undefined

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

  /** Add the lease of the device a /v1/devices/{id}… path is for, if one is held. */
  private leaseHeader(path: string, headers: http.OutgoingHttpHeaders): void {
    const id = pathDevice(path)
    const lease = id === undefined ? undefined : this.leases.get(id)
    if (lease) headers[LEASE_HEADER] = lease
  }

  /** Forget a lease the daemon reported expired (409 LEASE_EXPIRED for a request that carried it). */
  private noteLeaseExpired(path: string, lease: string | undefined, res: RawResponse): void {
    const id = pathDevice(path)
    if (id === undefined || !lease || res.status !== 409) return
    // A HEAD's answer has no body: its code is in the X-Maestro-Error-Code header.
    if (MaestroError.fromResponse(res.status, res.body.toString('utf8'), undefined, codeFromHeaders(res.headers)).code !== 'LEASE_EXPIRED') return
    if (this.leases.get(id) === lease) this.leases.delete(id)
    this.onLeaseExpired?.(id, lease)
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
    return opts.requestId !== undefined || method === 'GET' || method === 'HEAD' || (method === 'POST' && /\/attach(\?|$)/.test(path))
  }

  /** Perform a request and return the raw response, retrying a dropped link. */
  async raw(path: string, opts: RequestOptions = {}): Promise<RawResponse> {
    const delays = this.retryable(path, opts) ? this.retryDelays : []
    const id = pathDevice(path)
    const cached = id === undefined ? undefined : this.leases.get(id)
    let lease = opts.lease === undefined ? cached : opts.lease ?? undefined
    let swapped = false
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
      // Refused under a lease that has not started or has ended, while the
      // client holds another on the device (a lease and the reservation
      // after it, told apart by a clock up to a minute off the hub's): sent
      // once more with that one. The hub refused before running anything.
      if (opts.lease === undefined && !swapped && lease && id !== undefined && res.status === 409 && this.otherLease) {
        const code = MaestroError.fromResponse(res.status, res.body.toString('utf8'), undefined, codeFromHeaders(res.headers)).code
        const alt = code === 'LEASE_NOT_STARTED' || code === 'LEASE_EXPIRED' ? this.otherLease(id, lease) : undefined
        if (alt && alt !== lease) {
          this.noteLeaseExpired(path, lease, res)
          this.leases.set(id, alt)
          lease = alt
          swapped = true
          if (opts.requestId !== undefined) opts = { ...opts, requestId: randomUUID() }
          continue
        }
      }
      this.noteLeaseExpired(path, lease, res)
      return res
    }
  }

  private once(path: string, opts: RequestOptions): Promise<RawResponse> {
    const method = opts.method ?? 'GET'
    const headers: http.OutgoingHttpHeaders = {
      'X-Maestro-Api-Version': API_VERSION,
      [CLIENT_MACHINE_HEADER]: machineName(),
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
    if (opts.lease === undefined) this.leaseHeader(path, headers)
    else if (opts.lease) headers[LEASE_HEADER] = opts.lease
    // On a hub this client names its output files and fetches them back.
    if (this.hub && method === 'POST') headers[CLIENT_OUTPUTS_HEADER] = '1'

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
   * PUT bytes [offset, size) of a local file, streamed, with a
   * Content-Range when the file is not empty. Not retried here: the caller
   * asks the daemon how much it holds and resumes from there.
   */
  putFile(path: string, file: string, offset: number, size: number, signal?: AbortSignal): Promise<RawResponse> {
    const headers: http.OutgoingHttpHeaders = {
      'X-Maestro-Api-Version': API_VERSION,
      [CLIENT_MACHINE_HEADER]: machineName(),
      Accept: 'application/json',
      'Content-Type': 'application/octet-stream',
      'Content-Length': size - offset,
    }
    if (size > 0) headers['Content-Range'] = `bytes ${offset}-${size - 1}/${size}`
    if (this.token) headers.Authorization = `Bearer ${this.token}`
    this.leaseHeader(path, headers)
    const reqOpts = { ...this.baseOptions('PUT', headers, signal), path }
    return new Promise<RawResponse>((resolve, reject) => {
      const req = this.request(reqOpts, (res) => {
        const chunks: Buffer[] = []
        res.on('data', (c: Buffer) => chunks.push(c))
        res.on('end', () => resolve({ status: res.statusCode ?? 0, headers: res.headers, body: Buffer.concat(chunks) }))
        res.on('error', reject)
      })
      req.on('error', (err) => reject(this.transportError(err, signal)))
      if (size - offset === 0) {
        req.end()
        return
      }
      const stream = fs.createReadStream(file, { start: offset, end: size - 1 })
      stream.on('error', (err) => req.destroy(err))
      stream.pipe(req)
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
      throw MaestroError.fromResponse(res.status, text, result as never, codeFromHeaders(res.headers))
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
      throw MaestroError.fromResponse(res.status, res.body.toString('utf8'), undefined, codeFromHeaders(res.headers))
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
      [CLIENT_MACHINE_HEADER]: machineName(),
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
      throw MaestroError.fromResponse(res.statusCode ?? 0, Buffer.concat(chunks).toString('utf8'), undefined, codeFromHeaders(res.headers))
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

  /**
   * Open a long-lived fragmented-MP4 stream (a device's screen) on a socket
   * of its own, with the device's lease. A non-200 answer rejects with the
   * daemon's error. The body carries whole top-level boxes only (a `moof`
   * together with its `mdat`), so it ends on a fragment boundary both when
   * the daemon ends it and when `signal` fires; it is never retried.
   * Cancelling the body (or abandoning it in a `for await` or a failed
   * `pipeline`) closes the connection.
   */
  async stream(path: string, opts: { signal?: AbortSignal; accept?: string } = {}): Promise<StreamResponse> {
    const { signal } = opts
    const headers: http.OutgoingHttpHeaders = {
      'X-Maestro-Api-Version': API_VERSION,
      [CLIENT_MACHINE_HEADER]: machineName(),
      Accept: opts.accept ?? 'video/mp4',
    }
    if (this.token) headers.Authorization = `Bearer ${this.token}`
    this.leaseHeader(path, headers)
    const id = pathDevice(path)
    const lease = id === undefined ? undefined : this.leases.get(id)
    // The signal is not handed to the request: aborting it would fail the
    // body. It ends the body here instead.
    const reqOpts: https.RequestOptions = { ...this.baseOptions('GET', headers), path, agent: false }
    if (this.ca) reqOpts.ca = this.ca
    if (signal?.aborted) throw MaestroError.of('INTERRUPTED', 'request aborted')

    let req: http.ClientRequest | undefined
    const res = await new Promise<http.IncomingMessage>((resolve, reject) => {
      const onAbort = (): void => {
        req?.destroy()
        reject(MaestroError.of('INTERRUPTED', 'request aborted'))
      }
      signal?.addEventListener('abort', onAbort, { once: true })
      req = this.request(reqOpts, (r) => {
        signal?.removeEventListener('abort', onAbort)
        resolve(r)
      })
      req.on('error', (err) => {
        signal?.removeEventListener('abort', onAbort)
        reject(this.transportError(err, signal))
      })
      req.end()
    })
    if (res.statusCode !== 200) {
      const chunks: Buffer[] = []
      for await (const c of res) chunks.push(c as Buffer)
      const raw: RawResponse = { status: res.statusCode ?? 0, headers: res.headers, body: Buffer.concat(chunks) }
      this.noteLeaseExpired(path, lease, raw)
      throw MaestroError.fromResponse(raw.status, raw.body.toString('utf8'), undefined, codeFromHeaders(res.headers))
    }
    const out = new Mp4Boxes()
    res.pipe(out)
    // The consumer can end the body itself: body.cancel(), a break out of
    // a for await over it, or a pipeline that fails on its write side all
    // destroy out, after which pipe only unpipes res. The response (and
    // its socket, agent: false) goes with it, or the daemon would keep
    // writing into a socket nobody reads.
    out.on('close', () => {
      if (!res.destroyed) res.destroy()
    })
    const finish = (): void => {
      res.unpipe(out)
      out.end()
      res.destroy()
    }
    if (signal?.aborted) finish()
    else signal?.addEventListener('abort', finish, { once: true })
    res.on('error', (err) => {
      if (signal?.aborted) return
      out.destroy(this.transportError(err, signal))
    })
    res.on('close', () => {
      signal?.removeEventListener('abort', finish)
      // A connection that dropped before the body's end is an error.
      if (!res.complete && !signal?.aborted && !out.destroyed) out.destroy(MaestroError.of('DAEMON_UNAVAILABLE', `daemon ${this.describe}: the stream was cut off`))
    })
    return { headers: res.headers, body: Readable.toWeb(out) as ReadableStream<Uint8Array> }
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

/**
 * Splits an MP4 byte stream into whole top-level boxes, as the Go client's
 * boxWriter does: a box passes once it is complete, and a `moof` only
 * together with the box after it (its `mdat`). An incomplete box when the
 * stream ends is dropped, so the output always ends on a box boundary.
 */
export class Mp4Boxes extends Transform {
  private chunks: Buffer[] = []
  private size = 0
  private held: Buffer | undefined

  override _transform(chunk: Buffer, _enc: BufferEncoding, cb: TransformCallback): void {
    this.chunks.push(chunk)
    this.size += chunk.length
    for (;;) {
      const n = boxSize(this.peek(16))
      if (n === undefined || this.size < n) break
      const box = this.shift(n)
      if (box.toString('latin1', 4, 8) === 'moof') {
        this.held = box
        continue
      }
      if (this.held !== undefined) {
        this.push(Buffer.concat([this.held, box]))
        this.held = undefined
      } else {
        this.push(box)
      }
    }
    cb()
  }

  override _flush(cb: TransformCallback): void {
    this.chunks = []
    this.size = 0
    this.held = undefined
    cb()
  }

  /** The first `n` buffered bytes, or fewer when fewer are buffered. */
  private peek(n: number): Buffer {
    const first = this.chunks[0]
    if (first === undefined || first.length >= n || this.chunks.length === 1) return (first ?? Buffer.alloc(0)).subarray(0, n)
    const all = Buffer.concat(this.chunks)
    this.chunks = [all]
    return all.subarray(0, n)
  }

  /** Removes and returns the first `n` buffered bytes (n ≤ size). */
  private shift(n: number): Buffer {
    const first = this.chunks[0]
    const all = this.chunks.length === 1 && first !== undefined ? first : Buffer.concat(this.chunks)
    const rest = all.subarray(n)
    this.chunks = rest.length > 0 ? [rest] : []
    this.size = rest.length
    return all.subarray(0, n)
  }
}

/**
 * A box header's total size, or undefined while the header is incomplete.
 * A size below the header's own length (0, "to the end of the file",
 * included) never completes.
 */
export function boxSize(b: Buffer): number | undefined {
  if (b.length < 8) return undefined
  let size = b.readUInt32BE(0)
  let hdr = 8
  if (size === 1) {
    if (b.length < 16) return undefined
    size = Number(b.readBigUInt64BE(8))
    hdr = 16
  }
  if (size < hdr || size > 2 ** 40) return undefined
  return size
}
