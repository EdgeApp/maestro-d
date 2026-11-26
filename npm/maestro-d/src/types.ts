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
  | 'HUB_RUNNING'
  | 'HUB_NAME_TAKEN'
  | 'HUB_VERSION'
  | 'LEASE_REQUIRED'
  | 'LEASE_EXPIRED'
  | 'LEASE_NOT_STARTED'
  | 'CAPTURE_UNAVAILABLE'
  | 'UNSUPPORTED'

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
  /** On a hub, qualified: `<localId>@<hub>`. */
  id: string
  /** On a hub: the hub that owns the device. */
  hub?: string
  /** On a hub: the device's id on its own hub's machine. */
  localId?: string
  /** android | ios | web | mock | macos (the computer device, `mac`) */
  platform: string
  /** device | emulator | simulator | tv | computer */
  kind?: string
  /** An Android phone's user-set name (else its model), an emulator's AVD name, an iOS device's or simulator's name. */
  name?: string
  /** Hardware model: `ro.product.model` on Android (`Pixel 8`), the product type on iOS (`iPhone14,2`). */
  model?: string
  osVersion?: string
  state?: string
  ready: boolean
  /** 'daemon' when this daemon booted it, 'external' otherwise. */
  bootedBy?: 'daemon' | 'external' | string
  bootedAt?: string
  attached: boolean
  /** Present on attach responses when the hub reports whether the existing driver was reused. */
  attachReused?: boolean
  attachedAt?: string
  appId?: string
  /** The attached app's version and build number, as the driver read them. */
  appVersion?: string
  appBuild?: string
  driver?: string
  reportDir?: string
  busy?: boolean
  /** Name of the other daemon holding the device, when listing conflicts. */
  owner?: string
  /** 'maestro-d' for a simulator or AVD `createDevice()` made; only those delete without `force`. */
  createdBy?: string
  /** On a hub: the device's lease, when it is leased. */
  lease?: DeviceLease
  /** On a hub: the device's reservations that have not started, by start (holder and window, no lease id). */
  reservations?: DeviceLease[]
  /** On a hub: true for a ready device that is neither leased nor attached. */
  available?: boolean
  /** On a hub: the longest lease window the device takes, in seconds. */
  maxSec?: number
  /** The host name of the machine the device is attached to (for a cloud device, the machine holding its session). */
  machine?: string
  /** On a hub: the owning hub's advertised address, then that machine's non-loopback IPs. */
  addresses?: string[]
  /** True when the device's host has a traffic shaper configured for it (setTrafficShape). */
  trafficShaping?: boolean
  /** Where the device is, relative to this computer: local, mesh or cloud. */
  location?: DeviceLocation
  /** The cloud provider that holds the device (`devicefarm`); absent otherwise. */
  provider?: string
  /** A cloud device's provider session (a Device Farm session ARN). */
  providerRef?: string
  /** The session's page in the provider's console. */
  dashboardUrl?: string
  /** RFC 3339: when the provider ends the session. */
  expiresAt?: string
  /** Seconds without a request before the cloud device is released. */
  idleReleaseSec?: number
  /** RFC 3339: when it is released if no request comes. */
  idleReleaseAt?: string
  /** Why a cloud device's session ended (`state: 'ended'`). */
  endedReason?: string
  /** The device time a released cloud session used. */
  deviceMinutes?: DeviceMinutes
  formFactor?: string
  manufacturer?: string
  /** The Appium major version a cloud device's provider runs (`2` or `3`). */
  appiumVersion?: string
  /** Free text the device's host gives it in its config.json (`hubDevices[].description`). */
  description?: string
}

/** The device time a cloud session used. */
export interface DeviceMinutes {
  metered: number
  total: number
  /**
   * True until the provider settles its figure: then the minutes run from
   * the session's creation to its release, and the billed figure is higher,
   * because the provider also bills the cleanup after the release.
   */
  estimated?: boolean
}

