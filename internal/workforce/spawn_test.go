package workforce_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/herdr/herdrtest"
	"github.com/abanna/herdr-agentisan/internal/preflight"
	"github.com/abanna/herdr-agentisan/internal/settings"
	"github.com/abanna/herdr-agentisan/internal/store"
	"github.com/abanna/herdr-agentisan/internal/workforce"
)

// TestSpawnOpensOneTabPerWorker: each worker gets the lowest free logical id,
// a tab of its own in the team's workspace labelled with it, never focused,
// with the repo as its cwd and the id in its shell's environment; its agent
// starts there under its herdr name, Claude with --name; its row is bound and
// active.
func TestSpawnOpensOneTabPerWorker(t *testing.T) {
	t.Parallel()
	f := newFixture(t, teamConfig)

	res, err := f.svc.Spawn(t.Context(), workforce.SpawnRequest{Selector: workforce.Selector{Project: "herdr-agentisan"}, Group: "coder", Count: 2, Model: "sonnet"})
	require.NoError(t, err)

	tabs := f.h.params("tab.create")
	require.Len(t, tabs, 2)
	for i, id := range []string{"ha-coder-1", "ha-coder-2"} {
		assert.Equal(t, map[string]any{"workspace_id": f.ws, "cwd": f.repo, "label": id, "focus": false, "env": map[string]any{workforce.WorkerEnv: id}}, tabs[i])
	}
	starts := f.h.params("agent.start")
	require.Len(t, starts, 2)
	for _, s := range starts {
		id := s["name"].(string)
		assert.Equal(t, "claude", s["kind"])
		assert.Equal(t, []any{"--model", "sonnet", "--name", id}, s["args"])
		assert.InDelta(t, 90000, s["timeout_ms"], 1)
	}
	require.Len(t, res.Workers, 2)
	for i, w := range res.Workers {
		id := []string{"ha-coder-1", "ha-coder-2"}[i]
		assert.Equal(t, workforce.Spawned{ID: id, Group: "coder", PaneID: w.PaneID, Agent: "claude"}, w)
		f.h.mu.Lock()
		terminal := f.h.pane(w.PaneID).terminal
		f.h.mu.Unlock()
		assert.Equal(t, store.Worker{LogicalID: id, Project: "herdr-agentisan", Group: "coder", PaneID: w.PaneID, TerminalID: terminal, Status: store.StatusActive},
			f.row(t, id).Worker, "bound to its pane's terminal (A35)")
	}
	assert.Equal(t, "herdr-agentisan", res.Project)

	again, err := f.svc.Spawn(t.Context(), workforce.SpawnRequest{Selector: workforce.Selector{Project: "herdr-agentisan"}, Group: "coder", Count: 1})
	require.NoError(t, err)
	assert.Equal(t, "ha-coder-3", again.Workers[0].ID)
}

// firstIndex and lastIndex are where v first and last is in s, or -1.
func firstIndex(s []string, v string) int { return slices.Index(s, v) }

func lastIndex(s []string, v string) int {
	for i, x := range slices.Backward(s) {
		if x == v {
			return i
		}
	}
	return -1
}

// TestSpawnTakesAModelFromTheRepoFile: a repo file may still set a group's
// model, which reaches the agent as one argument after --model, while its
// command comes from the user's own layers (A46). A request's --model wins.
func TestSpawnTakesAModelFromTheRepoFile(t *testing.T) {
	t.Parallel()
	f := newFixture(t, teamConfig)
	f.repoFile(t, "[groups.coder.agent]\nmodel = \"sonnet\"\n")

	_, err := f.svc.Spawn(t.Context(), workforce.SpawnRequest{Group: "coder", Count: 1})
	require.NoError(t, err)
	starts := f.h.params("agent.start")
	require.Len(t, starts, 1)
	assert.Equal(t, "claude", starts[0]["kind"])
	assert.Equal(t, []any{"--model", "sonnet", "--name", "ha-coder-1"}, starts[0]["args"])

	_, err = f.svc.Spawn(t.Context(), workforce.SpawnRequest{Group: "coder", Count: 1, Model: "opus"})
	require.NoError(t, err)
	assert.Equal(t, []any{"--model", "opus", "--name", "ha-coder-2"}, f.h.params("agent.start")[1]["args"], "--model wins")
}

// TestPlanChecksTheProfilesModel: the model spawn passes after --model is
// checked where it is used, even from a profile Load did not build, which
// would have refused it first (A46). The error never holds the model.
func TestPlanChecksTheProfilesModel(t *testing.T) {
	t.Parallel()
	const bad = "opus'; touch pwned; '"
	claude := []string{"claude"}
	tests := map[string]struct {
		profile *settings.Profile
		typ     string
	}{
		"the top-level model":                  {profile: &settings.Profile{Agent: settings.Agent{Command: claude, Model: bad}}, typ: "coder"},
		"a group's model":                      {profile: &settings.Profile{Agent: settings.Agent{Command: claude}, Groups: map[string]settings.Group{"coder": {Agent: settings.Agent{Model: bad}}}}, typ: "coder"},
		"a group's model with its own command": {profile: &settings.Profile{Agent: settings.Agent{Command: claude}, Groups: map[string]settings.Group{"qa": {Agent: settings.Agent{Command: []string{"codex"}, Model: bad}}}}, typ: "qa"},
		"the review group's model":             {profile: &settings.Profile{Agent: settings.Agent{Command: claude}, Groups: map[string]settings.Group{settings.ReviewType: {Agent: settings.Agent{Model: bad}}}}, typ: settings.ReviewType},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, _, err := workforce.Plan(tc.profile, tc.typ, "", "")
			require.ErrorIs(t, err, workforce.ErrConfig)
			require.ErrorIs(t, err, settings.ErrInvalid)
			assert.Contains(t, err.Error(), tc.typ+"'s agent model: ")
			assert.NotContains(t, err.Error(), "pwned")

			_, args, err := workforce.Plan(tc.profile, tc.typ, "sonnet", "")
			require.NoError(t, err, "the request's model is the one passed")
			assert.Contains(t, args, "sonnet")
		})
	}
}

// TestPlanPassesAnEffort: an effort, which only NERD-5289's routing chooses,
// follows the model as each kind takes it, and one a shell could misread is
// refused without being echoed.
func TestPlanPassesAnEffort(t *testing.T) {
	t.Parallel()
	p := &settings.Profile{Agent: settings.Agent{Command: []string{"claude"}}}
	kind, args, err := workforce.Plan(p, "coder", "opus", "xhigh")
	require.NoError(t, err)
	assert.Equal(t, "claude", kind)
	assert.Equal(t, []string{"--model", "opus", "--effort", "xhigh", "--name", "ha-coder-1"}, args)

	kind, args, err = workforce.Plan(p, settings.ReviewType, "gpt-5.1-codex", "high")
	require.NoError(t, err)
	assert.Equal(t, "codex", kind)
	assert.Equal(t, []string{"--no-daemon", "--model", "gpt-5.1-codex", "-c", `model_reasoning_effort="high"`}, args)

	for _, effort := range []string{"High", "-high", "high;rm", "aaaaaaaaaaaaaaaaa"} {
		_, _, err := workforce.Plan(p, "coder", "", effort)
		require.ErrorIs(t, err, workforce.ErrConfig, effort)
		assert.NotContains(t, err.Error(), effort)
	}
	_, args, err = workforce.Plan(p, "coder", "", "aaaaaaaaaaaaaaaa")
	require.NoError(t, err, "16 letters")
	assert.Equal(t, []string{"--effort", "aaaaaaaaaaaaaaaa", "--name", "ha-coder-1"}, args)
}

