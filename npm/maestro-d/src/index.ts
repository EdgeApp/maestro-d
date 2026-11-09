// maestro-d: run Maestro YAML commands one at a time from JavaScript.
//
//   import { MaestroD } from 'maestro-d'
//   const m = await MaestroD.attach({ device: '29271FDH200ABP', appId: 'co.edgesecure.app' })
//   await m.launchApp({ clearState: true })
//   await m.tapOn('Login')
//   await m.assertVisible('Welcome', { timeout: 10_000 })

export { MaestroD, hubURL, ENV_HUB, ENV_TOKEN, ENV_CA, DEFAULT_HUB_PORT } from './daemon.js'
export type { ConnectOptions, DeviceAttachOptions, ShutdownOptions } from './daemon.js'
export { MaestroDevice, buildValue } from './device.js'
export type { CallOptions, CommandMethod, CommandMethods, FlowOptions, RunOptions, Scalar } from './device.js'
export { MaestroError, isMaestroError, codeFromStatus, EXIT_CODES, HTTP_STATUS } from './errors.js'
export { binaryPath, PACKAGE_BY_TARGET } from './binary.js'
export { fetchBundleOutputs, pack, packFlow, upload, uploadBundle, uploadPath } from './pack.js'
export type { BundleManifest, FlowBundle, PackFlowOptions, PackUnit } from './pack.js'
export { resolveName, home, runDir, liveInfo, readInfo, ENV_NAME, ENV_HOME, ENV_DEVICE, ENV_BIN, DEFAULT_NAME } from './runfiles.js'
export { COMMANDS } from './generated/commands.js'
export type { CommandName, CommandParams, CommandSpec } from './generated/commands.js'
export * from './generated/commands.js'
export type * from './types.js'
