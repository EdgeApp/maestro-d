// Command covchanged reports statement coverage of the lines a branch adds
// or changes. It reads Go coverage profiles (the unit run's coverage.out
// and the subprocess run's e2e.out) and `git diff -U0 <base>...HEAD`, plus
// any uncommitted and untracked changes, and keeps the statement blocks
// that overlap a changed line in non-test .go files. A block is covered
// when any profile covered it. It prints covered/total statements per file
// and in total, and exits 1 below the threshold.
//
//	go run ./tools/covchanged [-base origin/master] [-min 90] coverage.out [e2e.out …]
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

func main() {
	base := flag.String("base", "origin/master", "the ref the branch is compared with")
	minPct := flag.Float64("min", 90, "the lowest passing percentage")
	flag.Parse()
	code, err := run(*base, *minPct, flag.Args(), os.Stdout)
	if err != nil {
		fmt.Fprintln(os.Stderr, "covchanged:", err)
		os.Exit(2)
	}
	os.Exit(code)
}

func run(base string, minPct float64, profiles []string, w io.Writer) (int, error) {
	if len(profiles) == 0 {
		return 0, fmt.Errorf("usage: covchanged [-base REF] [-min PCT] coverage.out [more.out …]")
	}
	module, err := modulePath("go.mod")
	if err != nil {
		return 0, err
	}
	changed, err := changedLines(base)
	if err != nil {
		return 0, err
	}
	blocks := map[blockKey]*block{}
	for _, p := range profiles {
		f, err := os.Open(p)
		if os.IsNotExist(err) {
			fmt.Fprintf(w, "(no %s; skipped)\n", p)
			continue
		}
		if err != nil {
			return 0, err
		}
		err = readProfile(f, module, blocks)
		f.Close()
		if err != nil {
			return 0, fmt.Errorf("%s: %w", p, err)
		}
	}
	return report(blocks, changed, minPct, w), nil
}

// modulePath reads the module line of go.mod.
func modulePath(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if m, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
			return strings.TrimSpace(m), nil
		}
	}
	return "", fmt.Errorf("%s has no module line", path)
}

// counted reports whether a repository path is measured.
func counted(path string) bool {
	return strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") &&
		filepath.Base(path) != "commands_gen.go" && !strings.HasPrefix(path, "pkg/hub/hubtest/")
}

var hunkRe = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@`)

// parseDiff collects the added and changed lines of a -U0 diff per file.
func parseDiff(r io.Reader, out map[string]map[int]bool) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	file := ""
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "+++ "):
			file = strings.TrimPrefix(strings.TrimPrefix(line, "+++ "), "b/")
			if file == "/dev/null" {
				file = ""
			}
		case strings.HasPrefix(line, "@@"):
			m := hunkRe.FindStringSubmatch(line)
			if m == nil || file == "" || !counted(file) {
				continue
			}
			start, _ := strconv.Atoi(m[1])
			n := 1
			if m[2] != "" {
				n, _ = strconv.Atoi(m[2])
			}
			if out[file] == nil {
				out[file] = map[int]bool{}
			}
			for i := start; i < start+n; i++ {
				out[file][i] = true
			}
		}
	}
	return sc.Err()
}

// changedLines merges the committed branch diff, uncommitted changes and
// untracked files (every line of which is new).
func changedLines(base string) (map[string]map[int]bool, error) {
	out := map[string]map[int]bool{}
	for _, args := range [][]string{
		{"diff", "-U0", "--no-color", base + "...HEAD"},
		{"diff", "-U0", "--no-color", "HEAD"},
	} {
		data, err := exec.Command("git", args...).Output()
		if err != nil {
			return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
		}
		if err := parseDiff(strings.NewReader(string(data)), out); err != nil {
			return nil, err
		}
	}
	data, err := exec.Command("git", "ls-files", "--others", "--exclude-standard").Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-files: %w", err)
	}
	for _, f := range strings.Fields(string(data)) {
		if !counted(f) {
			continue
		}
		content, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		out[f] = map[int]bool{}
		for i := 1; i <= strings.Count(string(content), "\n")+1; i++ {
			out[f][i] = true
		}
	}
	return out, nil
}

type blockKey struct {
	file                         string
	startLine, startCol, endLine int
	endCol                       int
}

type block struct {
	stmts   int
	covered bool
}

// readProfile adds a profile's blocks, keyed by repository path; a block
// listed twice (several test binaries, several profiles) is covered when
// any listing has a count.
func readProfile(r io.Reader, module string, blocks map[blockKey]*block) error {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "mode:") {
			continue
		}
		var k blockKey
		var stmts, count int
		colon := strings.LastIndex(line, ":")
		if colon < 0 {
			return fmt.Errorf("bad line %q", line)
		}
		k.file = strings.TrimPrefix(strings.TrimPrefix(line[:colon], module), "/")
		if _, err := fmt.Sscanf(line[colon+1:], "%d.%d,%d.%d %d %d", &k.startLine, &k.startCol, &k.endLine, &k.endCol, &stmts, &count); err != nil {
			return fmt.Errorf("bad line %q: %w", line, err)
		}
		b := blocks[k]
		if b == nil {
			b = &block{stmts: stmts}
			blocks[k] = b
		}
		b.covered = b.covered || count > 0
	}
	return sc.Err()
}

// report prints the per-file and total coverage of changed lines and
// returns the exit code.
func report(blocks map[blockKey]*block, changed map[string]map[int]bool, minPct float64, w io.Writer) int {
	type tally struct{ covered, total int }
	files := map[string]*tally{}
	for k, b := range blocks {
		lines := changed[k.file]
		if lines == nil || !counted(k.file) {
			continue
		}
		hit := false
		for l := k.startLine; l <= k.endLine; l++ {
			if lines[l] {
				hit = true
				break
			}
		}
		if !hit {
			continue
		}
		t := files[k.file]
		if t == nil {
			t = &tally{}
			files[k.file] = t
		}
		t.total += b.stmts
		if b.covered {
			t.covered += b.stmts
		}
	}
	names := make([]string, 0, len(files))
	for f := range files {
		names = append(names, f)
	}
	sort.Strings(names)
	var all tally
	for _, f := range names {
		t := files[f]
		all.covered += t.covered
		all.total += t.total
		fmt.Fprintf(w, "%-60s %5d/%-5d %6.1f%%\n", f, t.covered, t.total, pct(t.covered, t.total))
	}
	if all.total == 0 {
		fmt.Fprintln(w, "changed lines: no measured statements")
		return 0
	}
	got := pct(all.covered, all.total)
	fmt.Fprintf(w, "%-60s %5d/%-5d %6.1f%%\n", "total (changed lines)", all.covered, all.total, got)
	if got < minPct {
		fmt.Fprintf(w, "FAIL: changed-line coverage %.1f%% is below %.0f%%\n", got, minPct)
		return 1
	}
	fmt.Fprintf(w, "ok: changed-line coverage %.1f%% meets %.0f%%\n", got, minPct)
	return 0
}

func pct(covered, total int) float64 {
	if total == 0 {
		return 100
	}
	return 100 * float64(covered) / float64(total)
}
