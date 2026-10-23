package notice_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
		"a bound worker by its logical id": {team.Worker{ID: "ha-coder-2", Bound: true, Pane: team.Pane{ID: "w1:p1", Name: "other", Agent: "claude"}}, "ha-coder-2 finished a turn"},
		"an unbound worker by its name":    {team.Worker{ID: "w1:p1", Pane: team.Pane{ID: "w1:p1", Name: "c1", Agent: "claude"}}, "c1 finished a turn"},
		"an unnamed agent by its pane":     {team.Worker{ID: "w1:p1", Pane: team.Pane{ID: "w1:p1", Agent: "codex"}}, "pane w1:p1 finished a turn"},
		"ctx, item and stage": {
			team.Worker{ID: "w1:p1", Pane: team.Pane{ID: "w1:p1", Name: "c1", Agent: "claude", Ctx: "44", Item: "NERD-5322", Stage: "verify"}},
			"c1 finished a turn · ctx 44% · NERD-5322 · verify",
		},
		"a ctx that is not one":          {team.Worker{ID: "w1:p1", Pane: team.Pane{ID: "w1:p1", Name: "c1", Agent: "claude", Ctx: "044", Stage: "s"}}, "c1 finished a turn · s"},
		"ctx 0":                          {team.Worker{ID: "w1:p1", Pane: team.Pane{ID: "w1:p1", Name: "c1", Agent: "claude", Ctx: "0"}}, "c1 finished a turn · ctx 0%"},
		"ctx 100":                        {team.Worker{ID: "w1:p1", Pane: team.Pane{ID: "w1:p1", Name: "c1", Agent: "claude", Ctx: "100"}}, "c1 finished a turn · ctx 100%"},
		"ctx past 100":                   {team.Worker{ID: "w1:p1", Pane: team.Pane{ID: "w1:p1", Name: "c1", Agent: "claude", Ctx: "101"}}, "c1 finished a turn"},
		"a negative ctx":                 {team.Worker{ID: "w1:p1", Pane: team.Pane{ID: "w1:p1", Name: "c1", Agent: "claude", Ctx: "-1"}}, "c1 finished a turn"},
		"a non-ASCII name":               {team.Worker{ID: "w1:p1", Pane: team.Pane{ID: "w1:p1", Name: "エージェント", Agent: "claude", Item: "NERD-5325"}}, "エージェント finished a turn · NERD-5325"},
		"a decomposed name is kept":      {team.Worker{ID: "w1:p1", Pane: team.Pane{ID: "w1:p1", Name: "pée", Agent: "claude"}}, "pée finished a turn"},
		"an option-like name":            {team.Worker{ID: "w1:p1", Pane: team.Pane{ID: "w1:p1", Name: "--rf", Agent: "claude"}}, "--rf finished a turn"},
		"shell metacharacters are text":  {team.Worker{ID: "w1:p1", Pane: team.Pane{ID: "w1:p1", Name: "c1", Agent: "claude", Stage: "$(rm -rf ~); `x` | y > z"}}, "c1 finished a turn · $(rm -rf ~); `x` | y > z"},
		"a byte-order mark and a NUL":    {team.Worker{ID: "w1:p1", Pane: team.Pane{ID: "w1:p1", Name: "\ufeffc1", Agent: "claude", Item: "a\x00b"}}, "c1 finished a turn · a b"},
		"a truncated multibyte sequence": {team.Worker{ID: "w1:p1", Pane: team.Pane{ID: "w1:p1", Name: "c1\xe2\x82", Agent: "claude"}}, "c1� finished a turn"},
		"Latin-1 bytes":                  {team.Worker{ID: "w1:p1", Pane: team.Pane{ID: "w1:p1", Name: "caf\xe9", Agent: "claude"}}, "caf� finished a turn"},
		"control characters are spaces":  {team.Worker{ID: "w1:p1", Pane: team.Pane{ID: "w1:p1", Name: "c\n1", Agent: "claude", Stage: "a\r\n\tb​c\x1b[31m"}}, "c 1 finished a turn · a b c [31m"},
		"blank details are left out":     {team.Worker{ID: "w1:p1", Pane: team.Pane{ID: "w1:p1", Name: "c1", Agent: "claude", Item: " \t", Stage: "\n"}}, "c1 finished a turn"},
		"invalid UTF-8 is replaced":      {team.Worker{ID: "w1:p1", Pane: team.Pane{ID: "w1:p1", Name: "c\xff1", Agent: "claude"}}, "c�1 finished a turn"},
		"a long stage is cut at 80":      {team.Worker{ID: "w1:p1", Pane: team.Pane{ID: "w1:p1", Name: "c1", Agent: "claude", Stage: long}}, "c1 finished a turn · " + long[:2*79] + "…"},
		"a stage of exactly 80 is whole": {team.Worker{ID: "w1:p1", Pane: team.Pane{ID: "w1:p1", Name: "c1", Agent: "claude", Stage: long[:2*80]}}, "c1 finished a turn · " + long[:2*80]},
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
// by it, then its ctx, item and stage, as every line does (A44).
func TestWatchGoneText(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		worker team.Worker
		want   string
	}{
		"bound": {
			team.Worker{ID: "ha-qa-1", Bound: true, Pane: team.Pane{ID: "w1:p7", Agent: "claude", Status: team.StatusIdle, Ctx: "44", Item: "PR#59", Stage: "qa"}},
			"ha-qa-1 left the team (pane w1:p7) · ctx 44% · PR#59 · qa",
		},
		"named":   {team.Worker{ID: "w1:p7", Pane: team.Pane{ID: "w1:p7", Name: "qa", Agent: "claude", Status: team.StatusIdle}}, "qa left the team (pane w1:p7)"},
		"unnamed": {team.Worker{ID: "w1:p7", Pane: team.Pane{ID: "w1:p7", Agent: "codex", Status: team.StatusIdle, Stage: "verify"}}, "pane w1:p7 left the team · verify"},
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

// TestWatchAgentExited: an agent that exits while its pane stays open turns
// the pane into a shell, and that is a worker leaving the team: Gone, at
// once, named as the last poll knew it, its status changed or not. The
// boss's own agent and a shell that stays one tell nothing, nor does the
// shell's pane closing later.
func TestWatchAgentExited(t *testing.T) {
	t.Parallel()
	shell := team.Worker{ID: "w1:p2", Pane: team.Pane{ID: "w1:p2", Status: team.StatusUnknown}}
	pee := agentPane("w1:p2", "pee01", team.StatusIdle)
	pee.Pane.Ctx, pee.Pane.Item = "40", "NERD-5325"
	unknown := agentPane("w1:p2", "pee01", team.StatusUnknown)
	bound := agentPane("w1:p2", "", team.StatusWorking)
	bound.ID, bound.Bound = "ha-coder-2", true
	unnamed := agentPane("w1:p2", "", team.StatusDone)
	tests := map[string]struct {
		prev, next team.Model
		want       []string
	}{
		"an agent exits, its pane stays":  {teamOf(nil, pee), teamOf(nil, shell), []string{"pee01's agent exited (pane w1:p2) · ctx 40% · NERD-5325"}},
		"its status did not change":       {teamOf(nil, unknown), teamOf(nil, shell), []string{"pee01's agent exited (pane w1:p2)"}},
		"a bound worker":                  {teamOf(nil, bound), teamOf(nil, shell), []string{"ha-coder-2's agent exited (pane w1:p2)"}},
		"an agent herdr names no name":    {teamOf(nil, unnamed), teamOf(nil, shell), []string{"pane w1:p2's agent exited"}},
		"the boss's own agent":            {teamOf([]team.Worker{pee}), teamOf([]team.Worker{shell}), nil},
		"a worker's pane made the boss's": {teamOf(nil, pee), teamOf([]team.Worker{shell}), nil},
		"a shell stays a shell":           {teamOf(nil, shell), teamOf(nil, shell), nil},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			w := notice.NewWatch()
			w.Observe(tc.prev, nil, true)
			var got []string
			for _, n := range w.Observe(tc.next, nil, true) {
				assert.Equal(t, notice.Gone, n.Kind)
				assert.Equal(t, "w1:p2", n.Subject)
				got = append(got, n.Text)
			}
			assert.Equal(t, tc.want, got)
			assert.Empty(t, w.Observe(teamOf(nil), nil, true), "the shell's pane closing later tells nothing more")
		})
	}
}

