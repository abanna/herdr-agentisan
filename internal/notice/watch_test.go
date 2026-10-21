package notice_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/notice"
	"github.com/abanna/herdr-agentisan/internal/team"
)

// agentPane is a group worker running an agent, unbound and named name.
func agentPane(id, name string, status team.Status) team.Worker {
	return team.Worker{ID: id, Pane: team.Pane{ID: id, Name: name, Agent: "claude", Status: status}}
}

// teamOf is one project, "ha", whose boss is boss and whose coder group
// holds workers.
func teamOf(boss []team.Worker, workers ...team.Worker) team.Model {
	return team.Model{Projects: []team.Project{{Name: "ha", Boss: boss, Groups: []team.Group{{Name: "coder", Workers: workers}}}}}
}

// agent is agent.list's entry for pane id, in terminal term, its last turn
// ended at s (nil for none).
func agent(id, term string, s *uint64) herdr.AgentInfo {
	return herdr.AgentInfo{PaneID: id, TerminalID: term, CompletionSeq: s}
}

// kinds is each notice's subject and kind.
func kinds(ns []notice.Notice) []string {
	out := []string{}
	for _, n := range ns {
		out = append(out, n.Subject+" "+string(n.Kind))
	}
	return out
}

// TestWatchFirstCallIsTheBaseline: the first model only starts the watch,
// with one started notice per team, whatever its panes say.
func TestWatchFirstCallIsTheBaseline(t *testing.T) {
	t.Parallel()
	m := teamOf(nil, agentPane("w1:p1", "c1", team.StatusBlocked))
	m.Projects = append(m.Projects, team.Project{Name: "skills"})
	for _, named := range []bool{true, false} {
		w := notice.NewWatch()
		got := w.Observe(m, []herdr.AgentInfo{agent("w1:p1", "t1", new(uint64(9)))}, named)
		assert.Equal(t, []string{"ha started", "skills started"}, kinds(got), "named %v", named)
		assert.Equal(t, "ha", got[0].Project)
		assert.Contains(t, got[0].Text, "turns that ended while it was down are not reported")
		assert.Empty(t, w.Observe(m, []herdr.AgentInfo{agent("w1:p1", "t1", new(uint64(9)))}, true), "nothing changed since")
	}
}

// TestWatchTurns: a turn ends when the terminal's completion_seq changes to
// a new value, and only then, compared as values, never ordered.
func TestWatchTurns(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		before, after *uint64
		want          []string
	}{
		"a new value":          {new(uint64(7)), new(uint64(9)), []string{"w1:p1 turn_ended"}},
		"a smaller value":      {new(uint64(9)), new(uint64(7)), []string{"w1:p1 turn_ended"}},
		"none to a value":      {nil, new(uint64(7)), []string{"w1:p1 turn_ended"}},
		"zero to a value":      {new(uint64(0)), new(uint64(1)), []string{"w1:p1 turn_ended"}},
		"the same value":       {new(uint64(7)), new(uint64(7)), []string{}},
		"a value to none":      {new(uint64(7)), nil, []string{}},
		"none to none":         {nil, nil, []string{}},
		"the largest value":    {new(uint64(1)), new(^uint64(0)), []string{"w1:p1 turn_ended"}},
		"from the largest one": {new(^uint64(0)), new(uint64(0)), []string{"w1:p1 turn_ended"}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m := teamOf(nil, agentPane("w1:p1", "c1", team.StatusDone))
			w := notice.NewWatch()
			w.Observe(m, []herdr.AgentInfo{agent("w1:p1", "t1", tc.before)}, true)
			assert.Equal(t, tc.want, kinds(w.Observe(m, []herdr.AgentInfo{agent("w1:p1", "t1", tc.after)}, true)))
		})
	}
}

// TestWatchTurnBaselines: a terminal is compared only with itself. A
// terminal seen for the first time, one that came back, and a pane id
// herdr reissued to another terminal are each a baseline.
func TestWatchTurnBaselines(t *testing.T) {
	t.Parallel()
	m := teamOf(nil, agentPane("w1:p1", "c1", team.StatusDone))
	w := notice.NewWatch()
	w.Observe(m, []herdr.AgentInfo{agent("w1:p1", "t1", new(uint64(1)))}, true)

	assert.Empty(t, w.Observe(m, []herdr.AgentInfo{agent("w1:p1", "t2", new(uint64(5)))}, true), "a reissued pane id, a new terminal")
	assert.Equal(t, []string{"w1:p1 turn_ended"}, kinds(w.Observe(m, []herdr.AgentInfo{agent("w1:p1", "t2", new(uint64(6)))}, true)))

	assert.Empty(t, w.Observe(m, []herdr.AgentInfo{}, true), "agent.list no longer lists it")
	assert.Empty(t, w.Observe(m, []herdr.AgentInfo{agent("w1:p1", "t2", new(uint64(8)))}, true), "it came back: a baseline")

	two := teamOf(nil, agentPane("w1:p1", "c1", team.StatusDone), agentPane("w1:p2", "c2", team.StatusDone))
	assert.Empty(t, w.Observe(two, []herdr.AgentInfo{agent("w1:p1", "t2", new(uint64(8))), agent("w1:p2", "t3", new(uint64(4)))}, true), "a pane seen for the first time")
}

