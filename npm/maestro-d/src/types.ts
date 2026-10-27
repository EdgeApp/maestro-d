// Wire types shared with the Go daemon (pkg/daemon/protocol.go). Field
// names are the JSON names the daemon emits; keep the two in sync.

// ---------------------------------------------------------------------------
// YAML building blocks referenced by the generated command params

/** An element selector, as in a flow file (`tapOn: {id: 'x', index: 1}`). */
export interface Selector {
  text?: string
  id?: string
  width?: number
  height?: number
  tolerance?: number
  enabled?: boolean
  selected?: boolean
  checked?: boolean
  focused?: boolean
  index?: string | number
  traits?: string
  css?: string
  placeholder?: string
  role?: string
  textContains?: string
  href?: string
  alt?: string
  title?: string
  name?: string
  testId?: string
  textRegex?: string
  nth?: number
  childOf?: Selector
  below?: Selector
  above?: Selector
  leftOf?: Selector
  rightOf?: Selector
  containsChild?: Selector
  containsDescendants?: Selector[]
  insideOf?: Selector
  point?: string
  optional?: boolean
  [key: string]: unknown
}

/** A condition (`when`, `while`, `assertCondition`). */
export interface Condition {
  visible?: string | Selector
  notVisible?: string | Selector
  true?: string
  platform?: string
  timeout?: number
  [key: string]: unknown
}

/**
 * One step exactly as a flow file lists it: a bare command name
 * (`'back'`) or a single-key object (`{tapOn: 'Login'}`,
 * `{tapOn: {id: 'x', optional: true}}`).
 */
export type Step = string | { [command: string]: unknown }

// ---------------------------------------------------------------------------
// Errors

/** Failure classes. Each maps to one CLI exit code and one HTTP status. */
export type ErrorCode =
  | 'COMMAND_FAILED'
  | 'USAGE'
  | 'DAEMON_UNAVAILABLE'
  | 'DEVICE_ERROR'
  | 'BUSY'
  | 'DEVICE_EXTERNAL'
  | 'DEVICE_IN_USE'
  | 'DEVICE_NOT_FOUND'
  | 'DEVICE_NOT_ATTACHED'
  | 'INTERRUPTED'
  | 'INTERNAL'

export interface ErrorBody {
  code: ErrorCode
  message: string
  /** Description of the step that failed, when one did. */
  step?: string
  /** Structured context: artifact paths, owning daemon for DEVICE_IN_USE, … */
  details?: Record<string, unknown>
}

/** Every JSON response carries this. */
export interface Envelope {
  ok: boolean
  exitCode: number
  error?: ErrorBody
}

// ---------------------------------------------------------------------------
// Step results

export interface Bounds {
  x: number
  y: number
  width: number
  height: number
}

export interface ElementInfo {
  id?: string
  text?: string
  bounds?: Bounds
  [key: string]: unknown
}

export interface CommandArtifacts {
  screenshotBefore?: string
  screenshotAfter?: string
  viewHierarchy?: string
  [key: string]: unknown
}

/** A nested step result (runFlow / repeat / retry children). */
export interface SubStep {
  id: string
  index: number
  type: string
  yaml: string
  status: 'passed' | 'failed' | 'skipped' | string
  duration: number
  error?: { message: string; [key: string]: unknown }
  artifacts?: CommandArtifacts
  subSteps?: SubStep[]
  [key: string]: unknown
}

/** The result of one command. */
export interface StepResult extends Envelope {
  device: string
  /** Human description, e.g. `tapOn: text="Login"`. */
  step: string
  /** Command name. */
  type: string
  /** Position in the device's session (0-based). */
  index: number
  durationMs: number
  message?: string
  skipped?: boolean
  optional?: boolean
  element?: ElementInfo
  /** Step-specific payload: extracted text, cookies, base64 screenshot, … */
  data?: unknown
  artifacts: CommandArtifacts
  subSteps?: SubStep[]
  reportDir?: string
}

export interface StepsResult extends Envelope {
  device: string
  results: StepResult[]
  passed: number
  failed: number
  skipped: number
}

