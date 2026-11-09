// MaestroD: a handle on one daemon process. `connect()` finds the named
// daemon (spawning it when needed, like the Edge CLI's engine), and the
// handle boots devices and attaches to them.

import * as fs from 'node:fs'

import { MaestroError } from './errors.js'
import { MaestroDevice, anySignal } from './device.js'
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

  private constructor(name: string, transport: Transport, started: boolean, info?: DaemonInfo, signal?: AbortSignal) {
    this.name = name
    this.transport = transport
    this.started = started
    if (info) this.info = info
    if (signal) this.signal = signal
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
      const d = new MaestroD(hub !== undefined ? t.describe : name, t, false, undefined, opts.signal)
      const st = await d.status()
      d.info = st.daemon
      return d
    }
    const { info, transport, started } = await ensureDaemon(name, { ...opts, signal: opts.signal })
    return new MaestroD(name, transport, started, info, opts.signal)
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
   * emulators it booted, and exits.
   */
  async shutdown(opts: ShutdownOptions = {}): Promise<void> {
    await this.transport.json<Envelope>('/v1/shutdown', { method: 'POST', signal: this.sig(opts.signal) })
    this.transport.close()
    const wait = opts.wait ?? 15_000
    if (wait > 0 && this.info) {
      if (!(await waitForExit(this.info, wait))) {
        throw MaestroError.of('DAEMON_UNAVAILABLE', `daemon "${this.name}" (pid ${this.info.pid}) did not exit within ${wait}ms`)
      }
    }
  }

  /** Close idle connections; the daemon keeps running. */
  close(): void {
    this.transport.close()
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
    const id = await this.resolveDevice(device, signal)
    const res = await this.transport.json<Envelope & { device: DeviceInfo }>(`/v1/devices/${encodeURIComponent(id)}/attach`, {
      method: 'POST',
      body: cfg,
      signal: this.sig(signal),
    })
    return new MaestroDevice(this.transport, this, res.device, anySignal(signal, this.signal))
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
