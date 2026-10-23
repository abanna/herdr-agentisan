package workforce_test

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/report"
	"github.com/abanna/herdr-agentisan/internal/settings"
	"github.com/abanna/herdr-agentisan/internal/team"
	"github.com/abanna/herdr-agentisan/internal/workforce"
)

// TestStatus: every configured group with its counts, and any type found in
// herdr; members named by row, else tab label; ctx null unless a percentage
// on a pane herdr detects an agent in (ADR-001 A27); idle_s from the poll's
// clock; the boss's panes apart, named by pane id.
func TestStatus(t *testing.T) {
	t.Parallel()
	f := newFixture(t, teamConfig)
	coder := f.h.addTab(f.ws, "ha-coder-1", "claude", "idle")
	f.bindRow(t, "ha-coder-1", "coder", coder)
	hand := f.h.addTab(f.ws, "ha-designer-1", "claude", "working")
	split := f.h.addPane(f.boss, "", "unknown")
	f.h.setPane(coder, func(p *fakePane) {
		p.tokens = map[string]string{"ctx": "42", "item": "NERD-5254", "stage": "build_test"}
	})
	f.h.setPane(hand, func(p *fakePane) { p.tokens = map[string]string{"ctx": "42%"} })
	f.h.setPane(split, func(p *fakePane) { p.name, p.tokens = "ha-hooked", map[string]string{"ctx": "61"} })
	t0 := time.Date(2026, 10, 19, 21, 0, 0, 0, time.UTC)
	f.svc.Now = func() time.Time { return t0.Add(90 * time.Second) }
	f.svc.Idle.Observe(team.Model{Projects: []team.Project{{Name: "herdr-agentisan", Groups: []team.Group{{Name: "coder", Workers: []team.Worker{{Pane: team.Pane{ID: coder, Status: team.StatusIdle}}}}}}}}, t0)

	got, err := f.svc.Status(t.Context(), workforce.StatusRequest{})
	require.NoError(t, err)
	ctx42, idle90 := 42, int64(90)
	assert.Equal(t, "herdr-agentisan", got.Project)
	assert.Equal(t, "tabs", got.Layout)
	assert.Equal(t, []string{f.ws}, got.Workspaces)
	require.Len(t, got.Groups, 4)
	assert.Equal(t, workforce.GroupStatus{
		Name: "coder", Workers: 2, Slots: 3, Occupied: 1, Open: 2, Configured: true, Targets: []string{f.ws},
		Members: []workforce.Member{{ID: "ha-coder-1", Recorded: true, PaneID: coder, Agent: "claude", Status: "idle", Ctx: &ctx42, Item: "NERD-5254", Stage: "build_test", IdleS: &idle90}},
	}, got.Groups[0])
	assert.Equal(t, workforce.GroupStatus{Name: "qa", Workers: 1, Slots: 1, Open: 1, Configured: true, Targets: []string{}, Members: []workforce.Member{}}, got.Groups[1])
	assert.Equal(t, "review", got.Groups[2].Name)
	assert.Equal(t, workforce.GroupStatus{
		Name: "designer", Occupied: 1, Targets: []string{f.ws},
		Members: []workforce.Member{{ID: "ha-designer-1", PaneID: hand, Agent: "claude", Status: "working"}},
	}, got.Groups[3], "a type found but not configured has no slots, and a ctx that is not a percentage is null")
	assert.Equal(t, []workforce.Member{
		{ID: f.boss, PaneID: f.boss, Agent: "claude", Status: "working"},
		{ID: split, PaneID: split, Status: "unknown"},
	}, got.Boss, "named by pane id: the boss's tab holds more than the boss; a ctx a Codex left in a named shell is null")

	f.svc.Now = func() time.Time { return t0.Add(-5 * time.Second) }
	got, err = f.svc.Status(t.Context(), workforce.StatusRequest{})
	require.NoError(t, err)
	assert.Equal(t, int64(0), *got.Groups[0].Members[0].IdleS, "a clock stepped back reports no rest, not a negative one")
}

