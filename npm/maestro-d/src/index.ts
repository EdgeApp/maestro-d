// maestro-d: run Maestro YAML commands one at a time from JavaScript.
//
//   import { MaestroD } from 'maestro-d'
//   const m = await MaestroD.attach({ device: 'FAKEPIXEL0002', appId: 'co.edgesecure.app' })
//   await m.launchApp({ clearState: true })
//   await m.tapOn('Login')
//   await m.assertVisible('Welcome', { timeout: 10_000 })

export { machineName, CLIENT_MACHINE_HEADER } from './transport.js'
export { MaestroD, DeviceFarmUtilities, hubURL, withQuery, ENV_HUB, ENV_TOKEN, ENV_CA, DEFAULT_HUB_PORT } from './daemon.js'
export type { ClaimOptions, ConnectOptions, DeviceAttachOptions, LeaseOptions, ReserveOptions, ShutdownOptions } from './daemon.js'
export { MaestroDevice, buildValue, supportsStatusBar } from './device.js'
export type { CallOptions, CommandMethod, CommandMethods, FlowOptions, RunOptions, Scalar, ScreenshotOptions } from './device.js'
export { MaestroError, isMaestroError, codeFromStatus, EXIT_CODES, HTTP_STATUS } from './errors.js'
export { binaryPath, hubBinary, PACKAGE_BY_TARGET } from './binary.js'
export { fetchBundleOutputs, pack, packFlow, upload, uploadBundle, uploadPath } from './pack.js'
export type { BundleManifest, FlowBundle, PackFlowOptions, PackUnit } from './pack.js'
export { resolveName, home, runDir, liveInfo, readInfo, ENV_NAME, ENV_HOME, ENV_DEVICE, ENV_BIN, DEFAULT_NAME } from './runfiles.js'
export { COMMANDS } from './generated/commands.js'
export type { CommandName, CommandParams, CommandSpec } from './generated/commands.js'
export * from './generated/commands.js'
export type * from './types.js'
