package workforce_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/routing"
	"github.com/abanna/herdr-agentisan/internal/settings"
	"github.com/abanna/herdr-agentisan/internal/store"
	"github.com/abanna/herdr-agentisan/internal/workforce"
)

// opsGroup is a group of the configuration that no routing role has.
const opsGroup = `
[projects.herdr-agentisan.groups.ops]
workers = 0
slots = 1
`

// routingOn is teamConfig, with an ops group, while routing decides.
const routingOn = teamConfig + opsGroup + `
[routing]
enabled = true
`

// routedAt is when the decisions of these tests are made.
var routedAt = time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

// recorder keeps what a spawn hands the Routed hook.
type recorder struct {
	mu     sync.Mutex
	routed []workforce.Routed
}

func (r *recorder) record(_ context.Context, rt workforce.Routed) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.routed = append(r.routed, rt)
}

func (r *recorder) all() []workforce.Routed {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]workforce.Routed(nil), r.routed...)
}

// routedFixture is a fixture on config whose spawns are recorded, at
// routedAt.
func routedFixture(t *testing.T, config string) (*fixture, *recorder) {
	t.Helper()
	f := newFixture(t, config)
	rec := &recorder{}
	f.svc.Routed = rec.record
	f.svc.Now = func() time.Time { return routedAt }
	return f, rec
}

// routingOf is the routing logicalID's row records, its time checked and
// left out, so the rest compares exactly.
func routingOf(t *testing.T, f *fixture, logicalID string) *store.Routing {
	t.Helper()
	r := f.row(t, logicalID).Routing
	if r != nil {
		assert.True(t, r.RoutedAt.Equal(routedAt), "routed at %v", r.RoutedAt)
		r.RoutedAt = time.Time{}
	}
	return r
}

// TestSpawnLaunchesWhatRoutingChose: with routing on, each type starts on
// its role's default for the agent its command runs, at the role's effort:
// Claude with --model and --effort, Codex with --model and -c. One decision
// covers every worker of the spawn; each row records it, and the result
// says it (A50).
func TestSpawnLaunchesWhatRoutingChose(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		group, kind, role, binding, model string
		count                             int
		args                              func(id string) []any
		reason                            routing.Reason
	}{
		"a coder, on Claude": {
			group: "coder", count: 2, kind: "claude", role: "coder", binding: "claude_mid", model: "claude-sonnet-5-5", reason: routing.ReasonTaskNotFound,
			args: func(id string) []any {
				return withHooksAny("--model", "claude-sonnet-5-5", "--effort", "high", "--name", id)
			},
		},
		"qa takes pre_check's binding": {
			group: "qa", count: 1, kind: "claude", role: "pre_check", binding: "claude_mid", model: "claude-sonnet-5-5",
			args: func(id string) []any {
				return withHooksAny("--model", "claude-sonnet-5-5", "--effort", "high", "--name", id)
			},
		},
		"review, on Codex": {
			group: "review", count: 1, kind: "codex", role: "code_review", binding: "codex_strong", model: "gpt-6.1-sol",
			args: func(string) []any {
				return []any{"--no-daemon", "--sandbox", "read-only", "--model", "gpt-6.1-sol", "-c", `model_reasoning_effort="high"`}
			},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f, rec := routedFixture(t, routingOn)

			res, err := f.svc.Spawn(t.Context(), workforce.SpawnRequest{Group: tc.group, Count: tc.count})
			require.NoError(t, err)
			starts := f.h.params("agent.start")
			require.Len(t, starts, tc.count)
			for _, s := range starts {
				assert.Equal(t, tc.kind, s["kind"])
				assert.Equal(t, tc.args(s["name"].(string)), s["args"])
			}
			routed := rec.all()
			require.Len(t, routed, 1, "one decision for the spawn")
			d := routed[0].Decision
			require.NotNil(t, d)
			assert.Nil(t, routed[0].Refused)
			assert.Equal(t, "worker spawn", routed[0].Op)
			assert.Equal(t, "herdr-agentisan", routed[0].Project)
			assert.Equal(t, tc.group, routed[0].Group)
			assert.True(t, d.Applied)
			assert.Equal(t, tc.reason, d.FallbackReason)
			require.Len(t, res.Workers, tc.count)
			ids := make([]string, 0, tc.count)
			for _, w := range res.Workers {
				ids = append(ids, w.ID)
				assert.Equal(t, workforce.Spawned{ID: w.ID, Group: tc.group, PaneID: w.PaneID, Agent: tc.kind, Binding: tc.binding, Model: tc.model, Effort: "high", FallbackReason: tc.reason}, w)
				assert.Equal(t, &store.Routing{Role: tc.role, Provider: tc.kind, Binding: tc.binding, ModelID: tc.model, Effort: "high", PolicyVersion: d.PolicyVersion}, routingOf(t, f, w.ID))
			}
			assert.Equal(t, ids, routed[0].IDs)
			assert.NotEmpty(t, d.PolicyVersion)
		})
	}
}

