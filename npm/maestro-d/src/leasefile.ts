// <home>/leases.json: the leases a client holds, shared by the CLI and
// this library (mirrors pkg/cli/lease.go).
//
//   {"leases": [{"hub": "http://hub.lan:7433", "device": "<id>@<hub>",
//                "id": "<lease>", "holder": "...", "expiresAt": "...", "ttlSec": 28800}]}
//
// Entries are keyed by the hub's address (its base URL, or unix:<daemon>
// for a hub reached over its unix socket) and the device id as the hub
// returns it. `maestro-d lease` in a shell records one there, and a script
// in the same home attaches with it.
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
  const cutoff = Date.now() - STALE_AFTER_MS
  return recs.filter((r) => {
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

/** The entry for one device on one hub. */
export function findLease(hub: string, device: string): LeaseRecord | undefined {
  return loadLeases().find((r) => r.hub === hub && r.device === device)
}

/** Record a lease, replacing the device's entry on that hub. */
export function recordLease(hub: string, lease: Lease): void {
  const recs = loadLeases().filter((r) => r.hub !== hub || r.device !== lease.device)
  const rec: LeaseRecord = { hub, device: lease.device, id: lease.id }
  if (lease.holder) rec.holder = lease.holder
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
