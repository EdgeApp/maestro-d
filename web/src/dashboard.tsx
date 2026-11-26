import { useEffect, useState } from 'react'
import { Devices, Icon, localTime } from './devices.js'
import { type Feed, LiveClient } from './live.js'

export function Dashboard() {
  const [feed, setFeed] = useState<Feed>({ status: 'offline' })
  const [now, setNow] = useState(() => new Date())
  const [narrow, setNarrow] = useState(() => window.matchMedia('(width < 900px)').matches)
  useEffect(() => {
    const client = new LiveClient(update => {
      setNow(new Date())
      setFeed(update)
    })
    client.connect()
    const timer = setInterval(() => setNow(new Date()), 30000)
    const media = window.matchMedia('(width < 900px)')
    const resize = () => setNarrow(media.matches)
    resize()
    media.addEventListener('change', resize)
    return () => {
      client.stop()
      clearInterval(timer)
      media.removeEventListener('change', resize)
    }
  }, [])
  const { status, snapshot } = feed
  const down = snapshot?.hubs.filter(hub => hub.state === 'down') ?? []
  const devices = snapshot?.devices ?? []
  const leaseCount = devices.filter(device => device.lease).length
  const upcomingCount = devices.reduce((sum, device) => sum + device.upcoming.length, 0)
  return <>
    <header className="topbar"><div className="topbar-content">
      <span className="brand"><span className="brand-mark"><Icon name="lan" /></span>maestro-d hubs</span>
      {snapshot && <span className="serving-hub"><Icon name="dns" />{snapshot.hub}</span>}
      <div className="connection" role="status" aria-live="polite">
        <span className={'signal ' + status}><i />{status}</span>
        {snapshot && <time dateTime={snapshot.generatedAt}>Last update {localTime(snapshot.generatedAt)}</time>}
      </div>
    </div></header>
    <main>
      <div className="page-heading"><h1>Shared devices</h1>
        {snapshot && <p className="totals">{devices.length} {devices.length === 1 ? 'device' : 'devices'} <b>·</b> {leaseCount} leased <b>·</b> {upcomingCount} upcoming</p>}
      </div>
      {status === 'reconnecting' && <aside className="m-banner m-banner--warning"><Icon name="refresh" />
        <div><strong>Reconnecting</strong>{snapshot && <p>Showing the last device update.</p>}</div>
      </aside>}
      {down.length > 0 && <aside className="m-banner m-banner--warning"><Icon name="warning" />
        <div><strong>{down.length === 1 ? 'Hub down' : 'Hubs down'}</strong><p>{down.map(hub => hub.name).join(', ')}</p></div>
      </aside>}
      {devices.length ? <Devices devices={devices} narrow={narrow} now={now} /> :
        <section className="m-card m-empty"><div className="m-empty__glyph"><Icon name="mobile" /></div>
          <h2 className="m-t-title3">{snapshot ? 'No devices shared on this mesh' : 'Waiting for device list'}</h2>
        </section>}
    </main>
  </>
}