// TestSpawnIDs: the lowest n not taken by one of the team's tab labels, any
// agent name on the server or a live row; another workspace's tab labels take
// none. A retired worker's id is reused and its row rebound at the next
// generation (I1). review launches codex.
func TestSpawnIDs(t *testing.T) {
	t.Parallel()
	f := newFixture(t, teamConfig)
	f.h.addTab(f.ws, "ha-coder-1", "", "unknown") // a shell tab still holds the label
	other := f.h.addWorkspace("elsewhere")
	f.h.addTab(other, "ha-coder-2", "claude", "idle") // an agent named so on the server
	f.h.addTab(other, "ha-coder-4", "", "unknown")    // a label in another workspace
	f.bindRow(t, "ha-coder-3", "coder", f.boss)       // a live row, whose pane herdr lists

	res, err := f.svc.Spawn(t.Context(), workforce.SpawnRequest{Selector: workforce.Selector{Project: "herdr-agentisan"}, Group: "coder", Count: 2})
	require.NoError(t, err)
	require.Len(t, res.Workers, 2)
	assert.Empty(t, res.Warnings, "no Codex worker")
	assert.Equal(t, "ha-coder-4", res.Workers[0].ID)
	assert.Equal(t, "ha-coder-5", res.Workers[1].ID)

	res, err = f.svc.Spawn(t.Context(), workforce.SpawnRequest{Selector: workforce.Selector{Project: "herdr-agentisan"}, Group: "review", Count: 1})
	require.NoError(t, err)
	assert.Equal(t, "ha-review-1", res.Workers[0].ID)
	assert.Equal(t, "codex", res.Workers[0].Agent, "review launches codex")
	starts := f.h.params("agent.start")
	assert.Equal(t, []any{"--no-daemon"}, starts[len(starts)-1]["args"], "codex off the daemon (A22), with no --name and no model")

	cfg := strings.Replace(teamConfig, "workers = 2\nslots = 3", "workers = 2\nslots = 9", 1)
	g := newFixture(t, cfg)
	g.h.addTab(g.ws, "ha-coder-1", "", "unknown")
	_, err = g.st.BindWorker(t.Context(), store.Worker{LogicalID: "ha-coder-2", Project: "herdr-agentisan", PaneID: "w99:p1", TerminalID: "term_gone"}, time.Now())
	require.NoError(t, err)
	require.NoError(t, g.st.SetWorkerStatus(t.Context(), store.Worker{LogicalID: "ha-coder-2"}, store.StatusRetired, time.Now()))
	res, err = g.svc.Spawn(t.Context(), workforce.SpawnRequest{Selector: workforce.Selector{Project: "herdr-agentisan"}, Group: "coder", Count: 1})
	require.NoError(t, err)
	assert.Equal(t, "ha-coder-2", res.Workers[0].ID, "the retired id is the lowest free")
	assert.Equal(t, int64(1), res.Workers[0].Generation)
	assert.Equal(t, int64(1), g.row(t, "ha-coder-2").Generation)
}

// TestSpawnReusesALostID: a lost row's id is free again, rebound at its next
// generation with the new terminal, unless a tab still carries its label; a
// live row whose terminal herdr lists nowhere keeps its id, since the team
// poll may yet re-key it (A35).
func TestSpawnReusesALostID(t *testing.T) {
	t.Parallel()
	f := newFixture(t, teamConfig)
	f.bindRow(t, "ha-coder-1", "coder", "w10:p99")
	f.exec(t, "UPDATE workers SET terminal_id = 'term_gone' WHERE logical_id = 'ha-coder-1'")
	f.markLost(t, "ha-coder-1")
	f.bindRow(t, "ha-coder-2", "coder", "w10:p98")
	f.exec(t, "UPDATE workers SET terminal_id = 'term_away' WHERE logical_id = 'ha-coder-2'")
	f.bindRow(t, "ha-coder-3", "coder", f.h.addTab(f.ws, "ha-coder-3", "", "unknown"))
	f.markLost(t, "ha-coder-3") // its shell tab still carries the label
	open := f.h.addTab(f.ws, "ha-qa-1", "claude", "idle")
	f.bindRow(t, "ha-qa-1", "qa", open)
	f.markLost(t, "ha-qa-1")

	res, err := f.svc.Spawn(t.Context(), workforce.SpawnRequest{Group: "coder", Count: 2})
	require.NoError(t, err)
	assert.Equal(t, []string{"ha-coder-1", "ha-coder-4"}, []string{res.Workers[0].ID, res.Workers[1].ID})
	assert.Equal(t, int64(1), res.Workers[0].Generation)
	row := f.row(t, "ha-coder-1")
	assert.Equal(t, store.StatusActive, row.Status)
	assert.Equal(t, res.Workers[0].PaneID, row.PaneID)
	assert.NotEqual(t, "term_gone", row.TerminalID, "bound to its new pane's terminal")

	res, err = f.svc.Spawn(t.Context(), workforce.SpawnRequest{Group: "qa", Count: 1})
	require.ErrorIs(t, err, workforce.ErrSlotCap, "the lost worker's agent still holds qa's one slot")
	assert.Empty(t, res.Workers)
}

