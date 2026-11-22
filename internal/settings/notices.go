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
	// noticesDeliveryPath is how a team's boss is told (A74): typed into
	// it, or posted on its board while it runs Claude.
	noticesDeliveryPath = "notices.delivery"
	// MaxNoticeGap bounds notices.gap, in seconds: an hour.
	MaxNoticeGap = 3600
	// MaxNoticeRepeat bounds notices.repeat, in seconds: a day.
	MaxNoticeRepeat = 86400
)

// The values of notices.delivery (A74).
const (
	// DeliveryTyped types the boss's notices and messages into it (A30,
	// A44): the default.
	DeliveryTyped = "typed"
	// DeliveryBoard posts them on its team's board while it runs Claude.
	DeliveryBoard = "board"
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
	// delivery is typed or board (A74).
	"delivery":  leafOf(kindString),
	"attention": attentionSchema,
}}

// attentionSchema is [notices.attention] (A74): when the user is alerted
// that a board-mode boss has left posts unread, or messages waiting, too
// long. The times are in seconds.
var attentionSchema = &node{kind: kindTable, fields: map[string]*node{
	"after":         leafOf(kindInt),
	"ceiling_after": leafOf(kindInt),
	"every":         leafOf(kindInt),
	"max_every":     leafOf(kindInt),
	"alerts":        leafOf(kindInt),
	"busy_grace":    leafOf(kindInt),
	"sound":         leafOf(kindString),
}}

// The [notices.attention] keys (A74).
const (
	attentionAfterPath        = "notices.attention.after"
	attentionCeilingAfterPath = "notices.attention.ceiling_after"
	attentionEveryPath        = "notices.attention.every"
	attentionMaxEveryPath     = "notices.attention.max_every"
	attentionAlertsPath       = "notices.attention.alerts"
	attentionBusyGracePath    = "notices.attention.busy_grace"
	attentionSoundPath        = "notices.attention.sound"
)

// The sounds an alert can make: herdr's (A74).
const (
	SoundNone    = "none"
	SoundDone    = "done"
	SoundRequest = "request"
)

// AttentionSounds are notices.attention.sound's values.
var AttentionSounds = []string{SoundNone, SoundDone, SoundRequest}

// inSeconds is how a bound in seconds is told.
const inSeconds = " seconds"

// AttentionConfig is one project's [notices.attention] (A74). PR 3b's
// daemon maps it onto attention.Config field by field: each field there has
// this one's name and type.
type AttentionConfig struct {
	// After is how long the oldest post or held message waits before an
	// episode's first alert; 0 turns every alert off. CeilingAfter is how
	// long a context ceiling waits. Every is the first reminder's gap, and
	// MaxEvery the longest. BusyGrace stands in for After, when longer,
	// while the boss is working.
	After, CeilingAfter, Every, MaxEvery, BusyGrace time.Duration
	// Alerts is the most alerts an episode shows; 0: no limit.
	Alerts int
	// Sound is one of AttentionSounds.
	Sound string
}

// NoticeKinds are the daemon's notice kinds (notice.Kinds), which a test
// there keeps in step. notices.skip can name each but noticeAlways.
var NoticeKinds = []string{"turn_ended", "blocked", "unblocked", "unknown", "gone", "started", "ctx_soft", "ctx_hard", "ctx_ceiling", "merged", "ci_failed", "ci_passed", "dirty", "approved", "review"}

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
	// Delivery is DeliveryTyped or DeliveryBoard (A74).
	Delivery string
	// Attention is when the user is alerted to what its boss has left
	// waiting, while Delivery is board (A74).
	Attention AttentionConfig
}

// NoticeSettings are every project's notices settings, and the settings of
// a team whose project the file does not name, such as the ◆ spaces with no
// [spaces] project.
type NoticeSettings struct {
	Default  NoticeConfig
	Projects map[string]NoticeConfig
}

// checkNotices: notices.skip names known kinds, each once; notices.gap and
// notices.repeat are seconds within their bounds; notices.delivery is typed
// or board.
func (m merged) checkNotices() error {
	if v, ok := m.get(noticesDeliveryPath); ok && v != DeliveryTyped && v != DeliveryBoard {
		return m.invalidf(noticesDeliveryPath, "%s is not typed or board", FormatValue(v))
	}
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
	return m.checkAttention()
}

// checkAttention: each [notices.attention] key within its bounds (A74).
func (m merged) checkAttention() error {
	n := func(path string) (int, bool) {
		v, ok := m.get(path)
		i, _ := v.(int)
		return i, ok
	}
	if v, ok := n(attentionAfterPath); ok && v != 0 && (v < 60 || v > 86400) {
		return m.invalidf(attentionAfterPath, "%d seconds is neither 0 (off) nor within 60..86400", v)
	}
	every, _ := n(attentionEveryPath)
	for _, c := range []struct {
		path     string
		min, max int
		unit     string
	}{
		{attentionCeilingAfterPath, 30, 3600, inSeconds},
		{attentionEveryPath, 60, 3600, inSeconds},
		{attentionMaxEveryPath, max(every, 60), 14400, inSeconds},
		{attentionAlertsPath, 0, 100, ""},
		{attentionBusyGracePath, 0, 86400, inSeconds},
	} {
		if v, ok := n(c.path); ok && (v < c.min || v > c.max) {
			if c.path == attentionMaxEveryPath && v < every {
				return m.invalidf(c.path, "%d seconds is below notices.attention.every, %d", v, every)
			}
			return m.invalidf(c.path, "%d%s is outside %d..%d", v, c.unit, c.min, c.max)
		}
	}
	if v, ok := m.get(attentionSoundPath); ok {
		if sound, _ := v.(string); !slices.Contains(AttentionSounds, sound) {
			return m.invalidf(attentionSoundPath, "%s is not one of %s", FormatValue(v), strings.Join(AttentionSounds, ", "))
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
	delivery, _ := m.get(noticesDeliveryPath)
	d, _ := delivery.(string)
	alerts, _ := m.get(attentionAlertsPath)
	sound, _ := m.get(attentionSoundPath)
	a := AttentionConfig{
		After: seconds(attentionAfterPath), CeilingAfter: seconds(attentionCeilingAfterPath),
		Every: seconds(attentionEveryPath), MaxEvery: seconds(attentionMaxEveryPath), BusyGrace: seconds(attentionBusyGracePath),
	}
	a.Alerts, _ = alerts.(int)
	a.Sound, _ = sound.(string)
	return NoticeConfig{Skip: slices.Clone(skip), Gap: seconds(noticesGapPath), Repeat: seconds(noticesRepeatPath), Delivery: d, Attention: a}
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