// TestStatusShowsACodexWorker (NERD-5507): a Codex worker's member shows its
// agent, status and ctx as a Claude's does, and a Codex started by hand in
// a group's tab takes the group's slot. A reviewer the fixture adds by its
// group alone is a Codex, as review's default provider says.
func TestStatusShowsACodexWorker(t *testing.T) {
	t.Parallel()
	f := newFixture(t, teamConfig)
	w := f.workerAs(t, "ha-coder-1", "coder", "codex")
	f.h.setPane(w.PaneID, func(p *fakePane) { p.status, p.tokens = "working", map[string]string{"ctx": "55"} })
	hand := f.h.addTab(f.ws, "ha-qa-2", "codex", "done")
	reviewer := f.worker(t, "ha-review-1", "review")

	got, err := f.svc.Status(t.Context(), workforce.StatusRequest{})
	require.NoError(t, err)
	ctx55 := 55
	require.Len(t, got.Groups, 3)
	assert.Equal(t, workforce.GroupStatus{
		Name: "coder", Workers: 2, Slots: 3, Occupied: 1, Open: 2, Configured: true, Targets: []string{f.ws},
		Members: []workforce.Member{{ID: "ha-coder-1", Recorded: true, PaneID: w.PaneID, Agent: "codex", Status: "working", Ctx: &ctx55}},
	}, got.Groups[0])
	assert.Equal(t, workforce.GroupStatus{
		Name: "qa", Workers: 1, Slots: 1, Occupied: 1, Configured: true, Targets: []string{f.ws},
		Members: []workforce.Member{{ID: "ha-qa-2", PaneID: hand, Agent: "codex", Status: "done"}},
	}, got.Groups[1], "a Codex started by hand takes the slot")
	assert.Equal(t, []workforce.Member{{ID: "ha-review-1", Recorded: true, PaneID: reviewer.PaneID, Agent: "codex", Status: "idle"}}, got.Groups[2].Members, "review's workers are Codex")
}

// TestStatusLeavesOutTheDashboard is the cutover's F2: the team's dashboard
// pane in the boss's tab, labelled as team up labels it or with the title
// prefix+d gives it, is no boss member; a plain shell split there still is.
func TestStatusLeavesOutTheDashboard(t *testing.T) {
	t.Parallel()
	f := upFixture(t, teamConfig)
	byTeamUp := f.h.addPane(f.boss, "", "unknown")
	f.h.setPane(byTeamUp, func(p *fakePane) { p.label = "ha-dashboard" })
	byKey := f.h.addPane(f.boss, "", "unknown")
	f.h.setPane(byKey, func(p *fakePane) { p.label = dashTitle })
	split := f.h.addPane(f.boss, "", "unknown")

	got, err := f.svc.Status(t.Context(), workforce.StatusRequest{})
	require.NoError(t, err)
	assert.Equal(t, []workforce.Member{
		{ID: f.boss, PaneID: f.boss, Agent: "claude", Status: "working"},
		{ID: split, PaneID: split, Status: "unknown"},
	}, got.Boss)
}

