package devicelab_ios

import "os/exec"

// execCommandContext is the seam over exec.CommandContext that starts
// `xcodebuild test-without-building`, so tests can fake the runner.
var execCommandContext = exec.CommandContext
