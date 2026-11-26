// MaestroD: a handle on one daemon process. `connect()` finds the named
// daemon (spawning it when needed, like the Edge CLI's engine), and the
// handle boots devices and attaches to them.

import * as fs from 'node:fs'
import * as os from 'node:os'
import * as path from 'node:path'

import { randomUUID } from 'node:crypto'

import { MaestroError } from './errors.js'
import { MaestroDevice, anySignal } from './device.js'
import { currentLease, dropLease, findLease, isEmulatorTarget, loadLeases, recordLease, type LeaseRecord } from './leasefile.js'
import { uploadPath } from './pack.js'
import { ENV_DEVICE, readHubInfo, pidAlive, resolveName } from './runfiles.js'
import { ensureDaemon, waitForExit, type SpawnOptions } from './spawn.js'
import { Transport } from './transport.js'
import type {
  AttachOptions,
  BootOptions,
  CatalogQuery,
  CatalogResult,
  CloudSpec,
  ProviderAccount,
  ProviderApps,
  ProviderSessions,
  ProviderUpload,
  CreateDeviceOptions,
  DaemonEvent,
  DaemonInfo,
  DeviceInfo,
  DeviceMinutes,
  DeviceQuery,
  Envelope,
  HubConfigDevice,
  HubDevicesResult,
  HubStatus,
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
  /** Connect through this home’s local hub socket. Never starts a process. */
  hubSocket?: boolean
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
  /**
   * Or the first free device that matches. With `location: 'cloud'` the hub
   * that holds the provider's credentials launches a cloud device for the
   * lease, and ends it when the lease ends.
   */
  select?: LeaseSelector
  /** A cloud lease's launch settings: idle release, Appium version, caps, app id. */
  cloud?: Pick<CloudSpec, 'idleReleaseSec' | 'appiumVersion' | 'caps' | 'appId' | 'bootTimeoutSec'>
  /**
   * The lease's window in seconds (default the hub's: 8 hours, or the
   * device's maximum when that is shorter). It is fixed: using the device
   * never extends it, and nothing renews it on its own; call
   * `device.renew()`. Over the device's maximum (`maxSec`) is USAGE.
   */
  ttlSec?: number
  /** Who holds the lease, as listings show it (default user@host). */
  holder?: string
  /** What the window is for, up to 200 characters in listings. */
  description?: string
  /**
   * How to attach the leased device (default `{}`: attach it with the hub's
   * defaults). `false` only reserves it.
   */
  attach?: AttachOptions | false
  signal?: AbortSignal
}

export interface ReserveOptions {
  /** The device to reserve, by id, as for `lease()`. */
  device?: string
  /** Or the first device that matches and is free for the whole window (ready now; never booted, an emulator or a cloud device). */
  select?: Omit<LeaseSelector, 'boot' | 'location' | 'provider'>
  /**
   * The window's start: a Date or an RFC 3339 string, at most the hub's
   * lead ahead (24 hours by default). This machine's clock goes with it,
   * and the hub refuses it more than 60 s off its own.
   */
  startAt: Date | string
  /** The window's length in seconds, as for `lease()`. */
  ttlSec?: number
  /** Who holds it, as listings show it (default user@host). */
  holder?: string
  /** What the window is for, up to 200 characters in listings. */
  description?: string
  signal?: AbortSignal
}

