package workforce_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/herdr/herdrtest"
	"github.com/abanna/herdr-agentisan/internal/settings"
	"github.com/abanna/herdr-agentisan/internal/store"
	"github.com/abanna/herdr-agentisan/internal/team"
	"github.com/abanna/herdr-agentisan/internal/workforce"
)

// dashboard is the plugin pane the daemon names.
var dashboard = workforce.DashboardPane{PluginID: "nerdsrun.agentisan", Entrypoint: "dashboard", Title: dashTitle}

// upFixture is newFixture with the daemon's dashboard pane named.
func upFixture(t *testing.T, config string) *fixture {
	t.Helper()
	f := newFixture(t, config)
	f.svc.Dashboard = dashboard
	return f
}

// tabPanes are the panes of pane's tab, in layout order.
func (f *fakeHerdr) tabPanes(pane string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	tab := f.pane(pane).tab
	var out []string
	for _, p := range f.panes {
		if p.tab == tab {
			out = append(out, p.id)
		}
	}
	return out
}

// TestUpFillsEmptySlotsOnly: a profile of 3 coders and 1 QA starts 4 workers,
// each in its group's tab under its id, groups in the team model's order and
// one that only describes left out; a second run spawns nothing (I9) and
// finds the dashboard the first opened.
func TestUpFillsEmptySlotsOnly(t *testing.T) {
	t.Parallel()
	f := upFixture(t, strings.Replace(teamConfig, "workers = 2\nslots = 3", "workers = 3\nslots = 3", 1)+`
[projects.herdr-agentisan.groups.clerk]
description = "keeps Linear in sync"
`)

	res, err := f.svc.Up(t.Context(), workforce.UpRequest{})
	require.NoError(t, err)
	assert.Equal(t, "herdr-agentisan", res.Project)
	assert.Equal(t, []workforce.GroupFill{{Name: "coder", Workers: 3, Spawned: 3}, {Name: "qa", Workers: 1, Spawned: 1}, {Name: "review"}}, res.Groups)
	ids := []string{"ha-coder-1", "ha-coder-2", "ha-coder-3", "ha-qa-1"}
	require.Len(t, res.Workers, 4)
	for i, id := range ids {
		assert.Equal(t, id, res.Workers[i].ID)
		assert.Equal(t, store.StatusActive, f.row(t, id).Status)
		assert.NotNil(t, f.h.tabByLabel(id))
	}
	assert.Equal(t, "qa", f.row(t, "ha-qa-1").Group)
	assert.Len(t, f.h.params("agent.start"), 4)
	require.NotNil(t, res.Dashboard)
	assert.True(t, res.Dashboard.Opened)
	assert.Empty(t, res.DashboardError)

	again, err := f.svc.Up(t.Context(), workforce.UpRequest{})
	require.NoError(t, err)
	assert.Equal(t, []workforce.GroupFill{{Name: "coder", Workers: 3, Occupied: 3}, {Name: "qa", Workers: 1, Occupied: 1}, {Name: "review"}}, again.Groups)
	assert.Empty(t, again.Workers)
	assert.Len(t, f.h.params("tab.create"), 4, "no tab opened")
	assert.Len(t, f.h.params("agent.start"), 4, "no agent started")
	assert.Equal(t, &workforce.Dashboard{PaneID: res.Dashboard.PaneID}, again.Dashboard)
	assert.Len(t, f.h.params("plugin.pane.open"), 1)
}

// TestUpCountsWhatIsThere: a slot is taken by an agent started by hand and by
// a live row whose agent may still be starting, as spawn counts them; the
// fill takes the lowest free ids.
func TestUpCountsWhatIsThere(t *testing.T) {
	t.Parallel()
	f := upFixture(t, strings.Replace(teamConfig, "workers = 2\nslots = 3", "workers = 3\nslots = 3", 1))
	f.h.addTab(f.ws, "ha-coder-1", "claude", "idle")
	f.bindRow(t, "ha-coder-2", "coder", f.h.addTab(f.ws, "ha-coder-2", "", "unknown"))

	res, err := f.svc.Up(t.Context(), workforce.UpRequest{Selector: workforce.Selector{Project: "herdr-agentisan"}})
	require.NoError(t, err)
	assert.Equal(t, workforce.GroupFill{Name: "coder", Workers: 3, Occupied: 2, Spawned: 1}, res.Groups[0])
	require.Len(t, res.Workers, 2)
	assert.Equal(t, []string{"ha-coder-3", "ha-qa-1"}, []string{res.Workers[0].ID, res.Workers[1].ID})
}

