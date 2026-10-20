package daemon

import (
	"errors"

	"github.com/abanna/herdr-agentisan/internal/settings"
	"github.com/abanna/herdr-agentisan/internal/snapshot"
	"github.com/abanna/herdr-agentisan/internal/testslots"
)

// slotCounter counts the header's test slots for each snapshot (ADR-001
// A31): which of [tests] locks some process holds a lock on, read from the
// kernel's lock list every poll, so a suite that starts or ends shows within
// one poll. Only the poll's goroutine uses it.
type slotCounter struct {
	locks     []string
	procLocks string
	// failing is the kind of failure last logged; "" while counting works.
	failing string
}

// newSlotCounter reads [tests] locks once, when the poll starts. None, or a
// file that does not resolve, counts nothing: no slots are reported.
func (o Options) newSlotCounter() *slotCounter {
	locks, err := settings.TestLocks(o.Team.ConfigDir)
	if err != nil {
		o.Logger.Warn().Err(err).Msg("test slots: herdr-agentisan.toml does not resolve; the header reports no slots until it is fixed and the daemon restarts")
		return &slotCounter{}
	}
	procLocks := o.Team.procLocks
	if procLocks == "" {
		procLocks = testslots.ProcLocks
	}
	if len(locks) > 0 {
		o.Logger.Info().Strs("locks", locks).Msg("test slots: counting")
	}
	return &slotCounter{locks: locks, procLocks: procLocks}
}

// fillSlots sets s's project's slots, when there are lock files to count
// and they can be counted. A failure is logged when it starts or changes
// kind, and a recovery when it comes, not every poll; while it lasts no
// slots are reported, never all free.
func (o Options) fillSlots(c *slotCounter, s *snapshot.Snapshot) {
	if c == nil || len(c.locks) == 0 {
		return
	}
	slots, err := testslots.Count(c.procLocks, c.locks)
	if err != nil {
		if kind := failureKind(err); kind != c.failing {
			c.failing = kind
			o.Logger.Warn().Err(err).Msg("test slots: cannot count them; the header reports none until it can")
		}
		return
	}
	if c.failing != "" {
		c.failing = ""
		o.Logger.Info().Msg("test slots: counting again")
	}
	if s.Project == nil {
		s.Project = &snapshot.Project{}
	}
	s.Project.Slots = &slots
}

// failureKind is what a counting failure is, for logging it once: the kind
// of read that failed, not its text, since a lock list that cannot be read
// names the line it stopped at, which moves as other locks come and go.
func failureKind(err error) string {
	switch {
	case errors.Is(err, testslots.ErrLocks):
		return "the lock list"
	case errors.Is(err, testslots.ErrLockFile):
		return "a lock file"
	default:
		return err.Error()
	}
}