// TestWatchAgentExitNeedsAgentList: a poll whose agent.list failed builds
// its model with no names, so an agent herdr knows only by name reads as a
// shell; that is never told as its agent exiting, and its pane's real close
// later is told as it leaving. Both models are built as the poll builds
// them.
func TestWatchAgentExitNeedsAgentList(t *testing.T) {
	t.Parallel()
	ws := []herdr.WorkspaceInfo{{WorkspaceID: "wN", Label: "◆ coders"}}
	panes := []herdr.PaneInfo{{PaneID: "wN:p1", WorkspaceID: "wN", AgentStatus: "working"}}
	agents := []herdr.AgentInfo{{PaneID: "wN:p1", Name: "pee01", TerminalID: "t1"}}
	build := func(panes []herdr.PaneInfo, agents []herdr.AgentInfo) team.Model {
		return team.Build(team.Layout{Workspaces: ws, Panes: panes, Agents: agents}, team.Spaces{}, nil)
	}
	w := notice.NewWatch()
	w.Observe(build(panes, agents), agents, true)
	assert.Empty(t, w.Observe(build(panes, nil), nil, false), "agent.list failed: nothing is told")
	assert.Empty(t, w.Observe(build(panes, agents), agents, true), "it answers again")
	assert.Equal(t, []string{"wN:p1 gone"}, kinds(w.Observe(build(nil, nil), nil, true)), "the real close")
}