// TestSpawnDecidesByTheResolvedPolicy: a spawn's decision carries the policy
// version config resolve --project prints for the same configuration, one
// whose binding facts and overrides are in that version (A50).
func TestSpawnDecidesByTheResolvedPolicy(t *testing.T) {
	t.Parallel()
	f, rec := routedFixture(t, routingOn+"\n[routing.bindings.claude_mid]\nmodel_id = \"claude-sonnet-5-6\"\n\n[routing.overrides]\nresearcher = \"claude_small\"\n")

	_, err := f.svc.Spawn(t.Context(), workforce.SpawnRequest{Group: "coder", Count: 1})
	require.NoError(t, err)
	res, err := settings.Load(settings.LoadOptions{ConfigDir: f.config, Project: "herdr-agentisan", LookupEnv: func(string) (string, bool) { return "", false }})
	require.NoError(t, err)
	resolved, err := res.Policy()
	require.NoError(t, err)
	plain, err := routing.New(routing.V1(), routing.Deployment{Enabled: true})
	require.NoError(t, err)
	require.Len(t, rec.all(), 1)
	d := rec.all()[0].Decision
	require.NotNil(t, d)
	assert.Equal(t, resolved.Version(), d.PolicyVersion)
	assert.NotEqual(t, plain.Version(), d.PolicyVersion, "the facts and the override are in it")
	assert.Equal(t, "claude-sonnet-5-6", d.ModelID)
}

// TestSpawnTakesTheBossOverride: --model names a binding the role allows, by
// its name or its model id, and --effort one it takes; both are recorded as
// the boss's override, and the role's default is not asked for.
func TestSpawnTakesTheBossOverride(t *testing.T) {
	t.Parallel()
	for _, model := range []string{"claude_strong", "claude-opus-5-5"} {
		t.Run(model, func(t *testing.T) {
			t.Parallel()
			f, rec := routedFixture(t, routingOn)

			res, err := f.svc.Spawn(t.Context(), workforce.SpawnRequest{Group: "coder", Count: 1, Model: model, Effort: "xhigh"})
			require.NoError(t, err)
			assert.Equal(t, withHooksAny("--model", "claude-opus-5-5", "--effort", "xhigh", "--name", "ha-coder-1"), f.h.params("agent.start")[0]["args"])
			assert.Equal(t, routing.ReasonNone, res.Workers[0].FallbackReason, "an override is no fallback")
			assert.Equal(t, &store.Routing{
				Role: "coder", Provider: "claude", Binding: "claude_strong", ModelID: "claude-opus-5-5", Effort: "xhigh",
				OverrideBinding: "claude_strong", OverrideBy: "boss", OverrideEffort: "xhigh", PolicyVersion: rec.all()[0].Decision.PolicyVersion,
			}, routingOf(t, f, "ha-coder-1"))
		})
	}
}

