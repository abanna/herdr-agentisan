package devctl

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// Go has no `--cov-fail-under`, so the coverage floor is enforced here: parse
// the total line emitted by `go tool cover -func` and compare it to a
// threshold. Without this the documented coverage gate would be a claim with
// nothing behind it.

// CoverageResult is the parsed total from a coverage profile.
type CoverageResult struct {
	// Percent is the total statement coverage, 0-100.
	Percent float64
	// Threshold is the floor the run was checked against.
	Threshold float64
}

// Meets reports whether coverage cleared the threshold.
func (r CoverageResult) Meets() bool { return r.Percent >= r.Threshold }

// String renders the pass/fail line.
func (r CoverageResult) String() string {
	verdict := "FAIL"
	if r.Meets() {
		verdict = "ok"
	}
	return fmt.Sprintf("coverage %.1f%% (floor %.1f%%) %s", r.Percent, r.Threshold, verdict)
}

// totalPrefix is the label `go tool cover -func` puts on its summary line.
const totalPrefix = "total:"

// ParseCoverageTotal reads `go tool cover -func` output and returns the total
// percentage. It fails loudly when no total line is present, because a silent
// zero would read as "no coverage" and fail a gate that never actually ran.
func ParseCoverageTotal(r io.Reader) (float64, error) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, totalPrefix) {
			continue
		}
		fields := strings.Fields(line)
		raw := fields[len(fields)-1]
		pct, err := strconv.ParseFloat(strings.TrimSuffix(raw, "%"), 64)
		if err != nil {
			return 0, fmt.Errorf("parse coverage total %q: %w", raw, err)
		}
		return pct, nil
	}
	if err := sc.Err(); err != nil {
		return 0, fmt.Errorf("read coverage output: %w", err)
	}
	return 0, fmt.Errorf("no %q line in coverage output: did `go tool cover -func` run?", totalPrefix)
}

// CheckCoverageFile parses the `go tool cover -func` output saved at path and
// compares it to threshold.
func CheckCoverageFile(path string, threshold float64) (CoverageResult, error) {
	// #nosec G304 -- path comes from the operator's --profile flag; reading an
	// arbitrary coverage summary is this function's entire purpose, and devctl
	// is a development tool that already runs with the developer's privileges.
	f, err := os.Open(path)
	if err != nil {
		return CoverageResult{}, fmt.Errorf("open coverage summary: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only file; close error is not actionable

	pct, err := ParseCoverageTotal(f)
	if err != nil {
		return CoverageResult{}, err
	}
	return CoverageResult{Percent: pct, Threshold: threshold}, nil
}
