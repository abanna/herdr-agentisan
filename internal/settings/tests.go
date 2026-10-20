package settings

import (
	"path/filepath"
	"slices"
	"strings"
)

// testLocksPath is the lock files behind the dashboard header's test slots
// (ADR-001 A31): one file per slot, held by whatever runs a test suite.
const testLocksPath = "tests.locks"

// MaxTestLocks bounds how many test slots tests.locks may name.
const MaxTestLocks = 32

// testsSchema is [tests]. Like [telemetry] it belongs to the shared file and
// --set only: the slots are the machine's, read by the one daemon per herdr
// server, so a project table or a repo file naming them is an unknown key.
var testsSchema = &node{kind: kindTable, fields: map[string]*node{
	"locks": leafOf(kindStrings),
}}

// checkTests checks every entry of tests.locks is an absolute path, named
// once: the daemon reads them from wherever it started, and ~ is not
// expanded. Two spellings of one path, such as /a//b and /a/b, are the same
// entry.
func (m merged) checkTests() error {
	v, ok := m.get(testLocksPath)
	if !ok {
		return nil
	}
	locks, _ := v.([]string)
	if len(locks) > MaxTestLocks {
		return m.invalidf(testLocksPath, "%d lock files; at most %d", len(locks), MaxTestLocks)
	}
	seen := make([]string, 0, len(locks))
	for i, p := range locks {
		switch {
		case p == "" || strings.ContainsRune(p, 0):
			return m.invalidf(testLocksPath, "entry %d is not a path", i+1)
		case !filepath.IsAbs(p):
			return m.invalidf(testLocksPath, "entry %d, %s, is not an absolute path; ~ is not expanded", i+1, FormatValue(p))
		case slices.Contains(seen, filepath.Clean(p)):
			return m.invalidf(testLocksPath, "entry %d, %s, names a lock file already listed", i+1, FormatValue(p))
		}
		seen = append(seen, filepath.Clean(p))
	}
	return nil
}

// TestLocks is tests.locks as resolved, which Load has checked, each entry
// cleaned; nil when it is unset or empty.
func (r Resolved) TestLocks() []string {
	v, _ := lookup(r.Values, testLocksPath)
	locks, _ := v.([]string)
	if len(locks) == 0 {
		return nil
	}
	out := make([]string, len(locks))
	for i, p := range locks {
		out[i] = filepath.Clean(p)
	}
	return out
}

// TestLocks is the lock files behind the test slots, read by the daemon once
// when it starts, from the shared file in configDir. nil is none, and no
// slots are reported; so is no configDir, or no file in it. The read selects
// no project, so it touches no repo and reads no environment variable.
func TestLocks(configDir string) ([]string, error) {
	if configDir == "" {
		return nil, nil
	}
	res, err := Load(LoadOptions{ConfigDir: configDir, LookupEnv: func(string) (string, bool) { return "", false }})
	if err != nil {
		return nil, err
	}
	return res.TestLocks(), nil
}
