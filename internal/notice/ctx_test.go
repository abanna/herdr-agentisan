package notice_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/notice"
	"github.com/abanna/herdr-agentisan/internal/team"
)

// at is a bound worker, ha-coder-1 on pane w1:p1 in terminal t1 running
// claude, its ctx token as ctx.
func at(ctx string) team.Worker {
	return team.Worker{ID: "ha-coder-1", Bound: true, Pane: team.Pane{ID: "w1:p1", TerminalID: "t1", Agent: "claude", Ctx: ctx}}
}

// defaults are every pane's thresholds, 55, 65 and 75.
func defaults(string, string) []int { return []int{55, 65, 75} }

// levels is each crossing's worker, level and ctx.
func levels(cs []notice.Crossing) []string {
	out := []string{}
	for _, c := range cs {
		out = append(out, fmt.Sprintf("%s %s %d", c.Worker.ID, c.Level, c.Ctx))
	}
	return out
}

// run observes one worker's ctx at each step and is what each step told.
func run(w *notice.CtxWatch, steps ...string) [][]string {
	out := [][]string{}
	for _, ctx := range steps {
		out = append(out, levels(w.Observe(teamOf(nil, at(ctx)))))
	}
	return out
}

// TestCtxWatchTellsEachLevelOnce: each level is told as ctx reaches its
// threshold, once, and a jump past several is told once, at the highest.
func TestCtxWatchTellsEachLevelOnce(t *testing.T) {
	t.Parallel()
	got := run(notice.NewCtxWatch(defaults), "50", "54", "55", "56", "64", "65", "70", "74", "75", "90", "100")
	assert.Equal(t, [][]string{
		{}, {}, {"ha-coder-1 soft 55"}, {}, {}, {"ha-coder-1 hard 65"}, {}, {}, {"ha-coder-1 ceiling 75"}, {}, {},
	}, got)
	assert.Equal(t, [][]string{{}, {"ha-coder-1 ceiling 78"}, {}}, run(notice.NewCtxWatch(defaults), "40", "78", "60"),
		"40 to 78 is one notice, the ceiling's; 60 re-arms hard and the ceiling, and soft stays told")
}

// TestCtxWatchRearms: a level is told again only after a known ctx fell to
// its threshold less 5, or under, which re-arms it and every level above.
func TestCtxWatchRearms(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		steps []string
		want  [][]string
	}{
		"a drop of 5 re-arms the ceiling": {
			[]string{"75", "70", "75"}, [][]string{{"ha-coder-1 ceiling 75"}, {}, {"ha-coder-1 ceiling 75"}},
		},
		"a drop of 4 does not": {
			[]string{"75", "71", "75"}, [][]string{{"ha-coder-1 ceiling 75"}, {}, {}},
		},
		"a drop of 5 under soft re-arms it": {
			[]string{"55", "50", "55"}, [][]string{{"ha-coder-1 soft 55"}, {}, {"ha-coder-1 soft 55"}},
		},
		"a drop of 4 under soft does not": {
			[]string{"55", "51", "55"}, [][]string{{"ha-coder-1 soft 55"}, {}, {}},
		},
		"re-arming the ceiling keeps hard told": {
			[]string{"80", "70", "74", "75"}, [][]string{{"ha-coder-1 ceiling 80"}, {}, {}, {"ha-coder-1 ceiling 75"}},
		},
		"a clear re-arms every level": {
			[]string{"80", "3", "56", "66", "76"},
			[][]string{{"ha-coder-1 ceiling 80"}, {}, {"ha-coder-1 soft 56"}, {"ha-coder-1 hard 66"}, {"ha-coder-1 ceiling 76"}},
		},
		"a drop to hard's threshold less 5 re-arms hard and the ceiling": {
			[]string{"80", "60", "65"}, [][]string{{"ha-coder-1 ceiling 80"}, {}, {"ha-coder-1 hard 65"}},
		},
	}
	for name, tc := range tests {
		assert.Equal(t, tc.want, run(notice.NewCtxWatch(defaults), tc.steps...), name)
	}
}

// TestCtxWatchUnknownTellsAndRearmsNothing: a ctx that is absent or
// expired, not a whole percentage as its writer leaves it, or on a pane
// herdr detects no agent in, is unknown: it tells nothing, and a level told
// before stays told through it.
func TestCtxWatchUnknownTellsAndRearmsNothing(t *testing.T) {
	t.Parallel()
	for _, ctx := range []string{"", "abc", "080", "+80", "80.0", "101", "-1", " 80", "８０", "\xff", "8\x000", "80\r\n", "99999999999999999999"} {
		w := notice.NewCtxWatch(defaults)
		assert.Equal(t, [][]string{{}}, run(w, ctx), "%q tells nothing", ctx)
		w = notice.NewCtxWatch(defaults)
		assert.Equal(t, [][]string{{"ha-coder-1 ceiling 80"}, {}, {}}, run(w, "80", ctx, "80"), "%q re-arms nothing", ctx)
	}
	shell := at("80")
	shell.Pane.Agent = ""
	assert.Empty(t, notice.NewCtxWatch(defaults).Observe(teamOf(nil, shell)), "a pane with no agent")
	named := shell
	named.Pane.Name = "c1"
	assert.Len(t, notice.NewCtxWatch(defaults).Observe(teamOf(nil, named)), 1, "an agent herdr knows only by name counts")
}

