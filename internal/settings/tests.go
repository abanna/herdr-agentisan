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
// --set, and to a project's own table (A40): the slots are the machine's,
// read by the one daemon per herdr server, so a repo file naming them is an
// unknown key.
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
	return locksIn(r.Values)
}

// locksIn is tests.locks in a resolved tree, each entry cleaned; nil when it
// is unset or empty.
func locksIn(values map[string]any) []string {
	v, _ := lookup(values, testLocksPath)
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

// TestLocks is the lock files behind each team's test slots (A31, A40).
type TestLocks struct {
	// Shared is [tests] locks: the slots of the unnamed project and of any
	// project the file does not configure.
	Shared []string
	// Projects holds every configured project's lock files: its own
	// [projects.<p>.tests] locks when its table sets them, an empty list
	// included, and the shared ones otherwise.
	Projects map[string][]string
}

// Of is project's lock files; nil is none, and no slots are reported. A
// configured project has its own entry, so its empty list never falls back
// to the shared one.
func (l TestLocks) Of(project string) []string {
	if locks, ok := l.Projects[project]; ok {
		return locks
	}
	return l.Shared
}

// ReadTestLocks reads every project's lock files, once, for the daemon, from
// the shared file in configDir. A project's merge as Headers' do: the
// defaults, the shared tables and its own table, where its list replaces the
// shared one whole; no repo file can set them. No configDir, or no file in
// it, is none. Like Headers it selects no project, so it touches no repo and
// reads no environment variable.
func ReadTestLocks(configDir string) (TestLocks, error) {
	if configDir == "" {
		return TestLocks{}, nil
	}
	// Load lints the whole file, every project's lock files included; the
	// merges below only read what it has checked.
	res, err := Load(LoadOptions{ConfigDir: configDir, LookupEnv: func(string) (string, bool) { return "", false }})
	if err != nil {
		return TestLocks{}, err
	}
	shared, projects, err := loadShared(filepath.Join(configDir, FileName))
	if err != nil {
		return TestLocks{}, err
	}
	out := TestLocks{Shared: res.TestLocks(), Projects: make(map[string][]string, len(projects))}
	for name, p := range projects {
		out.Projects[name] = locksIn(merge(defaults(), shared, p).values)
	}
	return out, nil
}
