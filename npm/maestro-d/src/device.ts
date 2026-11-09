// MaestroDevice: a handle on one attached device. Every YAML command is a
// method of the same name; each call becomes one step in the device's
// session, executed by the daemon with the unmodified maestro-runner code.

import { randomUUID } from 'node:crypto'
import * as fs from 'node:fs'
import * as os from 'node:os'
import * as path from 'node:path'

import { MaestroError } from './errors.js'
import { COMMANDS, type CommandName, type CommandParams } from './generated/commands.js'
import { type FlowBundle, HubFiles, fetchBundleOutputs, packFlow, uploadBundle, uploadPath } from './pack.js'
import type { Transport } from './transport.js'
import type {
  ArtifactInfo,
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
  /**
   * On a hub: globs (relative to cwd) of files the steps open dynamically,
   * packed with them.
   */
  include?: string[]
}

export interface FlowOptions extends CallOptions {
  /** Flow file path (absolute, or relative to cwd). */
  file?: string
  /** Inline flow YAML, header and all. */
  yaml?: string
  env?: Record<string, string>
  /**
   * On a hub: globs (relative to the flow's directory) of files the flow
   * opens dynamically, packed with it.
   */
  include?: string[]
}

/** Write `yaml` to a temporary root file, run `fn` on it, and remove the directory. */
async function withTempRoot<T>(yaml: string, fn: (root: string) => Promise<T>): Promise<T> {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'maestro-d-inline-'))
  try {
    const root = path.join(dir, '.maestro-d-inline.yaml')
    fs.writeFileSync(root, yaml)
    return await fn(root)
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }
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
  const listField = spec.bareField !== null && spec.bareField !== spec.scalar ? spec.bareField : undefined
  return mergeFields(name, value, fields, spec.scalar, listField)
}

/**
 * scalar: the field a bare value maps to, null when the command has none,
 * undefined when unknown. listField: the list field a bare value or list
 * fills when the command has no scalar (addMedia's files).
 */