export interface ClaimOptions {
  /** How to attach the device (default `{}`); `false` only takes the lease. */
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
  /** The handles holding the leases this process took, by lease id; `close()` releases them. */
  private readonly leased = new Map<string, MaestroDevice>()
  /**
   * AWS Device Farm itself, without a device: the account, the uploaded
   * apps, an upload from the daemon's machine (a local caller only) and the
   * remote access sessions. Through a hub they reach the hub that holds the
   * credentials; `hub` names one.
   */
  readonly devicefarm: DeviceFarmUtilities

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
    this.devicefarm = new DeviceFarmUtilities(transport, () => this.signal)
    if (info) this.info = info
    if (signal) this.signal = signal
    // Which of a device's leases has started is judged by this machine's
    // clock, which may be up to a minute off the hub's: at one lease's end
    // and the next one's start, a refused request goes once more with the
    // other one on file.
    transport.otherLease = (device, failed) => {
      if (isEmulatorTarget(device)) return undefined
      try {
        return currentLease(
          loadLeases().filter((r) => r.hub === leaseKey && r.id !== failed),
          device,
        )?.id
      } catch {
        return undefined
      }
    }
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
    if (opts.hubSocket) {
      const info = readHubInfo()
      if (!info || !pidAlive(info.pid)) throw MaestroError.of('DAEMON_UNAVAILABLE', 'the local hub is not running')
      const transport = new Transport({ socketPath: info.socketPath, hub: true })
      const local = new MaestroD(info.name, transport, false, 'unix:hub/run', info, opts.signal)
      await local.hubStatus()
      return local
    }
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
    const { name, hub, hubSocket, url, ca, signal, spawn, idleTimeout, http, token, startTimeout, extraArgs, bin, log, ...attach } = opts
    const d = await MaestroD.connect({ name, hub, hubSocket, url, ca, signal, spawn, idleTimeout, http, token, startTimeout, extraArgs, bin, log })
    // With an explicit hub, url can name the page for a web driver.
    if ((hub !== undefined || hubSocket) && url !== undefined) return d.attach({ ...attach, url, signal })
    return d.attach({ ...attach, signal })
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
    const body: Record<string, unknown> = { ttlSec: opts.ttlSec, holder: opts.holder ?? defaultHolder(), description: opts.description }
    if (opts.device !== undefined) body.device = await this.normalize(opts.device, signal)
    else body.select = opts.select
    if (opts.cloud) body.cloud = opts.cloud
    const cfg = opts.attach === false ? undefined : savedAttach(opts.attach ?? {})
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
    return this.bindDevice(info, lease, cfg, signal, true, opts.device)
  }

  /**
   * Reserve a device on a hub for a window that starts later
   * (`POST /v1/reservations`), and resolve with the lease, in state
   * 'scheduled'. Nothing is attached and nothing waits for the start: keep
   * `lease.id`, and from the start take the device with `claim(lease.id)`,
   * in this process or any other. It is recorded in `<home>/leases.json`
   * like `maestro-d reserve`. Before the start, requests with it reject
   * LEASE_NOT_STARTED. Cancel it with `DELETE /v1/leases/{id}` or
   * `maestro-d release --lease`. A hub that predates reservations rejects
   * USAGE "does not take reservations".
   */
  async reserve(opts: ReserveOptions): Promise<Lease> {
    if (!opts || (opts.device === undefined) === (opts.select === undefined)) {
      throw new TypeError('reserve() needs a device or a select, not both')
    }
    if (opts.startAt === undefined) throw new TypeError('reserve() needs startAt')
    if (!this.info?.hub) await this.status({ signal: opts.signal }).then((st) => (this.info = st.daemon))
    if (!this.info?.hub) {
      throw MaestroError.of('USAGE', `daemon ${this.name} is not a hub and has no reservations (start one with \`maestro-d hub start\`)`)
    }
    const signal = this.sig(opts.signal)
    const startAt = opts.startAt instanceof Date ? rfc3339(opts.startAt) : opts.startAt
    const body: Record<string, unknown> = { startAt, ttlSec: opts.ttlSec, holder: opts.holder ?? defaultHolder(), description: opts.description }
    if (opts.device !== undefined) body.device = await this.normalize(opts.device, signal)
    else body.select = opts.select
    body.clientTime = rfc3339(new Date())
    let res: Envelope & { lease: Lease; device?: DeviceInfo }
    try {
      res = await this.transport.json<Envelope & { lease: Lease; device?: DeviceInfo }>('/v1/reservations', {
        method: 'POST',
        body,
        signal,
        requestId: this.transport.tcp ? randomUUID() : undefined,
      })
    } catch (err) {
      if (err instanceof MaestroError && err.code === 'USAGE' && err.message.startsWith('no route for POST /v1/reservations')) {
        throw MaestroError.of('USAGE', `hub ${this.name} does not take reservations; it needs a maestro-d release that has them`, {
          details: { hub: this.name, reason: 'reservations-unsupported' },
        })
      }
      throw err
    }
    this.recordLease(res.lease)
    return res.lease
  }

  /**
   * Take a lease by its id (a reservation from its start, or a lease
   * handed over) and return its handle: attached (unless `attach: false`),
   * sending the lease, recorded in `<home>/leases.json`, and released by
   * `device.release()` or `close()` as a `lease()` handle is. A reservation
   * that has not started rejects LEASE_NOT_STARTED (`details.startsAt`), and
   * nothing is held.
   */
  async claim(leaseId: string, opts: ClaimOptions = {}): Promise<MaestroDevice> {
    if (!leaseId) throw new TypeError('claim() needs a lease id')
    const signal = this.sig(opts.signal)
    const { lease } = await this.transport.json<Envelope & { lease: Lease }>(`/v1/leases/${encodeURIComponent(leaseId)}`, { signal })
    if (lease.state === 'scheduled') {
      throw MaestroError.of('LEASE_NOT_STARTED', `lease ${lease.id} on ${lease.device} starts at ${lease.startsAt}; claim it then`, {
        details: { lease: lease.id, startsAt: lease.startsAt },
      })
    }
    if (!lease.device) throw MaestroError.of('DEVICE_NOT_FOUND', 'the leased emulator is unavailable')
    // A failed attach puts back the lease the device had in this process
    // (an earlier lease() or claim() handle keeps sending it).
    const previous = this.transport.leases.get(lease.device)
    this.transport.leases.set(lease.device, lease.id)
    let info: DeviceInfo
    try {
      info =
        opts.attach === false
          ? await this.device(lease.device, { signal })
          : await this.openAttach(lease.device, { ...(opts.attach ?? {}) }, signal)
    } catch (err) {
      if (previous === undefined) this.transport.leases.delete(lease.device)
      else this.transport.leases.set(lease.device, previous)
      throw err
    }
    return this.bindDevice(info, lease, opts.attach === false ? undefined : savedAttach(opts.attach ?? {}), signal, true)
  }

  private bindDevice(info: DeviceInfo, lease: Lease, cfg: AttachOptions | undefined, signal: AbortSignal | undefined, owned: boolean, requestedDevice?: string): MaestroDevice {
    this.recordLease(lease, cfg, owned, requestedDevice)
    const dev = new MaestroDevice(this.transport, this, info, signal)
    dev.holdLease(lease, {
      onRenew: (l) => this.recordLease(l, undefined, false),
      onRebind: (l) => this.recordLease(l, undefined, false),
      reattach: cfg && ((id, sig) => this.openAttach(id, cfg, sig)),
      onRelease: (_d, l) => {
        this.leased.delete(l.id)
        this.forgetLease(l)
      },
    })
    if (owned) this.leased.set(lease.id, dev)
    return dev
  }

  /** Record a lease in leases.json (best effort: the hub is the authority). */
  private recordLease(lease: Lease, attach?: AttachOptions, replaceStarted = true, requestedDevice?: string): void {
    try {
      recordLease(this.leaseKey, lease, attach, replaceStarted, requestedDevice)
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

  /**
   * Connected devices, booted simulators/emulators, attached sessions and
   * cloud devices, each with its `location` relative to this computer:
   * `local`, `mesh` or `cloud`. The options filter the list.
   */
  async devices(opts: DeviceQuery & { signal?: AbortSignal } = {}): Promise<DeviceInfo[]> {
    const { signal, ...q } = opts
    const path = withQuery('/v1/devices', q)
    const list = async (collectionLease?: string) =>
      (await this.transport.json<Envelope & { devices: DeviceInfo[] }>(path, {
        signal: this.sig(signal), collectionLease,
      })).devices
    if (!this.leasing) return list()
    const leases = new Set(this.transport.leases.values())
    for (const record of loadLeases()) {
      if (record.hub === this.leaseKey && record.id) leases.add(record.id)
    }
    if (leases.size === 0) return list()
    const devices = new Map<string, DeviceInfo>()
    for (const lease of leases) {
      for (const d of await list(lease)) {
        if (!devices.has(d.id)) devices.set(d.id, d)
      }
    }
    return [...devices.values()]
  }

  /** One device by id. */
  async device(id: string, opts: { signal?: AbortSignal } = {}): Promise<DeviceInfo> {
    const res = await this.transport.json<Envelope & { device: DeviceInfo }>(`/v1/devices/${encodeURIComponent(id)}`, {
      signal: this.sig(opts.signal),
    })
    return res.device
  }

  /**
   * The hub's name and version, and each peer's address, link state, RTT
   * and device count (`GET /v1/hub`). Fails `USAGE` on a daemon that is not
   * a hub.
   */
  async hubStatus(opts: { signal?: AbortSignal } = {}): Promise<HubStatus> {
    const res = await this.transport.json<Envelope & { hub: HubStatus }>('/v1/hub', { signal: this.sig(opts.signal) })
    return res.hub
  }

  /** Read the device config over the local hub socket. */
  async hubDevices(opts: { signal?: AbortSignal } = {}): Promise<HubDevicesResult> {
    return this.transport.json<HubDevicesResult>('/v1/config/devices', { signal: this.sig(opts.signal) })
  }

  /** Share a device or update its description. Omit description to keep it, or pass an empty string to clear it. */
  async setHubDevice(id: string, opts: { description?: string; signal?: AbortSignal } = {}): Promise<HubConfigDevice> {
    const res = await this.transport.json<Envelope & { device: HubConfigDevice }>(
      `/v1/config/devices/${encodeURIComponent(id)}`,
      { method: 'PUT', body: { description: opts.description }, signal: this.sig(opts.signal) },
    )
    return res.device
  }

  /** Remove a configured device. endLeases also ends its lease and cancels its reservations. */
  async removeHubDevice(id: string, opts: { endLeases?: boolean; signal?: AbortSignal } = {}): Promise<Envelope> {
    return this.transport.json<Envelope>(
      withQuery(`/v1/config/devices/${encodeURIComponent(id)}`, { endLeases: opts.endLeases }),
      { method: 'DELETE', signal: this.sig(opts.signal) },
    )
  }

  /**
   * What `createDevice()` can make: iOS runtimes and device types, Android
   * system images and hardware profiles. `available` adds the Android
   * images sdkmanager can download. `location: 'cloud'` lists the models
   * the cloud providers can launch instead (in `cloud`), filtered by the
   * other options; `hub` names the hub that answers.
   */
  async catalog(opts: CatalogQuery & { signal?: AbortSignal } = {}): Promise<CatalogResult> {
    const { signal, ...q } = opts
    return this.transport.json<CatalogResult>(withQuery('/v1/catalog', q), { signal: this.sig(signal) })
  }

  /**
   * Create a simulator or AVD and, unless `boot: false`, boot it. It is
   * recorded as created by maestro-d, so `deleteDevice()` accepts it.
   * Resolves with the booted device, or the shut-down one (an AVD as
   * `avd:<name>`).
   *
   * With `cloud` it launches a cloud device instead (direct mode: this
   * computer's own daemon, with its own AWS credentials), which is released
   * after `cloud.idleReleaseSec` without a request.
   */
  async createDevice(opts: CreateDeviceOptions & { signal?: AbortSignal }): Promise<DeviceInfo> {
    if (opts.cloud) {
      // A cloud device: direct mode, on this computer's own daemon.
      if (!opts.cloud.platform) throw new TypeError('createDevice({cloud}) needs cloud.platform')
      // App files are read by the daemon, whose working directory is not
      // this process's: send them absolute (an ARN or blob ref as is).
      const cloud = { ...opts.cloud }
      if (cloud.app) cloud.app = hostPath(cloud.app)
      if (cloud.auxiliaryApps) cloud.auxiliaryApps = cloud.auxiliaryApps.map(hostPath)
      const res = await this.transport.json<Envelope & { device: DeviceInfo }>('/v1/devices', {
        method: 'POST',
        body: { platform: cloud.platform, create: { cloud } },
        signal: this.sig(opts.signal),
      })
      return res.device
    }
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
   * not boot are refused with DEVICE_EXTERNAL unless `force`. A cloud
   * device's session is released, and its device minutes returned.
   */
  async stopDevice(id: string, opts: { force?: boolean; signal?: AbortSignal } = {}): Promise<{ deviceMinutes?: DeviceMinutes }> {
    const res = await this.transport.json<Envelope & { device?: DeviceInfo }>(`/v1/devices/${encodeURIComponent(id)}`, {
      method: 'DELETE',
      body: { force: opts.force === true },
      signal: this.sig(opts.signal),
    })
    // A cloud device's answer is the released device, with its minutes.
    return res.device?.deviceMinutes ? { deviceMinutes: res.device.deviceMinutes } : {}
  }

  /**
   * Open a driver on a device and return its handle. Attaching to an
   * already-attached device returns a handle without reconnecting.
   */
  async attach(opts: DeviceAttachOptions = {}): Promise<MaestroDevice> {
    const { device, signal, ...cfg } = opts
    let id = await this.resolveDevice(device, signal)
    const sig = this.sig(signal)
    let lease: Lease | undefined
    if (this.info?.hub) {
      if (isEmulatorTarget(id)) {
        // Match the saved target before discovery: the serial may now belong to another AVD.
        const matches = (v?: string) => v !== undefined && (v === id || v.split('@')[0] === id)
        const candidates = loadLeases().filter(r => r.hub === this.leaseKey &&
          [r.device, r.originalDevice, r.requestedDevice].some(matches))
        if (new Set(candidates.map(r => r.device.split('@')[1])).size > 1) throw MaestroError.of('USAGE', 'qualify the emulator target with its hub name')
        const original = candidates.filter(r => matches(r.originalDevice ?? r.device) || matches(r.requestedDevice))
        const grants = original.length ? original : candidates
        const rec = grants[0] && currentLease(grants, grants[0].device)
        if (rec) {
          const fetched = await this.transport.json<Envelope & { lease: Lease }>(`/v1/leases/${encodeURIComponent(rec.id)}`, { signal: sig })
          lease = fetched.lease
          if (lease.id !== rec.id) throw MaestroError.of('LEASE_EXPIRED', 'the hub returned a different grant')
          if (this.transport.leases.get(rec.device) === rec.id) this.transport.leases.delete(rec.device)
          if (!lease.device) throw MaestroError.of('DEVICE_NOT_FOUND', 'the leased emulator is unavailable')
          id = lease.device
          this.transport.leases.set(id, lease.id)
        }
      }
      if (!lease) {
        id = await this.normalize(id, sig)
        const held = this.heldLease(id)
        const key = this.transport.leases.get(id)
        lease = key ? this.leased.get(key)?.lease : undefined
        if (!lease && held) lease = { ...held, expiresAt: held.expiresAt ?? '', ttlSec: held.ttlSec ?? 0 }
      }
    }
    const attach = savedAttach(cfg)
    const info = await this.openAttach(id, attach, sig)
    return lease ? this.bindDevice(info, lease, attach, sig, false) : new MaestroDevice(this.transport, this, info, sig)
  }

  /** POST …/attach, uploading the app file first on a hub. */
  private async openAttach(id: string, cfg: AttachOptions, signal?: AbortSignal): Promise<DeviceInfo> {
    cfg = { ...cfg }
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
 * Resolves a file the daemon reads against this process's directory; an
 * ARN or a `blob:` ref is passed as is.
 */
function hostPath(p: string): string {
  return p.startsWith('arn:') || p.startsWith('blob:') ? p : path.resolve(p)
}

/**
 * Appends a filter object to path as a query string: arrays as comma
 * lists, true as "true", false and empty values left out.
 */
export function withQuery(path: string, q: Record<string, unknown>): string {
  const p = new URLSearchParams()
  for (const [k, v] of Object.entries(q)) {
    if (v === undefined || v === null || v === false || v === '') continue
    p.set(k, Array.isArray(v) ? v.join(',') : String(v))
  }
  const qs = p.toString()
  return qs ? `${path}?${qs}` : path
}

/** `md.devicefarm`: AWS Device Farm without a device. */
export class DeviceFarmUtilities {
  constructor(
    private readonly transport: Transport,
    private readonly signal: () => AbortSignal | undefined,
  ) {}

  private path(what: string, hub?: string): string {
    return withQuery(`/v1/providers/devicefarm/${what}`, { hub })
  }

  /** Trial minutes and unmetered devices (`GET /v1/providers/devicefarm/account`). */
  async account(opts: { hub?: string } = {}): Promise<ProviderAccount> {
    return this.transport.json<ProviderAccount>(this.path('account', opts.hub), { signal: this.signal() })
  }

  /** The project's uploaded apps. */
  async apps(opts: { hub?: string } = {}): Promise<ProviderApps> {
    return this.transport.json<ProviderApps>(this.path('apps', opts.hub), { signal: this.signal() })
  }

  /**
   * Upload an .apk or .ipa from the daemon's machine, reusing an identical
   * upload from the last 30 days. A remote caller is refused: install an app
   * on a leased cloud device instead.
   */
  async upload(file: string, opts: { hub?: string } = {}): Promise<ProviderUpload> {
    return this.transport.json<ProviderUpload>(this.path('apps', opts.hub), {
      method: 'POST',
      body: { app: path.resolve(file) },
      signal: this.signal(),
    })
  }

  /** The project's remote access sessions, each named with its device id. */
  async sessions(opts: { hub?: string } = {}): Promise<ProviderSessions> {
    return this.transport.json<ProviderSessions>(this.path('sessions', opts.hub), { signal: this.signal() })
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

function savedAttach(config: AttachOptions): AttachOptions {
  const copy = structuredClone(config)
  if (copy.appFile && !copy.appFile.startsWith('blob:')) copy.appFile = path.resolve(copy.appFile)
  return copy
}

/** True when an attach would upload a file of this machine (a hub refuses host paths). */
function hasLocalFiles(cfg: AttachOptions): boolean {
  return typeof cfg.appFile === 'string' && cfg.appFile !== '' && !cfg.appFile.startsWith('blob:')
}

/** An RFC 3339 time to the second, in UTC. */
function rfc3339(d: Date): string {
  return d.toISOString().replace(/\.\d{3}Z$/, 'Z')
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
