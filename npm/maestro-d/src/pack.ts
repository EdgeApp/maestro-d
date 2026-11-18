// Files for a hub: packing, uploading, and the file fields of a command.
//
// A hub is another machine, so a command's input files (the fields the
// generated table marks `path`) are packed by the maestro-d binary
// (`maestro-d pack --json`, the same packing the CLI does), uploaded when
// the hub lacks them, and sent as `blob:sha256:<hex>/<basename>` refs. Its
// output fields (`output`) are sent as names of the client's own,
// `<first 8 hex of sha256(caller path)>/<basename>`, and the files the
// result lists in `outputs` are fetched back to the caller's paths. Its
// flow-file fields (`flow`: runFlow's file and else, retry's file) are packed
// with their whole closure (`maestro-d pack --flow`) into one bundle and sent
// as `bundle:sha256:<manifest>/<root>` refs; whole flows and step lists go as
// bundles too (see `packFlow`). A bundle's outputs come back to the same
// path relative to its common directory.

import { execFile } from 'node:child_process'
import { createHash } from 'node:crypto'
import * as fs from 'node:fs'
import * as path from 'node:path'

import { binaryPath } from './binary.js'
import { codeFromHeaders, MaestroError } from './errors.js'
import { COMMANDS, type CommandName, type CommandSpec, type FileMark } from './generated/commands.js'
import { isRetryableResponse, type RawResponse, type Transport } from './transport.js'

/** One upload unit, as `maestro-d pack --json` prints it. */
export interface PackUnit {
  /** The caller's file or directory, absolute. */
  path: string
  /** The basename the blob ref carries. */
  name: string
  sha256: string
  size: number
  /** The bytes to upload: `path`, or the zip written for a directory. */
  file: string
  /** A zipped directory (an iOS .app); the daemon unpacks it. */
  dir?: boolean
  /** `blob:sha256:<hex>/<name>`. */
  ref: string
}

/** Pack files and directories with the maestro-d binary. */
export async function pack(paths: string[], opts: { bin?: string; signal?: AbortSignal } = {}): Promise<PackUnit[]> {
  return runPack(['pack', '--json', ...paths], (p) => p.units as PackUnit[] | undefined, opts)
}

/** A flow bundle's manifest: the root file, and each file's sha256 by bundle-relative path. */
export interface BundleManifest {
  root: string
  files: Record<string, string>
}

/** A packed flow, as `maestro-d pack --flow --json` prints it. */
export interface FlowBundle {
  manifest: BundleManifest
  /** The manifest's hash: its blob, and its directory on the hub. */
  sha256: string
  /** The files' common directory on this machine: the bundle root. */
  dir: string
  /** Each root file given, absolute, → its manifest path. */
  roots: Record<string, string>
  /** The files to upload, one per distinct hash. */
  units: PackUnit[]
  /** The manifest's own blob, for `bundle:` refs. */
  manifestUnit: PackUnit
}

export interface PackFlowOptions {
  /** Place the first root as if it sat in this directory (inline steps). */
  base?: string
  /** Values for `${VAR}` in paths (`-e`). */
  env?: Record<string, string>
  /** Globs (relative to the first root's directory) of files flows open dynamically. */
  include?: string[]
  bin?: string
  signal?: AbortSignal
}

/** Run the maestro-d binary with `args` and parse its `--json` envelope. */
function runPack<T>(args: string[], pick: (parsed: Record<string, unknown>) => T | undefined, opts: { bin?: string; signal?: AbortSignal }): Promise<T> {
  const bin = binaryPath(opts.bin)
  return new Promise((resolve, reject) => {
    execFile(bin, args, { signal: opts.signal, maxBuffer: 64 << 20 }, (err, stdout, stderr) => {
      let parsed: (Record<string, unknown> & { ok?: boolean; error?: { code: string; message: string } }) | undefined
      try {
        parsed = JSON.parse(stdout) as typeof parsed
      } catch {
        parsed = undefined
      }
      const value = parsed?.ok ? pick(parsed) : undefined
      if (value !== undefined) return resolve(value)
      if (parsed?.error) return reject(new MaestroError(parsed.error as never))
      reject(MaestroError.of('USAGE', `maestro-d pack: ${String(stderr || err?.message || 'failed').trim()}`, { cause: err }))
    })
  })
}