function mergeFields(
  name: string,
  value: unknown,
  fields: Record<string, unknown>,
  scalar: string | null | undefined,
  listField?: string,
): unknown {
  const bare = value !== undefined && value !== null && !isPlainObject(value) && !Array.isArray(value)
  if (bare && scalar === null && listField === undefined) {
    throw new TypeError(`${name} does not take a bare value; pass an object of fields`)
  }
  if (Object.keys(fields).length === 0) {
    return value === undefined ? null : value
  }
  if (value === undefined) return fields
  if (isPlainObject(value)) return { ...value, ...fields }
  if (listField !== undefined && (bare || Array.isArray(value))) {
    return { [listField]: Array.isArray(value) ? value : [value], ...fields }
  }
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
    let body = buildValue(name, value, fields)
    const cwd = this.transport.hub ? undefined : (call.cwd ?? process.cwd())
    const url =
      `${this.base}/commands/${encodeURIComponent(name)}` + query({ cwd, wait: call.wait === false ? 'false' : undefined })
    // On a hub, upload the input files and name the outputs ourselves; the
    // outputs are fetched back to the caller's paths, also after a failure
    // (an assertScreenshot mismatch's diff image).
    const files = this.transport.hub ? new HubFiles(this.transport, this.base, call.cwd ?? process.cwd(), this.sig(call)) : undefined
    if (files) body = await files.rewrite(name, body)
    let res: StepResult
    try {
      res = await this.transport.json<StepResult>(url, { method: 'POST', body, signal: this.sig(call), requestId: this.requestId() })
    } catch (err) {
      if (files && err instanceof MaestroError && err.result && 'outputs' in err.result) {
        await files.fetchOutputs(err.result.outputs)
      }
      throw err
    }
    if (files) await files.fetchOutputs(res.outputs)
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
    const url = `${this.base}/steps` + query({ wait: opts.wait === false ? 'false' : undefined })
    if (this.transport.hub) {
      // The steps' relative references are on this machine: write them to
      // a temporary root that stands in cwd, pack it with everything it
      // uses, and post the bundle (a hub refuses host directories).
      const cwd = opts.cwd ?? process.cwd()
      const yaml = req.yaml ?? JSON.stringify(req.steps)
      return withTempRoot(yaml, async (root) => {
        const bundle = await packFlow([root], { base: cwd, include: opts.include, signal: this.sig(opts) })
        const body = { bundle: bundle.manifest, continueOnError: opts.continueOnError === true }
        return this.postBundle<StepsResult>(url, body, bundle, opts)
      })
    }
    const body: Record<string, unknown> = { ...req, continueOnError: opts.continueOnError === true, cwd: opts.cwd ?? process.cwd() }
    const res = await this.transport.json<StepsResult>(url, { method: 'POST', body, signal: this.sig(opts), requestId: this.requestId() })
    return checkResult(res)
  }

  /**
   * Upload a bundle's files, post the call, and fetch its outputs back into
   * the bundle's directory, also after a failure.
   */
  private async postBundle<T extends StepResult | StepsResult>(
    url: string,
    body: Record<string, unknown>,
    bundle: FlowBundle,
    opts: CallOptions,
  ): Promise<T> {
    const signal = this.sig(opts)
    await uploadBundle(this.transport, this.base, bundle, { signal })
    let res: T
    try {
      res = await this.transport.json<T>(url, { method: 'POST', body, signal, requestId: this.requestId() })
    } catch (err) {
      if (err instanceof MaestroError && err.result && 'outputs' in err.result) {
        await fetchBundleOutputs(this.transport, this.base, bundle.dir, err.result.outputs, signal)
      }
      throw err
    }
    await fetchBundleOutputs(this.transport, this.base, bundle.dir, res.outputs, signal)
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
    const url = `${this.base}/flows` + query({ wait: opts.wait === false ? 'false' : undefined })
    if (this.transport.hub) {
      // The flow is on this machine, not the hub's: pack it with everything
      // it uses and post the bundle; inline YAML stands in cwd.
      const cwd = opts.cwd ?? process.cwd()
      const run = async (root: string, base?: string): Promise<StepResult> => {
        const bundle = await packFlow([root], { base, env: opts.env, include: opts.include, signal: this.sig(opts) })
        return this.postBundle<StepResult>(url, { bundle: bundle.manifest, env: opts.env }, bundle, opts)
      }
      if (opts.yaml !== undefined) return withTempRoot(opts.yaml, (root) => run(root, cwd))
      return run(path.resolve(cwd, opts.file as string))
    }
    const body = { file: opts.file, yaml: opts.yaml, env: opts.env, cwd: opts.cwd ?? process.cwd() }
    const res = await this.transport.json<StepResult>(url, { method: 'POST', body, signal: this.sig(opts), requestId: this.requestId() })
    return checkResult(res)
  }

  // -------------------------------------------------------------------------
  // Apps and artifacts

  /**
   * Install an app (.apk, .app or .ipa) without attaching. On a hub the
   * file is uploaded first, unless the hub already has it. Resolves to the
   * app id when the daemon can read it.
   */
  async installApp(file: string, opts: CallOptions & { reinstall?: boolean; platform?: string } = {}): Promise<{ appId?: string }> {
    if (typeof file !== 'string' || file === '') throw new TypeError('installApp() takes a file path')
    let app = path.resolve(opts.cwd ?? process.cwd(), file)
    if (this.transport.hub) app = (await uploadPath(this.transport, this.base, app, this.sig(opts))).ref
    const res = await this.transport.json<Envelope & { appId?: string }>(`${this.base}/apps`, {
      method: 'POST',
      body: { app, reinstall: opts.reinstall === true || undefined, platform: opts.platform },
      signal: this.sig(opts),
      requestId: this.requestId(),
    })
    return res.appId !== undefined ? { appId: res.appId } : {}
  }

  /** Uninstall an app by bundle id or package name. */
  async uninstallApp(appId: string, opts: CallOptions = {}): Promise<void> {
    if (typeof appId !== 'string' || appId === '') throw new TypeError('uninstallApp() takes an app id')
    await this.transport.json<Envelope>(`${this.base}/apps/${encodeURIComponent(appId)}`, { method: 'DELETE', signal: this.sig(opts) })
  }

  /** The files of the session's report directory (screenshots, outputs/, recordings, logs). */
  async artifacts(opts: CallOptions = {}): Promise<ArtifactInfo[]> {
    const res = await this.transport.json<Envelope & { artifacts: ArtifactInfo[] }>(`${this.base}/artifacts`, { signal: this.sig(opts) })
    return res.artifacts
  }

  /** One artifact's bytes; `file` is relative to the report directory, as `artifacts()` lists it. */
  async artifact(file: string, opts: CallOptions = {}): Promise<Buffer> {
    if (typeof file !== 'string' || file === '') throw new TypeError('artifact() takes a path')
    return this.transport.bytes(`${this.base}/artifacts/${file.split('/').map(encodeURIComponent).join('/')}`, { signal: this.sig(opts) })
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