// TestStatusStage: each member's stage carries its stage_at, in UTC, and
// stage_stale from the poll's clock, the same rule the snapshot uses (ADR-001
// A42): the coder worked a turn the clock saw whole and reported nothing in
// it; the designer reported during its turn; the boss's named shell, whose
// agent herdr no longer detects, is stale whatever the clock saw, and a
// stage_at that is no time is null.
func TestStatusStage(t *testing.T) {
	t.Parallel()
	f := newFixture(t, teamConfig)
	coder := f.h.addTab(f.ws, "ha-coder-1", "claude", "idle")
	f.bindRow(t, "ha-coder-1", "coder", coder)
	hand := f.h.addTab(f.ws, "ha-designer-1", "claude", "idle")
	split := f.h.addPane(f.boss, "", "unknown")
	t0 := time.Date(2026, 10, 19, 21, 0, 0, 0, time.UTC)
	reported := t0.Add(-time.Hour)
	f.h.setPane(coder, func(p *fakePane) {
		p.tokens = map[string]string{"item": "NERD-5254", "stage": "build_test", "stage_at": strconv.FormatInt(reported.Unix(), 10)}
	})
	f.h.setPane(hand, func(p *fakePane) {
		p.tokens = map[string]string{"stage": "review", "stage_at": strconv.FormatInt(t0.Add(4*time.Second).Unix(), 10)}
	})
	f.h.setPane(split, func(p *fakePane) {
		p.name, p.tokens = "ha-hooked", map[string]string{"stage": "waiting: boss", "stage_at": "soon"}
	})
	model := func(st team.Status) team.Model {
		pane := func(id string) team.Worker {
			return team.Worker{Pane: team.Pane{ID: id, Agent: "claude", Status: st, Stage: "build_test"}}
		}
		return team.Model{Projects: []team.Project{{Name: "herdr-agentisan", Groups: []team.Group{{Name: "coder", Workers: []team.Worker{pane(coder)}}}}}}
	}
	for i, st := range []team.Status{team.StatusIdle, team.StatusWorking, team.StatusIdle, team.StatusIdle} {
		f.svc.Idle.Observe(model(st), t0.Add(time.Duration(3*i)*time.Second))
	}
	f.svc.Now = func() time.Time { return t0.Add(90 * time.Second) }

	got, err := f.svc.Status(t.Context(), workforce.StatusRequest{})
	require.NoError(t, err)
	members := map[string]workforce.Member{}
	for _, g := range got.Groups {
		for _, m := range g.Members {
			members[m.PaneID] = m
		}
	}
	for _, m := range got.Boss {
		members[m.PaneID] = m
	}
	require.Contains(t, members, coder)
	assert.True(t, members[coder].StageStale, "a turn that reported nothing has ended")
	require.NotNil(t, members[coder].StageAt)
	assert.Equal(t, reported, *members[coder].StageAt)
	assert.Equal(t, time.UTC, members[coder].StageAt.Location())
	assert.False(t, members[hand].StageStale, "the clock saw no turn of it")
	require.NotNil(t, members[hand].StageAt)
	assert.True(t, members[split].StageStale, "no agent detected")
	assert.Nil(t, members[split].StageAt)

	raw, err := json.Marshal(members[coder])
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"stage_stale":true,"stage_at":"2026-10-19T20:00:00Z"`)
	raw, err = json.Marshal(members[split])
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"stage_stale":true,"stage_at":null`)
}

// TestStatusCountsEveryTakenSlot: an agent started by hand takes a slot, and
// so does a pane a live row binds whose agent is not up yet; a row whose
// terminal herdr no longer lists takes none, and nor does a lost one, whose
// pane id is only where it was last seen (A35). Both are listed unbound, and
// another team's rows are not.
func TestStatusCountsEveryTakenSlot(t *testing.T) {
	t.Parallel()
	f := newFixture(t, teamConfig)
	f.h.addTab(f.ws, "ha-coder-7", "claude", "working")
	starting := f.h.addTab(f.ws, "ha-coder-1", "", "unknown")
	f.bindRow(t, "ha-coder-1", "coder", starting)
	f.h.addTab(f.ws, "ha-coder-2", "", "unknown")  // a shell, bound by nothing
	f.bindRow(t, "ha-coder-3", "coder", "w10:p99") // its pane is gone
	reissued := f.h.addTab(f.ws, "ha-coder-4", "claude", "idle")
	f.bindRow(t, "ha-coder-4", "coder", reissued)
	f.markLost(t, "ha-coder-4")
	f.bindRow(t, "zz-coder-1", "coder", "w10:p98")
	f.moveProject(t, "zz-coder-1", "other")

	got, err := f.svc.Status(t.Context(), workforce.StatusRequest{})
	require.NoError(t, err)
	assert.Equal(t, 3, got.Groups[0].Occupied, "the lost row's pane still runs an agent")
	assert.Equal(t, 0, got.Groups[0].Open)
	require.Len(t, got.Groups[0].Members, 4)
	assert.False(t, got.Groups[0].Members[3].Recorded, "a lost row binds nothing: %+v", got.Groups[0].Members[3])
	assert.Equal(t, []workforce.UnboundWorker{
		{ID: "ha-coder-3", Group: "coder", PaneID: "w10:p99", Status: "active"},
		{ID: "ha-coder-4", Group: "coder", PaneID: reissued, Status: "lost"},
	}, got.Unbound)
}