// TestSpawnRefusals: each refusal is its own sentinel, and nothing opens.
func TestSpawnRefusals(t *testing.T) {
	t.Parallel()
	two := teamConfig + "\n[projects.assay]\nrepo = \"REPO\"\nshort = \"as\"\n"
	spaces := "schema_version = 1\n[spaces]\nproject = \"agentisan\"\n[projects.agentisan]\nrepo = \"REPO\"\n" + strings.TrimPrefix(teamConfig, "schema_version = 1\n")
	long := strings.Replace(teamConfig, "groups.review]", "groups.reviewer-of-absolutely-everything]", 1)
	x40 := strings.Repeat("x", 40) // a group name a repo file may create, too long for an id
	tests := map[string]struct {
		config string
		setup  func(*testing.T, *fixture)
		req    workforce.SpawnRequest
		want   error
		says   string
		hides  string // a model, which the error never holds (A46)
	}{
		"a count below 1":             {req: workforce.SpawnRequest{Group: "coder"}, want: workforce.ErrBadRequest, says: "--count 0"},
		"a negative count":            {req: workforce.SpawnRequest{Group: "coder", Count: -1}, want: workforce.ErrBadRequest, says: "--count -1"},
		"no group":                    {req: workforce.SpawnRequest{Count: 1}, want: workforce.ErrBadRequest, says: "--group"},
		"an unknown group":            {req: workforce.SpawnRequest{Group: "designer", Count: 1}, want: workforce.ErrUnknownGroup, says: "configured: coder, qa, review"},
		"a group that only describes": {config: teamConfig + "[projects.herdr-agentisan.groups.clerk]\ndescription = \"keeps Linear in sync\"\n", req: workforce.SpawnRequest{Group: "clerk", Count: 1}, want: workforce.ErrUnknownGroup, says: "configured: coder, qa, review"},
		"a non-ASCII group":           {req: workforce.SpawnRequest{Group: "cöder", Count: 1}, want: workforce.ErrUnknownGroup, says: `"cöder"`},
		"a group that is no UTF-8":    {req: workforce.SpawnRequest{Group: "cod\xffer", Count: 1}, want: workforce.ErrUnknownGroup, says: `"cod\xffer"`},
		"a group in another case":     {req: workforce.SpawnRequest{Group: "Coder", Count: 1}, want: workforce.ErrUnknownGroup},
		"a group with a newline":      {req: workforce.SpawnRequest{Group: "coder\n", Count: 1}, want: workforce.ErrUnknownGroup, says: `"coder\n"`},
		"an option-like group":        {req: workforce.SpawnRequest{Group: "--count", Count: 1}, want: workforce.ErrUnknownGroup},
		"past the slots":              {req: workforce.SpawnRequest{Group: "coder", Count: 4}, want: workforce.ErrSlotCap, says: "coder is capped at 3 slots (herdr-agentisan.toml): 0 occupied, 4 requested"},
		"an unknown project":          {req: workforce.SpawnRequest{Selector: workforce.Selector{Project: "nope"}, Group: "coder", Count: 1}, want: workforce.ErrUnknownProject, says: "configured: herdr-agentisan"},
		"the spaces project":          {config: spaces, req: workforce.SpawnRequest{Selector: workforce.Selector{Project: "agentisan"}, Group: "coder", Count: 1}, want: workforce.ErrNoTarget, says: "staffed by hand"},
		"no way to pick":              {config: two, req: workforce.SpawnRequest{Group: "coder", Count: 1}, want: workforce.ErrBadRequest, says: "assay, herdr-agentisan"},
		"an id past 32 chars":         {config: long, req: workforce.SpawnRequest{Group: "reviewer-of-absolutely-everything", Count: 1}, want: workforce.ErrInvalidID, says: "ha-reviewer-of-absolutely-everything-1"},
		"a config that breaks":        {config: "schema_version = 1\n[projects.herdr-agentisan]\nrepo = \"REPO\"\nlayout = \"grid\"\n", req: workforce.SpawnRequest{Group: "coder", Count: 1}, want: workforce.ErrConfig},
		"a model that is a flag":      {req: workforce.SpawnRequest{Group: "coder", Count: 1, Model: "--dangerously-skip-permissions"}, want: workforce.ErrBadRequest, says: "--model: ", hides: "--dangerously"},
		"a model a shell would split": {req: workforce.SpawnRequest{Group: "coder", Count: 1, Model: "opus'; touch pwned; '"}, want: settings.ErrInvalid, says: "--model: ", hides: "pwned"},
		"a model from the repo file a shell would split (A46)": {
			setup: func(t *testing.T, f *fixture) {
				f.repoFile(t, "[groups.coder.agent]\nmodel = \"opus'; touch pwned; '\"\n")
			},
			req: workforce.SpawnRequest{Group: "coder", Count: 1}, want: settings.ErrInvalid, says: "groups.coder.agent.model (repo, ", hides: "pwned",
		},
		"a long group from the repo file (A46)":                  {setup: func(t *testing.T, f *fixture) { f.repoFile(t, "[groups."+x40+"]\nworkers = 1\nslots = 1\n") }, req: workforce.SpawnRequest{Group: x40, Count: 1}, want: workforce.ErrInvalidID},
		"a model from the shared file a shell would split (A46)": {config: strings.Replace(teamConfig, "[projects.herdr-agentisan.groups.qa]\n", "[projects.herdr-agentisan.groups.qa]\nagent = { model = \"opus $(touch pwned)\" }\n", 1), req: workforce.SpawnRequest{Group: "qa", Count: 1}, want: workforce.ErrConfig, says: "groups.qa.agent.model", hides: "pwned"},
		"a hand-started agent takes a slot": {
			setup: func(_ *testing.T, f *fixture) {
				f.h.addTab(f.ws, "ha-coder-7", "claude", "working")
				f.h.addTab(f.ws, "ha-coder-8", "claude", "idle")
			},
			req: workforce.SpawnRequest{Group: "coder", Count: 2}, want: workforce.ErrSlotCap, says: "2 occupied, 2 requested",
		},
		"a count past every int": {
			setup: func(_ *testing.T, f *fixture) { f.h.addTab(f.ws, "ha-coder-7", "claude", "working") },
			req:   workforce.SpawnRequest{Group: "coder", Count: math.MaxInt}, want: workforce.ErrSlotCap, says: "1 occupied, 9223372036854775807 requested",
		},
		"a starting worker takes a slot": {
			setup: func(t *testing.T, f *fixture) {
				p := f.h.addTab(f.ws, "ha-coder-1", "", "unknown") // its agent not up yet
				f.bindRow(t, "ha-coder-1", "coder", p)
			},
			req: workforce.SpawnRequest{Group: "coder", Count: 3}, want: workforce.ErrSlotCap, says: "1 occupied, 3 requested",
		},
		"an agent named by its path": {
			config: strings.Replace(teamConfig, "[projects.herdr-agentisan.groups.qa]\n", "[projects.herdr-agentisan.groups.qa]\nagent = { command = [\"/opt/bin/codex\"] }\n", 1),
			req:    workforce.SpawnRequest{Group: "qa", Count: 1}, want: workforce.ErrConfig, says: "names the path /opt/bin/codex",
		},
		"an agent named by a relative path": {
			config: strings.Replace(teamConfig, "[projects.herdr-agentisan.groups.qa]\n", "[projects.herdr-agentisan.groups.qa]\nagent = { command = [\"bin/claude\"] }\n", 1),
			req:    workforce.SpawnRequest{Group: "qa", Count: 1}, want: workforce.ErrConfig, says: "names the path bin/claude",
		},
		"a group command from the repo file (A46)": {
			setup: func(t *testing.T, f *fixture) {
				f.repoFile(t, "[groups.coder.agent]\ncommand = [\"claude\", \"--from-the-repo\"]\n")
			},
			req: workforce.SpawnRequest{Group: "coder", Count: 1}, want: workforce.ErrConfig, says: "groups.coder.agent.command (repo, ",
		},
		"the top-level command from the repo file (A46)": {
			setup: func(t *testing.T, f *fixture) {
				f.repoFile(t, "[agent]\ncommand = [\"claude\", \"--from-the-repo\"]\n")
			},
			req: workforce.SpawnRequest{Group: "coder", Count: 1}, want: workforce.ErrConfig, says: "agent.command (repo, ",
		},
		"keys to type from the repo file (A46)": {
			setup: func(t *testing.T, f *fixture) { f.repoFile(t, "[spawn]\nshell_ready_keys = [\"y\", \"enter\"]\n") },
			req:   workforce.SpawnRequest{Group: "coder", Count: 1}, want: workforce.ErrConfig, says: "spawn.shell_ready_keys (repo, ",
		},
		"a group command that bypasses agentisan": {
			config: strings.Replace(teamConfig, "[projects.herdr-agentisan.groups.qa]\n", "[projects.herdr-agentisan.groups.qa]\nagent = { command = [\"claude\", \"--bare\"] }\n", 1),
			req:    workforce.SpawnRequest{Group: "qa", Count: 1}, want: settings.ErrBypassesAgentisan,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := tc.config
			if cfg == "" {
				cfg = teamConfig
			}
			f := newFixture(t, cfg)
			if tc.setup != nil {
				tc.setup(t, f)
			}

			_, err := f.svc.Spawn(t.Context(), tc.req)
			require.ErrorIs(t, err, tc.want)
			if tc.says != "" {
				assert.Contains(t, err.Error(), tc.says)
			}
			if tc.hides != "" {
				assert.NotContains(t, err.Error(), tc.hides)
			}
			assert.Empty(t, f.h.params("tab.create"), "nothing opens")
			assert.Empty(t, f.h.params("workspace.create"))
		})
	}
}

// TestConcurrentSpawnsShareTheCap: two spawns at once plan under one lock,
// so between them they never pass the cap or take one id twice.
func TestConcurrentSpawnsShareTheCap(t *testing.T) {
	t.Parallel()
	f := newFixture(t, teamConfig)
	f.h.readyAfter = 3
	results := make(chan error, 2)
	var mu sync.Mutex
	var ids []string
	for range 2 {
		go func() {
			res, err := f.svc.Spawn(t.Context(), workforce.SpawnRequest{Group: "coder", Count: 2})
			mu.Lock()
			for _, w := range res.Workers {
				ids = append(ids, w.ID)
			}
			mu.Unlock()
			results <- err
		}()
	}
	var refused int
	for range 2 {
		if err := <-results; err != nil {
			require.ErrorIs(t, err, workforce.ErrSlotCap)
			refused++
		}
	}
	assert.Equal(t, 1, refused, "the second spawn sees the first's tabs and rows")
	assert.ElementsMatch(t, []string{"ha-coder-1", "ha-coder-2"}, ids)
	assert.Len(t, f.h.params("tab.create"), 2)
}

// TestSpawnIDsFitHerdrsAgentNames: an id is a herdr agent name, at most 32
// characters: a type that makes the id 32 long spawns, and one a character
// longer is refused whole, before anything opens.
func TestSpawnIDsFitHerdrsAgentNames(t *testing.T) {
	t.Parallel()
	fits, over := strings.Repeat("a", 27), strings.Repeat("a", 28)
	f := newFixture(t, teamConfig+"\n[projects.herdr-agentisan.groups."+fits+"]\nworkers = 0\nslots = 1\n[projects.herdr-agentisan.groups."+over+"]\nworkers = 0\nslots = 1\n")

	res, err := f.svc.Spawn(t.Context(), workforce.SpawnRequest{Group: fits, Count: 1})
	require.NoError(t, err)
	assert.Len(t, res.Workers[0].ID, 32)
	_, err = f.svc.Spawn(t.Context(), workforce.SpawnRequest{Group: over, Count: 1})
	require.ErrorIs(t, err, workforce.ErrInvalidID)
	assert.Len(t, f.h.params("tab.create"), 1, "the refused one opened nothing")
}

// TestSpawnRunsThePreflight: the preflight (A34) checks the project's repo
// before anything opens; a refusal is ErrPreflight with the check's own
// error, and nothing opens. With no check set, preflight.HostCheck runs.
func TestSpawnRunsThePreflight(t *testing.T) {
	t.Parallel()
	f := newFixture(t, teamConfig)
	var checked []string
	f.svc.Preflight = func(_ context.Context, repo string) error {
		checked = append(checked, repo)
		return fmt.Errorf("%w: tdaddy: not found", preflight.ErrTdaddyMissing)
	}
	_, err := f.svc.Spawn(t.Context(), workforce.SpawnRequest{Group: "coder", Count: 1})
	require.ErrorIs(t, err, workforce.ErrPreflight)
	require.ErrorIs(t, err, preflight.ErrTdaddyMissing)
	assert.Equal(t, []string{f.repo}, checked)
	assert.Empty(t, f.h.params("tab.create"))
	assert.Empty(t, f.h.params("workspace.create"))

	// A go.mod that is a directory is HostCheck's ErrRepo, read from the repo
	// alone, never the host's PATH.
	require.NoError(t, os.Mkdir(filepath.Join(f.repo, "go.mod"), 0o700))
	f.svc.Preflight = nil
	_, err = f.svc.Spawn(t.Context(), workforce.SpawnRequest{Group: "coder", Count: 1})
	require.ErrorIs(t, err, preflight.ErrRepo)
	assert.Empty(t, f.h.params("tab.create"))
}

