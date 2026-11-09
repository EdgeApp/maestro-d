// MaestroD: a handle on one daemon process. `connect()` finds the named
// daemon (spawning it when needed, like the Edge CLI's engine), and the
// handle boots devices and attaches to them.

import * as fs from 'node:fs'
import * as os from 'node:os'
import * as path from 'node:path'

import { randomUUID } from 'node:crypto'

import { MaestroError } from './errors.js'
import { MaestroDevice, anySignal } from './device.js'
import { dropLease, findLease, recordLease, type LeaseRecord } from './leasefile.js'
import { uploadPath } from './pack.js'
import { ENV_DEVICE, resolveName } from './runfiles.js'
import { ensureDaemon, waitForExit, type SpawnOptions } from './spawn.js'
import { Transport } from './transport.js'
import type {
  AttachOptions,
  BootOptions,
  CatalogResult,
  CreateDeviceOptions,
  DaemonEvent,
  DaemonInfo,
  DeviceInfo,
  Envelope,
  Lease,
  LeaseSelector,
  StatusResult,
} from './types.js'

/** The variable naming a hub to connect to (`host[:port]`, `https://host[:port]`). */
export const ENV_HUB = 'MAESTRO_D_HUB'
/** The bearer token sent to a hub. */
export const ENV_TOKEN = 'MAESTRO_D_TOKEN'
/** A PEM file of trust roots for a hub with a self-signed certificate. */
export const ENV_CA = 'MAESTRO_D_CA'
/** The port a hub address without one uses. */
export const DEFAULT_HUB_PORT = 7433

export interface ConnectOptions extends SpawnOptions {
  /**
   * Local daemon name (default `$MAESTRO_D`, else 'default'). One daemon per
   * name. Ignored once a hub is targeted.
   */
  name?: string
  /**
   * Connect to a hub, or any daemon's TCP listener, at `host[:port]`
   * (default port 7433) or `https://host[:port]`; `true` means localhost.
   * Nothing is spawned, and calls send no host directories (`cwd`): the
   * hub is another machine. Default `$MAESTRO_D_HUB`.
   */
  hub?: string | true
  /**
   * Connect to a daemon's TCP listener (`http://host:7788`) instead of the
   * local unix socket. Nothing is spawned in this mode. Unlike `hub`, calls
   * still send the working directory (a loopback daemon shares the disk).
   */
  url?: string
  /**
   * PEM trust roots (the text, or a file path) for an https:// hub with a
   * self-signed certificate. Default `$MAESTRO_D_CA`.
   */
  ca?: string | Buffer
  /** Aborts every request made through this handle. */
  signal?: AbortSignal
}

export interface DeviceAttachOptions extends AttachOptions {
  /** Device id / UDID (default `$MAESTRO_DEVICE`, else the only attached device). */
  device?: string
  /** Aborts every request made through the returned device handle. */
  signal?: AbortSignal
}

export interface LeaseOptions {
  /** The device to lease, by id (normalized through the hub; an id the hub does not list rejects with DEVICE_NOT_FOUND). */
  device?: string
  /** Or the first free device that matches. */
  select?: LeaseSelector
  /**
   * The lease's window in seconds (default the hub's: 8 hours, or the
   * device's maximum when that is shorter). It is fixed: using the device
   * never extends it, and nothing renews it on its own; call
   * `device.renew()`. Over the device's maximum (`maxSec`) is USAGE.
   */
  ttlSec?: number
  /** Who holds the lease, as listings show it (default user@host). */
  holder?: string
  /**
   * How to attach the leased device (default `{}`: attach it with the hub's
   * defaults). `false` only reserves it.
   */
  attach?: AttachOptions | false
  signal?: AbortSignal
}

export interface ShutdownOptions {
  /** Milliseconds to wait for the process to exit (default 15000; 0 = don't wait). */
  wait?: number
  signal?: AbortSignal
}