/** What a cloud device launch asks for. */
export interface CloudSpec {
  /** Default `devicefarm`. */
  provider?: string
  platform: 'ios' | 'android' | string
  /** The model: exact name, catalog id or a regular expression. */
  name?: string
  osVersion?: string
  osVersionMin?: string
  osVersionMax?: string
  /**
   * An .apk or .ipa on the daemon's machine (a relative path is resolved
   * against this process's directory), a `blob:` ref, or an upload ARN.
   */
  app?: string
  /** At most 3 more apps to install, as `app`. */
  auxiliaryApps?: string[]
  /** The app's id, the default for attach. */
  appId?: string
  /** '2' or '3' (default '3' on iOS 27 and later). */
  appiumVersion?: string
  /** Merged into the WebDriver session's capabilities. */
  caps?: Record<string, unknown>
  /** Seconds without a request before release: default 600, 60–9000. */
  idleReleaseSec?: number
  /** Seconds to wait for the provider's session (default 900). */
  bootTimeoutSec?: number
}

/** Where a device is, relative to the caller's computer. */
export type DeviceLocation = 'local' | 'mesh' | 'cloud'

/** Filters for `devices()` (`GET /v1/devices`). */
export interface DeviceQuery {
  /** Keep only these locations. */
  location?: DeviceLocation | DeviceLocation[]
  /** The cloud provider (`devicefarm`). */
  provider?: string
  platform?: string
  /** device | emulator | simulator | tv | cloud */
  kind?: string
  /** A case-insensitive regular expression matched against name and id. */
  name?: string
  /** Inclusive OS version bounds; max compares at its own precision (17 admits 17.5). */
  osVersionMin?: string
  osVersionMax?: string
  /** Only devices a lease could take now. */
  available?: boolean
}

/** Filters for `catalog()` (`GET /v1/catalog`). */
export interface CatalogQuery extends Omit<DeviceQuery, 'kind'> {
  /** On a hub: the hub whose machine (or cloud credentials) answers. */
  hub?: string
}

/** A model a cloud provider can launch. */
export interface CloudModel {
  /** `df:<platform>:<slug>:<os>` */
  id: string
  arn: string
  platform: string
  name: string
  osVersion: string
  formFactor?: string
  manufacturer?: string
  /** HIGHLY_AVAILABLE | AVAILABLE | BUSY | TEMPORARY_NOT_AVAILABLE */
  availability?: string
  available: boolean
}

/** One cloud provider's launchable models. */
export interface CloudCatalog {
  provider: string
  /** The hub that launches them (absent on a plain daemon). */
  hub?: string
  models: CloudModel[]
}

/** An app uploaded to a cloud provider. */
export interface AppRef {
  /** The provider's upload reference (an ARN). */
  appRef: string
  name: string
  type?: string
  status?: string
  created?: string
  /** True when an identical earlier upload was reused. */
  reused?: boolean
}

/** `md.devicefarm.account()`. */
export interface ProviderAccount extends Envelope {
  provider: string
  hub?: string
  /** trialMinutes {total, remaining}, unmeteredDevices, … */
  account: Record<string, unknown>
}

/** `md.devicefarm.apps()`. */
export interface ProviderApps extends Envelope {
  provider: string
  hub?: string
  apps: AppRef[]
}

/** `md.devicefarm.upload()`. */
export interface ProviderUpload extends Envelope, AppRef {
  provider: string
  hub?: string
}

/** One provider session. */
export interface ProviderSession {
  /** The device id maestro-d gave it. */
  name: string
  ref: string
  status: string
  result?: string
  /** The device model, e.g. "Google Pixel 8". */
  model?: string
  created?: string
  deviceMinutes?: DeviceMinutes
}

/** `md.devicefarm.sessions()`. */
export interface ProviderSessions extends Envelope {
  provider: string
  hub?: string
  sessions: ProviderSession[]
}

/** A cloud provider a daemon or hub launches devices on. */
export interface CloudStatus {
  provider: string
  /** On a hub: how many cloud devices it holds at most. */
  maxDevices?: number
  /** How many of the provider's devices it holds now. */
  held: number
}

/** One entry from the hub's local device configuration. */
export interface HubConfigDevice {
  id: string
  description?: string
  trafficShaping: boolean
  /** Whether the hub currently discovers this configured device. */
  listed: boolean
  lease?: DeviceLease
  reservations?: DeviceLease[]
}