// TestStatusOrdersTheGroupsItFinds: the team model's types first, then every
// other type, configured or found in herdr, by name; a team herdr shows no
// workspace of has no model order, and is by name.
func TestStatusOrdersTheGroupsItFinds(t *testing.T) {
	t.Parallel()
	f := newFixture(t, teamConfig+"\n[projects.herdr-agentisan.groups.zeta]\nworkers = 0\nslots = 1\n")
	for _, label := range []string{"ha-mu-1", "ha-alpha-1", "ha-beta-1"} {
		f.h.addTab(f.ws, label, "claude", "idle")
	}

	got, err := f.svc.Status(t.Context(), workforce.StatusRequest{})
	require.NoError(t, err)
	var names []string
	for _, g := range got.Groups {
		names = append(names, g.Name)
	}
	assert.Equal(t, []string{"coder", "qa", "review", "alpha", "beta", "mu", "zeta"}, names)

	g := newFixture(t, teamConfig+"\n[projects.assay]\nrepo = \"REPO\"\nshort = \"as\"\n[projects.assay.groups.research]\nworkers = 0\nslots = 1\n[projects.assay.groups.librarian]\nworkers = 0\nslots = 1\n")
	got, err = g.svc.Status(t.Context(), workforce.StatusRequest{Selector: workforce.Selector{Project: "assay"}})
	require.NoError(t, err)
	names = nil
	for _, gs := range got.Groups {
		names = append(names, gs.Name)
	}
	assert.Equal(t, []string{"librarian", "research"}, names, "not the types' order, research before librarian")
}

// TestStatusOfATeamWithNoWorkspace: a configured tabs team herdr shows no
// workspace for lists its configured groups by name, each with every slot
// open and nothing found, and answers empty lists, never null: workspaces,
// each group's targets and members, the boss and the unbound rows (QA #39
// item 4).
func TestStatusOfATeamWithNoWorkspace(t *testing.T) {
	t.Parallel()
	f := newFixture(t, teamConfig+"\n[projects.assay]\nrepo = \"REPO\"\nshort = \"as\"\n[projects.assay.groups.research]\nworkers = 0\nslots = 1\n[projects.assay.groups.qa]\nworkers = 1\nslots = 2\n")

	got, err := f.svc.Status(t.Context(), workforce.StatusRequest{Selector: workforce.Selector{Project: "assay"}})
	require.NoError(t, err)
	assert.Equal(t, workforce.StatusResult{
		Project: "assay", Layout: "tabs", Workspaces: []string{},
		Groups: []workforce.GroupStatus{
			{Name: "qa", Workers: 1, Slots: 2, Occupied: 0, Open: 2, Configured: true, Targets: []string{}, Members: []workforce.Member{}},
			{Name: "research", Workers: 0, Slots: 1, Occupied: 0, Open: 1, Configured: true, Targets: []string{}, Members: []workforce.Member{}},
		},
		Boss: []workforce.Member{}, Unbound: []workforce.UnboundWorker{},
	}, got, "empty lists, not nil ones")
	raw, err := json.Marshal(got)
	require.NoError(t, err)
	assert.JSONEq(t, `{"project":"assay","layout":"tabs","workspaces":[],"groups":[`+
		`{"name":"qa","workers":1,"slots":2,"occupied":0,"open":2,"configured":true,"targets":[],"members":[]},`+
		`{"name":"research","workers":0,"slots":1,"occupied":0,"open":1,"configured":true,"targets":[],"members":[]}],`+
		`"boss":[],"unbound":[]}`, string(raw))
}