// TestUpLeavesAGroupPastItsDefault: a group the boss spawned past its
// workers count is left as it is, with nothing spawned there and never a
// negative count, and the other groups still fill (QA #96 P2).
func TestUpLeavesAGroupPastItsDefault(t *testing.T) {
	t.Parallel()
	f := upFixture(t, strings.Replace(teamConfig, "workers = 2\nslots = 3", "workers = 1\nslots = 3", 1))
	f.h.addTab(f.ws, "ha-coder-1", "claude", "idle")
	f.h.addTab(f.ws, "ha-coder-2", "claude", "idle")

	res, err := f.svc.Up(t.Context(), workforce.UpRequest{})
	require.NoError(t, err)
	assert.Equal(t, workforce.GroupFill{Name: "coder", Workers: 1, Occupied: 2}, res.Groups[0])
	require.Len(t, res.Workers, 1)
	assert.Equal(t, "ha-qa-1", res.Workers[0].ID)
}

// TestUpRefusals: a failed preflight, a repo the configuration moves to
// while it runs, or a group named past A53's bound, whose ids herdr would
// refuse, refuses the whole run before any tab or dashboard opens. Within the
// bound a team up's ids fit, since it fills from n = 1; spawn's
// TestSpawnIDsFitHerdrsAgentNames pins the id check itself.
func TestUpRefusals(t *testing.T) {
	t.Parallel()
	over := strings.Repeat("a", 28)
	tests := map[string]struct {
		config string
		check  func(t *testing.T, f *fixture) func(context.Context, string) error
		want   error
		says   string
	}{
		"the preflight": {config: teamConfig, check: func(*testing.T, *fixture) func(context.Context, string) error {
			return func(context.Context, string) error { return errors.New("tdaddy is not on PATH") }
		}, want: workforce.ErrPreflight},
		"the repo changed during the preflight": {config: teamConfig, check: func(t *testing.T, f *fixture) func(context.Context, string) error {
			t.Helper()
			other := t.TempDir()
			marker := filepath.Join(other, filepath.FromSlash(settings.BundleMarker))
			require.NoError(t, os.MkdirAll(filepath.Dir(marker), 0o750))
			require.NoError(t, os.WriteFile(marker, []byte("1\n"), 0o600))
			return func(context.Context, string) error {
				return os.WriteFile(filepath.Join(f.config, settings.FileName), []byte(strings.ReplaceAll(teamConfig, "REPO", other)), 0o600)
			}
		}, want: workforce.ErrPreflight},
		"a group named past the bound (A53)": {config: teamConfig + "\n[projects.herdr-agentisan.groups." + over + "]\nworkers = 1\nslots = 1\n", want: workforce.ErrConfig, says: "a group's name is at most 20 characters"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := upFixture(t, tc.config)
			if tc.check != nil {
				f.svc.Preflight = tc.check(t, f)
			}

			_, err := f.svc.Up(t.Context(), workforce.UpRequest{})
			require.ErrorIs(t, err, tc.want)
			assert.Contains(t, err.Error(), tc.says)
			assert.Empty(t, f.h.params("tab.create"))
			assert.Empty(t, f.h.params("plugin.pane.open"))
		})
	}
}