// TestSpawnReadsTheTaskPacket: the packet's task and assignment reach the
// decision, which records the assignment by its digest only.
func TestSpawnReadsTheTaskPacket(t *testing.T) {
	t.Parallel()
	f, rec := routedFixture(t, routingOn)
	id := "NERD-5289"
	task := &routing.Task{TaskID: &id, LookupStatus: routing.LookupFound, WorkerAssignment: "wire spawn to routing", RequestedRole: "coder"}

	res, err := f.svc.Spawn(t.Context(), workforce.SpawnRequest{Group: "coder", Count: 1, Task: task})
	require.NoError(t, err)
	d := rec.all()[0].Decision
	require.NotNil(t, d)
	assert.Equal(t, &id, d.TaskID)
	assert.Equal(t, routing.LookupFound, d.TaskLookupStatus)
	assert.Equal(t, routing.ReasonJevDisabled, d.FallbackReason)
	assert.Equal(t, routing.ReasonJevDisabled, res.Workers[0].FallbackReason)
	assert.True(t, strings.HasPrefix(d.AssignmentDigest, "sha256:"), d.AssignmentDigest)
	r := routingOf(t, f, "ha-coder-1")
	require.NotNil(t, r)
	assert.Equal(t, d.AssignmentDigest, r.AssignmentDigest)
}

