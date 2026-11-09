// MaestroDevice: a handle on one attached device. Every YAML command is a
// method of the same name; each call becomes one step in the device's
// session, executed by the daemon with the unmodified maestro-runner code.

import { randomUUID } from 'node:crypto'
import * as fs from 'node:fs'
import * as path from 'node:path'

import { MaestroError } from './errors.js'
import { COMMANDS, type CommandName, type CommandParams } from './generated/commands.js'
import type { Transport } from './transport.js'
import type {
  DaemonEvent,
  DeviceInfo,
  Envelope,
  HierarchyNode,
  PlatformInfo,
  Step,
  StepResult,
  StepsResult,
} from './types.js'

/** A bare command value: `tapOn('Login')`, `swipe('UP')`, `setDarkMode(true)`. */
export type Scalar = string | number | boolean

/** Per-call options that are not part of the step itself. */
export interface CallOptions {
  /** Return BUSY instead of queueing behind a step already running on the device. */
  wait?: boolean
  /**
   * Directory relative paths in the step resolve against (default
   * process.cwd()). Not sent to a hub, which is another machine.
   */
  cwd?: string
  signal?: AbortSignal
}

export interface RunOptions extends CallOptions {
  /** Keep going after a failed step; the result still reports `ok: false`. */
  continueOnError?: boolean
}

export interface FlowOptions extends CallOptions {
  /** Flow file path (absolute, or relative to cwd). */
  file?: string
  /** Inline flow YAML, header and all. */
  yaml?: string
  env?: Record<string, string>
}

/** The typed method for each command. */
export type CommandMethod<K extends CommandName> = (typeof COMMANDS)[K]['valueLess'] extends true
  ? () => Promise<StepResult>
  : (value?: Scalar | CommandParams[K], opts?: CommandParams[K] & CallOptions) => Promise<StepResult>

export type CommandMethods = { [K in CommandName]: CommandMethod<K> }

const CALL_KEYS = new Set(['wait', 'cwd', 'signal'])

/**
 * Split `opts` into the step fields (merged into the YAML value) and the
 * per-call transport options.
 */
function splitOptions(opts: Record<string, unknown> | undefined): { fields: Record<string, unknown>; call: CallOptions } {
  const fields: Record<string, unknown> = {}
  const call: CallOptions = {}
  if (opts) {
    for (const [k, v] of Object.entries(opts)) {
      if (v === undefined) continue
      if (CALL_KEYS.has(k)) (call as Record<string, unknown>)[k] = v
      else fields[k] = v
    }
  }
  return { fields, call }
}

/**
 * Build the YAML value a flow file would hold for this command from a
 * method call: `tapOn('Login', {optional: true})` → `{text: 'Login', optional: true}`.
 */
export function buildValue(name: string, value: unknown, fields: Record<string, unknown>): unknown {
  const spec = COMMANDS[name as CommandName]
  if (!spec) {
    // Unknown here (newer daemon?): pass through and let the daemon judge.
    return mergeFields(name, value, fields, undefined)
  }
  if (spec.valueLess) {
    if (value !== undefined || Object.keys(fields).length > 0) {
      throw new TypeError(`${name} takes no value`)
    }
    return null
  }
  return mergeFields(name, value, fields, spec.scalar)
}

/** scalar: the field a bare value maps to, null when the command has none, undefined when unknown. */
function mergeFields(name: string, value: unknown, fields: Record<string, unknown>, scalar: string | null | undefined): unknown {
  const bare = value !== undefined && value !== null && !isPlainObject(value) && !Array.isArray(value)
  if (bare && scalar === null) {
    throw new TypeError(`${name} does not take a bare value; pass an object of fields`)
  }
  if (Object.keys(fields).length === 0) {
    return value === undefined ? null : value
  }
  if (value === undefined) return fields
  if (isPlainObject(value)) return { ...value, ...fields }
  if (Array.isArray(value)) {
    throw new TypeError(`${name}: a list value cannot be combined with options`)
  }
  if (!scalar) {
    throw new TypeError(`${name}: a bare value cannot be combined with options; pass an object`)
  }
  return { [scalar]: value, ...fields }
}

function isPlainObject(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v) && !Buffer.isBuffer(v)
}

/** A signal that fires when either input does (AbortSignal.any needs Node 20). */
export function anySignal(a?: AbortSignal, b?: AbortSignal): AbortSignal | undefined {
  if (!a || !b) return a ?? b
  if (a.aborted) return a
  if (b.aborted) return b
  const ctl = new AbortController()
  const onAbort = (): void => ctl.abort()
  a.addEventListener('abort', onAbort, { once: true })
  b.addEventListener('abort', onAbort, { once: true })
  return ctl.signal
}

function query(params: Record<string, string | undefined>): string {
  const q = new URLSearchParams()
  for (const [k, v] of Object.entries(params)) {
    if (v !== undefined && v !== '') q.set(k, v)
  }
  const s = q.toString()
  return s ? `?${s}` : ''
}

