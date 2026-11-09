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
  | 'FORBIDDEN'
  | 'PEER_UNAVAILABLE'
  | 'HUB_NAME_TAKEN'
  | 'HUB_VERSION'
  | 'LEASE_REQUIRED'
  | 'LEASE_EXPIRED'

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
  /**
   * Files a hub call's output fields wrote, as `outputs/<path>` under the
   * report directory (`device.artifact(path)`). Absent for local calls; on
   * a hub the library fetches them back to the paths you gave.
   */
  outputs?: string[]
}

export interface StepsResult extends Envelope {
  device: string
  results: StepResult[]
  passed: number
  failed: number
  skipped: number
  /** Every result's `outputs`. */
  outputs?: string[]
}

/** One file of a session's report directory. */
export interface ArtifactInfo {
  /** Relative to the report directory, with / separators. */
  path: string
  size: number
  modTime: string
}

// ---------------------------------------------------------------------------
// Devices

/** A node of the normalized view hierarchy: the same shape for every platform. */
export interface HierarchyNode {
  type?: string
  id?: string
  text?: string
  bounds?: { x: number; y: number; width: number; height: number }
  /** Only present when notable: enabled false, checked for checkables, selected/focused true. */
  enabled?: boolean
  checked?: boolean
  selected?: boolean
  focused?: boolean
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
  /** 'maestro-d' for a simulator or AVD `createDevice()` made; only those delete without `force`. */
  createdBy?: string
  /** On a hub: the device's lease, when it is leased. */
  lease?: DeviceLease
  /** On a hub: true for a ready device that is neither leased nor attached. */
  available?: boolean
  /** On a hub: the longest lease window the device takes, in seconds. */
  maxSec?: number
}

/** The lease state a hub's device listings carry. */
export interface DeviceLease {
  holder?: string
  /** RFC 3339: the end of the lease's fixed window. */
  expiresAt: string
}

/** A hub's lease on one device (`POST /v1/leases`). */
export interface Lease {
  /** Sent as X-Maestro-Lease. */
  id: string
  device: string
  holder?: string
  /** RFC 3339: the end of the window. Using the lease never moves it; only `renew()` does. */
  expiresAt: string
  /** The window's length in seconds. */
  ttlSec: number
  /** The device's maximum window in seconds. */
  maxSec?: number
  /** True when the hub booted the device for this lease; it shuts it down again when the lease ends. */
  booted?: boolean
}

/**
 * Picks a device on a hub by its properties: platform and kind exactly,
 * name as the device's name or id, osVersion as the version or a prefix of
 * it ('26' matches '26.5'), hub as the hub's name.
 */
export interface LeaseSelector {
  platform?: string
  kind?: string
  name?: string
  osVersion?: string
  hub?: string
  /** Let a shut-down simulator or AVD be picked; the hub boots it for the lease. */
  boot?: boolean
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
  /**
   * Driver-specific extras: headed, browser, windowSize, userDataDir, androidTcpForward, capsFile, newCommandTimeout.
   * A hub refuses capsFile, driver appium and appiumUrl as FORBIDDEN: Appium attaches only over the unix socket.
   */
  extra?: Record<string, unknown>
}

export interface BootOptions {
  /**
   * ios | android. May be left out: an `avd:<name>` id means android, and a
   * name or UDID the daemon lists picks that device's platform.
   */
  platform?: string
  /** Simulator name or UDID, AVD name or `avd:<name>`, e.g. 'iPhone 16'. */
  name: string
  /** Seconds. */
  bootTimeout?: number
}

/** Options for `createDevice()`: a simulator (`type`, `runtime`) or an AVD (`image`, `profile`, …). */
export interface CreateDeviceOptions {
  platform: 'ios' | 'android' | string
  /** Name of the new simulator or AVD. */
  name: string
  /** iOS device type name or id (default: the newest iPhone the runtime supports). */
  type?: string
  /** iOS runtime, e.g. 'iOS-26-5' or '26.5' (default: the newest installed). */
  runtime?: string
  /** Android system image package (default: the newest installed google_apis image for the host's ABI). */
  image?: string
  /** Android hardware profile (default 'pixel_8'). */
  profile?: string
  /** Android data partition size (default '6G', at most '10G'). */
  dataSize?: string
  /** Download a missing Android image with sdkmanager; needs `acceptLicenses`. */
  installImage?: boolean
  acceptLicenses?: boolean
  /** Boot it once created (default true). */
  boot?: boolean
  /** Seconds. */
  bootTimeout?: number
}

/** One iOS simulator runtime in the catalog. */
export interface CatalogRuntime {
  id: string
  name: string
  version: string
  build?: string
  available: boolean
  deviceTypes?: string[]
}

/** One iPhone or iPad simulator type in the catalog. */
export interface CatalogDeviceType {
  id: string
  name: string
  productFamily: string
  minRuntimeVersion?: string
}

/** One Android system image in the catalog. */
export interface CatalogImage {
  /** e.g. 'system-images;android-35;google_apis;arm64-v8a' */
  id: string
  api: string
  tag: string
  abi: string
  description?: string
  installed: boolean
}

/** What `createDevice()` can make (`GET /v1/catalog`). */
export interface CatalogResult extends Envelope {
  ios?: { runtimes: CatalogRuntime[]; deviceTypes: CatalogDeviceType[] }
  android?: { images: CatalogImage[]; profiles: string[] }
  /** Why a platform's catalog is missing, when no platform was asked for. */
  errors?: Record<string, string>
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
  /** True when the TCP listener serves HTTPS. */
  tls?: boolean
  /** The hub's name when the daemon is a hub: it leases its devices. */
  hub?: string
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