// TestCtxWatchRemembersByTerminal: the memory is the terminal's, so a pane
// that moved keeps it, a reissued pane id showing another terminal starts
// fresh, a terminal the model no longer holds is forgotten, and a pane with
// no terminal is never told about.
func TestCtxWatchRemembersByTerminal(t *testing.T) {
	t.Parallel()
	w := notice.NewCtxWatch(defaults)
	require.Len(t, w.Observe(teamOf(nil, at("80"))), 1)

	moved := at("80")
	moved.Pane.ID = "w2:p4"
	assert.Empty(t, w.Observe(teamOf(nil, moved)), "the same terminal on a new pane id")

	reissued := at("80")
	reissued.Pane.TerminalID = "t2"
	assert.Equal(t, []string{"ha-coder-1 ceiling 80"}, levels(w.Observe(teamOf(nil, reissued))), "another terminal on the old pane id")

	assert.Empty(t, w.Observe(teamOf(nil)), "gone")
	assert.Equal(t, []string{"ha-coder-1 ceiling 80"}, levels(w.Observe(teamOf(nil, reissued))), "a terminal back after it was gone is new")

	none := at("80")
	none.Pane.TerminalID = ""
	assert.Empty(t, notice.NewCtxWatch(defaults).Observe(teamOf(nil, none)))
}

// TestCtxWatchCoversTheBoss: the boss is told about its own context, by the
// project's thresholds, before its workers, and each pane once.
func TestCtxWatchCoversTheBoss(t *testing.T) {
	t.Parallel()
	boss := team.Worker{ID: "w1:p0", Pane: team.Pane{ID: "w1:p0", TerminalID: "t0", Agent: "claude", Name: "ha-boss", Ctx: "60"}}
	asked := map[string]bool{}
	w := notice.NewCtxWatch(func(project, group string) []int {
		asked[project+"/"+group] = true
		if group == "" {
			return []int{50, 70, 90}
		}
		return []int{55, 65, 75}
	})
	m := teamOf([]team.Worker{boss}, at("60"))
	m.Projects[0].Groups = append(m.Projects[0].Groups, team.Group{Name: "qa", Workers: []team.Worker{at("60")}})

	got := w.Observe(m)
	require.Len(t, got, 2, "the pane in two places is told about once")
	assert.True(t, got[0].Boss)
	assert.Equal(t, "", got[0].Group)
	assert.Equal(t, team.LevelSoft, got[0].Level)
	assert.Equal(t, 50, got[0].Threshold)
	assert.Equal(t, "you are at 60% context (soft: 50%): finish your unit, then hand off", got[0].Notice(false).Text)
	assert.False(t, got[1].Boss)
	assert.Equal(t, "coder", got[1].Group)
	assert.Equal(t, map[string]bool{"ha/": true, "ha/coder": true}, asked)
}

// TestCtxWatchPerGroupThresholds: each pane's thresholds are its group's.
func TestCtxWatchPerGroupThresholds(t *testing.T) {
	t.Parallel()
	qa := at("45")
	qa.ID, qa.Pane.ID, qa.Pane.TerminalID = "ha-qa-1", "w1:p2", "t2"
	m := teamOf(nil, at("45"))
	m.Projects[0].Groups = append(m.Projects[0].Groups, team.Group{Name: "qa", Workers: []team.Worker{qa}})
	w := notice.NewCtxWatch(func(_, group string) []int {
		if group == "qa" {
			return []int{40, 50, 60}
		}
		return []int{55, 65, 75}
	})
	assert.Equal(t, []string{"ha-qa-1 soft 45"}, levels(w.Observe(m)))
}