// TestSpawnRoutingRefusals: with routing on, an ask routing cannot follow
// opens nothing, and each is the sentinel the daemon answers with its own
// code; a malformed packet is a bad request.
func TestSpawnRoutingRefusals(t *testing.T) {
	t.Parallel()
	maybe := "maybe"
	long := strings.Repeat("x", 513)
	tests := map[string]struct {
		config string
		req    workforce.SpawnRequest
		want   error
		says   string
	}{
		"a group no role has":        {req: workforce.SpawnRequest{Group: "ops", Count: 1}, want: routing.ErrUnknownRole, says: `"ops"`},
		"an unknown group stays one": {req: workforce.SpawnRequest{Group: "designer", Count: 1}, want: workforce.ErrUnknownGroup, says: "configured: coder, ops, qa, review"},
		"a group that only describes": {
			config: routingOn + "\n[projects.herdr-agentisan.groups.notes]\ndescription = \"takes notes\"\n",
			req:    workforce.SpawnRequest{Group: "notes", Count: 1}, want: workforce.ErrUnknownGroup, says: "configured: coder, ops, qa, review",
		},
		"claude by its path":        {config: strings.Replace(routingOn, "groups.qa]\n", "groups.qa]\nagent = { command = [\"/usr/local/bin/claude\"] }\n", 1), req: workforce.SpawnRequest{Group: "qa", Count: 1}, want: workforce.ErrConfig, says: "names the path /usr/local/bin/claude"},
		"claude by a relative path": {config: strings.Replace(routingOn, "groups.qa]\n", "groups.qa]\nagent = { command = [\"bin/claude\"] }\n", 1), req: workforce.SpawnRequest{Group: "qa", Count: 1}, want: workforce.ErrConfig, says: "names the path bin/claude"},
		// Load refuses it before routing would block it (A62): a gap until
		// routing v2 and a read-only Claude review launch.
		"review under claude, a gap until routing v2": {
			config: strings.Replace(routingOn, "groups.review]\nworkers = 0\nslots = 1\n", "groups.review]\nworkers = 0\nslots = 1\nagent = { command = [\"claude\"] }\n", 1),
			req:    workforce.SpawnRequest{Group: "review", Count: 1}, want: settings.ErrProviderRefused, says: "NERD-5529",
		},
		"the user's override, disabled": {
			config: routingOn + "\n[routing.overrides]\npre_check = \"claude_mid\"\n\n[routing.bindings.claude_mid]\nenabled = false\n",
			req:    workforce.SpawnRequest{Group: "qa", Count: 1}, want: routing.ErrOverrideRefused, says: "claude_mid, which the user's [routing.overrides] chose for pre_check, is disabled",
		},
		"a binding the role refuses":  {req: workforce.SpawnRequest{Group: "coder", Count: 1, Model: "claude_small"}, want: routing.ErrOverrideRefused, says: "claude_mid, claude_strong, codex_strong"},
		"an alias, not a binding":     {req: workforce.SpawnRequest{Group: "coder", Count: 1, Model: "opus"}, want: routing.ErrOverrideRefused},
		"a premium binding":           {req: workforce.SpawnRequest{Group: "debugger", Count: 1, Model: "claude_premium"}, config: strings.Replace(routingOn, opsGroup, "\n[projects.herdr-agentisan.groups.debugger]\nworkers = 0\nslots = 1\n", 1) + debuggerEnvFile, want: routing.ErrOverrideRefused, says: "premium"},
		"the other provider's model":  {req: workforce.SpawnRequest{Group: "coder", Count: 1, Model: "codex_strong"}, want: routing.ErrProviderMismatch, says: "and the worker type coder runs claude"},
		"an effort the binding lacks": {req: workforce.SpawnRequest{Group: "coder", Count: 1, Effort: "low"}, want: routing.ErrOverrideRefused, says: "--effort"},
		"a packet --provider disagrees with": {
			req:  workforce.SpawnRequest{Group: "coder", Count: 1, Provider: "claude", Task: &routing.Task{LookupStatus: routing.LookupFound, ProviderPreference: "codex"}},
			want: routing.ErrProviderMismatch, says: "--provider is claude, and the task packet's provider_preference is codex",
		},
		"a packet preferring no provider": {
			req:  workforce.SpawnRequest{Group: "coder", Count: 1, Task: &routing.Task{LookupStatus: routing.LookupFound, ProviderPreference: "gemini"}},
			want: workforce.ErrBadRequest, says: "provider_preference is neither claude nor codex",
		},
		"the other provider's binding, its hint": {
			req: workforce.SpawnRequest{Group: "coder", Count: 1, Model: "codex_strong"}, want: routing.ErrProviderMismatch,
			says: "--model never chooses the provider: pass --provider with it",
		},
		"a provider no agent is":    {req: workforce.SpawnRequest{Group: "coder", Count: 1, Provider: "gemini"}, want: workforce.ErrBadRequest, says: "--provider is neither claude nor codex"},
		"a provider's case variant": {req: workforce.SpawnRequest{Group: "coder", Count: 1, Provider: "CODEX"}, want: workforce.ErrBadRequest, says: "--provider is neither claude nor codex"},
		"a provider the type cannot run yet": {
			req: workforce.SpawnRequest{Group: "review", Count: 1, Provider: "claude"}, want: settings.ErrProviderRefused, says: "NERD-5529",
		},
		"a disabled default": {
			config: routingOn + "\n[routing.bindings.claude_mid]\nenabled = false\n",
			req:    workforce.SpawnRequest{Group: "qa", Count: 1}, want: routing.ErrBlocked, says: "disabled",
		},
		"a packet's unknown lookup":     {req: workforce.SpawnRequest{Group: "coder", Count: 1, Task: &routing.Task{LookupStatus: maybe}}, want: workforce.ErrBadRequest, says: "task_lookup_status"},
		"a packet for another role":     {req: workforce.SpawnRequest{Group: "coder", Count: 1, Task: &routing.Task{LookupStatus: routing.LookupFound, RequestedRole: "clerk"}}, want: workforce.ErrBadRequest, says: "requested_role"},
		"a packet past its bounds":      {req: workforce.SpawnRequest{Group: "coder", Count: 1, Task: &routing.Task{LookupStatus: routing.LookupFound, Title: long}}, want: workforce.ErrBadRequest, says: "title"},
		"an effort a shell could split": {req: workforce.SpawnRequest{Group: "coder", Count: 1, Effort: "high;rm"}, want: workforce.ErrBadRequest, says: "--effort"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := tc.config
			if cfg == "" {
				cfg = routingOn
			}
			f, rec := routedFixture(t, cfg)
			if tc.req.Group == settings.DebuggerType {
				f.svc.Debugger = pluginLaunch(t) // its launch is built before routing (A32)
			}

			_, err := f.svc.Spawn(t.Context(), tc.req)
			require.ErrorIs(t, err, tc.want)
			if tc.says != "" {
				assert.Contains(t, err.Error(), tc.says)
			}
			assert.Empty(t, f.h.params("tab.create"), "nothing opens")
			assert.Empty(t, rec.all(), "nothing is recorded")
		})
	}
}