/**
 * Pack flow files with their whole dependency closure into one bundle
 * (`maestro-d pack --flow`); the first root is the manifest's root.
 */
export async function packFlow(roots: string[], opts: PackFlowOptions = {}): Promise<FlowBundle> {
  const args = ['pack', '--flow', '--json']
  if (opts.base !== undefined) args.push('--base', opts.base)
  for (const [k, v] of Object.entries(opts.env ?? {})) args.push('-e', `${k}=${v}`)
  for (const g of opts.include ?? []) args.push('--include', g)
  args.push(...roots)
  return runPack(args, (p) => p.bundle as FlowBundle | undefined, opts)
}

/**
 * Make sure the device's daemon holds every file of `bundle`, and its
 * manifest when `manifest` (for `bundle:` refs).
 */
export async function uploadBundle(
  transport: Transport,
  deviceBase: string,
  bundle: FlowBundle,
  opts: { manifest?: boolean; signal?: AbortSignal } = {},
): Promise<void> {
  for (const unit of opts.manifest ? [...bundle.units, bundle.manifestUnit] : bundle.units) {
    await upload(transport, deviceBase, unit, opts.signal)
  }
}

/** True when `rel` is a plain bundle-relative path (no `..`, `.`, empty element or absolute start). */
function isBundlePath(rel: string): boolean {
  return rel !== '' && !rel.startsWith('/') && !rel.includes('\\') && rel.split('/').every((el) => el !== '' && el !== '.' && el !== '..')
}

/** Download one artifact to `target`, through a temporary file beside it. */
async function fetchTo(transport: Transport, deviceBase: string, entry: string, target: string, signal?: AbortSignal): Promise<void> {
  const data = await transport.bytes(`${deviceBase}/artifacts/${entry.split('/').map(encodeURIComponent).join('/')}`, { signal })
  fs.mkdirSync(path.dirname(target), { recursive: true })
  const tmp = path.join(path.dirname(target), `.${path.basename(target)}.${process.pid}.tmp`)
  fs.writeFileSync(tmp, data)
  fs.renameSync(tmp, target)
}

/**
 * Write each file a bundled call lists in `outputs` (`outputs/<path>`) to
 * the same path relative to the bundle root on this machine, `dir`. An
 * entry that would leave `dir` is skipped.
 */
export async function fetchBundleOutputs(
  transport: Transport,
  deviceBase: string,
  dir: string,
  outputs: string[] | undefined,
  signal?: AbortSignal,
): Promise<void> {
  for (const entry of outputs ?? []) {
    if (!entry.startsWith('outputs/')) continue
    const rel = entry.slice('outputs/'.length)
    if (!isBundlePath(rel)) continue
    await fetchTo(transport, deviceBase, entry, path.join(dir, ...rel.split('/')), signal)
  }
}

const UPLOAD_RETRIES = 3

/** Errors made from a gateway's 502/504 without a maestro-d envelope. */
const gatewayErrors = new WeakSet<MaestroError>()

/** Decode a failed blob request, marking the ones isRetryableResponse would retry. */
function blobError(res: RawResponse): MaestroError {
  const status = res.status
  const text = res.body.toString('utf8')
  const err = MaestroError.fromResponse(status, text, undefined, codeFromHeaders(res.headers))
  if (isRetryableResponse(status, text)) gatewayErrors.add(err)
  return err
}

/**
 * True when a blob HEAD or PUT failure is worth asking for the offset again
 * and resuming: the daemon or the owning hub was unreachable
 * (DAEMON_UNAVAILABLE, PEER_UNAVAILABLE; a HEAD's 503 has no body to tell
 * them apart), or a gateway answered 502/504.
 */
function uploadRetryable(err: unknown): boolean {
  return err instanceof MaestroError && (err.code === 'DAEMON_UNAVAILABLE' || err.code === 'PEER_UNAVAILABLE' || gatewayErrors.has(err))
}