// TestWatchAcrossAFailedAgentList: a poll whose agent.list failed knows no
// names, so it keeps each agent pane as the last poll judged it. An exit it
// cannot judge is told by the next poll that answers; a pane that closes,
// and a status that changes, meanwhile are told under the worker's name.
// Each model is built as the poll builds it.
func TestWatchAcrossAFailedAgentList(t *testing.T) {
	t.Parallel()
	ws := []herdr.WorkspaceInfo{{WorkspaceID: "wN", Label: "◆ coders"}}
	pane := func(kind, status string) []herdr.PaneInfo {
		return []herdr.PaneInfo{{PaneID: "wN:p1", WorkspaceID: "wN", Agent: kind, AgentStatus: status}}
	}
	named := []herdr.AgentInfo{{PaneID: "wN:p1", Name: "pee01", TerminalID: "t1"}}
	type poll struct {
		panes  []herdr.PaneInfo
		agents []herdr.AgentInfo
		named  bool
	}
	failed := func(panes []herdr.PaneInfo) poll { return poll{panes, nil, false} }
	answered := func(panes []herdr.PaneInfo, agents []herdr.AgentInfo) poll { return poll{panes, agents, true} }
	tests := map[string]struct {
		polls []poll
		want  []string // each later poll's notices, joined
	}{
		"a detected agent exits as agent.list fails": {
			[]poll{answered(pane("claude", "working"), named), failed(pane("", "unknown")), answered(pane("", "unknown"), nil)},
			[]string{"", "pee01's agent exited (pane wN:p1)"},
		},
		"a named agent lives through it, then exits": {
			[]poll{answered(pane("", "working"), named), failed(pane("", "working")), answered(pane("", "unknown"), nil)},
			[]string{"", "pee01's agent exited (pane wN:p1)"},
		},
		"a named agent's pane closes right after it": {
			[]poll{answered(pane("", "working"), named), failed(pane("", "working")), answered(nil, nil)},
			[]string{"", "pee01 left the team (pane wN:p1)"},
		},
		"a named agent turns blocked as it fails": {
			[]poll{answered(pane("", "working"), named), failed(pane("", "blocked")), answered(pane("", "blocked"), named)},
			[]string{"pee01 is blocked", ""},
		},
		"agent.list drops the name a poll before pane.list drops the agent": {
			[]poll{answered(pane("claude", "working"), named), answered(pane("claude", "working"), nil), answered(pane("", "unknown"), nil)},
			[]string{"", "pee01's agent exited (pane wN:p1)"},
		},
		"a named agent lives through it": {
			[]poll{answered(pane("", "working"), named), failed(pane("", "working")), answered(pane("", "working"), named)},
			[]string{"", ""},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			w := notice.NewWatch()
			var got []string
			for i, p := range tc.polls {
				ns := w.Observe(team.Build(team.Layout{Workspaces: ws, Panes: p.panes, Agents: p.agents}, team.Spaces{}, nil), p.agents, p.named)
				if i == 0 {
					continue
				}
				var texts []string
				for _, n := range ns {
					texts = append(texts, n.Text)
				}
				got = append(got, strings.Join(texts, " | "))
			}
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestWatchUnblocked (A44, Q4): a blocked worker that turns working, idle or
// done is told at once, as no longer blocked; one that turns unknown or
// leaves is told as that instead, and the boss's own never.
func TestWatchUnblocked(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		to   team.Status
		want []string
	}{
		"to working": {team.StatusWorking, []string{"w1:p1 unblocked"}},
		"to idle":    {team.StatusIdle, []string{"w1:p1 unblocked"}},
		"to done":    {team.StatusDone, []string{"w1:p1 unblocked"}},
		"to unknown": {team.StatusUnknown, []string{}},
		"to blocked": {team.StatusBlocked, []string{}},
		// A status herdr does not name here is no answer it knows of.
		"to a status herdr does not name": {team.Status("asleep"), []string{}},
		"to no status":                    {team.Status(""), []string{}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			w := notice.NewWatch()
			w.Observe(teamOf(nil, agentPane("w1:p1", "c1", team.StatusWorking)), nil, false)
			w.Observe(teamOf(nil, agentPane("w1:p1", "c1", team.StatusBlocked)), nil, false)
			got := w.Observe(teamOf(nil, agentPane("w1:p1", "c1", tc.to)), nil, false)
			assert.Equal(t, tc.want, kinds(got))
			if len(got) == 1 {
				assert.Equal(t, "c1 is no longer blocked", got[0].Text)
				assert.Equal(t, "ha", got[0].Project)
			}
		})
	}

	w := notice.NewWatch()
	w.Observe(teamOf(nil, agentPane("w1:p1", "c1", team.StatusWorking)), nil, false)
	assert.Empty(t, w.Observe(teamOf(nil, agentPane("w1:p1", "c1", team.StatusIdle)), nil, false), "working to idle is no unblock")

	boss := notice.NewWatch()
	boss.Observe(teamOf([]team.Worker{agentPane("w1:pB", "boss", team.StatusBlocked)}), nil, false)
	assert.Empty(t, boss.Observe(teamOf([]team.Worker{agentPane("w1:pB", "boss", team.StatusWorking)}), nil, false), "the boss's own panes are never told")

	gone := notice.NewWatch()
	gone.Observe(teamOf(nil, agentPane("w1:p1", "c1", team.StatusBlocked)), nil, false)
	assert.Equal(t, []string{"w1:p1 gone"}, kinds(gone.Observe(teamOf(nil), nil, false)), "a blocked worker that leaves is gone")
}

// TestWatchNamesTheWorker: every notice about a worker carries the name a
// mute takes (WorkerName): its logical id when bound, else its agent's
// name, else its pane id; the started line carries none.
func TestWatchNamesTheWorker(t *testing.T) {
	t.Parallel()
	bound := team.Worker{ID: "ha-coder-2", Bound: true, Pane: team.Pane{ID: "w1:p1", Name: "other", Agent: "claude", Status: team.StatusWorking}}
	named := team.Worker{ID: "w1:p2", Pane: team.Pane{ID: "w1:p2", Name: "c2", Agent: "claude", Status: team.StatusWorking}}
	unnamed := team.Worker{ID: "w1:p3", Pane: team.Pane{ID: "w1:p3", Agent: "codex", Status: team.StatusWorking}}
	with := func(s team.Status, ws ...team.Worker) []team.Worker {
		out := make([]team.Worker, len(ws))
		for i, w := range ws {
			w.Pane.Status = s
			out[i] = w
		}
		return out
	}
	workers := func(ns []notice.Notice) []string {
		out := []string{}
		for _, n := range ns {
			out = append(out, n.Subject+" "+string(n.Kind)+" "+n.Worker)
		}
		return out
	}
	agents := func(seq uint64) []herdr.AgentInfo {
		return []herdr.AgentInfo{agent("w1:p1", "t1", new(seq)), agent("w1:p2", "t2", new(seq)), agent("w1:p3", "t3", new(seq))}
	}
	w := notice.NewWatch()
	started := w.Observe(teamOf(nil, bound, named, unnamed), agents(1), true)
	assert.Equal(t, []string{"ha started "}, workers(started), "the started line is about no worker")

	assert.Equal(t, []string{
		"w1:p1 turn_ended ha-coder-2", "w1:p2 turn_ended c2", "w1:p3 turn_ended w1:p3",
	}, workers(w.Observe(teamOf(nil, bound, named, unnamed), agents(2), true)))
	assert.Equal(t, []string{
		"w1:p1 blocked ha-coder-2", "w1:p2 blocked c2", "w1:p3 blocked w1:p3",
	}, workers(w.Observe(teamOf(nil, with(team.StatusBlocked, bound, named, unnamed)...), agents(2), true)))
	assert.Equal(t, []string{
		"w1:p1 unblocked ha-coder-2", "w1:p2 unblocked c2", "w1:p3 unblocked w1:p3",
	}, workers(w.Observe(teamOf(nil, with(team.StatusWorking, bound, named, unnamed)...), agents(2), true)))
	w.Observe(teamOf(nil, with(team.StatusUnknown, bound, named, unnamed)...), agents(2), true)
	assert.Equal(t, []string{
		"w1:p1 unknown ha-coder-2", "w1:p2 unknown c2", "w1:p3 unknown w1:p3",
	}, workers(w.Observe(teamOf(nil, with(team.StatusUnknown, bound, named, unnamed)...), agents(2), true)))

	shell := named
	shell.Pane.Agent, shell.Pane.Name = "", ""
	assert.Equal(t, []string{"w1:p2 gone c2", "w1:p3 gone w1:p3"},
		workers(w.Observe(teamOf(nil, with(team.StatusUnknown, bound, shell)...), agents(2)[:1], true)),
		"an exit and a close each name the worker as the last poll did")
}

// TestWorkerName: the name a notice carries and a mute takes, as herdr
// reports it, never cleaned for a line.
func TestWorkerName(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "ha-coder-2", notice.WorkerName(team.Worker{ID: "ha-coder-2", Bound: true, Pane: team.Pane{ID: "w1:p1", Name: "other"}}))
	assert.Equal(t, "c\n1", notice.WorkerName(team.Worker{ID: "w1:p1", Pane: team.Pane{ID: "w1:p1", Name: "c\n1"}}))
	assert.Equal(t, "w1:p1", notice.WorkerName(team.Worker{ID: "w1:p1", Pane: team.Pane{ID: "w1:p1"}}))
}

