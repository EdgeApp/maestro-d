// Run with Node's TypeScript support after building npm/maestro-d.
import { createRequire } from 'node:module'
const { MaestroD } = createRequire(import.meta.url)('../npm/maestro-d/dist/cjs/index.js')

const device = process.env.TV_DEVICE_UDID
if (!device) throw new Error('Set TV_DEVICE_UDID to your test TV or simulator')
const appId = process.env.TV_PROBE_APP_ID ?? 'com.example.maestro.SwipeProbe'
const name = process.env.TV_DAEMON ?? 'tv-swipes'
let daemon = await MaestroD.connect({ name })
if (process.env.TV_COLD_START === '1') {
  await daemon.shutdown()
  await daemon.close()
  daemon = await MaestroD.connect({ name })
}
let tv
try {
  tv = await daemon.attach({
    device, platform: 'ios', artifacts: 'none',
    teamId: process.env.TV_TEAM_ID,
    wdaBundleId: process.env.TV_WDA_BUNDLE_ID
  })
  await tv.launchApp({ appId, stopApp: true })
  await tv.assertVisible({ text: 'Waiting for swipe', timeout: 30000 })
  for (const direction of ['DOWN', 'UP', 'LEFT', 'RIGHT']) {
    await tv.swipe({ direction, duration: 100 })
    await tv.assertVisible({ text: `Swipe ${direction}`, timeout: 5000 })
    console.log(`PASS ${direction}`)
  }
  await tv.swipe({ start: '50%,20%', end: '50%,80%', duration: 100 })
  await tv.assertVisible({ text: 'Swipe DOWN', timeout: 5000 })
  console.log('PASS coordinate swipe')
} finally {
  try { if (tv) await tv.stopApp({ appId }) }
  finally { await daemon.close() }
}