// TestUpOpensTheDashboardAboveTheBoss: with none open, team up splits the
// boss's pane down with the plugin's dashboard, unfocused, labels it, swaps
// it above the boss and gives focus back to the pane that had it.
func TestUpOpensTheDashboardAboveTheBoss(t *testing.T) {
	t.Parallel()
	f := upFixture(t, teamConfig)
	away := f.h.addTab(f.h.addWorkspace("elsewhere"), "notes", "", "unknown")
	f.h.focused = away

	res, err := f.svc.Up(t.Context(), workforce.UpRequest{})
	require.NoError(t, err)
	opens := f.h.params("plugin.pane.open")
	require.Len(t, opens, 1)
	assert.Equal(t, map[string]any{"plugin_id": "nerdsrun.agentisan", "entrypoint": "dashboard", "placement": "split", "target_pane_id": f.boss, "direction": "down", "focus": false}, opens[0])
	require.NotNil(t, res.Dashboard)
	dash := res.Dashboard.PaneID
	assert.Equal(t, workforce.DashboardOutcome{Dashboard: &workforce.Dashboard{PaneID: dash, Opened: true}}, res.DashboardOutcome)
	assert.Equal(t, []string{dash, f.boss}, f.h.tabPanes(f.boss), "above the boss")
	f.h.mu.Lock()
	assert.Equal(t, team.DashboardLabel("ha"), f.h.pane(dash).label)
	assert.Equal(t, away, f.h.focused, "focus given back")
	f.h.mu.Unlock()
	assert.Equal(t, "ha-dashboard", team.DashboardLabel("ha"))
}

// TestUpFocusesTheBossThenGivesFocusBack: herdr's swap focuses the
// dashboard in the boss's tab, which the tab keeps for the user's next visit,
// and switches to that tab. So team up focuses the boss again, then the pane
// that had focus when it was not the boss: the user is back where they were,
// and the boss's tab shows its boss. A declined swap moved no focus, so only
// the pane that had it gets it back; a refused focus on the boss still gives
// it back (QA #96 N1).
func TestUpFocusesTheBossThenGivesFocusBack(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		// before is the pane focused at load: "away", "boss", "beside" (a
		// pane in the boss's tab) or "" for none.
		before  string
		decline bool
		// refuse is the pane whose pane.focus herdr refuses.
		refuse string
		// focuses are the panes focused, in order; after, the user's focus
		// at the end; tab, the boss's tab's own, "dashboard" for the one
		// opened.
		focuses    []string
		after, tab string
	}{
		"focus in another workspace": {before: "away", focuses: []string{"boss", "away"}, after: "away", tab: "boss"},
		"focus on the boss":          {before: "boss", focuses: []string{"boss"}, after: "boss", tab: "boss"},
		"focus beside the boss":      {before: "beside", focuses: []string{"boss", "beside"}, after: "beside", tab: "beside"},
		"no pane focused":            {before: "", focuses: []string{"boss"}, after: "boss", tab: "boss"},
		"the swap declined":          {before: "away", decline: true, focuses: []string{"away"}, after: "away", tab: ""},
		"the boss's focus refused":   {before: "away", refuse: "boss", focuses: []string{"boss", "away"}, after: "away", tab: "dashboard"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := upFixture(t, teamConfig)
			panes := map[string]string{
				"":       "",
				"boss":   f.boss,
				"away":   f.h.addTab(f.h.addWorkspace("elsewhere"), "notes", "", "unknown"),
				"beside": f.h.addPane(f.boss, "", "unknown"),
			}
			f.h.focused = panes[tc.before]
			srv := wrapHerdr(t, f, func(r herdrtest.Request, next func() herdrtest.Reply) herdrtest.Reply {
				switch {
				case r.Method == "pane.swap" && tc.decline:
					return herdrtest.Reply{Result: map[string]any{"type": "pane_swap", "swap": map[string]any{"changed": false, "reason": "no_neighbor", "source_pane_id": "x", "focused_pane_id": "x", "layout": map[string]any{}}}}
				case r.Method == "pane.focus" && tc.refuse != "" && strings.Contains(string(r.Params), `"`+panes[tc.refuse]+`"`):
					return errReply("server_error")
				}
				return next()
			})

			res, err := f.svc.Up(t.Context(), workforce.UpRequest{})
			require.NoError(t, err)
			require.NotNil(t, res.Dashboard)
			panes["dashboard"] = res.Dashboard.PaneID
			if tc.refuse != "" {
				assert.Contains(t, res.DashboardError, "give focus back to "+panes[tc.refuse])
			}
			var focused []string
			for _, r := range srv.Requests() {
				if r.Method == "pane.focus" {
					var p struct {
						PaneID string `json:"pane_id"`
					}
					require.NoError(t, json.Unmarshal(r.Params, &p))
					focused = append(focused, p.PaneID)
				}
			}
			want := make([]string, 0, len(tc.focuses))
			for _, k := range tc.focuses {
				want = append(want, panes[k])
			}
			assert.Equal(t, want, focused)
			f.h.mu.Lock()
			defer f.h.mu.Unlock()
			assert.Equal(t, panes[tc.after], f.h.focused, "the user's focus")
			assert.Equal(t, panes[tc.tab], f.h.tabFocus[f.h.pane(f.boss).tab], "the boss's tab's own focus")
		})
	}
}

