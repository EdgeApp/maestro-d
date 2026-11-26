import { expect, test } from '@playwright/test'
import { mkdir } from 'node:fs/promises'
import { hostname } from 'node:os'
import path from 'node:path'
import { createHub } from './hub.mjs'

test('live shared devices, reservations, reconnect, and peer health', async ({ browser }, testInfo) => {
  test.setTimeout(120_000)
  const artifactDir = process.env.MAESTRO_D_WEB_ARTIFACTS
  if (artifactDir) await mkdir(artifactDir, { recursive: true })
  const hub = createHub('table')
  const peer = createHub('table-peer', {}, hub)
  const context = await browser.newContext({
    ignoreHTTPSErrors: true,
    viewport: { width: 2200, height: 900 },
    deviceScaleFactor: 2,
    colorScheme: 'dark',
    ...(artifactDir && {
      recordVideo: { dir: testInfo.outputPath('recordings'), size: { width: 1760, height: 720 } }
    })
  })
  const page = await context.newPage()
  const photo = async (name: string) => {
    if (artifactDir)
      await page.screenshot({ path: path.join(artifactDir, name), fullPage: true, animations: 'disabled' })
  }
  try {
    hub.start()
    await page.goto(hub.web)
    const table = page.getByRole('table')
    const row = (name: string) => table.getByRole('row').filter({ hasText: name })
    const groups = table.getByRole('columnheader', { name: /^Upcoming \d+$/i })
    const pixel = row('Bench Pixel'),
      ipad = row('Lab iPad'),
      prototype = row('Prototype')
    await expect(pixel).toContainText('Pixel 8 · Android 16')
    await expect(ipad).toContainText('iPad Pro 11-inch (M4) · iPadOS 26.3')
    await expect(prototype).toContainText('iPhone99,1')
    await expect(pixel).toContainText(hostname())
    await expect(pixel).toContainText('Free')
    await expect(groups).toHaveCount(0)

    let loads = 0
    page.on('load', () => {
      loads++
    })
    await hub.request('POST', 'leases', {
      device: 'mock-1@table',
      holder: 'mobile-qa',
      description: 'Checkout regression',
      ttlSec: 1800
    })
    await expect(pixel).toContainText('mobile-qa', { timeout: 3000 })
    await expect(pixel).toContainText('Checkout regression')
    await expect(pixel).toContainText(/left/i)
    const now = Date.now()
    const reserve = async (device: string, offset: number, holder: string, description: string) => {
      const grant = await hub.request('POST', 'reservations', {
        device: device + '@table',
        holder,
        description,
        startAt: new Date(now + offset * 60000).toISOString(),
        clientTime: new Date().toISOString(),
        ttlSec: 900
      })
      return grant.lease.id as string
    }
    const last = await reserve('mock-2', 120, 'release-qa', 'Release signing')
    await reserve('mock-2', 60, 'app-team', 'Tablet layout')
    const middle = await reserve('mock-2', 90, 'test-team', 'Upgrade flow')
    await reserve('mock-1', 90, 'android-qa', 'Network checks')
    await expect(groups).toHaveCount(3)
    await expect(ipad).toContainText(/Tablet layout.*Upgrade flow.*Release signing/)
    await expect(pixel).toContainText('Network checks')
    await expect(prototype).toContainText('Free')
    const pixelCells = await pixel.getByRole('cell').allTextContents()
    const freeCells = await prototype.getByRole('cell').allTextContents()
    expect(pixelCells.slice(10)).toEqual(Array(8).fill(''))
    expect(freeCells.slice(6)).toEqual(Array(12).fill(''))
    await page.evaluate(() => document.fonts.ready)
    await photo('web-table-dark.png')
    const light = await browser.newContext({
      ignoreHTTPSErrors: true,
      viewport: { width: 2200, height: 900 },
      deviceScaleFactor: 2,
      colorScheme: 'light'
    })
    try {
      const lp = await light.newPage()
      await lp.goto(hub.web)
      await expect(lp.getByText('Tablet layout').first()).toBeVisible()
      await lp.evaluate(() => document.fonts.ready)
      if (artifactDir)
        await lp.screenshot({ path: path.join(artifactDir, 'web-table-light.png'), fullPage: true })
      await lp.setViewportSize({ width: 390, height: 844 })
      await expect(lp.getByRole('table')).toBeHidden()
      await expect(lp.getByText('Release signing', { exact: true }).last()).toBeVisible()
      if (artifactDir)
        await lp.screenshot({ path: path.join(artifactDir, 'web-table-mobile.png'), fullPage: true })
    } finally {
      await light.close()
    }
    for (const id of [last, middle]) await hub.request('DELETE', 'leases/' + encodeURIComponent(id))
    await expect(groups).toHaveCount(1)
    await expect(ipad).toContainText('Tablet layout')
    expect(loads).toBe(0)

    hub.stop()
    await expect(page.getByText(/reconnecting/i).first()).toBeVisible()
    await expect(pixel).toContainText('Checkout regression')
    await photo('web-reconnecting.png')
    hub.start()
    await expect(page.getByText(/reconnecting/i)).toHaveCount(0, { timeout: 20_000 })
    await expect(pixel).toContainText('Checkout regression')
    await expect(groups).toHaveCount(1)
    for (const id of ['mock-1', 'mock-2', 'mock-3']) hub.cli('devices', 'remove', id, '--end-leases')
    await expect(page.getByText('No devices shared on this mesh')).toBeVisible()
    await photo('web-empty.png')
    for (const id of ['mock-1', 'mock-2', 'mock-3']) hub.cli('devices', 'add', id)
    await expect(pixel).toBeVisible()
    peer.start(hub.address)
    await expect
      .poll(async () =>
        (await hub.request('GET', 'hub')).hub.peers.some((p) => p.name === peer.name && p.state === 'linked')
      )
      .toBe(true)
    peer.crash()
    await expect(page.locator('aside').filter({ hasText: 'Hub down' })).toContainText(peer.name, {
      timeout: 12_000
    })
    await expect(page.getByRole('status')).toContainText('connected')
    await photo('web-down-peer.png')
  } finally {
    const video = page.video()
    await context.close()
    try {
      if (artifactDir && video) await video.saveAs(path.join(artifactDir, 'web-live-update.webm'))
    } finally {
      try {
        peer.dispose()
      } finally {
        hub.dispose()
      }
    }
  }
})