// TestStatusOfADescriptionOnlyGroup: a group that only describes is no
// worker type (ADR-001 A36), so it is listed only as herdr shows it, never
// as configured; a group that sets both counts to 0 is still one.
func TestStatusOfADescriptionOnlyGroup(t *testing.T) {
	t.Parallel()
	f := newFixture(t, teamConfig+`
[projects.herdr-agentisan.groups.clerk]
description = "keeps Linear in sync"

[projects.herdr-agentisan.groups.zeta]
workers = 0
slots = 0
description = "parked"
`)
	byName := func() map[string]workforce.GroupStatus {
		t.Helper()
		got, err := f.svc.Status(t.Context(), workforce.StatusRequest{})
		require.NoError(t, err)
		out := map[string]workforce.GroupStatus{}
		for _, g := range got.Groups {
			out[g.Name] = g
		}
		return out
	}

	groups := byName()
	assert.NotContains(t, groups, "clerk", "a group that only describes, which herdr does not show, is not listed")
	assert.Equal(t, workforce.GroupStatus{Name: "zeta", Configured: true, Targets: []string{}, Members: []workforce.Member{}}, groups["zeta"], "a group with both counts set to 0 is a configured type")

	clerk := f.h.addTab(f.ws, "ha-clerk-1", "claude", "idle")
	assert.Equal(t, workforce.GroupStatus{
		Name: "clerk", Occupied: 1, Targets: []string{f.ws},
		Members: []workforce.Member{{ID: "ha-clerk-1", PaneID: clerk, Agent: "claude", Status: "idle"}},
	}, byName()["clerk"], "one herdr shows is listed as found, not configured")

	only := newFixture(t, "schema_version = 1\n[projects.herdr-agentisan]\nrepo = \"REPO\"\nshort = \"ha\"\nlayout = \"tabs\"\n[projects.herdr-agentisan.groups.clerk]\ndescription = \"keeps Linear in sync\"\n")
	got, err := only.svc.Status(t.Context(), workforce.StatusRequest{})
	require.NoError(t, err)
	assert.Empty(t, got.Groups, "a team whose every group only describes has no worker type")
	assert.NotNil(t, got.Groups, "an empty list, not null")
}

// TestStatusOfTheSpacesProject: --project names the ◆ spaces project too,
// whose groups are its spaces, in their own order; the default never picks
// it.
func TestStatusOfTheSpacesProject(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "schema_version = 1\n[projects.agentisan]\nrepo = \"REPO\"\n[projects.agentisan.groups.coders]\nworkers = 1\nslots = 2\n")
	coders := f.h.addWorkspace("◆ coders")
	p := f.h.addTab(coders, "1", "claude", "working")

	got, err := f.svc.Status(t.Context(), workforce.StatusRequest{Selector: workforce.Selector{Project: "agentisan"}})
	require.NoError(t, err)
	assert.Equal(t, "spaces", got.Layout)
	require.Len(t, got.Groups, 1)
	assert.Equal(t, []workforce.Member{{ID: p, PaneID: p, Agent: "claude", Status: "working"}}, got.Groups[0].Members, "a space's pane is named by its pane id, not its tab")
	assert.Equal(t, 1, got.Groups[0].Occupied)
	assert.Equal(t, 1, got.Groups[0].Open)

	_, err = f.svc.Status(t.Context(), workforce.StatusRequest{})
	require.ErrorIs(t, err, workforce.ErrBadRequest, "the default never picks the spaces project")

	for _, space := range []string{"◆ codex", "◆ qa"} {
		f.h.addTab(f.h.addWorkspace(space), "1", "claude", "idle")
	}
	got, err = f.svc.Status(t.Context(), workforce.StatusRequest{Selector: workforce.Selector{Project: "agentisan"}})
	require.NoError(t, err)
	var names []string
	for _, g := range got.Groups {
		names = append(names, g.Name)
	}
	assert.Equal(t, []string{"coders", "qa", "codex"}, names, "the spaces' own order (v12's), not the tabs teams' or by name")
}