// TestUpFindsTheTeamsDashboard: a pane in the team's workspace labelled as
// team up labels it, or with the entrypoint's title as prefix+d opens it, is
// the team's dashboard, left as it is; one in another workspace is not.
func TestUpFindsTheTeamsDashboard(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		label string
		other bool
	}{
		"labelled by team up":  {label: "ha-dashboard"},
		"opened with prefix+d": {label: dashTitle},
		"in another workspace": {label: "ha-dashboard", other: true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := upFixture(t, teamConfig)
			beside := f.boss
			if tc.other {
				beside = f.h.addTab(f.h.addWorkspace("elsewhere"), "notes", "", "unknown")
			}
			existing := f.h.addPane(beside, "", "unknown")
			f.h.setPane(existing, func(p *fakePane) { p.label = tc.label })

			res, err := f.svc.Up(t.Context(), workforce.UpRequest{})
			require.NoError(t, err)
			require.NotNil(t, res.Dashboard)
			if tc.other {
				assert.True(t, res.Dashboard.Opened)
				assert.NotEqual(t, existing, res.Dashboard.PaneID)
				return
			}
			assert.Equal(t, workforce.DashboardOutcome{Dashboard: &workforce.Dashboard{PaneID: existing}}, res.DashboardOutcome)
			assert.Empty(t, f.h.params("plugin.pane.open"))
			assert.Empty(t, f.h.params("pane.rename"), "the user's pane is never renamed")
		})
	}
}

// TestUpOpensTheDashboardAboveTheBossAgent: the boss is the first agent in
// the boss's tab, as the snapshot names it, not a shell before it.
func TestUpOpensTheDashboardAboveTheBossAgent(t *testing.T) {
	t.Parallel()
	f := upFixture(t, teamConfig)
	f.h.setPane(f.boss, func(p *fakePane) { p.agent, p.name = "", "" })
	boss := f.h.addPane(f.boss, "claude", "working")

	_, err := f.svc.Up(t.Context(), workforce.UpRequest{})
	require.NoError(t, err)
	opens := f.h.params("plugin.pane.open")
	require.Len(t, opens, 1)
	assert.Equal(t, boss, opens[0]["target_pane_id"])
}

