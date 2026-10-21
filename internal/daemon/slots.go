package daemon

import (
	"errors"

	"github.com/abanna/herdr-agentisan/internal/settings"
	"github.com/abanna/herdr-agentisan/internal/snapshot"
	"github.com/abanna/herdr-agentisan/internal/testslots"
)

// slotCounter counts the header's test slots for each snapshot (ADR-001
// A31, A40): which of a project's lock files some process holds a lock on,
// and which of those a boss's hold keeps, read from the kernel's lock list
// every poll, so a suite that starts or ends shows within one poll. Only the
// poll's goroutine uses it.
type slotCounter struct {
	locks settings.TestLocks
	// procLocks and proc stand for /proc/locks and /proc.
	procLocks, proc string
	// failing is the lock list's failure last logged; "" while it reads.
	failing string
	// failingFor is each project's failure last logged, while it lasts.
	failingFor map[string]string
}

// newSlotCounter reads every project's lock files once, when the poll
// starts. None, or a file that does not resolve, counts nothing: no slots
// are reported.
func (o Options) newSlotCounter() *slotCounter {
	locks, err := settings.ReadTestLocks(o.Team.ConfigDir)
	if err != nil {
		o.Logger.Warn().Err(err).Msg("test slots: herdr-agentisan.toml does not resolve; the header reports no slots until it is fixed and the daemon restarts")
		return &slotCounter{}
	}
	c := &slotCounter{locks: locks, procLocks: o.Team.procLocks, proc: o.Team.proc, failingFor: map[string]string{}}
	if c.procLocks == "" {
		c.procLocks = testslots.ProcLocks
	}
	if c.proc == "" {
		c.proc = testslots.Proc
	}
	if c.any() {
		o.Logger.Info().Strs("locks", locks.Shared).Any("projects", locks.Projects).Msg("test slots: counting")
	}
	return c
}

// any reports whether some project has lock files to count.
func (c *slotCounter) any() bool {
	if len(c.locks.Shared) > 0 {
		return true
	}
	for _, l := range c.locks.Projects {
		if len(l) > 0 {
			return true
		}
	}
	return false
}

// countSlots reads the kernel's lock list once for the poll and returns each
// project's test slots, counted from that one reading (A40): nil when the
// project has no lock files or they cannot be counted. A failure is logged
// when it starts or changes kind, and a recovery when it comes, not every
// poll: the lock list's once for every project, a lock file's once for each
// project that lists it. While it lasts no slots are reported, never all
// free.
func (o Options) countSlots(c *slotCounter) func(project string) *snapshot.Slots {
	none := func(string) *snapshot.Slots { return nil }
	if c == nil || !c.any() {
		return none
	}
	r, err := testslots.Read(c.procLocks, c.proc)
	if err != nil {
		if kind := failureKind(err); kind != c.failing {
			c.failing = kind
			o.Logger.Warn().Err(err).Msg("test slots: cannot count them; the header reports none until it can")
		}
		return none
	}
	if c.failing != "" {
		c.failing = ""
		o.Logger.Info().Msg("test slots: counting again")
	}
	counted := map[string]*snapshot.Slots{}
	return func(project string) *snapshot.Slots {
		if s, ok := counted[project]; ok {
			return s
		}
		s := o.countProject(c, r, project)
		counted[project] = s
		return s
	}
}

// countProject is project's test slots from the poll's reading r, or nil.
func (o Options) countProject(c *slotCounter, r *testslots.Reading, project string) *snapshot.Slots {
	locks := c.locks.Of(project)
	if len(locks) == 0 {
		return nil
	}
	slots, err := r.Count(locks)
	if err != nil {
		if kind := failureKind(err); kind != c.failingFor[project] {
			c.failingFor[project] = kind
			o.Logger.Warn().Err(err).Str("project", project).Msg("test slots: cannot count them; the header reports none until it can")
		}
		return nil
	}
	if _, ok := c.failingFor[project]; ok {
		delete(c.failingFor, project)
		o.Logger.Info().Str("project", project).Msg("test slots: counting again")
	}
	return &slots
}

// setSlots gives s's project its own copy of slots; nil reports none.
func setSlots(s *snapshot.Snapshot, slots *snapshot.Slots) {
	if slots == nil {
		return
	}
	if s.Project == nil {
		s.Project = &snapshot.Project{}
	}
	s.Project.Slots = new(*slots)
}

// failureKind is what a counting failure is, for logging it once. A lock
// list that cannot be read is one failure, whatever its text says: that
// names the line it stopped at, which moves as other locks come and go. Any
// other failure is its text, which names the lock file and what went wrong.
func failureKind(err error) string {
	if errors.Is(err, testslots.ErrLocks) {
		return "the lock list"
	}
	return err.Error()
}