// TestSpawnUnderAnOverrideOfTheOtherProvider: the user's override binds only
// workers of its binding's provider, so a codex worker under the live
// coder = "claude_strong" takes its own default, codex_strong, and a list sets
// the codex binding too; each row records what applied (A63).
func TestSpawnUnderAnOverrideOfTheOtherProvider(t *testing.T) {
	t.Parallel()
	codexCoder := strings.Replace(routingOn, "groups.coder]\n", "groups.coder]\nagent = { command = [\"codex\"] }\n", 1)
	// A coder's Codex starts with full access (A59).
	codexArgs := []any{"--no-daemon", "--sandbox", "danger-full-access", "--model", "gpt-6.1-sol", "-c", `model_reasoning_effort="high"`}
	tests := map[string]struct {
		overrides string
		reason    routing.Reason
		row       *store.Routing
	}{
		"the live string": {overrides: "coder = \"claude_strong\"\n", reason: routing.ReasonTaskNotFound, row: &store.Routing{Role: "coder", Provider: "codex", Binding: "codex_strong", ModelID: "gpt-6.1-sol", Effort: "high"}},
		"a list": {overrides: "coder = [\"claude_strong\", \"codex_strong\"]\n", row: &store.Routing{
			Role: "coder", Provider: "codex", Binding: "codex_strong", ModelID: "gpt-6.1-sol", Effort: "high", OverrideBinding: "codex_strong", OverrideBy: "user",
		}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f, rec := routedFixture(t, codexCoder+"\n[routing.overrides]\n"+tc.overrides)

			res, err := f.svc.Spawn(t.Context(), workforce.SpawnRequest{Group: "coder", Count: 1})
			require.NoError(t, err, "a codex coder is no longer refused as provider_mismatch")
			starts := f.h.params("agent.start")
			require.Len(t, starts, 1)
			assert.Equal(t, "codex", starts[0]["kind"])
			assert.Equal(t, codexArgs, starts[0]["args"])
			d := rec.all()[0].Decision
			require.NotNil(t, d)
			tc.row.PolicyVersion = d.PolicyVersion
			assert.Equal(t, tc.row, routingOf(t, f, res.Workers[0].ID))
			assert.Equal(t, tc.reason, d.FallbackReason, "the default's reason, or none for an override")
			assert.Equal(t, routing.Codex, d.Provider)
		})
	}
}

