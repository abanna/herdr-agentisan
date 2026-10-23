// Package notice is what the daemon tells a team's boss (ADR-001 A44): a
// worker finished a turn, turned blocked or unknown, or left the team, as
// the team poll sees it. Sources turn what they see into Notices, a Batcher
// coalesces each boss's into one Message at a time, and a Sink delivers it.
// The daemon only informs (D1, D7): nothing here acts on a worker.
package notice

import (
	"context"
	"slices"
	"time"
)

// Kind is what a notice reports. The names are the configuration's
// (`[projects.<p>.notices] skip`). It is open: a new source brings kinds of
// its own, which the Batcher paces like any other.
type Kind string

const (
	// TurnEnded is a worker's turn ending: herdr's completion_seq for its
	// terminal changed to a new value.
	TurnEnded Kind = "turn_ended"
	// Blocked is a worker turning blocked, which is how herdr shows an
	// agent waiting on a prompt.
	Blocked Kind = "blocked"
	// Unblocked is a blocked worker turning working, idle or done: the
	// prompt it waited on was answered.
	Unblocked Kind = "unblocked"
	// Unknown is a worker whose status herdr cannot tell, for two polls in
	// a row.
	Unknown Kind = "unknown"
	// Gone is a worker leaving the team: its pane closed, or moved out of
	// every place the team's resolver knows.
	Gone Kind = "gone"
	// Started is the daemon starting, once per team: what happened while it
	// was down is not reported.
	Started Kind = "started"
	// CtxSoft is a worker's context crossing D7's soft level. The context
	// levels are NERD-5261's source; they are named here so a team's
	// configuration can skip the soft and hard ones.
	CtxSoft Kind = "ctx_soft"
	// CtxHard is a worker's context crossing D7's hard level.
	CtxHard Kind = "ctx_hard"
	// CtxCeiling is a worker's context reaching D7's ceiling, which is never
	// skipped.
	CtxCeiling Kind = "ctx_ceiling"
	// Merged is a PR merged (A45): the GitHub collector's kinds follow.
	Merged Kind = "merged"
	// CIFailed is a PR's CI failing at its head.
	CIFailed Kind = "ci_failed"
	// CIPassed is a PR's CI passing at its head.
	CIPassed Kind = "ci_passed"
	// Dirty is a PR with a merge conflict at its head.
	Dirty Kind = "dirty"
	// Approved is a PR's first 👍.
	Approved Kind = "approved"
	// Review is a PR's new review, with its P-badge counts.
	Review Kind = "review"
)

// Kinds are every kind, in the order the configuration documents them.
var Kinds = []Kind{TurnEnded, Blocked, Unblocked, Unknown, Gone, Started, CtxSoft, CtxHard, CtxCeiling, Merged, CIFailed, CIPassed, Dirty, Approved, Review}

// Skippable reports whether a team's configuration can skip k: every kind
// but the context ceiling, which always reaches the boss (D7).
func (k Kind) Skippable() bool { return k != CtxCeiling }

// Repeats reports whether a notice of k again inside the repeat window is
// dropped. A turn's end and a start are events, never repeats, and the
// context levels are not either: their source keeps a memory of its own
// per level, and a level crossed again after it re-armed is news. Nor are
// GitHub's: its seen-set tells each once, and a PR's CI failing again at a
// new head is news.
func (k Kind) Repeats() bool {
	return !slices.Contains([]Kind{TurnEnded, Started, CtxSoft, CtxHard, CtxCeiling, Merged, CIFailed, CIPassed, Dirty, Approved, Review}, k)
}

// Notice is one line for one team's boss.
type Notice struct {
	// Project is the team whose boss it is for.
	Project string
	// Subject is what it is about, such as a pane id: a repeat is the same
	// subject and kind again (Config.Repeat).
	Subject string
	// Worker is the worker it is about, by the name a mute takes
	// (WorkerName); "" for a notice about no worker, which no mute silences.
	Worker string
	Kind   Kind
	// Text is the line itself.
	Text string
	// Ack, when set, runs once the notice is settled: sent in a message the
	// sink accepted, or dropped as configured, muted or as a repeat. A
	// notice cut for size, or in a message the sink refused, is not
	// acknowledged.
	Ack func()
}

// Config is how one team's boss is told.
type Config struct {
	// Skip holds the kinds the boss is not told about; a kind that is not
	// Skippable is told all the same.
	Skip map[Kind]bool
	// Gap is the least time between two messages to the boss: notices that
	// come sooner wait and go out together. Zero sends every poll's.
	Gap time.Duration
	// Repeat is how long the same subject and kind is dropped after it was
	// let through, for a kind that repeats (Kind.Repeats). Zero drops none.
	Repeat time.Duration
}

// Defaults: one message every 30 s at most, and a repeat dropped for 5 min.
const (
	DefaultGap    = 30 * time.Second
	DefaultRepeat = 5 * time.Minute
)

// DefaultConfig tells the boss everything, at the default pace.
func DefaultConfig() Config {
	return Config{Gap: DefaultGap, Repeat: DefaultRepeat}
}

// Configs are every team's Config, by project; a team with none set has
// Default.
type Configs struct {
	Default  Config
	Projects map[string]Config
}

// For is project's Config.
func (c Configs) For(project string) Config {
	if cfg, ok := c.Projects[project]; ok {
		return cfg
	}
	return c.Default
}

// Sink delivers a message to its team's boss.
type Sink interface {
	Send(ctx context.Context, m Message) error
}
