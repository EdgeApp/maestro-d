package simulator

// maestro-d: device listings carry each simulator's model identifier, the
// product type its device type simulates.

import (
	"bytes"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/execbound"
	"github.com/devicelab-dev/maestro-runner/pkg/logger"
)

// modelsLoadTimeout bounds one `simctl list devicetypes` read. The read
// runs off the listing path, so the bound only stops a wedged simctl: on a
// loaded Mac the read takes well over a second.
var modelsLoadTimeout = 30 * time.Second

// modelsListingWait is how long a listing waits for the first read, counted
// from when it started. simulatorEntries starts it beside `simctl list
// devices`, so a listing usually waits for nothing; past the wait it lists
// no model, and later listings get the read's answer.
var modelsListingWait = time.Second

// modelsFailFresh is how long a failed read stands before it is retried,
// and how often a type missing from a read makes it read again.
const modelsFailFresh = 30 * time.Second

// modelsNow is the clock, swappable in tests.
var modelsNow = time.Now

var models struct {
	mu       sync.Mutex
	byType   map[string]string // device type identifier → modelIdentifier
	loadedAt time.Time
	failedAt time.Time
	// loading is closed when the read in progress ends; nil when none
	// runs. loadStart is when it started, on the wall clock.
	loading   chan struct{}
	loadStart time.Time
}

// WarmModels starts reading the device types, unless they are read or a
// read is running, and returns at once.
func WarmModels() {
	models.mu.Lock()
	defer models.mu.Unlock()
	if models.byType == nil && retryDueLocked() {
		startLoadLocked()
	}
}

// ModelIdentifier returns the model identifier of a simulator device type
// (com.apple.CoreSimulator.SimDeviceType.iPhone-16 → "iPhone17,3"), or ""
// when the type is unknown or the device types are not read yet. It waits
// up to modelsListingWait for the first read. A type missing from a read
// starts another, at most once per modelsFailFresh, so a running hub learns
// the types an Xcode update adds.
func ModelIdentifier(deviceType string) string {
	if deviceType == "" {
		return ""
	}
	models.mu.Lock()
	model, ok := models.byType[deviceType]
	if !ok && retryDueLocked() && (models.byType == nil || modelsNow().Sub(models.loadedAt) >= modelsFailFresh) {
		startLoadLocked()
	}
	loading, start, first := models.loading, models.loadStart, models.byType == nil
	models.mu.Unlock()
	if ok || !first || loading == nil {
		return model
	}
	if wait := modelsListingWait - time.Since(start); wait > 0 {
		select {
		case <-loading:
		case <-time.After(wait):
		}
	}
	models.mu.Lock()
	defer models.mu.Unlock()
	return models.byType[deviceType]
}

// retryDueLocked reports whether no read runs and no failed read is
// inside modelsFailFresh. The caller holds models.mu.
func retryDueLocked() bool {
	return models.loading == nil && (models.failedAt.IsZero() || modelsNow().Sub(models.failedAt) >= modelsFailFresh)
}

// startLoadLocked reads the device types on a goroutine. A failed read
// keeps the types an earlier one found. The caller holds models.mu.
func startLoadLocked() {
	done := make(chan struct{})
	models.loading, models.loadStart = done, time.Now()
	go func() {
		var byType map[string]string
		err := errModelsPanic // until loadModels returns
		defer func() {
			if r := recover(); r != nil {
				logger.Error("simulator device types: %v", r)
			}
			models.mu.Lock()
			if err == nil {
				models.byType, models.loadedAt, models.failedAt = byType, modelsNow(), time.Time{}
			} else {
				models.failedAt = modelsNow()
			}
			models.loading = nil
			models.mu.Unlock()
			close(done)
		}()
		byType, err = loadModels()
	}()
}

var errModelsPanic = errors.New("simctl device types read panicked")

// loadModels reads `simctl list devicetypes -j`, killed at
// modelsLoadTimeout.
func loadModels() (map[string]string, error) {
	cmd := execCommand("xcrun", "simctl", "list", "devicetypes", "-j")
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := execbound.Run(cmd, modelsLoadTimeout); err != nil {
		return nil, err
	}
	var doc simctlCatalogDeviceTypes
	if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
		return nil, err
	}
	byType := make(map[string]string, len(doc.DeviceTypes))
	for _, t := range doc.DeviceTypes {
		if t.Identifier != "" && t.ModelIdentifier != "" {
			byType[t.Identifier] = t.ModelIdentifier
		}
	}
	return byType, nil
}