// TestSpawnWhileRoutingIsOff: off, a spawn launches as part 4 does, with
// --model and --effort as given, and records what routing would have
// chosen, without them; a spawn routing would refuse goes ahead, recorded as
// refused. A malformed packet is refused either way. No row records a
// decision it did not launch.
func TestSpawnWhileRoutingIsOff(t *testing.T) {
	t.Parallel()
	t.Run("a decision", func(t *testing.T) {
		t.Parallel()
		f, rec := routedFixture(t, teamConfig)

		res, err := f.svc.Spawn(t.Context(), workforce.SpawnRequest{Group: "coder", Count: 1, Model: "sonnet", Effort: "xhigh"})
		require.NoError(t, err)
		assert.Equal(t, withHooksAny("--model", "sonnet", "--effort", "xhigh", "--name", "ha-coder-1"), f.h.params("agent.start")[0]["args"])
		assert.Equal(t, workforce.Spawned{ID: "ha-coder-1", Group: "coder", PaneID: res.Workers[0].PaneID, Agent: "claude"}, res.Workers[0])
		assert.Nil(t, routingOf(t, f, "ha-coder-1"))
		routed := rec.all()
		require.Len(t, routed, 1)
		d := routed[0].Decision
		require.NotNil(t, d)
		assert.False(t, d.Applied)
		assert.Equal(t, "claude_mid", d.SelectedBinding, "the role's default: --model is not a binding while off")
		assert.Equal(t, "high", d.Effort)
		assert.Nil(t, d.Override)
	})
	for name, tc := range map[string]struct {
		config, group string
		want          error
	}{
		"a group no role has": {config: teamConfig + opsGroup, group: "ops", want: routing.ErrUnknownRole},
		"a disabled default":  {config: teamConfig + "\n[routing.bindings.claude_mid]\nenabled = false\n", group: "qa", want: routing.ErrBlocked},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f, rec := routedFixture(t, tc.config)

			res, err := f.svc.Spawn(t.Context(), workforce.SpawnRequest{Group: tc.group, Count: 1})
			require.NoError(t, err)
			require.Len(t, res.Workers, 1)
			routed := rec.all()
			require.Len(t, routed, 1)
			assert.Nil(t, routed[0].Decision)
			require.ErrorIs(t, routed[0].Refused, tc.want)
			assert.Equal(t, []string{res.Workers[0].ID}, routed[0].IDs)
		})
	}
	t.Run("a malformed packet", func(t *testing.T) {
		t.Parallel()
		f, rec := routedFixture(t, teamConfig)

		_, err := f.svc.Spawn(t.Context(), workforce.SpawnRequest{Group: "coder", Count: 1, Task: &routing.Task{LookupStatus: "maybe"}})
		require.ErrorIs(t, err, workforce.ErrBadRequest)
		assert.Empty(t, f.h.params("tab.create"))
		assert.Empty(t, rec.all())
	})
}

// TestPlanRefusesAChangedAgent: a launch routing chose for one agent is
// refused when the type's command now runs the other, as a configuration
// edited between the decision and the plan.
func TestPlanRefusesAChangedAgent(t *testing.T) {
	t.Parallel()
	p := &settings.Profile{Agent: settings.Agent{Command: []string{"claude"}}}
	_, _, err := workforce.PlanRouted(p, "coder", "gpt-6.1-sol", "high", "codex")
	require.ErrorIs(t, err, workforce.ErrConfig)
	assert.Contains(t, err.Error(), "changed from codex to claude")

	kind, args, err := workforce.PlanRouted(p, "coder", "claude-sonnet-5-5", "high", "claude")
	require.NoError(t, err)
	assert.Equal(t, "claude", kind)
	assert.Equal(t, withHooks("--model", "claude-sonnet-5-5", "--effort", "high", "--name", "ha-coder-1"), args)
}

// TestSpawnRefusesAnAgentChangedAfterRouting: the type's agent changes after
// routing decided, here during the preflight, before the lock re-reads the
// configuration: the spawn is refused, never codex on a claude decision.
func TestSpawnRefusesAnAgentChangedAfterRouting(t *testing.T) {
	t.Parallel()
	f, rec := routedFixture(t, routingOn)
	changed := strings.Replace(routingOn, "[projects.herdr-agentisan.groups.coder]\n", "[projects.herdr-agentisan.groups.coder]\nagent = { command = [\"codex\"] }\n", 1)
	require.NotEqual(t, routingOn, changed)
	f.svc.Preflight = func(context.Context, string) error {
		return os.WriteFile(filepath.Join(f.config, settings.FileName), []byte(strings.ReplaceAll(changed, "REPO", f.repo)), 0o600)
	}
	_, err := f.svc.Spawn(t.Context(), workforce.SpawnRequest{Group: "coder", Count: 1})
	require.ErrorIs(t, err, workforce.ErrConfig)
	assert.Contains(t, err.Error(), "changed from claude to codex")
	assert.Empty(t, f.h.params("tab.create"))
	assert.Empty(t, rec.all())
}

