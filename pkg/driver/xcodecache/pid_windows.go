//go:build windows

package xcodecache

// pidAlive is true on Windows, where Go cannot probe a process without
// killing it and no iOS runner runs: nothing is removed as an orphan.
var pidAlive = func(pid int) bool { return true }