// TestWatchTurnsWaitForAgentList: a poll whose agent.list failed compares
// nothing and keeps the turns, so a turn that ended during it is reported
// at the next poll that has agent.list.
func TestWatchTurnsWaitForAgentList(t *testing.T) {
	t.Parallel()
	m := teamOf(nil, agentPane("w1:p1", "c1", team.StatusDone))
	w := notice.NewWatch()
	w.Observe(m, []herdr.AgentInfo{agent("w1:p1", "t1", new(uint64(1)))}, true)
	assert.Empty(t, w.Observe(m, nil, false))
	assert.Empty(t, w.Observe(m, []herdr.AgentInfo{agent("w1:p1", "t1", new(uint64(2)))}, false), "unnamed: not compared")
	assert.Equal(t, []string{"w1:p1 turn_ended"}, kinds(w.Observe(m, []herdr.AgentInfo{agent("w1:p1", "t1", new(uint64(2)))}, true)))

	first := notice.NewWatch()
	first.Observe(m, nil, false)
	assert.Empty(t, first.Observe(m, []herdr.AgentInfo{agent("w1:p1", "t1", new(uint64(2)))}, true), "the first named poll is the turns' baseline")
}

// TestWatchTurnsSkip: the boss's own turns, a shell, an agent with no
// terminal and a pane agent.list does not list are never a turn; the first
// of two entries for one pane counts.
func TestWatchTurnsSkip(t *testing.T) {
	t.Parallel()
	shell := team.Worker{ID: "w1:p3", Pane: team.Pane{ID: "w1:p3", Status: team.StatusUnknown}}
	m := teamOf([]team.Worker{agentPane("w1:pB", "boss", team.StatusDone)},
		agentPane("w1:p1", "c1", team.StatusDone), agentPane("w1:p2", "c2", team.StatusDone), shell, agentPane("w1:p4", "c4", team.StatusDone))
	list := func(s uint64) []herdr.AgentInfo {
		return []herdr.AgentInfo{
			agent("w1:pB", "tB", new(s)), agent("w1:p1", "", new(s)), agent("w1:p3", "t3", new(s)),
			agent("w1:p2", "t2", new(s)), agent("w1:p2", "t9", new(s+100)),
		}
	}
	w := notice.NewWatch()
	w.Observe(m, list(1), true)
	assert.Equal(t, []string{"w1:p2 turn_ended"}, kinds(w.Observe(m, list(2), true)))
}

// TestWatchStatuses: blocked and gone at once; unknown only when it holds
// for a second poll, and once; the boss's panes and shells never.
func TestWatchStatuses(t *testing.T) {
	t.Parallel()
	boss := []team.Worker{agentPane("w1:pB", "boss", team.StatusWorking)}
	shell := team.Worker{ID: "w1:p9", Pane: team.Pane{ID: "w1:p9", Status: team.StatusUnknown}}
	m := func(boss []team.Worker, ws ...team.Worker) team.Model { return teamOf(boss, ws...) }
	w := notice.NewWatch()
	w.Observe(m(boss, agentPane("w1:p1", "c1", team.StatusWorking), agentPane("w1:p2", "c2", team.StatusWorking), shell), nil, false)

	blockedBoss := []team.Worker{agentPane("w1:pB", "boss", team.StatusBlocked)}
	got := w.Observe(m(blockedBoss, agentPane("w1:p1", "c1", team.StatusBlocked), agentPane("w1:p2", "c2", team.StatusUnknown)), nil, false)
	assert.Equal(t, []string{"w1:p1 blocked"}, kinds(got), "blocked at once; unknown held; the boss and a leaving shell never")
	assert.Equal(t, "c1 is blocked", got[0].Text)
	assert.Equal(t, "ha", got[0].Project)

	assert.Equal(t, []string{"w1:p2 unknown"}, kinds(w.Observe(m(blockedBoss, agentPane("w1:p1", "c1", team.StatusBlocked), agentPane("w1:p2", "c2", team.StatusUnknown)), nil, false)),
		"still unknown at the next poll; still blocked is no change")
	assert.Empty(t, w.Observe(m(blockedBoss, agentPane("w1:p1", "c1", team.StatusBlocked), agentPane("w1:p2", "c2", team.StatusUnknown)), nil, false), "unknown is told once")

	w2 := notice.NewWatch()
	w2.Observe(m(nil, agentPane("w1:p2", "c2", team.StatusWorking)), nil, false)
	w2.Observe(m(nil, agentPane("w1:p2", "c2", team.StatusUnknown)), nil, false)
	assert.Empty(t, w2.Observe(m(nil, agentPane("w1:p2", "c2", team.StatusWorking)), nil, false), "an unknown for one poll is not told")
	w2.Observe(m(nil, agentPane("w1:p2", "c2", team.StatusUnknown)), nil, false)
	assert.Equal(t, []string{"w1:p2 gone"}, kinds(w2.Observe(m(nil), nil, false)), "a pane that left is gone, not unknown")

	w3 := notice.NewWatch()
	w3.Observe(m(boss), nil, false)
	assert.Equal(t, []string{"w1:p5 blocked"}, kinds(w3.Observe(m(nil, agentPane("w1:p5", "c5", team.StatusBlocked), shell), nil, false)),
		"a pane that appears blocked; the boss leaving and a shell appearing are not told")
}