// TestWatchFreshUnknown is the cutover's F3: an agent pane never seen in a
// known status is told unknown only at the 20th poll in a row that judged
// it to run an agent, counted from the first, and once; a spawned agent
// herdr has not yet classified is no news. One that shows a known status
// first keeps the two-poll rule, and one that stops running an agent and
// comes back starts again.
func TestWatchFreshUnknown(t *testing.T) {
	t.Parallel()
	fresh := func(status team.Status) team.Model { return teamOf(nil, agentPane("w1:p3", "c3", status)) }
	// polls observes m n times and returns every notice's kind, by poll.
	polls := func(w *notice.Watch, m team.Model, n int) map[int][]string {
		out := map[int][]string{}
		for i := 1; i <= n; i++ {
			if got := kinds(w.Observe(m, nil, false)); len(got) > 0 {
				out[i] = got
			}
		}
		return out
	}

	t.Run("a spawned agent, unknown from the start", func(t *testing.T) {
		t.Parallel()
		w := notice.NewWatch()
		w.Observe(teamOf(nil), nil, false)
		got := polls(w, fresh(team.StatusUnknown), 25)
		assert.Equal(t, map[int][]string{20: {"w1:p3 unknown"}}, got, "polls 1 to 19 silent, the 20th told, and once")
	})
	t.Run("unknown at the daemon's first poll", func(t *testing.T) {
		t.Parallel()
		w := notice.NewWatch()
		got := polls(w, fresh(team.StatusUnknown), 21)
		assert.Equal(t, map[int][]string{1: {"ha started"}, 20: {"w1:p3 unknown"}}, got, "the baseline is its first poll")
	})
	t.Run("classified within the grace", func(t *testing.T) {
		t.Parallel()
		w := notice.NewWatch()
		w.Observe(teamOf(nil), nil, false)
		assert.Empty(t, polls(w, fresh(team.StatusUnknown), 19))
		assert.Empty(t, polls(w, fresh(team.StatusIdle), 1), "known at its 20th poll: never told")
		assert.Empty(t, polls(w, fresh(team.StatusIdle), 5))
	})
	t.Run("known, then unknown: the two-poll rule", func(t *testing.T) {
		t.Parallel()
		w := notice.NewWatch()
		w.Observe(teamOf(nil), nil, false)
		polls(w, fresh(team.StatusUnknown), 3)
		polls(w, fresh(team.StatusWorking), 1)
		got := polls(w, fresh(team.StatusUnknown), 22)
		assert.Equal(t, map[int][]string{2: {"w1:p3 unknown"}}, got, "told at its second unknown poll, not after twenty")
	})
	t.Run("gone and back counts again", func(t *testing.T) {
		t.Parallel()
		w := notice.NewWatch()
		w.Observe(teamOf(nil), nil, false)
		assert.Empty(t, polls(w, fresh(team.StatusUnknown), 10))
		assert.Equal(t, []string{"w1:p3 gone"}, kinds(w.Observe(teamOf(nil), nil, false)))
		got := polls(w, fresh(team.StatusUnknown), 20)
		assert.Equal(t, map[int][]string{20: {"w1:p3 unknown"}}, got, "its count starts again from 1")
	})
	t.Run("known, gone and back is fresh again", func(t *testing.T) {
		t.Parallel()
		w := notice.NewWatch()
		w.Observe(teamOf(nil), nil, false)
		polls(w, fresh(team.StatusIdle), 2)
		assert.Equal(t, []string{"w1:p3 gone"}, kinds(w.Observe(teamOf(nil), nil, false)))
		got := polls(w, fresh(team.StatusUnknown), 20)
		assert.Equal(t, map[int][]string{20: {"w1:p3 unknown"}}, got, "a new agent there is not the known one")
	})
	t.Run("a shell's polls do not count", func(t *testing.T) {
		t.Parallel()
		w := notice.NewWatch()
		w.Observe(teamOf(nil), nil, false)
		shell := teamOf(nil, team.Worker{ID: "w1:p3", Pane: team.Pane{ID: "w1:p3", Status: team.StatusUnknown}})
		assert.Empty(t, polls(w, shell, 30))
		got := polls(w, fresh(team.StatusUnknown), 20)
		assert.Equal(t, map[int][]string{20: {"w1:p3 unknown"}}, got, "counted from the poll that first judged it to run an agent")
	})
}