/**
 * Make sure the device's daemon holds `unit`: ask first (HEAD), and upload
 * from the daemon's offset only when it lacks it. After a dropped link,
 * DAEMON_UNAVAILABLE, PEER_UNAVAILABLE (a hub lost its link to the device's
 * hub) or a gateway's 502/504, on the ask or the upload, it waits (the
 * transport's retry delays, at most UPLOAD_RETRIES times), asks again and
 * resumes from the new offset.
 * Returns true when bytes were sent.
 */
export async function upload(transport: Transport, deviceBase: string, unit: PackUnit, signal?: AbortSignal): Promise<boolean> {
  const url = `${deviceBase}/blobs/${unit.sha256}`
  const delays = transport.retryDelays.slice(0, UPLOAD_RETRIES)
  let sent = false
  for (let attempt = 0; ; attempt++) {
    try {
      const head = await transport.raw(url, { method: 'HEAD', signal })
      if (head.status === 200) return sent
      // A blob the daemon lacks is a bare 404; an error's 404 names its code.
      if (head.status !== 404 || codeFromHeaders(head.headers) !== undefined) throw blobError(head)
      let offset = Number(head.headers['upload-offset'] ?? 0)
      if (!(offset >= 0 && offset <= unit.size)) offset = 0
      sent = true
      const res = await transport.putFile(url, unit.file, offset, unit.size, signal)
      const text = res.body.toString('utf8')
      if (res.status >= 300) throw blobError(res)
      const body = JSON.parse(text) as { complete?: boolean; offset?: number }
      if (body.complete) return true
      throw MaestroError.of('DAEMON_UNAVAILABLE', `blob ${unit.sha256}: upload stopped at byte ${String(body.offset)} of ${unit.size}`)
    } catch (err) {
      if (!uploadRetryable(err) || attempt >= delays.length || signal?.aborted) throw err
      await new Promise((r) => setTimeout(r, delays[attempt]))
    }
  }
}

/** Pack one path and upload it; returns its unit. */
export async function uploadPath(transport: Transport, deviceBase: string, file: string, signal?: AbortSignal): Promise<PackUnit> {
  const [unit] = await pack([file], { signal })
  if (!unit) throw MaestroError.of('INTERNAL', `maestro-d pack printed no unit for ${file}`)
  await upload(transport, deviceBase, unit, signal)
  return unit
}

function isObject(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v)
}

function isRef(v: string): boolean {
  return v.startsWith('blob:') || v.startsWith('bundle:')
}

/** ".png" added to a path with no extension, as the executor does to screenshot paths. */
export function withPNG(p: string): string {
  return path.extname(p) === '' ? p + '.png' : p
}

/** `screen.png` → `screen_diff.png`, as the executor names a mismatch's diff image. */
export function diffScreenshotPath(ref: string): string {
  const ext = path.extname(ref)
  return ref.slice(0, ref.length - ext.length) + '_diff' + (ext === '' ? '.png' : ext)
}

/** The first 8 hex digits of sha256(s). */
export function hash8(s: string): string {
  return createHash('sha256').update(s).digest('hex').slice(0, 8)
}

/** The file fields of one command call sent to a hub. */
export class HubFiles {
  /** outputs entry (`outputs/<name>`) → the caller's path it is written to. */
  readonly fetch = new Map<string, string>()
  /** The common directory of the bundle this call's flow fields were packed into. */
  bundleDir: string | undefined

  constructor(
    private readonly transport: Transport,
    private readonly deviceBase: string,
    private readonly cwd: string,
    private readonly signal?: AbortSignal,
    private readonly env?: Record<string, string>,
  ) {}

