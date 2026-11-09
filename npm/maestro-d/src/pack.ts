// Files for a hub: packing, uploading, and the file fields of a command.
//
// A hub is another machine, so a command's input files (the fields the
// generated table marks `path`) are packed by the maestro-d binary
// (`maestro-d pack --json`, the same packing the CLI does), uploaded when
// the hub lacks them, and sent as `blob:sha256:<hex>/<basename>` refs. Its
// output fields (`output`) are sent as names of the client's own,
// `<first 8 hex of sha256(caller path)>/<basename>`, and the files the
// result lists in `outputs` are fetched back to the caller's paths.

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
  const bin = binaryPath(opts.bin)
  return new Promise((resolve, reject) => {
    execFile(bin, ['pack', '--json', ...paths], { signal: opts.signal, maxBuffer: 16 << 20 }, (err, stdout, stderr) => {
      let parsed: { ok?: boolean; units?: PackUnit[]; error?: { code: string; message: string } } | undefined
      try {
        parsed = JSON.parse(stdout) as typeof parsed
      } catch {
        parsed = undefined
      }
      if (parsed?.ok && parsed.units) return resolve(parsed.units)
      if (parsed?.error) return reject(new MaestroError(parsed.error as never))
      reject(MaestroError.of('USAGE', `maestro-d pack: ${String(stderr || err?.message || 'failed').trim()}`, { cause: err }))
    })
  })
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

  constructor(
    private readonly transport: Transport,
    private readonly deviceBase: string,
    private readonly cwd: string,
    private readonly signal?: AbortSignal,
  ) {}

  private output(v: string): string {
    const target = path.resolve(this.cwd, v)
    const name = `${hash8(target)}/${path.basename(target)}`
    this.fetch.set(`outputs/${name}`, target)
    // takeScreenshot and an assertScreenshot baseline add .png.
    if (path.extname(name) === '') this.fetch.set(`outputs/${name}.png`, target + '.png')
    return name
  }

  private async apply(command: string, mark: FileMark, v: string): Promise<string> {
    if (v === '' || isRef(v) || (mark.suffix !== undefined && !v.endsWith(mark.suffix))) return v
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

  /** Fetch each listed output this call named to the caller's path. */
  async fetchOutputs(outputs: string[] | undefined): Promise<void> {
    for (const entry of outputs ?? []) {
      const target = this.fetch.get(entry)
      if (target === undefined) continue
      const data = await this.transport.bytes(`${this.deviceBase}/artifacts/${entry.split('/').map(encodeURIComponent).join('/')}`, {
        signal: this.signal,
      })
      fs.mkdirSync(path.dirname(target), { recursive: true })
      const tmp = path.join(path.dirname(target), `.${path.basename(target)}.${process.pid}.tmp`)
      fs.writeFileSync(tmp, data)
      fs.renameSync(tmp, target)
    }
  }
}