// TestSpawnOpensInTheRepoItChecked: a configuration edited during the
// preflight to another repo refuses the spawn, so no tab opens in a repo the
// check never saw.
func TestSpawnOpensInTheRepoItChecked(t *testing.T) {
	t.Parallel()
	f := newFixture(t, teamConfig)
	other := t.TempDir()
	marker := filepath.Join(other, filepath.FromSlash(settings.BundleMarker))
	require.NoError(t, os.MkdirAll(filepath.Dir(marker), 0o750))
	require.NoError(t, os.WriteFile(marker, []byte("1\n"), 0o600))
	f.svc.Preflight = func(context.Context, string) error {
		return os.WriteFile(filepath.Join(f.config, settings.FileName), []byte(strings.ReplaceAll(teamConfig, "REPO", other)), 0o600)
	}

	_, err := f.svc.Spawn(t.Context(), workforce.SpawnRequest{Group: "coder", Count: 1})
	require.ErrorIs(t, err, workforce.ErrPreflight)
	assert.Contains(t, err.Error(), "the repo changed")
	assert.Empty(t, f.h.params("tab.create"))
}

// TestSpawnPicksTheTeam: --project wins; else the team whose workspace holds
// the caller's pane, as FindCallerPane confirmed it; else the only tabs team.
func TestSpawnPicksTheTeam(t *testing.T) {
	t.Parallel()
	two := teamConfig + "\n[projects.assay]\nrepo = \"REPO\"\nshort = \"as\"\n[projects.assay.groups.coder]\nworkers = 0\nslots = 2\n"
	f := newFixture(t, two)
	as := f.h.addWorkspace("assay")
	asBoss := f.h.addTab(as, "as-boss", "claude", "idle")

	res, err := f.svc.Spawn(t.Context(), workforce.SpawnRequest{Selector: workforce.Selector{CallerPane: asBoss}, Group: "coder", Count: 1})
	require.NoError(t, err)
	assert.Equal(t, "assay", res.Project)
	assert.Equal(t, "as-coder-1", res.Workers[0].ID)

	res, err = f.svc.Spawn(t.Context(), workforce.SpawnRequest{Selector: workforce.Selector{Project: "herdr-agentisan", CallerPane: asBoss}, Group: "coder", Count: 1})
	require.NoError(t, err)
	assert.Equal(t, "ha-coder-1", res.Workers[0].ID, "--project beats the caller's pane")

	g := newFixture(t, teamConfig)
	res, err = g.svc.Spawn(t.Context(), workforce.SpawnRequest{Selector: workforce.Selector{CallerPane: "w99:p9"}, Group: "qa", Count: 1})
	require.NoError(t, err)
	assert.Equal(t, "herdr-agentisan", res.Project, "a pane in no team falls back to the only tabs team")
}

// TestSpawnOpensTheTeamWorkspace: a team with no workspace gets one,
// labelled with the project, never focused; its first tab is the first
// worker's, relabelled with its id.
func TestSpawnOpensTheTeamWorkspace(t *testing.T) {
	t.Parallel()
	cfg := teamConfig + "\n[projects.assay]\nrepo = \"REPO\"\nshort = \"as\"\n[projects.assay.groups.qa]\nworkers = 1\nslots = 2\n"
	f := newFixture(t, cfg)

	res, err := f.svc.Spawn(t.Context(), workforce.SpawnRequest{Selector: workforce.Selector{Project: "assay"}, Group: "qa", Count: 2})
	require.NoError(t, err)
	assert.Equal(t, []map[string]any{{"cwd": f.repo, "label": "assay", "focus": false, "env": map[string]any{workforce.WorkerEnv: "as-qa-1"}}}, f.h.params("workspace.create"))
	renames := f.h.params("tab.rename")
	require.Len(t, renames, 1)
	assert.Equal(t, "as-qa-1", renames[0]["label"])
	creates := f.h.params("tab.create")
	require.Len(t, creates, 1, "the second worker opens a tab in the new workspace")
	assert.Equal(t, "as-qa-2", creates[0]["label"])
	assert.Equal(t, renames[0]["tab_id"].(string)[:3], creates[0]["workspace_id"])
	assert.Len(t, res.Workers, 2)
}

// TestSpawnReadiness: the agent is waited for as herdr's CLI waits; one that
// is busy for a while still starts; every way it can fail closes its pane,
// marks its row failed and names it in ErrSpawnFailed.
func TestSpawnReadiness(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		knobs   func(*fakeHerdr)
		group   string
		wantErr string
	}{
		"ready after a few polls": {knobs: func(h *fakeHerdr) { h.readyAfter = 5 }},
		"busy, then a shell":      {knobs: func(h *fakeHerdr) { h.busy = 3 }},
		"codex is ready unknown":  {group: "review"},
		"busy for good":           {knobs: func(h *fakeHerdr) { h.busy = 1 << 20 }, wantErr: "agent_pane_busy"},
		"blocked during startup":  {knobs: func(h *fakeHerdr) { h.startStatus = "blocked" }, wantErr: "blocked during startup"},
		"exited before ready":     {knobs: func(h *fakeHerdr) { h.startStatus = "exited" }, wantErr: "exited before it became interactive"},
		"another kind detected":   {knobs: func(h *fakeHerdr) { h.startStatus = "kind:codex" }, wantErr: "expected claude, herdr detected codex"},
		"agent.start refused":     {knobs: func(h *fakeHerdr) { h.fail["agent.start"] = "invalid_agent_argument" }, wantErr: "invalid_agent_argument"},
		"agent.get answers badly": {knobs: func(h *fakeHerdr) { h.badGet = true }, wantErr: "ask herdr about"},
		"another terminal starts": {knobs: func(h *fakeHerdr) { h.startTerminal = "term99" }, wantErr: "agent.start answered for terminal term99"},
		"the name moves terminal": {knobs: func(h *fakeHerdr) { h.getTerminal = "term99" }, wantErr: "no longer owns its terminal"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, teamConfig)
			f.svc.BusyWindow = 300 * time.Millisecond
			if tc.knobs != nil {
				tc.knobs(f.h)
			}
			group := tc.group
			if group == "" {
				group = "coder"
			}

			res, err := f.svc.Spawn(t.Context(), workforce.SpawnRequest{Group: group, Count: 1})
			id := "ha-" + group + "-1"
			if tc.wantErr == "" {
				require.NoError(t, err)
				assert.Equal(t, store.StatusActive, f.row(t, id).Status)
				return
			}
			require.ErrorIs(t, err, workforce.ErrSpawnFailed)
			assert.Contains(t, err.Error(), id)
			assert.Contains(t, err.Error(), tc.wantErr)
			assert.Empty(t, res.Workers)
			assert.Nil(t, f.h.tabByLabel(id), "its pane, and so its tab, is closed")
			assert.Equal(t, store.StatusFailed, f.row(t, id).Status)
		})
	}
}

// TestSpawnRereadsABusyPane: a busy pane is re-read before each retry, and
// one herdr reissued to another terminal meanwhile gets no command and stays
// open (QA #78, F3.3); a re-read herdr refuses ends the retries (QA #78 R2).
func TestSpawnRereadsABusyPane(t *testing.T) {
	t.Parallel()
	f := newFixture(t, teamConfig)
	f.h.busy = 1
	f.h.onCall = func(method string) {
		if method != "agent.start" {
			return
		}
		for _, p := range f.h.panes {
			p.terminal = "term-stranger"
		}
	}

	_, err := f.svc.Spawn(t.Context(), workforce.SpawnRequest{Group: "coder", Count: 1})
	require.ErrorIs(t, err, workforce.ErrSpawnFailed)
	assert.Contains(t, err.Error(), "is no longer ha-coder-1's pane")
	assert.Len(t, f.h.params("agent.start"), 1, "nothing typed into another terminal")
	assert.NotNil(t, f.h.tabByLabel("ha-coder-1"), "another terminal's pane is left open")
	assert.Equal(t, store.StatusFailed, f.row(t, "ha-coder-1").Status)

	g := newFixture(t, teamConfig)
	g.h.busy = 1
	g.h.fail["pane.get"] = "server_error"
	_, err = g.svc.Spawn(t.Context(), workforce.SpawnRequest{Group: "coder", Count: 1})
	require.ErrorIs(t, err, workforce.ErrSpawnFailed)
	require.ErrorIs(t, err, workforce.ErrHerdr)
	assert.Contains(t, err.Error(), "re-read ha-coder-1's pane")
	assert.Len(t, g.h.params("agent.start"), 1, "no retry on a pane it could not re-read")
}