  /**
   * Pack the command's flow-file fields (string values: runFlow's else may
   * be inline steps) into one bundle, upload it with its manifest, and
   * return each field's `bundle:` ref.
   */
  private async flows(spec: CommandSpec, files: Record<string, FileMark>, value: unknown): Promise<Map<string, string>> {
    const vals = new Map<string, string>()
    let env = this.env
    if (isObject(value)) {
      for (const [k, mark] of Object.entries(files)) {
        const v = value[k]
        if (mark.kind === 'flow' && typeof v === 'string' && v !== '' && !isRef(v)) vals.set(k, v)
      }
      if (isObject(value.env)) {
        env = { ...env, ...Object.fromEntries(Object.entries(value.env).map(([k, v]) => [k, String(v)])) }
      }
    } else if (typeof value === 'string' && spec.bareField && files[spec.bareField]?.kind === 'flow' && value !== '' && !isRef(value)) {
      vals.set(spec.bareField, value)
    }
    const refs = new Map<string, string>()
    if (vals.size === 0) return refs
    // file first, so it is the manifest's root.
    const keys = [...vals.keys()].sort((a, b) => (a === 'file' ? -1 : b === 'file' ? 1 : a.localeCompare(b)))
    const roots = keys.map((k) => path.resolve(this.cwd, vals.get(k) as string))
    const bundle = await packFlow(roots, { env, signal: this.signal })
    await uploadBundle(this.transport, this.deviceBase, bundle, { manifest: true, signal: this.signal })
    this.bundleDir = bundle.dir
    keys.forEach((k, i) => refs.set(k, `bundle:sha256:${bundle.sha256}/${bundle.roots[roots[i] as string]}`))
    return refs
  }

  private output(v: string): string {
    const target = path.resolve(this.cwd, v)
    const name = `${hash8(target)}/${path.basename(target)}`
    this.fetch.set(`outputs/${name}`, target)
    // takeScreenshot and an assertScreenshot baseline add .png.
    if (path.extname(name) === '') this.fetch.set(`outputs/${name}.png`, target + '.png')
    return name
  }

  private async apply(command: string, mark: FileMark, v: string): Promise<string> {
    if (v === '' || isRef(v) || mark.kind === 'flow' || (mark.suffix !== undefined && !v.endsWith(mark.suffix))) return v
    if (mark.kind === 'output') return this.output(v)
    let file = path.resolve(this.cwd, v)
    if (command === 'assertScreenshot') {
      // A missing reference is a baseline the run writes.
      file = withPNG(file)
      if (!fs.existsSync(file)) return this.output(v)
    }
    const unit = await uploadPath(this.transport, this.deviceBase, file, this.signal)
    if (command === 'assertScreenshot') {
      this.fetch.set(`outputs/${unit.sha256.slice(0, 8)}/${diffScreenshotPath(unit.name)}`, diffScreenshotPath(file))
    }
    return unit.ref
  }

  private async applyAny(command: string, mark: FileMark, v: unknown): Promise<unknown> {
    if (typeof v === 'string') return this.apply(command, mark, v)
    if (Array.isArray(v)) {
      const out: unknown[] = []
      for (const e of v) out.push(typeof e === 'string' ? await this.apply(command, mark, e) : e)
      return out
    }
    return v
  }

  /**
   * The command's value with its file fields rewritten. A bare value (a
   * scalar, or a sequence) fills the command's `bareField`.
   */
  async rewrite(command: string, value: unknown): Promise<unknown> {
    const spec = COMMANDS[command as CommandName] as CommandSpec | undefined
    const files = spec?.files
    if (!spec || !files || value === null || value === undefined) return value
    const refs = await this.flows(spec, files, value)
    if (typeof value === 'string' && spec.bareField && refs.has(spec.bareField)) return refs.get(spec.bareField)
    if (isObject(value) && refs.size > 0) value = { ...value, ...Object.fromEntries(refs) }
    if (typeof value === 'object' && !Array.isArray(value)) {
      const out: Record<string, unknown> = {}
      for (const [k, v] of Object.entries(value as Record<string, unknown>)) {
        const mark = files[k]
        out[k] = mark ? await this.applyAny(command, mark, v) : v
      }
      return out
    }
    const mark = spec.bareField ? files[spec.bareField] : undefined
    return mark ? this.applyAny(command, mark, value) : value
  }

  /**
   * Fetch each listed output this call named to the caller's path, and the
   * rest (a packed subflow's) relative to its bundle's directory.
   */
  async fetchOutputs(outputs: string[] | undefined): Promise<void> {
    const rest: string[] = []
    for (const entry of outputs ?? []) {
      const target = this.fetch.get(entry)
      if (target === undefined) rest.push(entry)
      else await fetchTo(this.transport, this.deviceBase, entry, target, this.signal)
    }
    if (this.bundleDir !== undefined) await fetchBundleOutputs(this.transport, this.deviceBase, this.bundleDir, rest, this.signal)
  }
}