export class MaestroD {
  /** Daemon name. */
  readonly name: string
  /** daemon.json as of connect (undefined for `url` connections until `status()`). */
  info?: DaemonInfo
  /** True when `connect()` spawned the process. */
  readonly started: boolean
  readonly signal?: AbortSignal
  private readonly transport: Transport
  /**
   * The daemon's key in `<home>/leases.json`: its base URL, or
   * `unix:<name>` over the unix socket (as the CLI keys it).
   */
  readonly leaseKey: string
  /** The handles holding the leases this process took, by device id; `close()` releases them. */
  private readonly leased = new Map<string, MaestroDevice>()

  private constructor(
    name: string,
    transport: Transport,
    started: boolean,
    leaseKey: string,
    info?: DaemonInfo,
    signal?: AbortSignal,
  ) {
    this.name = name
    this.transport = transport
    this.started = started
    this.leaseKey = leaseKey
    if (info) this.info = info
    if (signal) this.signal = signal
    // A lease the hub reports expired leaves leases.json too.
    transport.onLeaseExpired = (_device, lease) => {
      try {
        dropLease(leaseKey, lease)
      } catch {
        // leases.json is a convenience; the hub is the authority
      }
    }
  }

  /**
   * Connect to the named daemon, starting `maestro-d serve` in the
   * background when it is not running (pass `spawn: false` to refuse).
   */
  static async connect(opts: ConnectOptions = {}): Promise<MaestroD> {
    const name = resolveName(opts.name)
    const hub = opts.hub ?? (opts.url ? undefined : process.env[ENV_HUB] || undefined)
    if (hub !== undefined || opts.url) {
      const url = hub !== undefined ? hubURL(hub) : (opts.url as string)
      const token = opts.token ?? (process.env[ENV_TOKEN] || undefined)
      const ca = readCA(opts.ca ?? (process.env[ENV_CA] || undefined))
      const t = new Transport({ url, token, ca, hub: hub !== undefined })
      const d = new MaestroD(hub !== undefined ? t.describe : name, t, false, baseURL(url), undefined, opts.signal)
      const st = await d.status()
      d.info = st.daemon
      return d
    }
    const { info, transport, started } = await ensureDaemon(name, { ...opts, signal: opts.signal })
    return new MaestroD(name, transport, started, `unix:${name}`, info, opts.signal)
  }

  /** `connect()` and `attach()` in one call. */
  static async attach(opts: ConnectOptions & DeviceAttachOptions): Promise<MaestroDevice> {
    const d = await MaestroD.connect(opts)
    return d.attach(opts)
  }

  private sig(signal?: AbortSignal): AbortSignal | undefined {
    return anySignal(signal, this.signal)
  }

  // -------------------------------------------------------------------------
  // Daemon

  /** Daemon info and every device it knows. */
  async status(opts: { signal?: AbortSignal } = {}): Promise<StatusResult> {
    return this.transport.json<StatusResult>('/v1/status', { signal: this.sig(opts.signal) })
  }

  /** Resolves when the daemon answers; rejects with DAEMON_UNAVAILABLE. */
  async ping(timeoutMs = 3000): Promise<void> {
    await this.transport.json<StatusResult>('/v1/status', { timeout: timeoutMs })
  }

  /**
   * Stop the daemon: detaches every device, shuts down the simulators and
   * emulators it booted, and exits. A hub is stopped only from its own
   * machine: over TCP this rejects with FORBIDDEN.
   */
  async shutdown(opts: ShutdownOptions = {}): Promise<void> {
    await Promise.allSettled([...this.leased.values()].map((dev) => dev.release()))
    await this.transport.json<Envelope>('/v1/shutdown', { method: 'POST', signal: this.sig(opts.signal) })
    this.transport.close()
    const wait = opts.wait ?? 15_000
    if (wait > 0 && this.info) {
      if (!(await waitForExit(this.info, wait))) {
        throw MaestroError.of('DAEMON_UNAVAILABLE', `daemon "${this.name}" (pid ${this.info.pid}) did not exit within ${wait}ms`)
      }
    }
  }

