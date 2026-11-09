import type { ErrorBody, ErrorCode, StepResult, StepsResult } from './types.js'

/** CLI exit code for each failure class (mirrors pkg/daemon Code.ExitCode). */
export const EXIT_CODES: Record<ErrorCode, number> = {
  COMMAND_FAILED: 1,
  USAGE: 2,
  DEVICE_NOT_ATTACHED: 2,
  FORBIDDEN: 2,
  DAEMON_UNAVAILABLE: 3,
  PEER_UNAVAILABLE: 3,
  DEVICE_ERROR: 4,
  DEVICE_NOT_FOUND: 4,
  INTERNAL: 4,
  BUSY: 5,
  DEVICE_EXTERNAL: 5,
  DEVICE_IN_USE: 5,
  HUB_NAME_TAKEN: 5,
  HUB_VERSION: 5,
  LEASE_REQUIRED: 5,
  LEASE_EXPIRED: 5,
  INTERRUPTED: 130,
}

/** HTTP status for each failure class (mirrors Code.HTTPStatus). */
export const HTTP_STATUS: Record<ErrorCode, number> = {
  COMMAND_FAILED: 422,
  USAGE: 400,
  DAEMON_UNAVAILABLE: 503,
  PEER_UNAVAILABLE: 503,
  DEVICE_ERROR: 502,
  FORBIDDEN: 403,
  BUSY: 409,
  DEVICE_EXTERNAL: 409,
  DEVICE_IN_USE: 409,
  HUB_NAME_TAKEN: 409,
  HUB_VERSION: 409,
  LEASE_REQUIRED: 409,
  LEASE_EXPIRED: 409,
  DEVICE_NOT_FOUND: 404,
  DEVICE_NOT_ATTACHED: 404,
  INTERRUPTED: 499,
  INTERNAL: 500,
}

/**
 * The failure class for an HTTP status whose body carries none (mirrors
 * CodeFromHTTPStatus): a proxy's error page, a 401 from the token check.
 */
export function codeFromStatus(status: number): ErrorCode {
  switch (status) {
    case 422:
      return 'COMMAND_FAILED'
    case 400:
    case 401:
      return 'USAGE'
    case 403:
      return 'FORBIDDEN'
    case 503:
      return 'DAEMON_UNAVAILABLE'
    case 502:
      return 'DEVICE_ERROR'
    case 409:
      return 'BUSY'
    case 404:
      return 'DEVICE_NOT_FOUND'
    default:
      return 'INTERNAL'
  }
}

/**
 * The one error type the library rejects with. `code` is the failure class,
 * `exitCode`/`httpStatus` are what the CLI and REST API report for it, and
 * `result` is the step result when a command ran and failed (artifacts,
 * sub-steps, report directory).
 */
export class MaestroError extends Error {
  readonly code: ErrorCode
  readonly exitCode: number
  readonly httpStatus: number
  readonly step?: string
  readonly details?: Record<string, unknown>
  /** The failed command's result, when the daemon produced one. */
  readonly result?: StepResult | StepsResult
  /** The daemon's message without the step/code decoration of `message`. */
  readonly reason: string

  constructor(body: ErrorBody, options: { cause?: unknown; result?: StepResult | StepsResult } = {}) {
    super(body.step ? `${body.step}: ${body.message} (${body.code})` : `${body.message} (${body.code})`, {
      cause: options.cause,
    })
    this.name = 'MaestroError'
    this.reason = body.message
    this.code = body.code
    this.exitCode = EXIT_CODES[body.code] ?? 4
    this.httpStatus = HTTP_STATUS[body.code] ?? 500
    if (body.step !== undefined) this.step = body.step
    if (body.details !== undefined) this.details = body.details
    if (options.result !== undefined) this.result = options.result
  }

  /** Build an error from a code and message. */
  static of(code: ErrorCode, message: string, options: { cause?: unknown; details?: Record<string, unknown> } = {}): MaestroError {
    const body: ErrorBody = { code, message }
    if (options.details !== undefined) body.details = options.details
    return new MaestroError(body, { cause: options.cause })
  }

  /** Decode a failure response body, falling back on the HTTP status. */
  static fromResponse(status: number, body: string, result?: StepResult | StepsResult): MaestroError {
    try {
      const parsed = JSON.parse(body) as { error?: ErrorBody }
      if (parsed && parsed.error && parsed.error.code) {
        return new MaestroError(parsed.error, { result })
      }
    } catch {
      // not JSON
    }
    let msg = body.trim()
    if (msg === '') msg = `HTTP ${status}`
    if (msg.length > 200) msg = msg.slice(0, 200) + '…'
    return MaestroError.of(codeFromStatus(status), `HTTP ${status}: ${msg}`)
  }

  /** The same shape the CLI prints and the REST API returns. */
  toJSON(): { ok: false; exitCode: number; error: ErrorBody } {
    const error: ErrorBody = { code: this.code, message: this.reason }
    if (this.step !== undefined) error.step = this.step
    if (this.details !== undefined) error.details = this.details
    return { ok: false, exitCode: this.exitCode, error }
  }
}

/** True when `err` is a MaestroError with the given code. */
export function isMaestroError(err: unknown, code?: ErrorCode): err is MaestroError {
  return err instanceof MaestroError && (code === undefined || err.code === code)
}
