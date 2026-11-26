import { Fragment, type ReactNode } from 'react'
import type { Device } from './live.js'

const currentFields = ['Leased by', 'Description', 'Time']
const upcomingFields = ['Requested by', 'Description', 'Starts', 'Duration']
const clock = new Intl.DateTimeFormat(undefined, { hour: '2-digit', minute: '2-digit', hourCycle: 'h23' })
const weekday = new Intl.DateTimeFormat('en', { weekday: 'short' })

export function localTime(date: string) {
  return clock.format(new Date(date))
}

function duration(ms: number) {
  const minutes = Math.max(0, Math.ceil(ms / 60000))
  const hours = Math.floor(minutes / 60)
  const rest = minutes % 60
  return [hours && hours + ' h', (rest || !hours) && rest + ' m'].filter(Boolean).join(' ')
}

// The Mason subset includes these codepoints without their text ligatures.
const symbols = {
  android: 0xe859, apps: 0xe5c3, ios: 0xe027, mobile: 0xe0d4,
  lan: 0xeb2f, dns: 0xe875, refresh: 0xe5d5, warning: 0xe002,
} as const

export function Icon({ name }: { name: keyof typeof symbols }) {
  return <span className="m-icon" aria-hidden="true">{String.fromCodePoint(symbols[name])}</span>
}

function DeviceName({ device }: { device: Device }) {
  const icons: Record<string, keyof typeof symbols> = { android: 'android', web: 'apps', ios: 'ios', ipados: 'ios', tvos: 'ios' }
  const icon = icons[device.platform] ?? 'mobile'
  return <div className="identity">
    <span className={'device-icon platform-' + device.platform}><Icon name={icon} /></span>
    <span className="identity-text"><strong>{device.name}</strong>
      <code title={device.key}>{device.key.slice(0, 12)}</code>
    </span>
  </div>
}

function Computer({ device }: { device: Device }) {
  return <><span>{device.computer}</span>{device.hub !== device.computer && <small>{device.hub}</small>}</>
}

function currentValues(device: Device, now: Date): ReactNode[] {
  const lease = device.lease
  if (!lease) {
    if (device.state === 'busy') return [<span className="m-status m-status--warning">Busy</span>, '', '']
    if (device.state === 'offline') return [<span className="m-status m-status--neutral">Offline</span>, '', '']
    return [<span className="m-status m-status--success free"><i />Free</span>, '', '']
  }
  const left = duration(Date.parse(lease.expiresAt) - now.getTime())
  return [lease.holder, lease.description, <span className="lease-time">{left} left <small>· until {localTime(lease.expiresAt)}</small></span>]
}

function upcomingValues(device: Device, now: Date) {
  return [...device.upcoming].sort((a, b) => Date.parse(a.startsAt) - Date.parse(b.startsAt)).map(lease => {
    const start = new Date(lease.startsAt)
    const day = start.toDateString() === now.toDateString() ? 'Today' : weekday.format(start)
    return [lease.holder, lease.description, day + ' ' + localTime(lease.startsAt), duration(Date.parse(lease.expiresAt) - start.getTime())]
  })
}

function cells(fields: string[], values: ReactNode[]) {
  return fields.map((field, index) => <td key={field} className={index === 0 ? 'group-start' : undefined}>{values[index]}</td>)
}

function DeviceTable({ devices, now }: { devices: Device[]; now: Date }) {
  const count = devices.reduce((max, d) => Math.max(max, d.upcoming.length), 0)
  const upcomingGroups = Array.from({ length: count }, (_, i) => i)
  return <div className="m-table-wrap m-card table-card" tabIndex={0} role="region" aria-label="Scrollable shared devices">
    <table className="m-table" aria-label="Shared devices">
      <colgroup span={3} /><colgroup span={currentFields.length} />
      {upcomingGroups.map(i => <colgroup key={i} span={upcomingFields.length} />)}
      <thead>
        <tr>
          {['Device', 'Type', 'Computer'].map((field, i) => <th key={field} scope="col" rowSpan={2} className={i === 0 ? 'sticky-device' : undefined}>{field}</th>)}
          <th className="group-start" scope="colgroup" colSpan={3}>Current lease</th>
          {upcomingGroups.map(i => <th key={i} className="group-start" scope="colgroup" colSpan={4}>Upcoming {i + 1}</th>)}
        </tr>
        <tr>{[currentFields, ...upcomingGroups.map(() => upcomingFields)].map((group, i) => <Fragment key={i}>
          {group.map((field, index) => <th key={field} className={index === 0 ? 'group-start' : undefined} scope="col">{field}</th>)}
        </Fragment>)}</tr>
      </thead>
      <tbody>{devices.map(device => {
        const upcoming = upcomingValues(device, now)
        return <tr key={device.key} data-state={device.state}>
          <td className="sticky-device"><DeviceName device={device} /></td>
          <td>{device.type}</td><td><Computer device={device} /></td>
          {cells(currentFields, currentValues(device, now))}
          {upcomingGroups.map(i => <Fragment key={i}>{cells(upcomingFields, upcoming[i] ?? [])}</Fragment>)}
        </tr>
      })}</tbody>
    </table>
  </div>
}

function Fields({ labels, values }: { labels: string[]; values: ReactNode[] }) {
  return <dl>{labels.map((label, i) => <div key={label}><dt>{label}</dt><dd>{values[i]}</dd></div>)}</dl>
}

function DeviceCard({ device, now }: { device: Device; now: Date }) {
  return <article className="m-card device-card" data-state={device.state} aria-label={device.name}>
    <header><DeviceName device={device} /></header>
    <Fields labels={['Type', 'Computer']} values={[device.type, <Computer device={device} />]} />
    <h3>Current lease</h3>
    <Fields labels={currentFields} values={currentValues(device, now)} />
    {upcomingValues(device, now).map((group, i) => <Fragment key={i}><h3>Upcoming {i + 1}</h3><Fields labels={upcomingFields} values={group} /></Fragment>)}
  </article>
}

export function Devices({ devices, narrow, now }: { devices: Device[]; narrow: boolean; now: Date }) {
  const sorted = [...devices].sort((a, b) => a.computer.localeCompare(b.computer) || a.name.localeCompare(b.name))
  if (narrow) return <div className="cards">{sorted.map(device => <DeviceCard key={device.key} device={device} now={now} />)}</div>
  return <DeviceTable devices={sorted} now={now} />
}