// TestUpDashboardIsBestEffort: whatever the dashboard lacks is reported
// beside a team up that still succeeds; a dashboard opened stays reported.
func TestUpDashboardIsBestEffort(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		setup  func(*fixture)
		opened bool
		says   string
	}{
		"no dashboard pane named": {setup: func(f *fixture) { f.svc.Dashboard = workforce.DashboardPane{} }, says: "names no dashboard pane"},
		"no boss agent": {setup: func(f *fixture) {
			f.h.setPane(f.boss, func(p *fakePane) { p.agent, p.name = "", "" })
		}, says: "no agent runs in the ha-boss tab"},
		"the split refused": {setup: func(f *fixture) { f.h.fail["plugin.pane.open"] = "plugin_pane_open_failed" }, says: "open the dashboard"},
		"the label refused": {setup: func(f *fixture) { f.h.fail["pane.rename"] = "server_error" }, opened: true, says: "label the dashboard ha-dashboard"},
		"the swap declined": {setup: func(f *fixture) {
			wrapHerdr(t, f, func(r herdrtest.Request, next func() herdrtest.Reply) herdrtest.Reply {
				if r.Method == "pane.swap" {
					return herdrtest.Reply{Result: map[string]any{"type": "pane_swap", "swap": map[string]any{"changed": false, "reason": "cross_tab", "source_pane_id": "x", "focused_pane_id": "x", "layout": map[string]any{}}}}
				}
				return next()
			})
		}, opened: true, says: "move the dashboard above the boss; it is below"},
		"focus refused": {setup: func(f *fixture) { f.h.focused, f.h.fail["pane.focus"] = f.boss, "server_error" }, opened: true, says: "give focus back to " + "w10:p1"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := upFixture(t, teamConfig)
			tc.setup(f)

			res, err := f.svc.Up(t.Context(), workforce.UpRequest{})
			require.NoError(t, err)
			assert.Len(t, res.Workers, 3, "the workers came up")
			assert.Contains(t, res.DashboardError, tc.says)
			assert.NotContains(t, res.DashboardError, "\n")
			assert.Equal(t, tc.opened, res.Dashboard != nil && res.Dashboard.Opened)
		})
	}
}

// TestUpDashboardKeepsToItsBudget: herdr holds each of the dashboard's calls
// 3 s. The dashboard gives up within its 5 s, says why, and the agents start
// then: a team up never waits out every dashboard call one after another
// (QA #96 P1).
func TestUpDashboardKeepsToItsBudget(t *testing.T) {
	t.Parallel()
	f := upFixture(t, teamConfig)
	f.h.focused = f.boss
	var openAt, startAt atomic.Int64
	wrapHerdr(t, f, func(r herdrtest.Request, next func() herdrtest.Reply) herdrtest.Reply {
		switch r.Method {
		case "plugin.pane.open", "pane.rename", "pane.swap", "pane.focus":
			openAt.CompareAndSwap(0, time.Now().UnixNano())
			time.Sleep(3 * time.Second)
		case "agent.start":
			startAt.CompareAndSwap(0, time.Now().UnixNano())
		}
		return next()
	})

	res, err := f.svc.Up(t.Context(), workforce.UpRequest{})
	require.NoError(t, err, "the dashboard never fails team up")
	assert.Len(t, res.Workers, 3)
	require.NotNil(t, res.Dashboard)
	assert.True(t, res.Dashboard.Opened)
	assert.NotEmpty(t, res.DashboardError)
	took := time.Duration(startAt.Load() - openAt.Load())
	assert.Less(t, took, 6500*time.Millisecond, "within the dashboard's 5 s, not 4 x 3 s: %s", res.DashboardError)
}

// TestConcurrentUpsOpenOneDashboard: two runs at once fill the team once and
// open one dashboard, which the second finds, even while herdr is slow to
// answer the first's open.
func TestConcurrentUpsOpenOneDashboard(t *testing.T) {
	t.Parallel()
	f := upFixture(t, teamConfig)
	var held sync.Once
	srv := wrapHerdr(t, f, func(r herdrtest.Request, next func() herdrtest.Reply) herdrtest.Reply {
		if r.Method == "plugin.pane.open" {
			held.Do(func() { time.Sleep(300 * time.Millisecond) })
		}
		return next()
	})
	var wg sync.WaitGroup
	res := make([]workforce.UpResult, 2)
	for i := range res {
		wg.Go(func() {
			var err error
			res[i], err = f.svc.Up(t.Context(), workforce.UpRequest{})
			assert.NoError(t, err)
		})
	}
	wg.Wait()

	assert.Equal(t, 1, countCalls(srv, "plugin.pane.open"))
	assert.Equal(t, 3, countCalls(srv, "tab.create"))
	require.NotNil(t, res[0].Dashboard)
	require.NotNil(t, res[1].Dashboard)
	assert.Equal(t, res[0].Dashboard.PaneID, res[1].Dashboard.PaneID)
	assert.NotEqual(t, res[0].Dashboard.Opened, res[1].Dashboard.Opened)
}