// TestSpawnTimesOut: an agent that never reports ready, or a shell whose
// prompt never shows, fails the spawn when the op's budget ends, as out of
// time and never as a herdr failure, and is cleaned up even so. A Claude
// that settles in unknown is not ready, interactive or not: only codex
// reports that.
func TestSpawnTimesOut(t *testing.T) {
	t.Parallel()
	withPrompt := strings.Replace(teamConfig, "layout = \"tabs\"\n", "layout = \"tabs\"\n\n[projects.herdr-agentisan.spawn]\nshell_prompt = '❯\\s*$'\n", 1)
	tests := map[string]struct {
		config string
		knobs  func(*fakeHerdr)
	}{
		"never settles":             {knobs: func(h *fakeHerdr) { h.readyAfter = 1 << 30 }},
		"a Claude in unknown":       {knobs: func(h *fakeHerdr) { h.startStatus = "unknown" }},
		"a prompt that never shows": {config: withPrompt, knobs: func(h *fakeHerdr) { h.promptAfter = 1 << 30 }},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := tc.config
			if cfg == "" {
				cfg = teamConfig
			}
			f := newFixture(t, cfg)
			tc.knobs(f.h)
			f.svc.ReadyPoll = 20 * time.Millisecond
			// Long enough that the deadline ends the wait, not the reads before
			// it, a spawn's preflight read included, on a loaded machine.
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()

			_, err := f.svc.Spawn(ctx, workforce.SpawnRequest{Group: "coder", Count: 1})
			require.ErrorIs(t, err, workforce.ErrSpawnFailed)
			require.ErrorIs(t, err, context.DeadlineExceeded)
			assert.NotErrorIs(t, err, workforce.ErrHerdr, "%v", err)
			assert.Equal(t, store.StatusFailed, f.row(t, "ha-coder-1").Status)
		})
	}
}

// edgeCtx has a deadline but never reports itself done: the instant between
// its deadline passing and its timer firing, held open.
type edgeCtx struct {
	context.Context
	deadline time.Time
}

func (c edgeCtx) Deadline() (time.Time, bool) { return c.deadline, true }

// TestSpawnAtTheDeadlineEdge: a herdr call the op's deadline ends, in the
// instant before the context's timer marks it done, is out of time, never a
// herdr failure (QA #78 R1): the prompt wait, and agent.start.
func TestSpawnAtTheDeadlineEdge(t *testing.T) {
	t.Parallel()
	withPrompt := strings.Replace(teamConfig, "layout = \"tabs\"\n", "layout = \"tabs\"\n\n[projects.herdr-agentisan.spawn]\nshell_prompt = '❯\\s*$'\n", 1)
	tests := map[string]struct{ config, held, says string }{
		"the prompt wait": {config: withPrompt, held: "pane.wait_for_output", says: "matched spawn.shell_prompt \"❯\\\\s*$\" in time: context deadline exceeded"},
		"agent.start":     {config: teamConfig, held: "agent.start", says: "start ha-coder-1: out of time: context deadline exceeded"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, tc.config)
			f.h.promptAfter = 1 << 30
			wrapHerdr(t, f, func(r herdrtest.Request, next func() herdrtest.Reply) herdrtest.Reply {
				if r.Method == tc.held {
					time.Sleep(3 * time.Second) // herdr holding the call past the op's deadline
				}
				return next()
			})
			ctx := edgeCtx{Context: t.Context(), deadline: time.Now().Add(1500 * time.Millisecond)}

			_, err := f.svc.Spawn(ctx, workforce.SpawnRequest{Group: "coder", Count: 1})
			require.ErrorIs(t, err, workforce.ErrSpawnFailed)
			require.ErrorIs(t, err, context.DeadlineExceeded)
			assert.NotErrorIs(t, err, workforce.ErrHerdr, "%v", err)
			assert.Contains(t, err.Error(), tc.says)
		})
	}
}

// TestSpawnKeepsAWorkerThatCameUp: a sibling that runs the op to its
// deadline fails alone; a worker already up stays active, its tab open.
func TestSpawnKeepsAWorkerThatCameUp(t *testing.T) {
	t.Parallel()
	f := newFixture(t, teamConfig)
	f.svc.ReadyPoll = 20 * time.Millisecond
	f.h.onCall = func(string) {
		for _, p := range f.h.panes {
			if p.name == "ha-coder-2" {
				p.polls = math.MinInt32 // never settles
			}
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()

	res, err := f.svc.Spawn(ctx, workforce.SpawnRequest{Group: "coder", Count: 2})
	require.ErrorIs(t, err, workforce.ErrSpawnFailed)
	assert.NotContains(t, err.Error(), "ha-coder-1")
	require.Len(t, res.Workers, 1)
	assert.Equal(t, "ha-coder-1", res.Workers[0].ID)
	assert.Equal(t, store.StatusActive, f.row(t, "ha-coder-1").Status)
	assert.NotNil(t, f.h.tabByLabel("ha-coder-1"))
	assert.Equal(t, store.StatusFailed, f.row(t, "ha-coder-2").Status)
	assert.Nil(t, f.h.tabByLabel("ha-coder-2"))
}

// TestSpawnWaitsForTheShellPrompt: with spawn.shell_prompt set, agent.start
// waits until a line of the screen matches it. The keys are sent during that
// wait, but only while a program, not the shell, has the terminal: while the
// shell itself asks oh-my-zsh's update question, nothing is typed into it.
func TestSpawnWaitsForTheShellPrompt(t *testing.T) {
	t.Parallel()
	withPrompt := strings.Replace(teamConfig, "layout = \"tabs\"\n", "layout = \"tabs\"\n\n[projects.herdr-agentisan.spawn]\nshell_prompt = '❯\\s*$'\nshell_ready_keys = [\"enter\"]\nshell_ready_after_s = 0\n", 1)
	tests := map[string]struct {
		fgProgram bool
		wantKeys  bool
	}{
		"the shell asks a question itself": {},
		"a program has the terminal":       {fgProgram: true, wantKeys: true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, withPrompt)
			f.h.promptAfter = 3
			f.h.fgProgram = tc.fgProgram

			res, err := f.svc.Spawn(t.Context(), workforce.SpawnRequest{Group: "coder", Count: 1})
			require.NoError(t, err)
			pane := res.Workers[0].PaneID
			m := f.h.methods()
			assert.Less(t, lastIndex(m, "pane.wait_for_output"), lastIndex(m, "agent.start"), "the prompt is waited for first")
			if tc.wantKeys {
				assert.Equal(t, [][]string{{"enter"}}, f.h.keys[pane], "sent once")
				assert.Less(t, firstIndex(m, "pane.send_keys"), lastIndex(m, "pane.wait_for_output"), "sent while the prompt is waited for")
			} else {
				assert.Empty(t, f.h.keys[pane], "never typed at the shell's own question")
			}
		})
	}
}

// TestThePromptWaitOutOfTime: a prompt wait with no time left asks herdr
// nothing and fails as the deadline does, as a wait the deadline cuts short
// does (TestSpawnTimesOut).
func TestThePromptWaitOutOfTime(t *testing.T) {
	t.Parallel()
	f := newFixture(t, teamConfig)
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()

	err := f.svc.WaitForPrompt(ctx, settings.Spawn{ShellPrompt: `❯\s*$`}, f.boss)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Contains(t, err.Error(), "spawn.shell_prompt")
	assert.Empty(t, f.h.params("pane.wait_for_output"))
}

// TestSpawnStopsWhenThePromptWaitFails: a prompt wait herdr refuses, rather
// than one that saw no match yet, fails the worker at once, naming the key
// herdr refused: a pattern Go compiles and Rust's regex does not reaches
// herdr as invalid_regex.
func TestSpawnStopsWhenThePromptWaitFails(t *testing.T) {
	t.Parallel()
	withPrompt := strings.Replace(teamConfig, "layout = \"tabs\"\n", "layout = \"tabs\"\n\n[projects.herdr-agentisan.spawn]\nshell_prompt = '❯\\s*$'\n", 1)
	f := newFixture(t, withPrompt)
	f.h.fail["pane.wait_for_output"] = "invalid_regex"
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	_, err := f.svc.Spawn(ctx, workforce.SpawnRequest{Group: "coder", Count: 1})
	require.ErrorIs(t, err, workforce.ErrSpawnFailed)
	require.ErrorIs(t, err, workforce.ErrHerdr)
	assert.Contains(t, err.Error(), "shell prompt, spawn.shell_prompt", "names the key herdr refused (NERD-5352 item 6)")
	assert.Contains(t, err.Error(), "invalid_regex")
	assert.NotErrorIs(t, err, context.DeadlineExceeded, "not waited out")
	assert.Len(t, f.h.params("pane.wait_for_output"), 1)
	assert.Empty(t, f.h.params("agent.start"))
	assert.Equal(t, store.StatusFailed, f.row(t, "ha-coder-1").Status)
}