// Declaration merging gives the class one typed method per command; the
// implementations are installed on the prototype below.
// eslint-disable-next-line @typescript-eslint/no-unsafe-declaration-merging
export interface MaestroDevice extends CommandMethods {}

// eslint-disable-next-line @typescript-eslint/no-unsafe-declaration-merging
export class MaestroDevice {
  /** Device id / UDID. */
  readonly id: string
  /** The daemon this device is attached through. */
  readonly daemon: { readonly name: string }
  /** Device record as of attach; refresh with `refresh()`. */
  info: DeviceInfo
  /** Aborts every request made through this handle. */
  readonly signal?: AbortSignal
  private readonly transport: Transport
  private readonly base: string

  /** @internal Use `MaestroD.attach()`. */
  constructor(transport: Transport, daemon: { readonly name: string }, info: DeviceInfo, signal?: AbortSignal) {
    this.transport = transport
    this.daemon = daemon
    this.id = info.id
    this.info = info
    if (signal) this.signal = signal
    this.base = `/v1/devices/${encodeURIComponent(info.id)}`
  }

  private sig(call: CallOptions): AbortSignal | undefined {
    return anySignal(call.signal, this.signal)
  }

  // -------------------------------------------------------------------------
  // Commands

  /**
   * Run one YAML command by name. `value` is what the flow file would hold
   * (`'Login'`, `{id: 'x'}`, `null`); `opts` adds step fields
   * (`optional`, `timeout`, …) plus the per-call `wait`/`cwd`/`signal`.
   */
  async command(name: string, value?: unknown, opts?: Record<string, unknown>): Promise<StepResult> {
    if (typeof name !== 'string' || name === '') throw new TypeError('command name must be a non-empty string')
    const { fields, call } = splitOptions(opts)
    const body = buildValue(name, value, fields)
    const cwd = this.transport.hub ? undefined : (call.cwd ?? process.cwd())
    const url =
      `${this.base}/commands/${encodeURIComponent(name)}` + query({ cwd, wait: call.wait === false ? 'false' : undefined })
    const res = await this.transport.json<StepResult>(url, { method: 'POST', body, signal: this.sig(call), requestId: this.requestId() })
    return checkResult(res)
  }

  /**
   * Run a list of steps as one request; stops at the first failure unless
   * `continueOnError`. Rejects with a MaestroError carrying the partial
   * results when any step failed.
   */
  async run(steps: Step[], opts: RunOptions = {}): Promise<StepsResult> {
    if (!Array.isArray(steps)) throw new TypeError('run() takes an array of steps')
    return this.steps({ steps }, opts)
  }

  /** `run()` for a YAML list of steps. */
  async runYaml(yaml: string, opts: RunOptions = {}): Promise<StepsResult> {
    if (typeof yaml !== 'string') throw new TypeError('runYaml() takes a YAML string')
    return this.steps({ yaml }, opts)
  }

  private async steps(req: { steps?: Step[]; yaml?: string }, opts: RunOptions): Promise<StepsResult> {
    const body: Record<string, unknown> = { ...req, continueOnError: opts.continueOnError === true }
    if (!this.transport.hub) body.cwd = opts.cwd ?? process.cwd()
    const url = `${this.base}/steps` + query({ wait: opts.wait === false ? 'false' : undefined })
    const res = await this.transport.json<StepsResult>(url, { method: 'POST', body, signal: this.sig(opts), requestId: this.requestId() })
    return checkResult(res)
  }

  /** A fresh request id for a step-running call over TCP (kept across retries). */
  private requestId(): string | undefined {
    return this.transport.tcp ? randomUUID() : undefined
  }

  /**
   * Run a whole flow file (or inline flow YAML with its header) as one
   * compound step, like `runFlow` but honouring the file's `appId`/`env`.
   */
  async flow(opts: FlowOptions): Promise<StepResult> {
    if (!opts.file && !opts.yaml) throw new TypeError('flow() needs a file or yaml')
    let body: Record<string, unknown>
    if (this.transport.hub) {
      // The file is on this machine, not the hub's: send its YAML, and no
      // working directory (a hub refuses host paths).
      let yaml = opts.yaml
      if (yaml === undefined) {
        const file = path.resolve(opts.cwd ?? process.cwd(), opts.file as string)
        try {
          yaml = fs.readFileSync(file, 'utf8')
        } catch (err) {
          throw MaestroError.of('USAGE', `flow(): ${(err as Error).message}`, { cause: err })
        }
      }
      body = { yaml, env: opts.env }
    } else {
      body = { file: opts.file, yaml: opts.yaml, env: opts.env, cwd: opts.cwd ?? process.cwd() }
    }
    const url = `${this.base}/flows` + query({ wait: opts.wait === false ? 'false' : undefined })
    const res = await this.transport.json<StepResult>(url, { method: 'POST', body, signal: this.sig(opts), requestId: this.requestId() })
    return checkResult(res)
  }

