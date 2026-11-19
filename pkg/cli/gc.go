package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/urfave/cli/v2"

	"github.com/devicelab-dev/maestro-runner/pkg/config"
	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
	"github.com/devicelab-dev/maestro-runner/pkg/driver/xcodecache"
)

// gcBuildCacheDirs are the iOS runner build caches under
// config.GetCacheDir() that --include-build-cache prunes.
var gcBuildCacheDirs = []string{"wda-builds", "devicelab-ios-runner-builds", "embedded-runner-src"}

var gcFlags = []dflag{
	daemonSelectFlags[1], // --json
	boolFlag("dry-run", "List what would be deleted without deleting it", nil,
		func(o *daemonOpts, v bool) { o.DryRun = v }),
	boolFlag("include-build-cache", "Also prune iOS runner builds unused for 30 days (the binary's cache, not MAESTRO_D_HOME)", nil,
		func(o *daemonOpts, v bool) { o.IncludeBuildCache = v }),
}

var gcCommand = &cli.Command{
	Name:      "gc",
	Usage:     "Delete what maestro-d no longer needs: old blobs, idle reports, dead daemons' run dirs, orphaned Chrome profiles",
	ArgsUsage: " ",
	Description: "Runs the housekeeping sweep every daemon runs half a minute after it starts and hourly, against\n" +
		"MAESTRO_D_HOME (default ~/.maestro-d), without starting a daemon.\n\n" + flagsDoc("", gcFlags),
	SkipFlagParsing: true,
	Action: withArgs(gcFlags, func(_ context.Context, o *daemonOpts, _ []string) error {
		res := daemon.Sweep(daemon.Home(), daemon.SweepOptions{DryRun: o.DryRun, KeepRunDir: serviceInstalled})
		if o.IncludeBuildCache {
			for _, dir := range gcBuildCacheDirs {
				for _, p := range xcodecache.PruneBuildCache(filepath.Join(config.GetCacheDir(), dir), "", xcodecache.DefaultMaxAge, o.DryRun) {
					res.Removed = append(res.Removed, daemon.SweepEntry{Path: p.Path, Size: p.Size, Reason: p.Reason})
				}
			}
		}
		if o.JSON {
			printJSON(os.Stdout, map[string]any{"ok": true, "exitCode": 0, "gc": res})
			return nil
		}
		if res.Skipped {
			msg := "Skipped the home: a daemon is sweeping it right now."
			if o.IncludeBuildCache {
				msg = "Skipped the home (a daemon is sweeping it right now); the build cache was still checked."
			}
			fmt.Println(msg)
		}
		verb := "Deleted"
		if o.DryRun {
			verb = "Would delete"
		}
		for _, e := range res.Removed {
			fmt.Printf("%s %s (%s): %s\n", verb, e.Path, humanSize(e.Size), e.Reason)
		}
		fmt.Printf("%s %d path(s), %s.\n", verb, len(res.Removed), humanSize(res.Bytes()))
		return nil
	}),
}

// humanSize formats a byte count as B, KB, MB or GB.
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	value, suffix := float64(n), "B"
	for _, s := range []string{"KB", "MB", "GB"} {
		if value < unit {
			break
		}
		value /= unit
		suffix = s
	}
	return fmt.Sprintf("%.1f %s", value, suffix)
}