// TestCrossingNotice: each level's kind, subject and line, the ceiling's
// saying whether the user was alerted, with the item and stage the pane
// reported, cut and made one line.
func TestCrossingNotice(t *testing.T) {
	t.Parallel()
	worker := at("70")
	worker.Pane.Item, worker.Pane.Stage = "NERD-5261", "verify\nnow"
	unbound := team.Worker{ID: "w1:p3", Pane: team.Pane{ID: "w1:p3", Name: "c3", Agent: "codex"}}
	boss := team.Worker{ID: "w1:p0", Pane: team.Pane{ID: "w1:p0", Name: "ha-boss", Agent: "claude"}}
	tests := map[string]struct {
		c       notice.Crossing
		alerted bool
		kind    notice.Kind
		text    string
	}{
		"soft": {
			c: notice.Crossing{Project: "ha", Worker: worker, Ctx: 56, Level: team.LevelSoft, Threshold: 55}, kind: notice.CtxSoft,
			text: "ha-coder-1 is at 56% context (soft: 55%) · NERD-5261 · verify now: let it finish its unit, then rotate it",
		},
		"hard, unbound": {
			c: notice.Crossing{Project: "ha", Worker: unbound, Ctx: 65, Level: team.LevelHard, Threshold: 65}, kind: notice.CtxHard,
			text: "c3 is at 65% context (hard: 65%): rotate it now",
		},
		"the ceiling, alerted": {
			c: notice.Crossing{Project: "ha", Worker: worker, Ctx: 75, Level: team.LevelCeiling, Threshold: 75}, alerted: true, kind: notice.CtxCeiling,
			text: "ha-coder-1 is at 75% context (ceiling: 75%) · NERD-5261 · verify now: act now; the user has been alerted",
		},
		"the ceiling, not shown": {
			c: notice.Crossing{Project: "ha", Worker: worker, Ctx: 99, Level: team.LevelCeiling, Threshold: 75}, kind: notice.CtxCeiling,
			text: "ha-coder-1 is at 99% context (ceiling: 75%) · NERD-5261 · verify now: act now; the user's alert was not shown",
		},
		"the boss, hard": {
			c: notice.Crossing{Project: "ha", Boss: true, Worker: boss, Ctx: 66, Level: team.LevelHard, Threshold: 65}, kind: notice.CtxHard,
			text: "you are at 66% context (hard: 65%): hand off now",
		},
		"the boss, the ceiling": {
			c: notice.Crossing{Project: "ha", Boss: true, Worker: boss, Ctx: 80, Level: team.LevelCeiling, Threshold: 75}, alerted: true, kind: notice.CtxCeiling,
			text: "you are at 80% context (ceiling: 75%): hand off now; the user has been alerted",
		},
	}
	odd := at("56")
	odd.ID = "ha-cöder-1"
	odd.Pane.Item, odd.Pane.Stage = "NERD\xff-1\x00\r", strings.Repeat("é", 90)
	tests["bytes a worker wrote"] = struct {
		c       notice.Crossing
		alerted bool
		kind    notice.Kind
		text    string
	}{
		c: notice.Crossing{Project: "ha", Worker: odd, Ctx: 56, Level: team.LevelSoft, Threshold: 55}, kind: notice.CtxSoft,
		text: "ha-cöder-1 is at 56% context (soft: 55%) · NERD\uFFFD-1 · " + strings.Repeat("é", 79) + "…: let it finish its unit, then rotate it",
	}
	for name, tc := range tests {
		n := tc.c.Notice(tc.alerted)
		assert.Equal(t, tc.kind, n.Kind, name)
		assert.Equal(t, "ha", n.Project, name)
		assert.Equal(t, tc.c.Worker.Pane.ID, n.Subject, name)
		assert.Equal(t, tc.text, n.Text, name)
		if tc.c.Boss {
			assert.Empty(t, n.Worker, "%s: the boss's own is about no worker, so no mute silences it", name)
		} else {
			assert.Equal(t, notice.WorkerName(tc.c.Worker), n.Worker, name)
		}
	}
	assert.Equal(t, "c3", tests["hard, unbound"].c.Notice(false).Worker)
}

// TestCrossingAlert: the ceiling's toast names the pane as its notice does,
// with its ctx, and asks for the request sound.
func TestCrossingAlert(t *testing.T) {
	t.Parallel()
	worker := notice.Crossing{Project: "ha", Worker: at("78"), Ctx: 78, Level: team.LevelCeiling, Threshold: 75}
	assert.Equal(t, herdr.Notification{Title: "agentisan: ha-coder-1 at 78% context", Body: "past the 75% ceiling; its boss is told to rotate it", Sound: "request"}, worker.Alert())
	boss := notice.Crossing{Project: "ha", Boss: true, Worker: team.Worker{ID: "w1:p0", Pane: team.Pane{ID: "w1:p0", Name: "ha-boss\n"}}, Ctx: 90, Level: team.LevelCeiling, Threshold: 75}
	assert.Equal(t, herdr.Notification{Title: "agentisan: ha-boss at 90% context", Body: "the boss is past the 75% ceiling and is told to hand off", Sound: "request"}, boss.Alert())
}