// TestSpawnSendsKeysToABusyPane: with no prompt configured, a pane busy
// because a program has its terminal gets the keys once they are due, then
// takes the agent; keys that do not free it are not sent again; with no keys
// configured, nothing is sent.
func TestSpawnSendsKeysToABusyPane(t *testing.T) {
	t.Parallel()
	withKeys := strings.Replace(teamConfig, "layout = \"tabs\"\n", "layout = \"tabs\"\n\n[projects.herdr-agentisan.spawn]\nshell_ready_keys = [\"enter\"]\nshell_ready_after_s = 0\n", 1)
	f := newFixture(t, withKeys)
	f.h.fgProgram = true

	res, err := f.svc.Spawn(t.Context(), workforce.SpawnRequest{Group: "coder", Count: 1})
	require.NoError(t, err)
	assert.Equal(t, [][]string{{"enter"}}, f.h.keys[res.Workers[0].PaneID])

	stuck := newFixture(t, withKeys)
	stuck.h.fgProgram, stuck.h.stuckFg = true, true
	stuck.svc.BusyWindow = 200 * time.Millisecond
	_, err = stuck.svc.Spawn(t.Context(), workforce.SpawnRequest{Group: "coder", Count: 1})
	require.ErrorIs(t, err, workforce.ErrSpawnFailed)
	require.Len(t, stuck.h.keys, 1)
	for _, sent := range stuck.h.keys {
		assert.Equal(t, [][]string{{"enter"}}, sent, "sent once, though the pane stayed busy")
	}

	g := newFixture(t, teamConfig)
	g.h.fgProgram = true
	g.svc.BusyWindow = 200 * time.Millisecond
	_, err = g.svc.Spawn(t.Context(), workforce.SpawnRequest{Group: "coder", Count: 1})
	require.ErrorIs(t, err, workforce.ErrSpawnFailed)
	assert.Empty(t, g.h.keys, "no keys configured, none sent")
}

// TestSpawnCleansUpWhenATabFails: a tab that cannot open fails the spawn
// before any agent starts, and the workers opened before it have their panes
// closed and their rows failed. A tab that opens but cannot be labelled, or
// whose row cannot be recorded, is closed again too.
func TestSpawnCleansUpWhenATabFails(t *testing.T) {
	t.Parallel()
	t.Run("the second tab", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t, teamConfig)
		f.h.fail["tab.create"] = "workspace_not_found"
		f.h.failAfter["tab.create"] = 1

		_, err := f.svc.Spawn(t.Context(), workforce.SpawnRequest{Group: "coder", Count: 2})
		require.ErrorIs(t, err, workforce.ErrHerdr)
		assert.Len(t, f.h.params("tab.create"), 2)
		assert.Empty(t, f.h.params("agent.start"))
		assert.Nil(t, f.h.tabByLabel("ha-coder-1"), "the first worker's tab is closed again")
		assert.Equal(t, store.StatusFailed, f.row(t, "ha-coder-1").Status)
	})
	t.Run("a new workspace's tab that cannot be labelled", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t, teamConfig+"\n[projects.assay]\nrepo = \"REPO\"\nshort = \"as\"\n[projects.assay.groups.qa]\nworkers = 0\nslots = 1\n")
		f.h.fail["tab.rename"] = "server_unavailable"

		_, err := f.svc.Spawn(t.Context(), workforce.SpawnRequest{Selector: workforce.Selector{Project: "assay"}, Group: "qa", Count: 1})
		require.ErrorIs(t, err, workforce.ErrHerdr)
		assert.Len(t, f.h.params("workspace.create"), 1)
		assert.Len(t, f.h.params("pane.close"), 1, "its root pane is closed, and the workspace with it")
		f.h.mu.Lock()
		for _, w := range f.h.workspaces {
			assert.NotEqual(t, "assay", w.label)
		}
		f.h.mu.Unlock()
		_, err = f.st.WorkerRow(t.Context(), "as-qa-1")
		require.ErrorIs(t, err, store.ErrNoWorker, "no row was bound")
	})
	t.Run("a row that cannot be recorded", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t, teamConfig)
		f.h.onCall = func(method string) {
			if method == "tab.create" {
				_ = f.st.Close()
			}
		}

		_, err := f.svc.Spawn(t.Context(), workforce.SpawnRequest{Group: "coder", Count: 1})
		require.ErrorIs(t, err, store.ErrStore)
		assert.Len(t, f.h.params("pane.close"), 1)
		assert.Nil(t, f.h.tabByLabel("ha-coder-1"), "its tab is closed again")
		assert.Empty(t, f.h.params("agent.start"))
	})
}

// TestSpawnCleanupOfAGonePane: cleaning up a worker that did not come up, a
// pane herdr no longer has is closed already, no failure of its own, and a
// pane id herdr gave another terminal is left alone (A35); a close herdr
// refuses is reported beside the worker's failure.
func TestSpawnCleanupOfAGonePane(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		code      string
		reissued  bool
		wantHerdr bool
	}{
		"gone already":    {},
		"its id reissued": {reissued: true},
		"close refused":   {code: "server_unavailable", wantHerdr: true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, teamConfig)
			f.h.startStatus = "blocked"
			switch {
			case tc.code != "":
				f.h.fail["pane.close"] = tc.code
			default:
				f.h.onCall = func(method string) { // under the fake's lock
					if method != "pane.get" {
						return
					}
					i := slices.IndexFunc(f.h.panes, func(p *fakePane) bool { return p.name == "ha-coder-1" })
					if i >= 0 && tc.reissued {
						f.h.panes[i].terminal, f.h.panes[i].name = "term_stranger", ""
					} else if i >= 0 {
						f.h.panes = slices.Delete(f.h.panes, i, i+1)
					}
				}
			}

			_, err := f.svc.Spawn(t.Context(), workforce.SpawnRequest{Group: "coder", Count: 1})
			require.ErrorIs(t, err, workforce.ErrSpawnFailed)
			assert.Contains(t, err.Error(), "blocked during startup")
			assert.Equal(t, tc.wantHerdr, errors.Is(err, workforce.ErrHerdr))
			assert.Equal(t, store.StatusFailed, f.row(t, "ha-coder-1").Status)
			if tc.reissued {
				assert.Empty(t, f.h.params("pane.close"), "a stranger at the worker's old id is never closed")
			}
		})
	}
}

// TestSpawnLeavesAWorkerRetiredWhileStarting: a worker retired while its
// agent starts keeps its retired row when the start then fails, and the
// failure is the agent's, not a missing live row's.
func TestSpawnLeavesAWorkerRetiredWhileStarting(t *testing.T) {
	t.Parallel()
	f := newFixture(t, teamConfig)
	f.h.startStatus = "blocked"
	var retireErr error
	f.h.onCall = func(method string) {
		if method == "agent.start" {
			retireErr = f.st.SetWorkerStatus(context.Background(), store.Worker{LogicalID: "ha-coder-1"}, store.StatusRetired, time.Now())
		}
	}

	_, err := f.svc.Spawn(t.Context(), workforce.SpawnRequest{Group: "coder", Count: 1})
	f.h.mu.Lock()
	rerr := retireErr
	f.h.mu.Unlock()
	require.NoError(t, rerr)
	require.ErrorIs(t, err, workforce.ErrSpawnFailed)
	assert.Contains(t, err.Error(), "blocked during startup")
	assert.NotErrorIs(t, err, store.ErrNoWorker)
	assert.Equal(t, store.StatusRetired, f.row(t, "ha-coder-1").Status)
}

// TestSpawnRunsCodexOffTheDaemon: every Codex launch gets --no-daemon (A22),
// once, since Codex refuses it twice; a Claude launch never does
// (TestSpawnOpensOneTabPerWorker).
func TestSpawnRunsCodexOffTheDaemon(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		agent string
		want  []any
	}{
		"a group's own codex":        {agent: `{ command = ["codex"] }`, want: []any{"--no-daemon"}},
		"with its model":             {agent: `{ command = ["codex"], model = "o3" }`, want: []any{"--no-daemon", "--model", "o3"}},
		"the flag already in it":     {agent: `{ command = ["codex", "-s", "workspace-write", "--no-daemon"] }`, want: []any{"-s", "workspace-write", "--no-daemon"}},
		"a command that is no codex": {agent: `{ command = ["claude"] }`, want: []any{"--name", "ha-qa-1"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, strings.Replace(teamConfig, "[projects.herdr-agentisan.groups.qa]\n", "[projects.herdr-agentisan.groups.qa]\nagent = "+tc.agent+"\n", 1))

			_, err := f.svc.Spawn(t.Context(), workforce.SpawnRequest{Group: "qa", Count: 1})
			require.NoError(t, err)
			assert.Equal(t, tc.want, f.h.params("agent.start")[0]["args"])
		})
	}
}