/** The local configuration file and its devices, in file order. */
export interface HubDevicesResult extends Envelope {
  path: string
  /** A malformed hand edit leaves the last good device list in place. */
  configError?: string
  devices: HubConfigDevice[]
}

/** A hub and its peers (`GET /v1/hub`, `md.hubStatus()`). */
export interface HubStatus {
  name: string
  version: string
  apiVersion: string
  /** The --advertise address, when set. */
  addr?: string
  listen?: string
  tls?: boolean
  /** False with --no-mdns. */
  mdns: boolean
  peers: HubPeer[]
  /** The cloud providers this hub launches devices on. */
  cloud?: CloudStatus[]
  /** How far ahead this hub's reservations may start, in seconds (--reservation-lead). */
  reservationLeadSec?: number
}

/** One peer of a hub. */
export interface HubPeer {
  name?: string
  addr?: string
  /** linked | down | stopped | connecting | refused */
  state: string
  /** outbound (this hub dialed) | inbound */
  direction?: string
  rttMs?: number
  /** How many devices a linked peer shares. */
  devices?: number
  since?: string
  error?: string
  /** The cloud providers a linked peer launches devices on. */
  cloud?: string[]
}

/** The lease state a hub's device listings carry. */
export interface DeviceLease {
  holder?: string
  description?: string
  /** RFC 3339 start of the grant or reservation. Older hubs omit the active start. */
  startsAt?: string
  /** RFC 3339: the end of the lease's fixed window. */
  expiresAt: string
}

/** A hub's lease on one device (`POST /v1/leases`). */
export interface Lease {
  /** The hub restored this active grant and awaits its holder’s next attach. */
  awaitingAttach?: boolean
  /** Sent as X-Maestro-Lease. */
  id: string
  device: string
  holder?: string
  description?: string
  /** RFC 3339: the start of the window (the grant, for a lease taken now). A renew leaves it. */
  startsAt?: string
  /** RFC 3339: the end of the window. Using the lease never moves it; only `renew()` does. */
  expiresAt: string
  /** 'scheduled' until the window starts (a reservation), then 'active'. */
  state?: 'scheduled' | 'active'
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
  /** 'cloud' launches a cloud device for the lease (name is then the model). */
  location?: 'cloud'
  /** The cloud provider (default devicefarm). */
  provider?: string
  /** A cloud lease's OS range, at max's own precision. */
  osVersionMin?: string
  osVersionMax?: string
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
  /** Report directory; one you name is never deleted by the daemon. */
  outputDir?: string
  /**
   * Delete the daemon-owned report directory at detach. By default it is
   * kept, and the daemon's housekeeping sweep deletes it after 3 idle days.
   */
  discardReports?: boolean
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
  /** Required for a simulator or AVD; a cloud device takes `cloud.platform`. */
  platform?: 'ios' | 'android' | string
  /** Name of the new simulator or AVD (required for one). */
  name?: string
  /** Launch a cloud device instead (direct mode, on this computer's daemon). */
  cloud?: CloudSpec
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
  /** With `location: 'cloud'`: each cloud provider's launchable models. */
  cloud?: CloudCatalog[]
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
  /** The cloud providers this daemon launches devices on. */
  cloud?: CloudStatus[]
}

export interface DaemonEvent {
  seq: number
  time: string
  /**
   * step | nestedStep | device.attached | device.detached | device.booted |
   * device.capture (message capturing | unavailable | lost, error the reason) | …
   */
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
// Screen streams (GET /stream)

export interface StreamOptions {
  /** Requested frame rate, 3 to 60 (default 15): a ceiling on Android, advisory on iOS. */
  fps?: number
  /** Include the device's audio when it has any (default false). */
  audio?: boolean
  /** Ends the stream: `body` then closes cleanly. */
  signal?: AbortSignal
}

/** A device's live screen: fragmented MP4 (an init segment, then one fragment per frame). */
export interface VideoStream {
  /** The running frame rate, or 'device' when the device picks it (iOS). */
  fps: number | 'device'
  /** Whether the stream carries an audio track. */
  audio: boolean
  /** The tracks' codecs, as MediaSource wants them: `avc3.640028`, `avc3.640028,mp4a.40.2`, `…,ipcm`, `…,fpcm`. */
  codecs: string
  body: ReadableStream<Uint8Array>
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