  // -------------------------------------------------------------------------
  // Inspection

  /** Current screen as PNG bytes (not recorded as a step). */
  async screenshot(opts: CallOptions = {}): Promise<Buffer> {
    return this.transport.bytes(`${this.base}/screenshot`, { accept: 'image/png', signal: this.sig(opts) })
  }

  /** View hierarchy as the normalized cross-platform tree (`maestro-runner hierarchy`'s shape). */
  async hierarchy(opts: CallOptions = {}): Promise<HierarchyNode> {
    const res = await this.transport.json<Envelope & { data: HierarchyNode }>(`${this.base}/hierarchy`, { signal: this.sig(opts) })
    return res.data
  }

  /** View hierarchy exactly as the driver produced it. */
  async hierarchyRaw(opts: CallOptions = {}): Promise<Buffer> {
    return this.transport.bytes(`${this.base}/hierarchy?format=raw`, { signal: this.sig(opts) })
  }

  /** Device state: foreground app, orientation, … */
  async state(opts: CallOptions = {}): Promise<unknown> {
    const res = await this.transport.json<Envelope & { data: unknown }>(`${this.base}/state`, { signal: this.sig(opts) })
    return res.data
  }

  /** Driver platform info: OS version, screen size, … */
  async platformInfo(opts: CallOptions = {}): Promise<PlatformInfo> {
    const res = await this.transport.json<Envelope & { data: PlatformInfo }>(`${this.base}/info`, { signal: this.sig(opts) })
    return res.data
  }

  /** Session variables (`${NAME}` in later steps). */
  async vars(opts: CallOptions = {}): Promise<Record<string, string>> {
    const res = await this.transport.json<Envelope & { vars: Record<string, string> }>(`${this.base}/vars`, { signal: this.sig(opts) })
    return res.vars
  }

  /** Set session variables; returns the full set. */
  async setVars(vars: Record<string, string>, opts: CallOptions = {}): Promise<Record<string, string>> {
    if (!isPlainObject(vars)) throw new TypeError('setVars() takes an object of NAME: value')
    const res = await this.transport.json<Envelope & { vars: Record<string, string> }>(`${this.base}/vars`, {
      method: 'PUT',
      body: vars,
      signal: this.sig(opts),
    })
    return res.vars
  }

  /** Evaluate JavaScript in the session's engine and return its value. */
  async eval(script: string, opts: CallOptions = {}): Promise<unknown> {
    if (typeof script !== 'string') throw new TypeError('eval() takes a script string')
    const res = await this.transport.json<Envelope & { value: unknown }>(`${this.base}/eval`, {
      method: 'POST',
      body: { script },
      signal: this.sig(opts),
    })
    return res.value
  }

  /** Re-read the device record from the daemon. */
  async refresh(opts: CallOptions = {}): Promise<DeviceInfo> {
    const res = await this.transport.json<Envelope & { device: DeviceInfo }>(this.base, { signal: this.sig(opts) })
    this.info = res.device
    return res.device
  }

  /** Step events for this device. Ends when `signal` fires. */
  events(opts: { after?: number; signal?: AbortSignal } = {}): AsyncIterable<DaemonEvent> {
    const path = '/v1/events' + query({ device: this.id, after: opts.after ? String(opts.after) : undefined })
    return this.transport.sse<DaemonEvent>(path, this.sig(opts))
  }

  // -------------------------------------------------------------------------
  // Lifecycle

  /** Close the driver. The device (and daemon) keep running. */
  async detach(opts: CallOptions = {}): Promise<void> {
    await this.transport.json<Envelope>(`${this.base}/detach`, { method: 'POST', signal: this.sig(opts) })
    this.info = { ...this.info, attached: false }
  }

  /**
   * Detach and shut the device down. Devices the daemon did not boot are
   * refused (DEVICE_EXTERNAL) unless `force`.
   */
  async stop(opts: CallOptions & { force?: boolean } = {}): Promise<void> {
    await this.transport.json<Envelope>(this.base, { method: 'DELETE', body: { force: opts.force === true }, signal: this.sig(opts) })
    this.info = { ...this.info, attached: false, state: 'Shutdown' }
  }
}

function checkResult<T extends Envelope>(res: T): T {
  if (!res.ok && res.error) {
    throw new MaestroError(res.error, { result: res as never })
  }
  return res
}

// Install one method per command. Names that clash with a hand-written
// member above are left alone (none do today; `command()` still reaches them).
for (const name of Object.keys(COMMANDS) as CommandName[]) {
  if (name in MaestroDevice.prototype) continue
  Object.defineProperty(MaestroDevice.prototype, name, {
    value: function (this: MaestroDevice, value?: unknown, opts?: Record<string, unknown>) {
      return this.command(name, value, opts)
    },
    writable: true,
    configurable: true,
    enumerable: false,
  })
}
