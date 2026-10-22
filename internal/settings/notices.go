package settings

import (
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// The notices keys (ADR-001 A44): what the daemon tells a project's boss
// from its team poll, and how often.
const (
	noticesSkipPath   = "notices.skip"
	noticesGapPath    = "notices.gap"
	noticesRepeatPath = "notices.repeat"
	// MaxNoticeGap bounds notices.gap, in seconds: an hour.
	MaxNoticeGap = 3600
	// MaxNoticeRepeat bounds notices.repeat, in seconds: a day.
	MaxNoticeRepeat = 86400
)

// noticesSchema is how the daemon tells the boss what its team poll sees
// (A44): the kinds it skips, and gap and repeat in seconds. The shared file
// and a project's own table set it, never a repo file: the daemon reads no
// repo file, so a repo's value would show in `config resolve` and never
// apply.
var noticesSchema = &node{kind: kindTable, fields: map[string]*node{
	"skip":   leafOf(kindStrings),
	"gap":    leafOf(kindInt),
	"repeat": leafOf(kindInt),
}}

// NoticeKinds are the daemon's notice kinds (notice.Kinds), which a test
// there keeps in step. notices.skip can name each but noticeAlways.
var NoticeKinds = []string{"turn_ended", "blocked", "unknown", "gone", "started", "ctx_soft", "ctx_hard", "ctx_ceiling"}

// noticeAlways is the kind no team can skip: a worker's context reaching the
// ceiling always reaches its boss (D7).
const noticeAlways = "ctx_ceiling"

// NoticeConfig is one project's notices settings.
type NoticeConfig struct {
	// Skip holds the kinds its boss is not told about.
	Skip []string
	// Gap is the least time between two messages to its boss.
	Gap time.Duration
	// Repeat is how long a repeat of one notice is dropped.
	Repeat time.Duration
}

// NoticeSettings are every project's notices settings, and the settings of
// a team whose project the file does not name, such as the ◆ spaces with no
// [spaces] project.
type NoticeSettings struct {
	Default  NoticeConfig
	Projects map[string]NoticeConfig
}

// checkNotices: notices.skip names known kinds, each once; notices.gap and
// notices.repeat are seconds within their bounds.
func (m merged) checkNotices() error {
	if v, ok := m.get(noticesSkipPath); ok {
		skip, _ := v.([]string)
		for i, k := range skip {
			switch {
			case k == noticeAlways:
				return m.invalidf(noticesSkipPath, "entry %d, %s, cannot be skipped: the context ceiling always reaches the boss", i+1, FormatValue(k))
			case !slices.Contains(NoticeKinds, k):
				return m.invalidf(noticesSkipPath, "entry %d, %s, is not one of %s", i+1, FormatValue(k), strings.Join(slices.DeleteFunc(slices.Clone(NoticeKinds), func(s string) bool { return s == noticeAlways }), ", "))
			case slices.Contains(skip[:i], k):
				return m.invalidf(noticesSkipPath, "entry %d, %s, is already listed", i+1, FormatValue(k))
			}
		}
	}
	for _, c := range []struct {
		path string
		max  int
	}{{noticesGapPath, MaxNoticeGap}, {noticesRepeatPath, MaxNoticeRepeat}} {
		v, ok := m.get(c.path)
		if n, _ := v.(int); ok && (n < 0 || n > c.max) {
			return m.invalidf(c.path, "%d seconds is outside 0..%d", n, c.max)
		}
	}
	return nil
}

// noticeConfig reads the notices keys from a checked merge.
func (m merged) noticeConfig() NoticeConfig {
	seconds := func(path string) time.Duration {
		v, _ := m.get(path)
		n, _ := v.(int)
		return time.Duration(n) * time.Second
	}
	v, _ := m.get(noticesSkipPath)
	skip, _ := v.([]string)
	return NoticeConfig{Skip: slices.Clone(skip), Gap: seconds(noticesGapPath), Repeat: seconds(noticesRepeatPath)}
}

// Notices reads every project's notices settings from the shared file in
// configDir, as the daemon does once when it starts (A19). Each merges as
// the profile does, from the defaults, the shared tables and the project's
// own table; the project's repo file is not read, as for its github repo
// (Headers). Like Headers it selects no project, so it runs no repo or D12
// check and reads no environment variable. No configDir, or no file in it,
// is the defaults for every team and not an error.
func Notices(configDir string) (NoticeSettings, error) {
	out := NoticeSettings{Default: merge(defaults()).noticeConfig(), Projects: map[string]NoticeConfig{}}
	if configDir == "" {
		return out, nil
	}
	// Load lints the whole file, as Headers does; the merge below only reads
	// what it has checked.
	if _, err := Load(LoadOptions{ConfigDir: configDir, LookupEnv: func(string) (string, bool) { return "", false }}); err != nil {
		return NoticeSettings{}, err
	}
	shared, projects, err := loadShared(filepath.Join(configDir, FileName))
	if err != nil {
		return NoticeSettings{}, err
	}
	out.Default = merge(defaults(), shared).noticeConfig()
	for name, p := range projects {
		out.Projects[name] = merge(defaults(), shared, p).noticeConfig()
	}
	return out, nil
}