// TestStatusPicksTheTeam: --project wins; else the team whose workspace holds
// the caller's pane, as FindCallerPane confirmed it; else the only tabs team;
// else it is refused. No caller pane, as when FindCallerPane confirms none,
// takes the fallbacks.
func TestStatusPicksTheTeam(t *testing.T) {
	t.Parallel()
	two := teamConfig + "\n[projects.assay]\nrepo = \"REPO\"\nshort = \"as\"\n[projects.assay.groups.coder]\nworkers = 0\nslots = 2\n"
	f := newFixture(t, two)
	as := f.h.addWorkspace("assay")
	asBoss := f.h.addTab(as, "as-boss", "claude", "idle")

	for name, tc := range map[string]struct {
		sel  workforce.Selector
		want string
	}{
		"the caller's team":                 {sel: workforce.Selector{CallerPane: asBoss}, want: "assay"},
		"--project beats the caller's pane": {sel: workforce.Selector{Project: "herdr-agentisan", CallerPane: asBoss}, want: "herdr-agentisan"},
	} {
		got, err := f.svc.Status(t.Context(), workforce.StatusRequest{Selector: tc.sel})
		require.NoError(t, err, name)
		assert.Equal(t, tc.want, got.Project, name)
	}

	_, err := f.svc.Status(t.Context(), workforce.StatusRequest{Selector: workforce.Selector{CallerPane: "w99:p9"}})
	require.ErrorIs(t, err, workforce.ErrBadRequest, "two tabs teams and the caller in neither")
	_, err = f.svc.Status(t.Context(), workforce.StatusRequest{})
	require.ErrorIs(t, err, workforce.ErrBadRequest, "two tabs teams and no caller pane")
	assert.Contains(t, err.Error(), "assay, herdr-agentisan")
	_, err = f.svc.Status(t.Context(), workforce.StatusRequest{Selector: workforce.Selector{Project: "nope"}})
	require.ErrorIs(t, err, workforce.ErrUnknownProject)
	assert.Contains(t, err.Error(), "configured: assay, herdr-agentisan")

	g := newFixture(t, teamConfig)
	got, err := g.svc.Status(t.Context(), workforce.StatusRequest{})
	require.NoError(t, err)
	assert.Equal(t, "herdr-agentisan", got.Project, "no caller pane falls back to the only tabs team")

	broken := newFixture(t, "schema_version = 1\n[projects.herdr-agentisan]\nrepo = \"REPO\"\nlayout = \"grid\"\n")
	_, err = broken.svc.Status(t.Context(), workforce.StatusRequest{})
	require.ErrorIs(t, err, workforce.ErrConfig)
}

// TestACallerInASpaceIsTheSpacesProjects: a caller whose pane is in a ◆
// space, ◆ boss included, is the spaces project's, never the only tabs
// team's: team status reports that project, and spawn and team up refuse it
// (no_target), opening nothing. With no [spaces] project configured the
// spaces are the unnamed project's, which has no configuration, and every op
// refuses it. A caller in any other workspace
// still gets the only tabs team (QA #39, the boss's answer).
func TestACallerInASpaceIsTheSpacesProjects(t *testing.T) {
	t.Parallel()
	const spaces = "\n[spaces]\nproject = \"agentisan\"\n[projects.agentisan]\nrepo = \"REPO\"\n[projects.agentisan.groups.coders]\nworkers = 1\nslots = 2\n"
	tests := map[string]struct {
		config string
		space  string
		// project is the team status reports; "" for a refusal.
		project string
	}{
		"a space of the configured project": {config: teamConfig + spaces, space: "◆ coders", project: "agentisan"},
		"its boss space":                    {config: teamConfig + spaces, space: "◆ boss", project: "agentisan"},
		"a space no project owns":           {config: teamConfig, space: "◆ coders"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, tc.config)
			caller := f.h.addTab(f.h.addWorkspace(tc.space), "1", "claude", "working")
			sel := workforce.Selector{CallerPane: caller}

			got, err := f.svc.Status(t.Context(), workforce.StatusRequest{Selector: sel})
			if tc.project == "" {
				require.ErrorIs(t, err, workforce.ErrNoTarget)
				assert.Contains(t, err.Error(), "no [spaces] project configures")
			} else {
				require.NoError(t, err)
				assert.Equal(t, tc.project, got.Project)
				assert.Equal(t, "spaces", got.Layout)
			}
			_, err = f.svc.Spawn(t.Context(), workforce.SpawnRequest{Selector: sel, Group: "coder", Count: 1})
			require.ErrorIs(t, err, workforce.ErrNoTarget, "spawn never fills the only tabs team")
			_, err = f.svc.Up(t.Context(), workforce.UpRequest{Selector: sel})
			require.ErrorIs(t, err, workforce.ErrNoTarget, "nor does team up")
			assert.Empty(t, f.h.params("tab.create"))
			assert.Empty(t, f.h.params("workspace.create"))
		})
	}

	f := newFixture(t, teamConfig+spaces)
	other := f.h.addTab(f.h.addWorkspace("notes"), "1", "", "unknown")
	got, err := f.svc.Status(t.Context(), workforce.StatusRequest{Selector: workforce.Selector{CallerPane: other}})
	require.NoError(t, err)
	assert.Equal(t, "herdr-agentisan", got.Project, "a caller in no space and no team's workspace gets the only tabs team")
}