  /**
   * Release every lease this handle holds (on a hub), then close idle
   * connections; the daemon keeps running.
   */
  async close(): Promise<void> {
    const held = [...this.leased.values()]
    try {
      await Promise.allSettled(held.map((dev) => dev.release()))
    } finally {
      this.transport.close()
    }
  }

  /** True when the daemon is a hub: it leases its devices (`maestro-d hub start`). */
  get leasing(): boolean {
    return Boolean(this.info?.hub)
  }

  /**
   * Lease a device on a hub, by id or by selector, and return its handle:
   * attached (unless `attach: false`) and sending the lease with every
   * request. The lease is a fixed window (`ttlSec`); nothing renews it on
   * its own (`device.renew()` does). It is recorded in
   * `<home>/leases.json`, like `maestro-d lease`, and ends with
   * `device.release()` or `close()`.
   */
  async lease(opts: LeaseOptions): Promise<MaestroDevice> {
    if (!opts || (opts.device === undefined) === (opts.select === undefined)) {
      throw new TypeError('lease() needs a device or a select, not both')
    }
    if (!this.info?.hub) await this.status({ signal: opts.signal }).then((st) => (this.info = st.daemon))
    if (!this.info?.hub) {
      throw MaestroError.of('USAGE', `daemon ${this.name} is not a hub and has no leases (start one with \`maestro-d hub start\`)`)
    }
    const signal = this.sig(opts.signal)
    const body: Record<string, unknown> = { ttlSec: opts.ttlSec, holder: opts.holder ?? defaultHolder() }
    if (opts.device !== undefined) body.device = await this.normalize(opts.device, signal)
    else body.select = opts.select
    const cfg = opts.attach === false ? undefined : { ...(opts.attach ?? {}) }
    // Files to upload need the lease first (a hub's blob routes do), so
    // such an attach follows the lease; any other goes in the same call.
    const later = cfg !== undefined && this.transport.hub && hasLocalFiles(cfg)
    if (cfg !== undefined && !later) body.attach = cfg
    const res = await this.transport.json<Envelope & { lease: Lease; device?: DeviceInfo }>('/v1/leases', {
      method: 'POST',
      body,
      signal,
      requestId: this.transport.tcp ? randomUUID() : undefined,
    })
    const lease = res.lease
    this.transport.leases.set(lease.device, lease.id)
    let info: DeviceInfo = res.device ?? { id: lease.device, platform: '', ready: true, attached: false }
    try {
      if (later) info = await this.openAttach(lease.device, cfg as AttachOptions, signal)
    } catch (err) {
      await this.transport.json<Envelope>(`/v1/leases/${encodeURIComponent(lease.id)}`, { method: 'DELETE' }).catch(() => undefined)
      this.transport.leases.delete(lease.device)
      throw err
    }
    this.recordLease(lease)
    const dev = new MaestroDevice(this.transport, this, info, signal)
    dev.holdLease(lease, {
      onRenew: (l) => this.recordLease(l),
      onRelease: (d, l) => {
        if (this.leased.get(d.id) === d) this.leased.delete(d.id)
        this.forgetLease(l)
      },
    })
    this.leased.set(lease.device, dev)
    return dev
  }

  /** Record a lease in leases.json (best effort: the hub is the authority). */
  private recordLease(lease: Lease): void {
    try {
      recordLease(this.leaseKey, lease)
    } catch {
      // an unwritable home only loses the shell's view of the lease
    }
  }

  private forgetLease(lease: Lease): void {
    try {
      dropLease(this.leaseKey, lease.id)
    } catch {
      // see recordLease
    }
  }

  /**
   * The lease this client holds on a hub's device: one taken in this
   * process, else one in leases.json (from `maestro-d lease` in the same
   * home), which is then sent with the device's requests. Undefined when
   * it holds none.
   */
  private heldLease(id: string): LeaseRecord | undefined {
    const inProcess = this.transport.leases.get(id)
    if (inProcess) return undefined
    let rec: LeaseRecord | undefined
    try {
      rec = findLease(this.leaseKey, id)
    } catch {
      return undefined
    }
    if (rec) this.transport.leases.set(id, rec.id)
    return rec
  }

