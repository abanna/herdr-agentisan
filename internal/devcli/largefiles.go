package devcli

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
)

// Git has no built-in size ceiling, so a multi-megabyte binary committed by an
// agent is caught only by review — and review is exactly what an autonomous
// agent bypasses. This check enumerates tracked files and fails on anything
// over the limit, which is cheap enough to run on every push.

// bytesPerKB is the conversion used for the ceiling comparison.
const bytesPerKB = 1024

// DefaultMaxFileKB is the size ceiling for a tracked file, in kilobytes.
// go.sum is the largest legitimate file in this repo at ~106KB; 512 leaves
// headroom for it to grow without inviting a checked-in binary.
const DefaultMaxFileKB = 512

// LargeFile is a tracked file that exceeds the ceiling.
type LargeFile struct {
	Path string
	// Bytes is the exact size. The comparison is done in bytes, not floored
	// kilobytes: `size/1024 > 512` lets a 524,800-byte file through because
	// the division truncates to exactly 512, making the real ceiling 513KB-1B
	// and looser than the pre-commit `check-added-large-files --maxkb` that
	// is supposed to back it up.
	Bytes int64
}

// KB reports the size rounded up, for display only — never for comparison.
func (f LargeFile) KB() int64 { return (f.Bytes + bytesPerKB - 1) / bytesPerKB }

// allowedLargePaths are tracked files permitted to exceed the ceiling, with
// the reason. A generated lockfile grows on its own and is not a judgment
// call; anything else should be argued for explicitly.
var allowedLargePaths = map[string]string{
	"go.sum": "generated dependency hash manifest; grows with the tool directive",
}

// FindLargeFiles returns tracked blobs over maxKB, excluding allowed paths.
//
// Sizes come from the INDEX, not the working tree. Stat-ing the checkout made
// the gate measure the wrong thing: under a cone sparse-checkout, or with any
// skip-worktree entry, `git ls-files` still lists the path while the file is
// absent from disk. Stat then returned ENOENT, the entry was skipped as
// "tracked but deleted", and the gate printed "no tracked file exceeds" over a
// blob that is in the index and would be pushed.
//
// `git cat-file --batch-check` reports the blob's real size and has no
// filesystem to fail on, which also removes the EACCES/EPERM case.
func FindLargeFiles(ctx context.Context, root string, maxKB int64) ([]LargeFile, error) {
	// #nosec G204 -- `root` comes from repoRoot() or a test, never untrusted
	// runtime input, and is passed as its own argv entry after -C.
	ls := exec.CommandContext(ctx, "git", "-C", root, "ls-files", "-z")
	paths, err := ls.Output()
	if err != nil {
		return nil, fmt.Errorf("list tracked files: %w", err)
	}

	// Ask for each path's staged blob size in one batch rather than one
	// process per file.
	var query bytes.Buffer
	var wanted []string
	sc := bufio.NewScanner(bytes.NewReader(paths))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	sc.Split(splitNul)
	for sc.Scan() {
		rel := sc.Text()
		if rel == "" {
			continue
		}
		if _, ok := allowedLargePaths[rel]; ok {
			continue
		}
		wanted = append(wanted, rel)
		fmt.Fprintf(&query, ":%s\n", rel)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("scan tracked files: %w", err)
	}
	if len(wanted) == 0 {
		return nil, nil
	}

	// #nosec G204 -- same reasoning as above.
	cat := exec.CommandContext(ctx, "git", "-C", root, "cat-file", "--batch-check")
	cat.Stdin = &query
	sizes, err := cat.Output()
	if err != nil {
		return nil, fmt.Errorf("read staged blob sizes: %w", err)
	}

	var found []LargeFile
	lines := bufio.NewScanner(bytes.NewReader(sizes))
	i := 0
	for lines.Scan() {
		if i >= len(wanted) {
			break
		}
		rel := wanted[i]
		i++

		// "<sha> <type> <size>", or "<spec> missing" for an unreadable entry.
		fields := strings.Fields(lines.Text())
		if len(fields) < 3 || fields[1] != "blob" {
			// A path in the index with no readable blob is a broken index,
			// not a file to wave through.
			return nil, fmt.Errorf("no blob for tracked path %s: %q", rel, lines.Text())
		}
		size, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parse blob size for %s: %w", rel, err)
		}
		if size > maxKB*bytesPerKB {
			found = append(found, LargeFile{Path: rel, Bytes: size})
		}
	}
	if err := lines.Err(); err != nil {
		return nil, fmt.Errorf("read blob sizes: %w", err)
	}

	sort.Slice(found, func(i, j int) bool { return found[i].Bytes > found[j].Bytes })
	return found, nil
}

// splitNul splits on NUL, matching `git ls-files -z` so paths containing
// newlines or spaces survive intact.
func splitNul(data []byte, atEOF bool) (advance int, token []byte, err error) {
	for i, b := range data {
		if b == 0 {
			return i + 1, data[:i], nil
		}
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}