// TestSpawnACodexThatStopsAtStartup: a Codex blocked at startup, or at its
// hook review, which herdr does not report as blocked, fails with a hint
// naming both of Codex's startup screens; a Claude blocked gets no Codex
// hint, and a screen herdr cannot read fails the worker.
func TestSpawnACodexThatStopsAtStartup(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		group  string
		knobs  func(*fakeHerdr)
		says   string
		noHint bool
		herdr  bool
	}{
		"a Codex blocked":       {group: "review", knobs: func(h *fakeHerdr) { h.startStatus = "blocked" }, says: "blocked during startup"},
		"a Codex at its review": {group: "review", knobs: func(h *fakeHerdr) { h.screen = "Hooks need review" }, says: "is at Codex's hook review"},
		"a Claude blocked":      {group: "coder", knobs: func(h *fakeHerdr) { h.startStatus = "blocked" }, says: "blocked during startup", noHint: true},
		"an unreadable screen":  {group: "review", knobs: func(h *fakeHerdr) { h.fail["pane.wait_for_output"] = "server_unavailable" }, says: "read ha-review-1's screen", noHint: true, herdr: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, teamConfig)
			tc.knobs(f.h)

			_, err := f.svc.Spawn(t.Context(), workforce.SpawnRequest{Group: tc.group, Count: 1})
			require.ErrorIs(t, err, workforce.ErrSpawnFailed)
			assert.Contains(t, err.Error(), tc.says)
			assert.Equal(t, !tc.noHint, strings.Contains(err.Error(), "hook review or its directory-trust prompt"), err.Error())
			assert.Equal(t, tc.herdr, errors.Is(err, workforce.ErrHerdr))
			assert.Equal(t, store.StatusFailed, f.row(t, "ha-"+tc.group+"-1").Status)
		})
	}
}

// TestSpawnWarnsOfCodexHooks: a spawn with a Codex worker warns, and never
// refuses, when Codex's ctx hooks are not installed or cannot be checked; a
// spawn with none asks nothing.
func TestSpawnWarnsOfCodexHooks(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		group     string
		installed bool
		err       error
		want      string
	}{
		"not installed":   {group: "review", want: "Codex's ctx hooks are not installed"},
		"installed":       {group: "review", installed: true},
		"not checkable":   {group: "review", err: errors.New("hooks.json is not JSON"), want: "could not check Codex's ctx hooks: hooks.json is not JSON"},
		"no Codex worker": {group: "coder"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, teamConfig)
			asked := false
			f.svc.CodexHooks = func() (bool, error) { asked = true; return tc.installed, tc.err }

			res, err := f.svc.Spawn(t.Context(), workforce.SpawnRequest{Group: tc.group, Count: 1})
			require.NoError(t, err)
			require.Len(t, res.Workers, 1)
			if tc.want == "" {
				assert.Empty(t, res.Warnings)
			} else {
				require.Len(t, res.Warnings, 1)
				assert.Contains(t, res.Warnings[0], tc.want)
			}
			assert.Equal(t, tc.group == "review", asked)
		})
	}
}

// wrapHerdr points the service at a server in front of f's fake that answers
// through h; next is the fake's own answer.
func wrapHerdr(t *testing.T, f *fixture, h func(r herdrtest.Request, next func() herdrtest.Reply) herdrtest.Reply) *herdrtest.Server {
	t.Helper()
	srv := herdrtest.Start(t, func(r herdrtest.Request) herdrtest.Reply {
		return h(r, func() herdrtest.Reply { return f.h.handle(r) })
	})
	f.svc.Herdr = herdr.Client{SocketPath: srv.Path}
	return srv
}

func countCalls(srv *herdrtest.Server, method string) int {
	n := 0
	for _, r := range srv.Requests() {
		if r.Method == method {
			n++
		}
	}
	return n
}

// TestSpawnPollsAFailedAgentGet: an agent.get herdr refuses, the name not yet
// registered, or one whose connection drops, is polled again, not failed
// (QA #78).
func TestSpawnPollsAFailedAgentGet(t *testing.T) {
	t.Parallel()
	for name, fail := range map[string]herdrtest.Reply{
		"agent_not_found":      {Error: &herdrtest.ErrorBody{Code: "agent_not_found", Message: "agent_not_found"}},
		"a dropped connection": {Silent: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, teamConfig)
			var mu sync.Mutex
			n := 0
			srv := wrapHerdr(t, f, func(r herdrtest.Request, next func() herdrtest.Reply) herdrtest.Reply {
				if r.Method == "agent.get" {
					mu.Lock()
					n++
					k := n
					mu.Unlock()
					if k <= 3 {
						return fail
					}
				}
				return next()
			})
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()

			res, err := f.svc.Spawn(ctx, workforce.SpawnRequest{Group: "coder", Count: 1})
			require.NoError(t, err)
			require.Len(t, res.Workers, 1)
			assert.Equal(t, store.StatusActive, f.row(t, "ha-coder-1").Status)
			assert.GreaterOrEqual(t, countCalls(srv, "agent.get"), 4)
		})
	}
}

// TestSpawnPollsAnAgentGetThatTimesOut: an agent.get the client's own call
// timeout ends is polled again, as an unreachable herdr is (QA #78, F1).
func TestSpawnPollsAnAgentGetThatTimesOut(t *testing.T) {
	t.Parallel()
	f := newFixture(t, teamConfig)
	var held atomic.Bool
	srv := wrapHerdr(t, f, func(r herdrtest.Request, next func() herdrtest.Reply) herdrtest.Reply {
		if r.Method == "agent.get" && held.CompareAndSwap(false, true) {
			time.Sleep(2 * time.Second)
		}
		return next()
	})
	f.svc.Herdr.Timeout = 500 * time.Millisecond

	_, err := f.svc.Spawn(t.Context(), workforce.SpawnRequest{Group: "coder", Count: 1})
	require.NoError(t, err)
	assert.Equal(t, store.StatusActive, f.row(t, "ha-coder-1").Status)
	assert.GreaterOrEqual(t, countCalls(srv, "agent.get"), 2)
}

// TestSpawnStartTimeoutFollowsTheDeadline: agent.start's timeout is the time
// the op has left, at most 90 s and never below herdr's 3,001 ms (QA #78).
func TestSpawnStartTimeoutFollowsTheDeadline(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		budget   time.Duration
		min, max float64
	}{
		"less than herdr's floor left": {budget: 2900 * time.Millisecond, min: 3001, max: 3001},
		"less than 90 s left":          {budget: 60 * time.Second, min: 45000, max: 60000},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, teamConfig)
			ctx, cancel := context.WithTimeout(t.Context(), tc.budget)
			defer cancel()

			_, err := f.svc.Spawn(ctx, workforce.SpawnRequest{Group: "coder", Count: 1})
			require.NoError(t, err)
			starts := f.h.params("agent.start")
			require.Len(t, starts, 1)
			ms, _ := starts[0]["timeout_ms"].(float64)
			assert.GreaterOrEqual(t, ms, tc.min)
			assert.LessOrEqual(t, ms, tc.max)
		})
	}
}

// TestSpawnRetriesOnlyABusyPane: only a busy pane is retried;
// agent_name_taken fails the worker on the first answer (QA #78).
func TestSpawnRetriesOnlyABusyPane(t *testing.T) {
	t.Parallel()
	f := newFixture(t, teamConfig)
	f.h.fail["agent.start"] = "agent_name_taken"

	_, err := f.svc.Spawn(t.Context(), workforce.SpawnRequest{Group: "coder", Count: 1})
	require.ErrorIs(t, err, workforce.ErrSpawnFailed)
	assert.Contains(t, err.Error(), "agent_name_taken")
	assert.Len(t, f.h.params("agent.start"), 1, "a taken name is not retried")
}

// TestSpawnWaitsForACodexStillStarting: a Codex still launching reports
// unknown and not interactive_ready; it is waited for, not taken as ready
// (QA #78).
func TestSpawnWaitsForACodexStillStarting(t *testing.T) {
	t.Parallel()
	f := newFixture(t, teamConfig)
	f.h.readyAfter = 3

	res, err := f.svc.Spawn(t.Context(), workforce.SpawnRequest{Group: "review", Count: 1})
	require.NoError(t, err)
	require.Len(t, res.Workers, 1)
	assert.GreaterOrEqual(t, len(f.h.params("agent.get")), 4, "polled until herdr settles it")
}