  /**
   * The id a hub returns for a device (GET /v1/devices/{id}); one it does
   * not list (DEVICE_NOT_FOUND) is kept as given.
   */
  private async normalize(id: string, signal?: AbortSignal): Promise<string> {
    try {
      return (await this.device(id, { signal })).id || id
    } catch (err) {
      if (err instanceof MaestroError && err.code === 'DEVICE_NOT_FOUND') return id
      throw err
    }
  }

  /** Events from every device. Ends when `signal` fires. */
  events(opts: { device?: string; after?: number; signal?: AbortSignal } = {}): AsyncIterable<DaemonEvent> {
    const q = new URLSearchParams()
    if (opts.device) q.set('device', opts.device)
    if (opts.after) q.set('after', String(opts.after))
    const qs = q.toString()
    return this.transport.sse<DaemonEvent>('/v1/events' + (qs ? `?${qs}` : ''), this.sig(opts.signal))
  }

  // -------------------------------------------------------------------------
  // Devices

  /** Connected devices, booted simulators/emulators and attached sessions. */
  async devices(opts: { platform?: string; signal?: AbortSignal } = {}): Promise<DeviceInfo[]> {
    const path = '/v1/devices' + (opts.platform ? `?platform=${encodeURIComponent(opts.platform)}` : '')
    const res = await this.transport.json<Envelope & { devices: DeviceInfo[] }>(path, { signal: this.sig(opts.signal) })
    return res.devices
  }

  /** One device by id. */
  async device(id: string, opts: { signal?: AbortSignal } = {}): Promise<DeviceInfo> {
    const res = await this.transport.json<Envelope & { device: DeviceInfo }>(`/v1/devices/${encodeURIComponent(id)}`, {
      signal: this.sig(opts.signal),
    })
    return res.device
  }

  /**
   * What `createDevice()` can make: iOS runtimes and device types, Android
   * system images and hardware profiles. `available` adds the Android
   * images sdkmanager can download.
   */
  async catalog(opts: { platform?: string; available?: boolean; signal?: AbortSignal } = {}): Promise<CatalogResult> {
    const q = new URLSearchParams()
    if (opts.platform) q.set('platform', opts.platform)
    if (opts.available) q.set('available', 'true')
    const qs = q.toString()
    return this.transport.json<CatalogResult>('/v1/catalog' + (qs ? `?${qs}` : ''), { signal: this.sig(opts.signal) })
  }

  /**
   * Create a simulator or AVD and, unless `boot: false`, boot it. It is
   * recorded as created by maestro-d, so `deleteDevice()` accepts it.
   * Resolves with the booted device, or the shut-down one (an AVD as
   * `avd:<name>`).
   */
  async createDevice(opts: CreateDeviceOptions & { signal?: AbortSignal }): Promise<DeviceInfo> {
    if (!opts.platform || !opts.name) throw new TypeError('createDevice() needs platform and name')
    const { signal, platform, name, boot, bootTimeout, ...create } = opts
    const body: Record<string, unknown> = { platform, name, create: { name, ...create } }
    if (boot !== undefined) body.boot = boot
    if (bootTimeout !== undefined) body.bootTimeout = bootTimeout
    const res = await this.transport.json<Envelope & { device: DeviceInfo }>('/v1/devices', {
      method: 'POST',
      body,
      signal: this.sig(signal),
    })
    return res.device
  }

