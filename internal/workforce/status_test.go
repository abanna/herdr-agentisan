package workforce_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/team"
	"github.com/abanna/herdr-agentisan/internal/workforce"
)

// TestStatus: every configured group with its counts, and any type found in
// herdr; members named by row, else tab label; ctx null unless a percentage;
// idle_s from the poll's clock; the boss's panes apart, named by pane id.
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
	}, got.Boss, "named by pane id: the boss's tab holds more than the boss")

	f.svc.Now = func() time.Time { return t0.Add(-5 * time.Second) }
	got, err = f.svc.Status(t.Context(), workforce.StatusRequest{})
	require.NoError(t, err)
	assert.Equal(t, int64(0), *got.Groups[0].Members[0].IdleS, "a clock stepped back reports no rest, not a negative one")
}

// TestStatusCountsEveryTakenSlot: an agent started by hand takes a slot, and
// so does a pane a live row binds whose agent is not up yet; a row whose pane
// herdr no longer lists takes none.
func TestStatusCountsEveryTakenSlot(t *testing.T) {
	t.Parallel()
	f := newFixture(t, teamConfig)
	f.h.addTab(f.ws, "ha-coder-7", "claude", "working")
	starting := f.h.addTab(f.ws, "ha-coder-1", "", "unknown")
	f.bindRow(t, "ha-coder-1", "coder", starting)
	f.h.addTab(f.ws, "ha-coder-2", "", "unknown")  // a shell, bound by nothing
	f.bindRow(t, "ha-coder-3", "coder", "w10:p99") // its pane is gone

	got, err := f.svc.Status(t.Context(), workforce.StatusRequest{})
	require.NoError(t, err)
	assert.Equal(t, 2, got.Groups[0].Occupied)
	assert.Equal(t, 1, got.Groups[0].Open)
	assert.Len(t, got.Groups[0].Members, 3)
}

// TestStatusOrdersTheGroupsItFinds: the team model's types first, then every
// other type, configured or found in herdr, by name.
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
}

// TestStatusOfTheSpacesProject: --project names the ◆ spaces project too,
// whose groups are its spaces; the default never picks it.
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
}

// TestStatusPicksTheTeam: --project wins; else the team whose workspace holds
// the caller's pane, resolved by herdr, since the shell's HERDR_PANE_ID can
// be an id herdr has since reissued; else the only tabs team; else it is
// refused.
func TestStatusPicksTheTeam(t *testing.T) {
	t.Parallel()
	two := teamConfig + "\n[projects.assay]\nrepo = \"REPO\"\nshort = \"as\"\n[projects.assay.groups.coder]\nworkers = 0\nslots = 2\n"
	f := newFixture(t, two)
	as := f.h.addWorkspace("assay")
	asBoss := f.h.addTab(as, "as-boss", "claude", "idle")
	f.h.mu.Lock()
	f.h.alias["w3:p1"] = asBoss
	f.h.mu.Unlock()

	for name, tc := range map[string]struct {
		sel  workforce.Selector
		want string
	}{
		"the caller's team":                   {sel: workforce.Selector{CallerPane: asBoss}, want: "assay"},
		"a frozen pane id, as herdr resolves": {sel: workforce.Selector{CallerPane: "w3:p1"}, want: "assay"},
		"--project beats the caller's pane":   {sel: workforce.Selector{Project: "herdr-agentisan", CallerPane: asBoss}, want: "herdr-agentisan"},
	} {
		got, err := f.svc.Status(t.Context(), workforce.StatusRequest{Selector: tc.sel})
		require.NoError(t, err, name)
		assert.Equal(t, tc.want, got.Project, name)
	}

	_, err := f.svc.Status(t.Context(), workforce.StatusRequest{Selector: workforce.Selector{CallerPane: "w99:p9"}})
	require.ErrorIs(t, err, workforce.ErrBadRequest, "two tabs teams and the caller in neither")
	assert.Contains(t, err.Error(), "assay, herdr-agentisan")
	_, err = f.svc.Status(t.Context(), workforce.StatusRequest{Selector: workforce.Selector{Project: "nope"}})
	require.ErrorIs(t, err, workforce.ErrUnknownProject)
	assert.Contains(t, err.Error(), "configured: assay, herdr-agentisan")

	g := newFixture(t, teamConfig)
	got, err := g.svc.Status(t.Context(), workforce.StatusRequest{Selector: workforce.Selector{CallerPane: "w99:p9"}})
	require.NoError(t, err)
	assert.Equal(t, "herdr-agentisan", got.Project, "a pane in no team falls back to the only tabs team")

	broken := newFixture(t, "schema_version = 1\n[projects.herdr-agentisan]\nrepo = \"REPO\"\nlayout = \"grid\"\n")
	_, err = broken.svc.Status(t.Context(), workforce.StatusRequest{})
	require.ErrorIs(t, err, workforce.ErrConfig)
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