// ---------------------------------------------------------------------------
// Devices

/** A node of the normalized view hierarchy: the same shape for every platform. */
export interface HierarchyNode {
  type?: string
  id?: string
  text?: string
  bounds?: { x: number; y: number; width: number; height: number }
  /** Present whenever the driver reports it, true or false. */
  enabled?: boolean
  /** Only present when notable: checked for checkables, selected/focused true. */
  checked?: boolean
  selected?: boolean
  focused?: boolean
  /** On the root only: the driver stopped at a node cap, so elements are missing. */
  truncated?: boolean
  children?: HierarchyNode[]
}

export interface DeviceInfo {
  id: string
  /** android | ios | web | mock */
  platform: string
  /** device | emulator | simulator */
  kind?: string
  name?: string
  osVersion?: string
  state?: string
  ready: boolean
  /** 'daemon' when this daemon booted it, 'external' otherwise. */
  bootedBy?: 'daemon' | 'external' | string
  bootedAt?: string
  attached: boolean
  attachedAt?: string
  appId?: string
  driver?: string
  reportDir?: string
  busy?: boolean
  /** Name of the other daemon holding the device, when listing conflicts. */
  owner?: string
}

/** Options for opening a driver on a device (`maestro-d attach`). */
export interface AttachOptions {
  /** android | ios | web | mock. Inferred from the device id when omitted. */
  platform?: string
  /** uiautomator2 | wda | appium | mock | cdp | playwright | … */
  driver?: string
  /** App to install (.apk / .app / .ipa). */
  appFile?: string
  /** App bundle id / package. */
  appId?: string
  /** URL for web sessions. */
  url?: string
  /** iOS development team id for signing WDA. */
  teamId?: string
  wdaBundleId?: string
  appiumUrl?: string
  /** Initial session variables. */
  env?: Record<string, string>
  /** Artifact capture: none | on-failure | all. */
  artifacts?: string
  noAppInstall?: boolean
  noDriverInstall?: boolean
  /** Seconds. */
  driverStartTimeout?: number
  /** Milliseconds. */
  waitForIdleTimeout?: number
  /** Milliseconds. */
  conditionTimeout?: number
  /** Milliseconds between steps. */
  stepDelay?: number
  typingFrequency?: number
  /** Hard per-command timeout in milliseconds (0 = none). */
  commandTimeout?: number
  /** Auto-dismiss system alerts between steps (default true). */
  alertMonitor?: boolean
  outputDir?: string
  /** Base directory for relative paths in steps. */
  flowDir?: string
  /** Driver-specific extras: headed, browser, windowSize, userDataDir, androidTcpForward, capsFile, newCommandTimeout. */
  extra?: Record<string, unknown>
}

export interface BootOptions {
  /** ios | android */
  platform: string
  /** Simulator / AVD name, e.g. 'iPhone 16'. */
  name: string
  /** Seconds. */
  bootTimeout?: number
}

// ---------------------------------------------------------------------------
// Daemon

export interface DaemonInfo {
  name: string
  pid: number
  apiVersion: string
  version: string
  socketPath: string
  httpAddr?: string
  startedAt: string
  startArgs?: string[]
  runDir: string
  reportsDir: string
  /** Seconds; 0 = never. */
  idleTimeout: number
}

export interface StatusResult extends Envelope {
  daemon: DaemonInfo
  devices: DeviceInfo[]
  idleShutdownAt?: string
}

export interface DaemonEvent {
  seq: number
  time: string
  /** step | nestedStep | device.attached | device.detached | device.booted | … */
  type: string
  device?: string
  depth?: number
  step?: string
  passed?: boolean
  durationMs?: number
  error?: string
  message?: string
}

// ---------------------------------------------------------------------------
// Platform info (GET /info)

export interface PlatformInfo {
  platform: string
  osVersion?: string
  deviceName?: string
  deviceId?: string
  isSimulator?: boolean
  screenWidth?: number
  screenHeight?: number
  [key: string]: unknown
}