// upRouting is a group's launch as routing chooses it in these tests.
type upRouting struct {
	kind, role, binding, model string
	reason                     routing.Reason
}

// upArgs is the launch's args as agent.start gets them, for worker id.
func (u upRouting) upArgs(id string) []any {
	if u.kind == "codex" {
		return []any{"--no-daemon", "--sandbox", "read-only", "--model", u.model, "-c", `model_reasoning_effort="high"`}
	}
	return withHooksAny("--model", u.model, "--effort", "high", "--name", id)
}

// TestUpLaunchesWhatRoutingChose: with routing on, team up decides each
// group it fills as a spawn of that group would: each worker launches with
// its group's binding, its row records it and the result names it, and each
// group's decision is told once, with that group's workers alone (A50).
func TestUpLaunchesWhatRoutingChose(t *testing.T) {
	t.Parallel()
	f, rec := routedFixture(t, strings.Replace(routingOn, "groups.review]\nworkers = 0", "groups.review]\nworkers = 1", 1))
	want := map[string]upRouting{
		"coder":  {"claude", "coder", "claude_mid", "claude-sonnet-5-5", routing.ReasonTaskNotFound},
		"qa":     {"claude", "pre_check", "claude_mid", "claude-sonnet-5-5", ""},
		"review": {"codex", "code_review", "codex_strong", "gpt-6.1-sol", ""},
	}

	res, err := f.svc.Up(t.Context(), workforce.UpRequest{})
	require.NoError(t, err)
	groups := map[string][]string{}
	for _, w := range res.Workers {
		u := want[w.Group]
		groups[w.Group] = append(groups[w.Group], w.ID)
		assert.Equal(t, workforce.Spawned{ID: w.ID, Group: w.Group, PaneID: w.PaneID, Agent: u.kind, Binding: u.binding, Model: u.model, Effort: "high", FallbackReason: u.reason}, w)
	}
	assert.Equal(t, map[string][]string{"coder": {"ha-coder-1", "ha-coder-2"}, "qa": {"ha-qa-1"}, "review": {"ha-review-1"}}, groups)
	starts := f.h.params("agent.start")
	require.Len(t, starts, 4)
	for _, s := range starts {
		id := s["name"].(string)
		u := want[f.row(t, id).Group]
		assert.Equal(t, u.kind, s["kind"])
		assert.Equal(t, u.upArgs(id), s["args"])
	}
	routed := rec.all()
	require.Len(t, routed, 3, "one decision per group filled")
	for i, g := range []string{"coder", "qa", "review"} {
		r, u := routed[i], want[g]
		assert.Equal(t, "team up", r.Op)
		assert.Equal(t, "herdr-agentisan", r.Project)
		assert.Equal(t, g, r.Group)
		assert.Equal(t, groups[g], r.IDs, "the group's own workers")
		assert.Nil(t, r.Refused)
		require.NotNil(t, r.Decision)
		assert.True(t, r.Decision.Applied)
		for _, id := range groups[g] {
			assert.Equal(t, &store.Routing{Role: u.role, Provider: u.kind, Binding: u.binding, ModelID: u.model, Effort: "high", PolicyVersion: r.Decision.PolicyVersion}, routingOf(t, f, id))
		}
	}
}