// TestSpawnWaitsOutAnIdlePendingLaunch: an agent idle but not yet interactive
// while its launch is pending is still starting, not exited (QA #78).
func TestSpawnWaitsOutAnIdlePendingLaunch(t *testing.T) {
	t.Parallel()
	f := newFixture(t, teamConfig)
	f.h.readyAfter = 3
	f.h.onCall = func(method string) {
		if method != "agent.get" {
			return
		}
		for _, p := range f.h.panes {
			if p.name == "ha-coder-1" && p.pending {
				p.status = "idle"
			}
		}
	}

	_, err := f.svc.Spawn(t.Context(), workforce.SpawnRequest{Group: "coder", Count: 1})
	require.NoError(t, err)
	assert.Equal(t, store.StatusActive, f.row(t, "ha-coder-1").Status)
}

// TestSpawnTakesDoneAndReadyAsUp: an agent herdr reports done and
// interactive_ready is up (QA #78).
func TestSpawnTakesDoneAndReadyAsUp(t *testing.T) {
	t.Parallel()
	f := newFixture(t, teamConfig)
	wrapHerdr(t, f, func(r herdrtest.Request, next func() herdrtest.Reply) herdrtest.Reply {
		rep := next()
		if r.Method == "agent.get" {
			if m, ok := rep.Result.(map[string]any); ok {
				if a, ok := m["agent"].(map[string]any); ok && a["agent_status"] == "idle" {
					a["agent_status"] = "done"
				}
			}
		}
		return rep
	})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	_, err := f.svc.Spawn(ctx, workforce.SpawnRequest{Group: "coder", Count: 1})
	require.NoError(t, err)
}

// TestSpawnKeysWaitForShellReadyAfter: the keys wait shell_ready_after_s; a
// pane freed before then never gets them, and one still busy at the window's
// end gets none either, as A23 says (QA #78).
func TestSpawnKeysWaitForShellReadyAfter(t *testing.T) {
	t.Parallel()
	cfg := strings.Replace(teamConfig, "layout = \"tabs\"\n", "layout = \"tabs\"\n\n[projects.herdr-agentisan.spawn]\nshell_ready_keys = [\"enter\"]\nshell_ready_after_s = 2\n", 1)
	f := newFixture(t, cfg)
	f.h.fgProgram = true
	f.svc.BusyWindow = 300 * time.Millisecond

	_, err := f.svc.Spawn(t.Context(), workforce.SpawnRequest{Group: "coder", Count: 1})
	require.ErrorIs(t, err, workforce.ErrSpawnFailed)
	assert.Empty(t, f.h.keys, "not due within the busy window")
}

// TestSpawnSendsNoKeysWithoutAShellPID: a pane whose shell pid herdr does not
// know gets no keys: spawn cannot tell whether the shell itself has the
// terminal (QA #78).
func TestSpawnSendsNoKeysWithoutAShellPID(t *testing.T) {
	t.Parallel()
	cfg := strings.Replace(teamConfig, "layout = \"tabs\"\n", "layout = \"tabs\"\n\n[projects.herdr-agentisan.spawn]\nshell_ready_keys = [\"enter\"]\nshell_ready_after_s = 0\n", 1)
	f := newFixture(t, cfg)
	f.h.fgProgram = true
	f.svc.BusyWindow = 300 * time.Millisecond
	wrapHerdr(t, f, func(r herdrtest.Request, next func() herdrtest.Reply) herdrtest.Reply {
		rep := next()
		if r.Method == "pane.process_info" {
			if m, ok := rep.Result.(map[string]any); ok {
				if pi, ok := m["process_info"].(map[string]any); ok {
					pi["shell_pid"] = 0
				}
			}
		}
		return rep
	})

	_, err := f.svc.Spawn(t.Context(), workforce.SpawnRequest{Group: "coder", Count: 1})
	require.ErrorIs(t, err, workforce.ErrSpawnFailed)
	assert.Empty(t, f.h.keys)
}

// TestSpawnSeesTheHookReviewByItsTrustLine: Codex's hook review is told by
// either of its lines (QA #78).
func TestSpawnSeesTheHookReviewByItsTrustLine(t *testing.T) {
	t.Parallel()
	f := newFixture(t, teamConfig)
	f.h.screen = "Trust all and continue"

	_, err := f.svc.Spawn(t.Context(), workforce.SpawnRequest{Group: "review", Count: 1})
	require.ErrorIs(t, err, workforce.ErrSpawnFailed)
	assert.Contains(t, err.Error(), "is at Codex's hook review")
}

// TestSpawnRefusesABadModelBeforeThePreflight: a --model A46 refuses never
// runs the preflight, as A23 says (QA #78).
func TestSpawnRefusesABadModelBeforeThePreflight(t *testing.T) {
	t.Parallel()
	f := newFixture(t, teamConfig)
	called := false
	f.svc.Preflight = func(context.Context, string) error { called = true; return nil }

	_, err := f.svc.Spawn(t.Context(), workforce.SpawnRequest{Group: "coder", Count: 1, Model: "-x"})
	require.ErrorIs(t, err, workforce.ErrBadRequest)
	assert.False(t, called)
}

// TestSpawnKeepsAnotherProjectsIDs: an id is one row across every project, so
// another project's, retired or lost, is never rebound into this one: its
// queue and handoffs stay that project's, and spawn takes the next free n
// (QA #78 F4).
func TestSpawnKeepsAnotherProjectsIDs(t *testing.T) {
	t.Parallel()
	f := newFixture(t, teamConfig)
	f.bindRow(t, "ha-coder-1", "coder", "w99:p1")
	f.exec(t, "UPDATE workers SET project = 'old-ha', terminal_id = 'term_old1', status = 'retired', retired_at = 1 WHERE logical_id = 'ha-coder-1'")
	f.bindRow(t, "ha-coder-2", "coder", "w99:p2")
	f.exec(t, "UPDATE workers SET project = 'old-ha', terminal_id = 'term_old2', status = 'lost' WHERE logical_id = 'ha-coder-2'")

	res, err := f.svc.Spawn(t.Context(), workforce.SpawnRequest{Group: "coder", Count: 2})
	require.NoError(t, err)
	require.Len(t, res.Workers, 2)
	assert.Equal(t, []string{"ha-coder-3", "ha-coder-4"}, []string{res.Workers[0].ID, res.Workers[1].ID})
	for _, id := range []string{"ha-coder-1", "ha-coder-2"} {
		row := f.row(t, id)
		assert.Equal(t, "old-ha", row.Project, id)
		assert.Zero(t, row.Generation, id)
	}
	assert.Equal(t, store.StatusLost, f.row(t, "ha-coder-2").Status)
}

// TestSpawnCleansUpAtOnce: the workers up are marked active first, then every
// worker that did not come up is cleaned up at once, on one budget, so a
// slow herdr holds the reply for one cleanup, not one after another (QA #78
// N1).
func TestSpawnCleansUpAtOnce(t *testing.T) {
	t.Parallel()
	f := newFixture(t, teamConfig)
	var (
		mu       sync.Mutex
		arrived  int
		together int
	)
	all := make(chan struct{})
	wrapHerdr(t, f, func(r herdrtest.Request, next func() herdrtest.Reply) herdrtest.Reply {
		switch {
		case r.Method == "agent.start" && !strings.Contains(string(r.Params), `"ha-coder-1"`):
			return errReply("invalid_agent_name")
		case r.Method == "pane.close":
			mu.Lock()
			if arrived++; arrived == 2 {
				close(all)
			}
			mu.Unlock()
			select {
			case <-all:
				mu.Lock()
				together++
				mu.Unlock()
			case <-time.After(2 * time.Second):
			}
		}
		return next()
	})

	res, err := f.svc.Spawn(t.Context(), workforce.SpawnRequest{Group: "coder", Count: 3})
	require.ErrorIs(t, err, workforce.ErrSpawnFailed)
	mu.Lock()
	assert.Equal(t, 2, together, "both cleanups were in flight at once")
	mu.Unlock()
	require.Len(t, res.Workers, 1)
	assert.Equal(t, store.StatusActive, f.row(t, "ha-coder-1").Status)
	for _, id := range []string{"ha-coder-2", "ha-coder-3"} {
		assert.Equal(t, store.StatusFailed, f.row(t, id).Status)
		assert.Nil(t, f.h.tabByLabel(id))
	}
}