  /**
   * Delete a simulator or AVD by id, name or `avd:<name>`, detaching and
   * stopping it first. Devices maestro-d did not create are refused with
   * DEVICE_EXTERNAL unless `force`.
   */
  async deleteDevice(id: string, opts: { force?: boolean; signal?: AbortSignal } = {}): Promise<DeviceInfo> {
    if (!id) throw new TypeError('deleteDevice() needs an id')
    const res = await this.transport.json<Envelope & { device: DeviceInfo }>(
      `/v1/devices/${encodeURIComponent(id)}?delete=true` + (opts.force ? '&force=true' : ''),
      { method: 'DELETE', signal: this.sig(opts.signal) },
    )
    return res.device
  }

  /**
   * Boot a simulator or emulator by name (or return it if already booted).
   * Anything booted here is shut down when the daemon exits. `platform` may
   * be left out for `avd:<name>` and for a name or UDID the daemon lists.
   */
  async startDevice(opts: BootOptions & { signal?: AbortSignal }): Promise<DeviceInfo> {
    if (!opts.name) throw new TypeError('startDevice() needs a name')
    const { signal, ...body } = opts
    const res = await this.transport.json<Envelope & { device: DeviceInfo }>('/v1/devices', {
      method: 'POST',
      body,
      signal: this.sig(signal),
    })
    return res.device
  }

  /**
   * Detach (if attached) and shut a device down. Devices this daemon did
   * not boot are refused with DEVICE_EXTERNAL unless `force`.
   */
  async stopDevice(id: string, opts: { force?: boolean; signal?: AbortSignal } = {}): Promise<void> {
    await this.transport.json<Envelope>(`/v1/devices/${encodeURIComponent(id)}`, {
      method: 'DELETE',
      body: { force: opts.force === true },
      signal: this.sig(opts.signal),
    })
  }

  /**
   * Open a driver on a device and return its handle. Attaching to an
   * already-attached device returns a handle without reconnecting.
   */
  async attach(opts: DeviceAttachOptions = {}): Promise<MaestroDevice> {
    const { device, signal, ...cfg } = opts
    let id = await this.resolveDevice(device, signal)
    if (this.info?.hub) {
      // A hub lends each device to one client, under a lease the client
      // took explicitly: md.lease() in this process, or `maestro-d lease`
      // in the same home (leases.json). This never takes one: without it
      // the attach goes without a lease, and the hub answers
      // LEASE_REQUIRED, naming md.lease() and `maestro-d lease`.
      id = await this.normalize(id, this.sig(signal))
    }
    const held = this.info?.hub ? this.heldLease(id) : undefined
    const info = await this.openAttach(id, cfg, this.sig(signal))
    const dev = new MaestroDevice(this.transport, this, info, anySignal(signal, this.signal))
    const own = this.leased.get(id)
    if (own?.lease) {
      dev.holdLease(own.lease, { onRenew: (l) => this.recordLease(l), onRelease: (_d, l) => this.forgetLease(l) })
    } else if (held) {
      // A lease from leases.json: this handle may renew or release it, but
      // close() leaves it to the shell that took it.
      const lease: Lease = { id: held.id, device: id, expiresAt: held.expiresAt ?? '', ttlSec: held.ttlSec ?? 0 }
      if (held.holder) lease.holder = held.holder
      dev.holdLease(lease, { onRenew: (l) => this.recordLease(l), onRelease: (_d, l) => this.forgetLease(l) })
    }
    return dev
  }

  /** POST …/attach, uploading the app file first on a hub. */
  private async openAttach(id: string, cfg: AttachOptions, signal?: AbortSignal): Promise<DeviceInfo> {
    const base = `/v1/devices/${encodeURIComponent(id)}`
    if (this.transport.hub && cfg.appFile && !cfg.appFile.startsWith('blob:')) {
      // The app file is on this machine: upload it and send a blob ref (a
      // hub refuses its own host paths, and any Appium extra.capsFile).
      cfg.appFile = (await uploadPath(this.transport, base, path.resolve(cfg.appFile), this.sig(signal))).ref
    }
    const res = await this.transport.json<Envelope & { device: DeviceInfo }>(`${base}/attach`, {
      method: 'POST',
      body: cfg,
      signal: this.sig(signal),
    })
    return res.device
  }

