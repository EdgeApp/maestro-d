import assert from 'node:assert/strict'
import { mock } from 'node:test'
import { act, cleanup, render, screen, within } from '@testing-library/react'
import { Dashboard } from './dashboard.js'
import type { Device, Snapshot } from './live.js'

const start = new Date(2026, 10, 25, 12, 0)
const stamp = (minutes: number) => new Date(start.getTime() + minutes * 60000).toISOString()
function device(name: string, computer = 'Desk'): Device {
  return { key: 'key-' + name, name, computer, hub: 'Lab', state: 'ready',
    kind: 'phone', platform: 'ios', type: 'iPhone 16 · iOS 26', upcoming: [] }
}
function reservation(hour: number) {
  return { holder: 'Sam', description: 'Booking ' + hour, startsAt: stamp(60 * hour), expiresAt: stamp(60 * (hour + 2)) }
}
function snapshot(devices: Device[]): Snapshot {
  return { devices, hub: 'Lab', generatedAt: stamp(0), hubs: [{ name: 'Lab', machine: 'Desk', state: 'self' }] }
}
class Socket {
  static instances: Socket[] = []
  onclose: (() => void) | null = null
  onmessage: ((event: { data: string }) => void) | null = null
  constructor(readonly url: string) { Socket.instances.push(this) }
  close() { this.onclose?.() }
  message(data: Snapshot) { this.onmessage?.({ data: JSON.stringify(data) }) }
}
const socket = () => Socket.instances.at(-1)!
const push = (data: Snapshot) => act(() => socket().message(data))
const tick = (ms: number) => act(() => mock.timers.tick(ms))
const rows = () => Array.from(document.querySelectorAll<HTMLTableRowElement>('tbody tr'))

class Media extends EventTarget {
  matches = false
  set(matches: boolean) {
    this.matches = matches
    this.dispatchEvent(new Event('change'))
  }
}
const width = new Media()
const scheme = new Media()
const originalSocket = globalThis.WebSocket

