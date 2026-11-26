export interface LeaseWindow {
  holder: string
  description: string
  startsAt?: string
  expiresAt: string
}

export interface Device {
  key: string
  name: string
  type: string
  platform: string
  kind: string
  computer: string
  hub: string
  state: string
  lease?: LeaseWindow
  upcoming: (LeaseWindow & { startsAt: string })[]
}

export interface Snapshot {
  hub: string
  generatedAt: string
  hubs: { name: string; machine: string; state: string }[]
  devices: Device[]
}

export interface Feed {
  status: 'connected' | 'reconnecting' | 'offline'
  snapshot?: Snapshot
}

export class LiveClient {
  private socket?: WebSocket
  private retry?: ReturnType<typeof setTimeout>
  private delay = 1000
  private snapshot?: Snapshot

  constructor(private readonly update: (feed: Feed) => void) {}

  connect() {
    const socket = new WebSocket('wss://' + window.location.host + '/ws')
    this.socket = socket
    socket.onmessage = event => {
      this.snapshot = JSON.parse(event.data) as Snapshot
      this.delay = 1000
      this.update({ status: 'connected', snapshot: this.snapshot })
    }
    socket.onclose = () => {
      this.update({ status: 'reconnecting', snapshot: this.snapshot })
      this.retry = setTimeout(() => this.connect(), this.delay)
      this.delay = Math.min(30000, this.delay * 2)
    }
  }

  stop() {
    clearTimeout(this.retry)
    if (this.socket) {
      this.socket.onclose = null
      this.socket.onmessage = null
      this.socket.close()
    }
  }
}
