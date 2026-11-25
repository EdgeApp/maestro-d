package daemon

// cloudledger.go: <RunRoot>/cloud.json, every cloud session a daemon of this
// home has created and not yet released. Run-file cleanup (RemoveRunFiles,
// LiveInfo, EnsureDaemon) and the registry's devices.json never touch it,
// so a crashed daemon's sessions stay findable: `device stop --orphans`
// releases a dead daemon's entries, and a daemon releases its own name's
// leftovers when it starts again.

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// CloudEntry is one cloud session in the ledger.
type CloudEntry struct {
	// Daemon and PID are the daemon that created the session.
	Daemon string `json:"daemon"`
	// Hub selects the singleton registry. Older records belong to named daemons.
	Hub bool `json:"hub,omitempty"`
	PID int  `json:"pid"`
	// ID is the device id the daemon gave it.
	ID       string `json:"id"`
	Provider string `json:"provider"`
	// ProviderRef is the provider's session reference (a Device Farm ARN).
	ProviderRef string `json:"providerRef"`
	StartedAt   string `json:"startedAt"`
}

// ledgerLockWait is how long a ledger write waits for another holder.
var ledgerLockWait = 10 * time.Second

// ledgerMu serialises this process's ledger writes. The file lock serialises
// processes, but guards nothing on Windows, where one daemon's concurrent
// launches and releases would otherwise lose each other's entries.
var ledgerMu sync.Mutex

// ledgerLock takes the ledger's file lock (a seam for tests).
var ledgerLock = lockLedger

// LedgerPath is the cloud ledger of this home.
func LedgerPath() string { return filepath.Join(RunRoot(), "cloud.json") }

func ledgerLockPath() string { return filepath.Join(RunRoot(), "cloud.lock") }

// ReadLedger returns the ledger's entries (none when it does not exist).
func ReadLedger() ([]CloudEntry, error) {
	data, err := os.ReadFile(LedgerPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []CloudEntry
	if len(data) == 0 {
		return nil, nil
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// updateLedger rewrites the ledger under its lock with fn's result.
func updateLedger(fn func([]CloudEntry) []CloudEntry) error {
	ledgerMu.Lock()
	defer ledgerMu.Unlock()
	if err := os.MkdirAll(RunRoot(), 0o700); err != nil {
		return err
	}
	f, err := ledgerLock(ledgerLockPath(), ledgerLockWait)
	if err != nil {
		return err
	}
	defer unlockRunDir(f)
	cur, err := ReadLedger()
	if err != nil {
		return err
	}
	next := fn(cur)
	sort.Slice(next, func(i, j int) bool { return next[i].StartedAt < next[j].StartedAt })
	if len(next) == 0 {
		err := os.Remove(LedgerPath())
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	return atomicWriteJSON(LedgerPath(), next)
}

// addLedgerEntry records a session.
func addLedgerEntry(e CloudEntry) error {
	return updateLedger(func(cur []CloudEntry) []CloudEntry {
		out := cur[:0:0]
		for _, c := range cur {
			if c.ProviderRef != e.ProviderRef {
				out = append(out, c)
			}
		}
		return append(out, e)
	})
}

// removeLedgerEntry drops the session ref.
func removeLedgerEntry(ref string) error {
	return updateLedger(func(cur []CloudEntry) []CloudEntry {
		out := cur[:0:0]
		for _, c := range cur {
			if c.ProviderRef != ref {
				out = append(out, c)
			}
		}
		return out
	})
}

// deadEntry reports whether e belongs to a daemon that is no longer
// running: no live daemon of that name, or a live one with another pid.
func deadEntry(e CloudEntry) bool {
	dir := RunDir(e.Daemon)
	if e.Hub {
		dir = HubRunDir()
	}
	info, err := readInfoAt(dir)
	return err != nil || info.PID != e.PID || !PIDAlive(e.PID)
}
