// Package notice is what the daemon tells a team's boss (ADR-001 A44): a
// worker finished a turn, turned blocked or unknown, or left the team, as
// the team poll sees it. Sources turn what they see into Notices, a Batcher
// coalesces each boss's into one Message at a time, and a Sink delivers it.
// The daemon only informs (D1, D7): nothing here acts on a worker.
package notice

import (
	"context"
	"time"
)

// Kind is what a notice reports. The names are the configuration's
// (`[projects.<p>.notices] skip`).
type Kind string

const (
	// TurnEnded is a worker's turn ending: herdr's completion_seq for its
	// terminal changed to a new value.
	TurnEnded Kind = "turn_ended"
	// Blocked is a worker turning blocked, which is how herdr shows an
	// agent waiting on a prompt.
	Blocked Kind = "blocked"
	// Unknown is a worker whose status herdr cannot tell, for two polls in
	// a row.
	Unknown Kind = "unknown"
	// Gone is a worker leaving the team: its pane closed, or moved out of
	// every place the team's resolver knows.
	Gone Kind = "gone"
	// Started is the daemon starting, once per team: what happened while it
	// was down is not reported.
	Started Kind = "started"
)

// Kinds are every kind, in the order the configuration documents them.
var Kinds = []Kind{TurnEnded, Blocked, Unknown, Gone, Started}

// Notice is one line for one team's boss.
type Notice struct {
	// Project is the team whose boss it is for.
	Project string
	// Subject is what it is about, such as a pane id: a repeat is the same
	// subject and kind again (Config.Repeat).
	Subject string
	Kind    Kind
	// Text is the line itself.
	Text string
	// Ack, when set, runs once the notice is settled: sent in a message the
	// sink accepted, or dropped as configured or as a repeat. A notice cut
	// for size, or in a message the sink refused, is not acknowledged.
	Ack func()
}

// Config is how one team's boss is told.
type Config struct {
	// Skip holds the kinds the boss is not told about.
	Skip map[Kind]bool
	// Gap is the least time between two messages to the boss: notices that
	// come sooner wait and go out together. Zero sends every poll's.
	Gap time.Duration
	// Repeat is how long the same subject and kind is dropped after it was
	// let through, except a turn ending, or a start. Zero drops none.
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