// TestStatusRefusesAUserOnlyKeyFromTheRepoFile: team status reads the
// project's committed repo file, so one that sets a key only the user's own
// layers may set is refused (ADR-001 A46): ErrConfig with settings' own
// ErrInvalid, naming the key, its layer and file, and never the value.
func TestStatusRefusesAUserOnlyKeyFromTheRepoFile(t *testing.T) {
	t.Parallel()
	f := newFixture(t, teamConfig)
	f.repoFile(t, "[spawn]\nshell_ready_keys = [\"from-the-repo\"]\n")

	_, err := f.svc.Status(t.Context(), workforce.StatusRequest{})
	require.ErrorIs(t, err, workforce.ErrConfig)
	require.ErrorIs(t, err, settings.ErrInvalid)
	assert.Contains(t, err.Error(), "spawn.shell_ready_keys (repo, ")
	assert.Contains(t, err.Error(), settings.RepoFileName)
	assert.NotContains(t, err.Error(), "from-the-repo", "never the value")
}

// paneFinder is a herdr whose panes answer pane.process_info by any id in
// panes, aliases included, and pane.list with listed; or fail with failing.
type paneFinder struct {
	panes   map[string]herdr.ProcessInfo
	listed  []herdr.PaneInfo
	failing error
}

func (f paneFinder) PaneProcessInfo(_ context.Context, id string) (herdr.ProcessInfo, error) {
	if info, ok := f.panes[id]; ok || f.failing != nil {
		return info, f.failing
	}
	return herdr.ProcessInfo{}, herdr.ErrPaneNotFound
}

func (f paneFinder) ListPanes(context.Context) ([]herdr.PaneInfo, error) { return f.listed, nil }

// TestFindCallerPane: HERDR_PANE_ID counts only when one of its pane's
// processes is in the command's lineage, so an alias resolves, and an id
// reissued to another team's pane yields the caller's own pane, else none,
// and selection takes its fallbacks (TestStatusPicksTheTeam).
func TestFindCallerPane(t *testing.T) {
	t.Parallel()
	// The command (5000) runs in w1:p1's shell (600); w2:p1, in another
	// team's workspace, runs 7000.
	procs := map[int]report.Stat{5000: {PPID: 600, Start: 90}, 600: {PPID: 1, Start: 40}}
	lineage := report.ReadLineage(5000, func(pid int) (report.Stat, error) {
		s, ok := procs[pid]
		if !ok {
			return report.Stat{}, errors.New("no such process")
		}
		return s, nil
	})
	ours, theirs := herdr.ProcessInfo{PaneID: "w1:p1", ShellPID: 600}, herdr.ProcessInfo{PaneID: "w2:p1", ShellPID: 7000}
	both := paneFinder{panes: map[string]herdr.ProcessInfo{"w1:p1": ours, "w9:p9": ours, "w2:p1": theirs}, listed: []herdr.PaneInfo{{PaneID: "w1:p1"}, {PaneID: "w2:p1"}}}
	failing := paneFinder{failing: herdr.ErrAPI}
	for name, tc := range map[string]struct {
		frozen  string
		h       paneFinder
		lineage *report.Lineage
		want    string
		err     error
	}{
		"the caller's pane":                      {frozen: "w1:p1", h: both, lineage: lineage, want: "w1:p1"},
		"a frozen alias, as herdr resolves it":   {frozen: "w9:p9", h: both, lineage: lineage, want: "w1:p1"},
		"an id reissued to another team's pane":  {frozen: "w2:p1", h: both, lineage: lineage, want: "w1:p1"},
		"that, and the caller's pane gone: none": {frozen: "w2:p1", h: paneFinder{panes: map[string]herdr.ProcessInfo{"w2:p1": theirs}, listed: []herdr.PaneInfo{{PaneID: "w2:p1"}}}, lineage: lineage},
		"two panes run the lineage: none": {frozen: "w8:p8", h: paneFinder{
			panes:  map[string]herdr.ProcessInfo{"w1:p1": ours, "w3:p1": {PaneID: "w3:p1", ShellPID: 600}},
			listed: []herdr.PaneInfo{{PaneID: "w1:p1"}, {PaneID: "w3:p1"}},
		}, lineage: lineage},
		"no lineage confirms nothing, herdr unasked": {frozen: "w1:p1", h: failing},
		"no HERDR_PANE_ID, herdr unasked":            {h: failing, lineage: lineage},
		"a failing herdr is ErrHerdr":                {frozen: "w1:p1", h: failing, lineage: lineage, err: workforce.ErrHerdr},
	} {
		got, err := workforce.FindCallerPane(t.Context(), tc.h, report.Pane{SocketPath: "unused", PaneID: tc.frozen, Lineage: tc.lineage})
		if tc.err != nil {
			require.ErrorIs(t, err, tc.err, name)
			continue
		}
		require.NoError(t, err, name)
		assert.Equal(t, tc.want, got, name)
	}
}