// TestWatchUnknownHeld: the second poll's unknown notice, exactly.
func TestWatchUnknownHeld(t *testing.T) {
	t.Parallel()
	w := notice.NewWatch()
	w.Observe(teamOf(nil, agentPane("w1:p2", "c2", team.StatusWorking)), nil, false)
	assert.Empty(t, w.Observe(teamOf(nil, agentPane("w1:p2", "c2", team.StatusUnknown)), nil, false))
	got := w.Observe(teamOf(nil, agentPane("w1:p2", "c2", team.StatusUnknown)), nil, false)
	assert.Equal(t, []string{"w1:p2 unknown"}, kinds(got))
	assert.Equal(t, "c2's status is unknown", got[0].Text)
}

// TestWatchText: who a line names, and the details it carries, each cleaned
// and cut.
func TestWatchText(t *testing.T) {
	t.Parallel()
	long := ""
	for range 100 {
		long += "é"
	}
	tests := map[string]struct {
		worker team.Worker
		want   string
	}{
		"a bound worker by its logical id": {team.Worker{ID: "ha-coder-2", Bound: true, Pane: team.Pane{ID: "w1:p1", Name: "other", Agent: "claude"}}, "ha-coder-2 finished a turn (done)"},
		"an unbound worker by its name":    {team.Worker{ID: "w1:p1", Pane: team.Pane{ID: "w1:p1", Name: "c1", Agent: "claude"}}, "c1 finished a turn (done)"},
		"an unnamed agent by its pane":     {team.Worker{ID: "w1:p1", Pane: team.Pane{ID: "w1:p1", Agent: "codex"}}, "pane w1:p1 finished a turn (done)"},
		"ctx, item and stage": {
			team.Worker{ID: "w1:p1", Pane: team.Pane{ID: "w1:p1", Name: "c1", Agent: "claude", Ctx: "44", Item: "NERD-5322", Stage: "verify"}},
			"c1 finished a turn (done) · ctx 44% · NERD-5322 · verify",
		},
		"a ctx that is not one":          {team.Worker{ID: "w1:p1", Pane: team.Pane{ID: "w1:p1", Name: "c1", Agent: "claude", Ctx: "044", Stage: "s"}}, "c1 finished a turn (done) · s"},
		"ctx 0":                          {team.Worker{ID: "w1:p1", Pane: team.Pane{ID: "w1:p1", Name: "c1", Agent: "claude", Ctx: "0"}}, "c1 finished a turn (done) · ctx 0%"},
		"ctx 100":                        {team.Worker{ID: "w1:p1", Pane: team.Pane{ID: "w1:p1", Name: "c1", Agent: "claude", Ctx: "100"}}, "c1 finished a turn (done) · ctx 100%"},
		"ctx past 100":                   {team.Worker{ID: "w1:p1", Pane: team.Pane{ID: "w1:p1", Name: "c1", Agent: "claude", Ctx: "101"}}, "c1 finished a turn (done)"},
		"a negative ctx":                 {team.Worker{ID: "w1:p1", Pane: team.Pane{ID: "w1:p1", Name: "c1", Agent: "claude", Ctx: "-1"}}, "c1 finished a turn (done)"},
		"a non-ASCII name":               {team.Worker{ID: "w1:p1", Pane: team.Pane{ID: "w1:p1", Name: "エージェント", Agent: "claude", Item: "NERD-5325"}}, "エージェント finished a turn (done) · NERD-5325"},
		"a decomposed name is kept":      {team.Worker{ID: "w1:p1", Pane: team.Pane{ID: "w1:p1", Name: "pée", Agent: "claude"}}, "pée finished a turn (done)"},
		"an option-like name":            {team.Worker{ID: "w1:p1", Pane: team.Pane{ID: "w1:p1", Name: "--rf", Agent: "claude"}}, "--rf finished a turn (done)"},
		"shell metacharacters are text":  {team.Worker{ID: "w1:p1", Pane: team.Pane{ID: "w1:p1", Name: "c1", Agent: "claude", Stage: "$(rm -rf ~); `x` | y > z"}}, "c1 finished a turn (done) · $(rm -rf ~); `x` | y > z"},
		"a byte-order mark and a NUL":    {team.Worker{ID: "w1:p1", Pane: team.Pane{ID: "w1:p1", Name: "\ufeffc1", Agent: "claude", Item: "a\x00b"}}, "c1 finished a turn (done) · a b"},
		"a truncated multibyte sequence": {team.Worker{ID: "w1:p1", Pane: team.Pane{ID: "w1:p1", Name: "c1\xe2\x82", Agent: "claude"}}, "c1� finished a turn (done)"},
		"Latin-1 bytes":                  {team.Worker{ID: "w1:p1", Pane: team.Pane{ID: "w1:p1", Name: "caf\xe9", Agent: "claude"}}, "caf� finished a turn (done)"},
		"control characters are spaces":  {team.Worker{ID: "w1:p1", Pane: team.Pane{ID: "w1:p1", Name: "c\n1", Agent: "claude", Stage: "a\r\n\tb​c\x1b[31m"}}, "c 1 finished a turn (done) · a b c [31m"},
		"blank details are left out":     {team.Worker{ID: "w1:p1", Pane: team.Pane{ID: "w1:p1", Name: "c1", Agent: "claude", Item: " \t", Stage: "\n"}}, "c1 finished a turn (done)"},
		"invalid UTF-8 is replaced":      {team.Worker{ID: "w1:p1", Pane: team.Pane{ID: "w1:p1", Name: "c\xff1", Agent: "claude"}}, "c�1 finished a turn (done)"},
		"a long stage is cut at 80":      {team.Worker{ID: "w1:p1", Pane: team.Pane{ID: "w1:p1", Name: "c1", Agent: "claude", Stage: long}}, "c1 finished a turn (done) · " + long[:2*79] + "…"},
		"a stage of exactly 80 is whole": {team.Worker{ID: "w1:p1", Pane: team.Pane{ID: "w1:p1", Name: "c1", Agent: "claude", Stage: long[:2*80]}}, "c1 finished a turn (done) · " + long[:2*80]},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			tc.worker.Pane.Status = team.StatusDone
			m := teamOf(nil, tc.worker)
			w := notice.NewWatch()
			w.Observe(m, []herdr.AgentInfo{agent("w1:p1", "t1", new(uint64(1)))}, true)
			got := w.Observe(m, []herdr.AgentInfo{agent("w1:p1", "t1", new(uint64(2)))}, true)
			if assert.Len(t, got, 1) {
				assert.Equal(t, tc.want, got[0].Text)
				assert.Equal(t, "w1:p1", got[0].Subject)
			}
		})
	}
}

// TestWatchGoneText names the pane that left, unless the worker is named
// by it.
func TestWatchGoneText(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		worker team.Worker
		want   string
	}{
		"bound":   {team.Worker{ID: "ha-qa-1", Bound: true, Pane: team.Pane{ID: "w1:p7", Agent: "claude", Status: team.StatusIdle, Stage: "qa"}}, "ha-qa-1 left the team (pane w1:p7)"},
		"named":   {team.Worker{ID: "w1:p7", Pane: team.Pane{ID: "w1:p7", Name: "qa", Agent: "claude", Status: team.StatusIdle}}, "qa left the team (pane w1:p7)"},
		"unnamed": {team.Worker{ID: "w1:p7", Pane: team.Pane{ID: "w1:p7", Agent: "codex", Status: team.StatusIdle}}, "pane w1:p7 left the team"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			w := notice.NewWatch()
			w.Observe(teamOf(nil, tc.worker), nil, false)
			got := w.Observe(teamOf(nil), nil, false)
			if assert.Len(t, got, 1) {
				assert.Equal(t, tc.want, got[0].Text)
				assert.Equal(t, notice.Gone, got[0].Kind)
			}
		})
	}
}
