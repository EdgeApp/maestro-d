// <home>/leases.json: the leases a client holds, shared by the CLI and
// this library (mirrors pkg/cli/lease.go).
//
//   {"leases": [{"hub": "http://hub.lan:7433", "device": "<id>@<hub>", "id": "<lease>",
//                "holder": "...", "startsAt": "...", "expiresAt": "...", "ttlSec": 28800}]}
//
// Entries are keyed by the hub's address (its base URL, or unix:<daemon>
// for a hub reached over its unix socket) and the lease id; a device may
// have several (a lease now and reservations later), and the one used is
// the one that started last, else the reservation that starts first
// (currentLease). `maestro-d lease` or `maestro-d reserve` in a shell
// records one there, and a script in the same home attaches with it.
//
// Locking: the CLI holds an advisory flock on leases.json.lock from its
// read to its rename, which Node cannot take. This library writes the
// whole file to a temporary file and renames it over leases.json, so a
// reader never sees a torn file; but a CLI and a script that update the
// file at the same instant can lose one of the two updates. Cross-process
// safety is the CLI's: lease from the shell (or one process) when several
// processes share a home and lease at once.

import * as fs from 'node:fs'
import * as path from 'node:path'

import { home } from './runfiles.js'
import type { Lease } from './types.js'

/** One entry of leases.json. */
export interface LeaseRecord {
  hub: string
  device: string
  id: string
  holder?: string
  /** The window's start: later than now for a reservation. A record without it has started. */
  startsAt?: string
  expiresAt?: string
  ttlSec?: number
}

/** Entries whose recorded expiry is this far past are dropped (as the CLI does). */
const STALE_AFTER_MS = 7 * 24 * 3600 * 1000

export function leasesPath(): string {
  return path.join(home(), 'leases.json')
}

/** Every entry on file; an unreadable file reads as empty. */
export function loadLeases(): LeaseRecord[] {
  let text: string
  try {
    text = fs.readFileSync(leasesPath(), 'utf8')
  } catch {
    return []
  }
  let recs: LeaseRecord[]
  try {
    recs = (JSON.parse(text) as { leases?: LeaseRecord[] }).leases ?? []
  } catch {
    return []
  }
  if (!Array.isArray(recs)) return []
  const cutoff = Date.now() - STALE_AFTER_MS
  return recs.filter((r) => {
    if (!r || typeof r.device !== 'string' || typeof r.id !== 'string' || typeof r.hub !== 'string') return false
    const t = r.expiresAt ? Date.parse(r.expiresAt) : NaN
    return Number.isNaN(t) || t >= cutoff
  })
}

function saveLeases(recs: LeaseRecord[]): void {
  fs.mkdirSync(home(), { recursive: true, mode: 0o700 })
  recs.sort((a, b) => (a.hub !== b.hub ? (a.hub < b.hub ? -1 : 1) : a.device < b.device ? -1 : a.device > b.device ? 1 : 0))
  const tmp = `${leasesPath()}.tmp.${process.pid}`
  fs.writeFileSync(tmp, JSON.stringify({ leases: recs }, null, 2) + '\n', { mode: 0o600 })
  fs.renameSync(tmp, leasesPath())
}

/** Whether r's window has started by `now`, and its start (0 when none is recorded: started, and earliest). */
function startOf(r: LeaseRecord, now: number): { started: boolean; start: number } {
  const t = r.startsAt ? Date.parse(r.startsAt) : NaN
  if (Number.isNaN(t)) return { started: true, start: 0 }
  return { started: t <= now, start: t }
}

/**
 * The record to use for `device` among `recs` (the CLI's currentLease): of
 * the ones that have started, the one that started last; with none
 * started, the reservation that starts first. The recorded expiresAt is
 * not weighed: an ended lease is still sent, the hub answers LEASE_EXPIRED
 * and the record is dropped.
 */
export function currentLease(recs: LeaseRecord[], device: string, now = Date.now()): LeaseRecord | undefined {
  let best: LeaseRecord | undefined
  let bestStart = 0
  let bestStarted = false
  for (const r of recs) {
    if (r.device !== device) continue
    const { started, start } = startOf(r, now)
    let take: boolean
    if (best === undefined) take = true
    else if (started !== bestStarted) take = started
    else if (started) take = start > bestStart
    else take = start < bestStart
    if (take) {
      best = r
      bestStart = start
      bestStarted = started
    }
  }
  return best
}

/** The entry to use for one device on one hub (currentLease). */
export function findLease(hub: string, device: string): LeaseRecord | undefined {
  return currentLease(
    loadLeases().filter((r) => r.hub === hub),
    device,
  )
}

/**
 * Record a lease, replacing the entry of the same lease id on that hub; the
 * device's reservations stay. A lease that has started also replaces the
 * device's other started entries: no two leases on a device overlap, so
 * those have ended.
 */
export function recordLease(hub: string, lease: Lease): void {
  const now = Date.now()
  const started = startOf({ hub, device: lease.device, id: lease.id, startsAt: lease.startsAt }, now).started
  const recs = loadLeases().filter(
    (r) => !(r.hub === hub && (r.id === lease.id || (started && r.device === lease.device && startOf(r, now).started))),
  )
  const rec: LeaseRecord = { hub, device: lease.device, id: lease.id }
  if (lease.holder) rec.holder = lease.holder
  if (lease.startsAt) rec.startsAt = lease.startsAt
  if (lease.expiresAt) rec.expiresAt = lease.expiresAt
  if (lease.ttlSec) rec.ttlSec = lease.ttlSec
  recs.push(rec)
  saveLeases(recs)
}

/** Remove the entry for lease id on one hub. */
export function dropLease(hub: string, id: string): void {
  const recs = loadLeases()
  const kept = recs.filter((r) => r.hub !== hub || r.id !== id)
  if (kept.length !== recs.length) saveLeases(kept)
}