describe('shared device dashboard', () => {
  beforeEach(() => {
    width.matches = false
    scheme.matches = false
    window.matchMedia = ((query: string) => query.includes('width') ? width : scheme) as unknown as typeof window.matchMedia
    globalThis.WebSocket = Socket as unknown as typeof WebSocket
    Socket.instances = []
    mock.timers.enable({ apis: ['Date', 'setTimeout', 'setInterval'], now: start.getTime() })
  })
  afterEach(() => {
    cleanup()
    mock.timers.reset()
    globalThis.WebSocket = originalSocket
  })

  it('W1 lists devices, types and computers from a snapshot', () => {
    render(<Dashboard />)
    const first = { ...device('Pixel'), platform: 'android', type: 'Pixel 8 · Android 16' }
    push(snapshot([first, device('iPad')]))
    screen.getByText(/^2 devices /)
    assert.equal(rows().length, 2)
    const row = rows().find(row => row.textContent.includes('Pixel'))!
    assert.match(row.cells[0].textContent, /Pixel.*key-Pixel/)
    assert.equal(row.querySelector('.m-icon')!.textContent, String.fromCodePoint(0xe859))
    assert.equal(row.cells[1].textContent, first.type)
    assert.equal(row.cells[2].textContent, 'DeskLab')
    push(snapshot([first]))
    screen.getByText(/^1 device /)
  })

  it('W2 shows free or occupied leases and preserves blank cells', () => {
    render(<Dashboard />)
    push(snapshot([device('Alpha'), { ...device('Beta'), lease: { holder: 'Alex', description: 'UI tests', expiresAt: stamp(80) } }]))
    const values = rows().map(row => Array.from(row.cells).slice(3).map(cell => cell.textContent))
    assert.deepEqual(values, [['Free', '', ''], ['Alex', 'UI tests', '1 h 20 m left · until 13:20']])
    push(snapshot([{ ...device('Alpha'), state: 'busy' }, { ...device('Beta'), state: 'offline' }]))
    assert.deepEqual(rows().map(row => row.cells[3].textContent), ['Busy', 'Offline'])
    act(() => width.set(true))
    screen.getByText('Busy')
    screen.getByText('Offline')
  })

  it('W3 grows and shrinks groups, padding shorter rows and ordering bookings', () => {
    render(<Dashboard />)
    const devices = [device('A'), device('B'), device('C')]
    push(snapshot(devices))
    assert.equal(screen.queryByText(/Upcoming 1/), null)
    devices[2].upcoming = [3, 1, 2].map(reservation)
    devices[1].upcoming = [reservation(5)]
    push(snapshot(devices))
    for (const n of [1, 2, 3]) screen.getByRole('columnheader', { name: 'Upcoming ' + n })
    assert.equal(rows()[0].cells.length, 18)
    assert.ok(Array.from(rows()[0].cells).slice(6).every(c => c.textContent === ''))
    assert.equal(rows()[1].cells[7].textContent, 'Booking 5')
    assert.ok(Array.from(rows()[1].cells).slice(10).every(c => c.textContent === ''))
    assert.deepEqual([7, 11, 15].map(i => rows()[2].cells[i].textContent), ['Booking 1', 'Booking 2', 'Booking 3'])
    devices[2].upcoming = []
    push(snapshot(devices))
    assert.equal(rows()[0].cells.length, 10)
    assert.equal(screen.queryByText('Upcoming 2'), null)
  })

  it('W4 updates the clock every 30 s and uses local day labels', () => {
    render(<Dashboard />)
    push(snapshot([{ ...device('Beta'), lease: { holder: 'T', description: '', expiresAt: stamp(1.5) }, upcoming: [reservation(3), reservation(21.5)] }]))
    screen.getByText('2 m left', { exact: false })
    screen.getByText('Today 15:00')
    screen.getByText('Thu 09:30')
    tick(30000)
    screen.getByText('1 m left', { exact: false })
    tick(15000)
    push(snapshot([{ ...device('New'), lease: { holder: 'T', description: '', expiresAt: stamp(30.75) } }]))
    screen.getByText('30 m left', { exact: false })
  })

  it('W5 distinguishes first connection, empty pool and named down hubs', () => {
    render(<Dashboard />)
    screen.getByText('Waiting for device list')
    const empty = snapshot([])
    empty.hubs.push({ name: 'West', machine: 'Mac', state: 'down' })
    push(empty)
    screen.getByText('No devices shared on this mesh')
    assert.equal(screen.queryByRole('table'), null)
    assert.match(screen.getByRole('complementary').textContent, /Hub downWest/)
  })

  it('W6 reconnects with capped backoff, retains data, resets after a snapshot and cancels retries on stop', () => {
    const view = render(<Dashboard />)
    assert.equal(socket().url, 'wss://hub.example:9400/ws')
    push(snapshot([device('First')]))
    screen.getByText('First')
    screen.getByText('connected')
    for (const [i, seconds] of [1, 2, 4, 8, 16, 30, 30].entries()) {
      act(() => socket().close())
      screen.getByText('First')
      assert.match(screen.getByRole('complementary').textContent, /Reconnecting/)
      tick(seconds * 1000 - 1)
      assert.equal(Socket.instances.length, i + 1)
      tick(1)
      assert.equal(Socket.instances.length, i + 2)
    }
    push(snapshot([device('Second')]))
    assert.equal(screen.queryByRole('complementary'), null)
    assert.equal(screen.queryByText('First'), null)
    act(() => socket().close())
    tick(1000)
    assert.equal(Socket.instances.length, 9)
    act(() => socket().close())
    view.unmount()
    tick(60000)
    assert.equal(Socket.instances.length, 9)
  })

  it('W7 responds to the 900 px breakpoint with cards containing every field', () => {
    render(<Dashboard />)
    const row = { ...device('Tablet', 'Lab'), lease: { holder: 'Morgan', description: 'Upgrade', expiresAt: stamp(125) },
      upcoming: [reservation(6), reservation(2)] }
    push(snapshot([row]))
    act(() => width.set(true))
    assert.equal(screen.queryByRole('table'), null)
    const card = within(screen.getByRole('article', { name: 'Tablet' }))
    for (const text of ['Tablet', 'key-Tablet', row.type, 'Computer', 'Lab', 'Current lease', 'Morgan', 'Upgrade', '2 h 5 m left', '· until 14:05', 'Today 14:00']) card.getByText(text)
    assert.deepEqual(card.getAllByRole('heading').map(h => h.textContent), ['Current lease', 'Upcoming 1', 'Upcoming 2'])
    assert.deepEqual(card.getAllByText(/^Booking/).map(e => e.textContent), ['Booking 2', 'Booking 6'])
    assert.equal(card.getAllByText('2 h').length, 2)
    act(() => width.set(false))
    screen.getByRole('table')
  })

  it('W8 sorts by computer then device, mutes offline rows and pins the device column', () => {
    render(<Dashboard />)
    push(snapshot([device('A', 'West'), device('C', 'East'), { ...device('B', 'East'), platform: 'web', state: 'offline' }]))
    assert.deepEqual(rows().map(r => r.querySelector('strong')!.textContent), ['B', 'C', 'A'])
    const style = (e: Element) => window.getComputedStyle(e)
    assert.equal(rows()[0].dataset.state, 'offline')
    assert.notEqual(style(rows()[0]).color, style(rows()[1]).color)
    assert.equal(style(rows()[0].cells[0]).position, 'sticky')
    assert.equal(style(screen.getByRole('columnheader', { name: 'Device' })).position, 'sticky')
  })

  it('W9 boots dark by default and follows light system appearance', async () => {
    await import('./theme-boot.js')
    assert.equal(document.documentElement.dataset.theme, 'dark')
    scheme.set(true)
    assert.equal(document.documentElement.dataset.theme, 'light')
  })

  it('W10 mounts the real entry point with scoped headers, group spans, focus retention and labelled live status', async () => {
    const host = document.createElement('div')
    host.id = 'root'
    document.body.append(host)
    await act(async () => { await import('./main.js') })
    const row = { ...device('Phone'), upcoming: [reservation(1)] }
    push(snapshot([row]))
    const region = screen.getByRole('region')
    region.focus()
    assert.equal(document.activeElement, region)
    push(snapshot([{ ...row, upcoming: [...row.upcoming, reservation(3)] }]))
    assert.equal(document.activeElement, region)
    const table = screen.getByRole('table')
    assert.deepEqual([...table.querySelectorAll('colgroup')].map(g => g.span), [3, 3, 4, 4])
    for (const th of table.querySelectorAll('th')) assert.equal(th.scope, th.colSpan > 1 ? 'colgroup' : 'col')
    assert.equal(screen.getByRole('status').getAttribute('aria-live'), 'polite')
    assert.match(screen.getByRole('status').textContent, /connectedLast update 12:00/)
    const { root } = await import('./main.js')
    act(() => root.unmount())
    host.remove()
  })
})
