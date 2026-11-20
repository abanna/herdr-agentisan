package devctl

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// Each CI test job writes its own coverage profile (NERD-5676). `go tool
// cover`, which `devctl coverage` runs, sums a repeated block's counts and
// counts its statements once, but fails on a second "mode:" line: so the
// merge writes one mode line, then every block, and refuses what cover
// would misread.

// coverModes are the modes go test's -covermode writes.
var coverModes = map[string]bool{"set": true, "count": true, "atomic": true}

// ShardProfile is the coverage profile a shard's test job writes.
func ShardProfile(shard string) string { return "coverage-" + shard + ".out" }

// ShardProfiles is every test shard's profile in dir. It refuses a missing
// one: a job dropped from ci.yml would otherwise leave its packages out.
func ShardProfiles(dir string) ([]string, error) {
	var paths []string
	for _, shard := range Shards() {
		path := filepath.Join(dir, ShardProfile(shard))
		if _, err := os.Stat(path); err != nil {
			return nil, fmt.Errorf("no coverage profile for test shard %s (%s): run `task test:unit` first, or its CI job uploaded none: %w", shard, path, err)
		}
		paths = append(paths, path)
	}
	return paths, nil
}

// MergeReport is what MergeCoverageProfiles wrote: the distinct blocks and
// statements of its profiles, the statements the coverage floor counts.
type MergeReport struct {
	Path       string
	Profiles   int
	Blocks     int
	Statements int
}

// String renders the report as one line.
func (r MergeReport) String() string {
	return fmt.Sprintf("merged %d profiles into %s: %d blocks, %d statements", r.Profiles, r.Path, r.Blocks, r.Statements)
}

// MergeCoverageProfiles writes to out every block of the inputs under one
// mode line. It refuses a profile with no mode line or no blocks, a second
// mode, a malformed line and a block whose statements differ between
// profiles; a refused merge leaves out untouched.
func MergeCoverageProfiles(out string, inputs []string) (MergeReport, error) {
	if len(inputs) == 0 {
		return MergeReport{}, errors.New("no coverage profiles to merge")
	}
	if out == "" {
		return MergeReport{}, errors.New("no path to write the merged coverage profile to")
	}
	// A new file beside out, renamed over it: out is replaced whole, and
	// nothing already there, a symlink included, is written through.
	f, err := os.CreateTemp(filepath.Dir(out), filepath.Base(out)+".*.tmp")
	if err != nil {
		return MergeReport{}, fmt.Errorf("create merged profile: %w", err)
	}
	tmp := f.Name()
	w := bufio.NewWriter(f)
	rep, err := mergeInto(w, inputs)
	if err == nil {
		err = w.Flush()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, out)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return MergeReport{}, fmt.Errorf("merge coverage profiles: %w", err)
	}
	rep.Path = out
	return rep, nil
}

// mergeInto writes the mode line and every block of inputs to w.
func mergeInto(w *bufio.Writer, inputs []string) (MergeReport, error) {
	mode := ""
	merged := map[string]int{} // a block's position -> its statements
	for _, path := range inputs {
		if err := copyProfile(w, path, &mode, merged); err != nil {
			return MergeReport{}, err
		}
	}
	rep := MergeReport{Profiles: len(inputs), Blocks: len(merged)}
	for _, n := range merged {
		rep.Statements += n
	}
	return rep, nil
}

// copyProfile appends path's blocks to w, writing the mode line first when
// mode is still empty, and records each block's statements in merged.
func copyProfile(w *bufio.Writer, path string, mode *string, merged map[string]int) error {
	// Opened without blocking, as a FIFO would block, and its type read from
	// the open file, so nothing can be swapped in after the check.
	// #nosec G304 -- path is a profile the operator named; reading it is the point.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return fmt.Errorf("open coverage profile: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only file; close error is not actionable
	if info, err := f.Stat(); err != nil {
		return fmt.Errorf("coverage profile: %w", err)
	} else if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file (mode %s)", path, info.Mode().Type())
	}

	blocks := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	if !sc.Scan() || !strings.HasPrefix(sc.Text(), "mode: ") || sc.Text() == "mode: " {
		return fmt.Errorf("%s: no mode line: is it a coverage profile?", path)
	}
	switch m := strings.TrimPrefix(sc.Text(), "mode: "); {
	case !coverModes[m]:
		return fmt.Errorf("%s: mode %s is none go test writes (set, count, atomic)", path, m)
	case *mode == "":
		*mode = m
		if _, err := fmt.Fprintf(w, "mode: %s\n", m); err != nil {
			return fmt.Errorf("write merged profile: %w", err)
		}
	case m != *mode:
		return fmt.Errorf("%s: mode %s, but the first profile's is %s", path, m, *mode)
	}
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		block, stmts, err := parseBlock(line)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if prev, seen := merged[block]; seen && prev != stmts {
			return fmt.Errorf("%s: block %s has %d statements, %d in an earlier profile: profiles of different trees?", path, block, stmts, prev)
		}
		merged[block] = stmts
		blocks++
		if _, err := fmt.Fprintln(w, line); err != nil {
			return fmt.Errorf("write merged profile: %w", err)
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if blocks == 0 {
		return fmt.Errorf("%s: no blocks: did its tests run?", path)
	}
	return nil
}

// parseBlock splits a profile line, "file.go:l.c,l.c statements count", into
// its position and statement count. It reads from the line's end, as go
// tool cover's parser does, so a file name may hold a space.
func parseBlock(line string) (string, int, error) {
	rest, count, ok := cutLast(line)
	block, stmtsText, ok2 := cutLast(rest)
	stmts, err := strconv.Atoi(stmtsText)
	_, cerr := strconv.ParseUint(count, 10, 64)
	// go test writes no CR and no NUL: such a line is from somewhere else.
	if !ok || !ok2 || !strings.Contains(block, ":") || err != nil || stmts < 0 || cerr != nil ||
		strings.ContainsAny(line, "\r\x00") {
		return "", 0, fmt.Errorf("malformed profile line %q", line)
	}
	return block, stmts, nil
}

// cutLast splits s at its last space.
func cutLast(s string) (before, after string, ok bool) {
	i := strings.LastIndexByte(s, ' ')
	if i < 0 {
		return "", "", false
	}
	return s[:i], s[i+1:], true
}