// TestSelectorFor: --project is the team without asking herdr; else the
// caller's pane, as FindCallerPane confirms it.
func TestSelectorFor(t *testing.T) {
	t.Parallel()
	lineage := report.ReadLineage(5000, func(int) (report.Stat, error) { return report.Stat{PPID: 1, Start: 40}, nil })
	pane := report.Pane{SocketPath: "unused", PaneID: "w1:p1", Lineage: lineage}
	failing := paneFinder{failing: herdr.ErrAPI}
	got, err := workforce.SelectorFor(t.Context(), failing, "ha", pane)
	require.NoError(t, err, "herdr is not asked")
	assert.Equal(t, workforce.Selector{Project: "ha"}, got)
	_, err = workforce.SelectorFor(t.Context(), failing, "", pane)
	require.ErrorIs(t, err, workforce.ErrHerdr)
	ours := paneFinder{panes: map[string]herdr.ProcessInfo{"w1:p1": {PaneID: "w1:p1", ShellPID: 5000}}}
	got, err = workforce.SelectorFor(t.Context(), ours, "", pane)
	require.NoError(t, err)
	assert.Equal(t, workforce.Selector{CallerPane: "w1:p1"}, got)
}

// TestStatusFailsWhenHerdrDoes: a herdr list that fails is ErrHerdr.
func TestStatusFailsWhenHerdrDoes(t *testing.T) {
	t.Parallel()
	for _, method := range []string{"pane.list", "workspace.list", "tab.list", "agent.list"} {
		t.Run(method, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, teamConfig)
			f.h.fail[method] = "server_unavailable"

			_, err := f.svc.Status(t.Context(), workforce.StatusRequest{})
			require.ErrorIs(t, err, workforce.ErrHerdr)
			assert.Contains(t, err.Error(), method[:len(method)-len(".list")])
		})
	}
}

// TestStatusGatesALastAgentsCtx: team status gates a member's ctx by the
// poll's last agent starts, as the snapshot is gated (A61): a ctx stamped
// before the pane's agent started is null, one stamped since is the
// member's, and with no starts at all it counts, as before.
func TestStatusGatesALastAgentsCtx(t *testing.T) {
	t.Parallel()
	f := newFixture(t, teamConfig)
	w := f.worker(t, "ha-coder-1", "coder")
	start := time.Date(2026, 10, 23, 5, 0, 0, 0, time.UTC)
	f.h.mu.Lock()
	terminal := f.h.pane(w.PaneID).terminal
	f.h.mu.Unlock()
	ctxAt := func(at time.Time) *int {
		t.Helper()
		f.h.setPane(w.PaneID, func(p *fakePane) {
			p.tokens = map[string]string{report.CtxKey: "70", report.CtxAtKey: report.FormatModelAt(at)}
		})
		got, err := f.svc.Status(t.Context(), workforce.StatusRequest{})
		require.NoError(t, err)
		require.Len(t, got.Groups[0].Members, 1)
		return got.Groups[0].Members[0].Ctx
	}
	require.NotNil(t, ctxAt(start.Add(-time.Hour)), "no starts: counted, as before")

	f.svc.Starts = &team.StartClock{}
	f.svc.Starts.Observe(team.Model{Projects: []team.Project{{Groups: []team.Group{{Workers: []team.Worker{
		{Pane: team.Pane{ID: w.PaneID, TerminalID: terminal}},
	}}}}}}, map[string]time.Time{w.PaneID: start})
	assert.Nil(t, ctxAt(start.Add(-time.Hour)), "the last agent's 70%")
	got := ctxAt(start.Add(time.Second))
	require.NotNil(t, got)
	assert.Equal(t, 70, *got)
}