  /** True when this handle talks to a hub (`hub` / MAESTRO_D_HUB). */
  get hub(): boolean {
    return this.transport.hub
  }

  /** Close the driver on a device by id. */
  async detach(id: string, opts: { signal?: AbortSignal } = {}): Promise<void> {
    await this.transport.json<Envelope>(`/v1/devices/${encodeURIComponent(id)}/detach`, {
      method: 'POST',
      signal: this.sig(opts.signal),
    })
  }

  /** Explicit id → $MAESTRO_DEVICE → the only attached device → USAGE. */
  private async resolveDevice(id: string | undefined, signal?: AbortSignal): Promise<string> {
    const chosen = id || process.env[ENV_DEVICE]
    if (chosen) return chosen
    const attached = (await this.devices({ signal })).filter((d) => d.attached)
    if (attached.length === 1 && attached[0]) return attached[0].id
    const list = attached.map((d) => `${d.id} (${d.platform})`).join(', ')
    throw MaestroError.of(
      'USAGE',
      attached.length === 0
        ? `no device given: pass {device: '<id>'} or set ${ENV_DEVICE}`
        : `several devices are attached; pass {device: '<id>'}: ${list}`,
      { details: { attached: attached.map((d) => d.id) } },
    )
  }
}

/**
 * The URL of a hub address: `host`, `host:port`, `[::1]:port`,
 * `http(s)://host[:port]`, or `true` for localhost; the port defaults to
 * 7433.
 */
export function hubURL(hub: string | true): string {
  if (hub === true) return `http://localhost:${DEFAULT_HUB_PORT}`
  let v = String(hub).trim()
  let scheme = 'http'
  const m = /^([a-z][a-z0-9+.-]*):\/\//i.exec(v)
  if (m) {
    scheme = (m[1] as string).toLowerCase()
    v = v.slice(m[0].length)
  }
  if (scheme !== 'http' && scheme !== 'https') {
    throw new TypeError(`hub: unsupported scheme ${scheme}:// (use http:// or https://)`)
  }
  v = v.replace(/\/+$/, '')
  if (v === '' || /[/?#@\s]/.test(v)) throw new TypeError(`hub: expected host[:port], got ${JSON.stringify(hub)}`)
  const hasPort = /^\[.*\]:\d+$/.test(v) || (/^[^:]+:\d+$/.test(v) && !v.startsWith('['))
  if (!hasPort) {
    const host = v.includes(':') && !v.startsWith('[') ? `[${v}]` : v
    v = `${host}:${DEFAULT_HUB_PORT}`
  }
  return `${scheme}://${v}`
}

/** PEM text from `ca`: the text itself, a Buffer, or a file path. */
function readCA(ca: string | Buffer | undefined): string | Buffer | undefined {
  if (ca === undefined || Buffer.isBuffer(ca)) return ca
  if (ca.includes('-----BEGIN')) return ca
  try {
    return fs.readFileSync(ca)
  } catch (err) {
    throw MaestroError.of('USAGE', `ca: ${(err as Error).message}`, { cause: err })
  }
}

/** A base URL as the CLI keys leases.json by it: no trailing slash, http:// when bare. */
function baseURL(url: string): string {
  const u = url.replace(/\/+$/, '')
  return u.includes('://') ? u : `http://${u}`
}

/** True when an attach would upload a file of this machine (a hub refuses host paths). */
function hasLocalFiles(cfg: AttachOptions): boolean {
  return typeof cfg.appFile === 'string' && cfg.appFile !== '' && !cfg.appFile.startsWith('blob:')
}

/** Names this client in a lease: user@host. */
function defaultHolder(): string {
  let user = process.env.USER ?? ''
  try {
    user = os.userInfo().username || user
  } catch {
    // no passwd entry (a container): keep $USER
  }
  const host = os.hostname().split('.')[0] ?? ''
  if (user && host) return `${user}@${host}`
  return host || 'maestro-d'
}