// TestUpWhileRoutingIsOff: with routing off, team up launches as before and
// no row records a decision; each group's decision, what routing would have
// chosen, is told with its workers, and a group routing would refuse still
// fills, its refusal told (A50).
func TestUpWhileRoutingIsOff(t *testing.T) {
	t.Parallel()
	f, rec := routedFixture(t, strings.Replace(teamConfig+opsGroup, "groups.ops]\nworkers = 0", "groups.ops]\nworkers = 1", 1))

	res, err := f.svc.Up(t.Context(), workforce.UpRequest{})
	require.NoError(t, err)
	require.Len(t, res.Workers, 4)
	for _, w := range res.Workers {
		assert.Equal(t, workforce.Spawned{ID: w.ID, Group: w.Group, PaneID: w.PaneID, Agent: "claude"}, w)
		assert.Nil(t, routingOf(t, f, w.ID))
	}
	for _, s := range f.h.params("agent.start") {
		assert.Equal(t, withHooksAny("--name", s["name"]), s["args"])
	}
	told := map[string]workforce.Routed{}
	for _, r := range rec.all() {
		assert.Equal(t, "team up", r.Op)
		told[r.Group] = r
	}
	require.Len(t, told, 3)
	for _, g := range []string{"coder", "qa"} {
		require.NotNil(t, told[g].Decision, g)
		assert.False(t, told[g].Decision.Applied)
		assert.Equal(t, "claude_mid", told[g].Decision.SelectedBinding)
	}
	assert.Equal(t, []string{"ha-coder-1", "ha-coder-2"}, told["coder"].IDs)
	assert.Nil(t, told["ops"].Decision)
	require.ErrorIs(t, told["ops"].Refused, routing.ErrUnknownRole)
	assert.Equal(t, []string{"ha-ops-1"}, told["ops"].IDs)
}

// TestUpRefusedWholeByRouting: with routing on, a group routing refuses
// refuses the whole up before anything opens, the groups it decided before
// included: no tab, no dashboard and nothing told (A50).
func TestUpRefusedWholeByRouting(t *testing.T) {
	t.Parallel()
	review := strings.Replace(routingOn, "groups.review]\nworkers = 0", "groups.review]\nworkers = 1", 1)
	for name, tc := range map[string]struct {
		config string
		want   error
	}{
		"a group no role has":               {config: strings.Replace(routingOn, "groups.ops]\nworkers = 0", "groups.ops]\nworkers = 1", 1), want: routing.ErrUnknownRole},
		"the last group's default disabled": {config: review + "\n[routing.bindings.codex_strong]\nenabled = false\n", want: routing.ErrBlocked},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f, rec := routedFixture(t, tc.config)
			f.svc.Dashboard = dashboard

			_, err := f.svc.Up(t.Context(), workforce.UpRequest{})
			require.ErrorIs(t, err, tc.want)
			assert.Empty(t, f.h.params("tab.create"))
			assert.Empty(t, f.h.params("plugin.pane.open"))
			assert.Empty(t, rec.all())
		})
	}
}

// TestUpRoutesOnTheConfigurationItLocked: team up asks routing on the
// configuration the lock read, not the preflight's: routing turned on
// during the preflight decides the launch.
func TestUpRoutesOnTheConfigurationItLocked(t *testing.T) {
	t.Parallel()
	f, rec := routedFixture(t, teamConfig)
	f.svc.Preflight = func(context.Context, string) error {
		return os.WriteFile(filepath.Join(f.config, settings.FileName), []byte(strings.ReplaceAll(routingOn, "REPO", f.repo)), 0o600)
	}

	res, err := f.svc.Up(t.Context(), workforce.UpRequest{})
	require.NoError(t, err)
	require.Len(t, res.Workers, 3)
	for _, s := range f.h.params("agent.start") {
		assert.Equal(t, withHooksAny("--model", "claude-sonnet-5-5", "--effort", "high", "--name", s["name"]), s["args"])
	}
	for _, r := range rec.all() {
		require.NotNil(t, r.Decision)
		assert.True(t, r.Decision.Applied, r.Group)
	}
}