// TestWatchFreshUnknownNamesItsTeamAndWorker: the unknown notice a
// never-known agent pane gets at its 20th poll names its project, the team
// whose boss gets it, and its worker, the name a mute takes, as the two-poll
// notice does. TestWatchFreshUnknown compares Subject and Kind alone.
func TestWatchFreshUnknownNamesItsTeamAndWorker(t *testing.T) {
	t.Parallel()
	fresh := teamOf(nil, agentPane("w1:p3", "c3", team.StatusUnknown))
	w := notice.NewWatch()
	w.Observe(teamOf(nil), nil, false)
	var got []notice.Notice
	for range 20 {
		got = append(got, w.Observe(fresh, nil, false)...)
	}
	require.Len(t, got, 1)
	assert.Equal(t, "ha", got[0].Project, "the notice goes to its team's boss")
	assert.Equal(t, "w1:p3", got[0].Subject)
	assert.Equal(t, "c3", got[0].Worker, "a mute of c3 silences it")
	assert.Equal(t, notice.Unknown, got[0].Kind)
	assert.Contains(t, got[0].Text, "c3's status is unknown")

	// The two-poll notice for a pane seen known carries the same fields.
	k := notice.NewWatch()
	k.Observe(teamOf(nil), nil, false)
	k.Observe(teamOf(nil, agentPane("w1:p3", "c3", team.StatusIdle)), nil, false)
	var two []notice.Notice
	for range 2 {
		two = append(two, k.Observe(fresh, nil, false)...)
	}
	require.Len(t, two, 1)
	assert.Equal(t, two[0].Project, got[0].Project)
	assert.Equal(t, two[0].Worker, got[0].Worker)
}
