package workforce_test

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/jev"
	"github.com/abanna/herdr-agentisan/internal/routing"
	"github.com/abanna/herdr-agentisan/internal/settings"
	"github.com/abanna/herdr-agentisan/internal/store"
	"github.com/abanna/herdr-agentisan/internal/workforce"
)

// jevEnvFile is where these tests' configuration keeps Jev's key.
const jevEnvFile = "/run/agentisan/typesafe.env"

// jevOn is routingOn with Jev enabled: a Claude coder's spawn with an
// assignment asks it.
const jevOn = routingOn + `
[routing.jev]
enabled = true
env_file = "` + jevEnvFile + `"
`

// jevAsk is one ask the fake Jev was given, and what spawn had opened when
// it was asked.
type jevAsk struct {
	envFile        string
	body           []byte
	timeout        time.Duration
	tabs, launched int
}

// fakeJev answers every ask with ans and err, and keeps each ask.
type fakeJev struct {
	f   *fixture
	ans jev.Answer
	err error

	mu   sync.Mutex
	asks []jevAsk
}

func (j *fakeJev) Ask(_ context.Context, envFile string, body []byte, timeout time.Duration) (jev.Answer, error) {
	a := jevAsk{envFile: envFile, body: body, timeout: timeout, tabs: len(j.f.h.params("tab.create")), launched: len(j.f.h.params("agent.start"))}
	j.mu.Lock()
	defer j.mu.Unlock()
	j.asks = append(j.asks, a)
	return j.ans, j.err
}

func (j *fakeJev) all() []jevAsk {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]jevAsk(nil), j.asks...)
}

// jevFixture is a routed fixture on config whose Jev is a fake answering
// ans and err.
func jevFixture(t *testing.T, config string, ans jev.Answer, err error) (*fixture, *recorder, *fakeJev) {
	t.Helper()
	f, rec := routedFixture(t, config)
	j := &fakeJev{f: f, ans: ans, err: err}
	f.svc.Jev = j
	return f, rec, j
}

// jevAnswer is Jev's answer as TypeSafe sends it: a choice with its
// confidence, and a noul.
func jevAnswer(choice string, confidence, noul float64) jev.Answer {
	return jev.Answer{Model: "jev-1.13.0", Answers: map[string]json.RawMessage{
		"complexity":             json.RawMessage(fmt.Sprintf(`{"type":"choice","choice":%q,"confidence":%v}`, choice, confidence)),
		"information_sufficient": json.RawMessage(fmt.Sprintf(`{"type":"noul","noul":%v}`, noul)),
	}}
}

// jevTask is a task packet Jev is asked about: found, with an assignment.
func jevTask() *routing.Task {
	id := "NERD-5289"
	return &routing.Task{
		TaskID: &id, LookupStatus: routing.LookupFound, RequestedRole: "coder",
		WorkerAssignment: "wire worker spawn to Jev", Requirements: "ask once, within the rules' timeout",
		AcceptanceCriteria: []string{"one ask", "no retry"}, PreviousAttempts: "none",
	}
}

// policyOf is the policy f's configuration resolves to.
func policyOf(t *testing.T, f *fixture) routing.Policy {
	t.Helper()
	res, err := settings.Load(settings.LoadOptions{ConfigDir: f.config, Project: "herdr-agentisan", LookupEnv: func(string) (string, bool) { return "", false }})
	require.NoError(t, err)
	p, err := res.Policy()
	require.NoError(t, err)
	return p
}

// TestSpawnAsksJevOnce: a Claude coder spawned with an assignment, while
// routing and Jev are on, asks Jev once for the whole spawn, after the
// preflight and before anything opens: JevRequest's body, the rules'
// timeout, the env file the configuration names. A demanding answer moves
// every worker of the spawn to the strong binding at high, on its launch,
// its row and the result, and the decision told records Jev's model and
// answers (A57).
func TestSpawnAsksJevOnce(t *testing.T) {
	t.Parallel()
	f, rec, j := jevFixture(t, jevOn, jevAnswer(routing.ComplexityDemanding, 0.9, 0.95), nil)
	task := jevTask()

	res, err := f.svc.Spawn(t.Context(), workforce.SpawnRequest{Group: "coder", Count: 2, Task: task})
	require.NoError(t, err)
	asks := j.all()
	require.Len(t, asks, 1, "one ask for the spawn")
	policy := policyOf(t, f)
	body, err := policy.JevRequest(task)
	require.NoError(t, err)
	assert.JSONEq(t, string(body), string(asks[0].body))
	assert.Equal(t, 2*time.Second, asks[0].timeout, "the rules' jev_timeout_ms")
	assert.Equal(t, policy.JevTimeout(), asks[0].timeout)
	assert.Equal(t, jevEnvFile, asks[0].envFile)
	assert.Zero(t, asks[0].tabs, "no tab opened before Jev answered")
	assert.Zero(t, asks[0].launched, "no agent started before Jev answered")

	starts := f.h.params("agent.start")
	require.Len(t, starts, 2)
	for _, s := range starts {
		assert.Equal(t, withHooksAny("--model", "claude-opus-5-5", "--effort", "high", "--name", s["name"]), s["args"])
	}
	routed := rec.all()
	require.Len(t, routed, 1)
	d := routed[0].Decision
	require.NotNil(t, d)
	assert.Equal(t, "claude_strong", d.SelectedBinding)
	assert.Equal(t, routing.ReasonNone, d.FallbackReason)
	assert.Equal(t, "jev-1.13.0", d.JevModelReturned)
	assert.JSONEq(t, `{"complexity":{"type":"choice","choice":"demanding","confidence":0.9},"information_sufficient":{"type":"noul","noul":0.95}}`, string(d.Answers))
	assert.Empty(t, d.JevError)
	require.Len(t, res.Workers, 2)
	for _, w := range res.Workers {
		assert.Equal(t, workforce.Spawned{ID: w.ID, Group: "coder", PaneID: w.PaneID, Agent: "claude", Binding: "claude_strong", Model: "claude-opus-5-5", Effort: "high"}, w)
		assert.Equal(t, &store.Routing{Role: "coder", Provider: "claude", Binding: "claude_strong", ModelID: "claude-opus-5-5", Effort: "high", AssignmentDigest: d.AssignmentDigest, PolicyVersion: d.PolicyVersion}, routingOf(t, f, w.ID))
	}
}

// TestSpawnLaunchesWhatJevsAnswerMaps: each answer, and each failure, lands
// on the launch, the row, the result and the decision told as routing maps
// it (A57): only a demanding answer it can follow moves the coder, at the
// boss's --effort when there is one; anything else keeps the default, with
// its reason, and a failure or a missing key with its error, said once.
func TestSpawnLaunchesWhatJevsAnswerMaps(t *testing.T) {
	t.Parallel()
	const key = jev.KeyVar
	noKey := fmt.Errorf("%w: %s did not answer within 1s; if 1Password mounts it, is 1Password unlocked?", jev.ErrNoKey, jevEnvFile)
	tests := map[string]struct {
		config   string
		ans      jev.Answer
		err      error
		effort   string
		binding  string
		model    string
		effortTo string
		reason   routing.Reason
		jevError string
	}{
		"bounded":                              {ans: jevAnswer(routing.ComplexityBounded, 0.9, 0.95), binding: "claude_mid", model: "claude-sonnet-5-5", effortTo: "high"},
		"demanding":                            {ans: jevAnswer(routing.ComplexityDemanding, 0.9, 0.95), binding: "claude_strong", model: "claude-opus-5-5", effortTo: "high"},
		"demanding, at the boss's effort":      {ans: jevAnswer(routing.ComplexityDemanding, 0.9, 0.95), config: jevOn + "\n[routing.bindings.claude_mid]\nefforts = [\"high\", \"xhigh\"]\n", effort: "xhigh", binding: "claude_strong", model: "claude-opus-5-5", effortTo: "xhigh"},
		"demanding, its threshold met exactly": {ans: jevAnswer(routing.ComplexityDemanding, 0.7, 0.85), config: jevOn + "confidence_pct = 70\nsufficiency_pct = 85\n", binding: "claude_strong", model: "claude-opus-5-5", effortTo: "high"},
		"demanding, the strong binding off":    {ans: jevAnswer(routing.ComplexityDemanding, 0.9, 0.95), config: jevOn + "\n[routing.bindings.claude_strong]\nenabled = false\n", binding: "claude_mid", model: "claude-sonnet-5-5", effortTo: "high", reason: routing.ReasonEscalationIneligible},
		"unknown":                              {ans: jevAnswer(routing.ComplexityUnknown, 0.9, 0.95), binding: "claude_mid", model: "claude-sonnet-5-5", effortTo: "high", reason: routing.ReasonJevUnknown},
		"low confidence":                       {ans: jevAnswer(routing.ComplexityDemanding, 0.74, 0.95), binding: "claude_mid", model: "claude-sonnet-5-5", effortTo: "high", reason: routing.ReasonJevLowConfidence},
		"insufficient":                         {ans: jevAnswer(routing.ComplexityDemanding, 0.9, 0.79), binding: "claude_mid", model: "claude-sonnet-5-5", effortTo: "high", reason: routing.ReasonJevInsufficient},
		"another model":                        {ans: jev.Answer{Model: "jev-2", Answers: jevAnswer(routing.ComplexityDemanding, 0.9, 0.95).Answers}, binding: "claude_mid", model: "claude-sonnet-5-5", effortTo: "high", reason: routing.ReasonJevFailure, jevError: "Jev answered as another model than jev-1.13.0"},
		"a 429":                                {err: fmt.Errorf("%w: 429 Too Many Requests", jev.ErrStatus), binding: "claude_mid", model: "claude-sonnet-5-5", effortTo: "high", reason: routing.ReasonJevFailure, jevError: "TypeSafe refused the request: 429 Too Many Requests"},
		"a refused key":                        {err: fmt.Errorf("%w: %w: 401 Unauthorized", jev.ErrStatus, jev.ErrUnauthorized), binding: "claude_mid", model: "claude-sonnet-5-5", effortTo: "high", reason: routing.ReasonJevFailure, jevError: "TypeSafe refused the request: the key was refused: 401 Unauthorized"},
		"the timeout":                          {err: fmt.Errorf("%w: %w", jev.ErrCall, context.DeadlineExceeded), binding: "claude_mid", model: "claude-sonnet-5-5", effortTo: "high", reason: routing.ReasonJevFailure, jevError: "the TypeSafe call failed: context deadline exceeded"},
		"no key":                               {err: noKey, binding: "claude_mid", model: "claude-sonnet-5-5", effortTo: "high", reason: routing.ReasonJevKeyUnavailable, jevError: noKey.Error()},
		"no key in the file":                   {err: fmt.Errorf("%w: %s's %s", jev.ErrNoKey, jevEnvFile, key), binding: "claude_mid", model: "claude-sonnet-5-5", effortTo: "high", reason: routing.ReasonJevKeyUnavailable, jevError: "no TypeSafe key: " + jevEnvFile + "'s " + key},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f, rec, j := jevFixture(t, cmp.Or(tc.config, jevOn), tc.ans, tc.err)

			res, err := f.svc.Spawn(t.Context(), workforce.SpawnRequest{Group: "coder", Count: 1, Effort: tc.effort, Task: jevTask()})
			require.NoError(t, err)
			require.Len(t, j.all(), 1)
			starts := f.h.params("agent.start")
			require.Len(t, starts, 1)
			assert.Equal(t, withHooksAny("--model", tc.model, "--effort", tc.effortTo, "--name", "ha-coder-1"), starts[0]["args"])
			d := rec.all()[0].Decision
			require.NotNil(t, d)
			assert.Equal(t, tc.reason, d.FallbackReason)
			assert.Equal(t, tc.jevError, d.JevError)
			require.Len(t, res.Workers, 1)
			w := res.Workers[0]
			assert.Equal(t, workforce.Spawned{ID: "ha-coder-1", Group: "coder", PaneID: w.PaneID, Agent: "claude", Binding: tc.binding, Model: tc.model, Effort: tc.effortTo, FallbackReason: tc.reason}, w)
			r := routingOf(t, f, "ha-coder-1")
			require.NotNil(t, r)
			assert.Equal(t, tc.binding, r.Binding)
			assert.Equal(t, tc.model, r.ModelID)
			assert.Equal(t, tc.effortTo, r.Effort)
		})
	}
}

// TestSpawnReadsJevsKeyUnderHOME: an env file under ~/ is read under the
// daemon's HOME; with no HOME there is no key to ask with, so Jev is not
// asked and the coder keeps its default as jev_key_unavailable (A57).
func TestSpawnReadsJevsKeyUnderHOME(t *testing.T) {
	t.Parallel()
	config := strings.Replace(jevOn, jevEnvFile, "~/.config/typesafe.env", 1)
	t.Run("with HOME", func(t *testing.T) {
		t.Parallel()
		f, _, j := jevFixture(t, config, jevAnswer(routing.ComplexityBounded, 0.9, 0.95), nil)
		f.svc.LookupEnv = func(k string) (string, bool) {
			if k == "HOME" {
				return "/home/u", true
			}
			return "", false
		}

		_, err := f.svc.Spawn(t.Context(), workforce.SpawnRequest{Group: "coder", Count: 1, Task: jevTask()})
		require.NoError(t, err)
		asks := j.all()
		require.Len(t, asks, 1)
		assert.Equal(t, "/home/u/.config/typesafe.env", asks[0].envFile)
	})
	t.Run("without", func(t *testing.T) {
		t.Parallel()
		f, rec, j := jevFixture(t, config, jevAnswer(routing.ComplexityDemanding, 0.9, 0.95), nil)

		res, err := f.svc.Spawn(t.Context(), workforce.SpawnRequest{Group: "coder", Count: 1, Task: jevTask()})
		require.NoError(t, err)
		assert.Empty(t, j.all(), "no key, no ask")
		d := rec.all()[0].Decision
		require.NotNil(t, d)
		assert.Equal(t, routing.ReasonJevKeyUnavailable, d.FallbackReason)
		assert.Equal(t, "invalid config: ~/.config/typesafe.env starts with ~ but HOME is not set", d.JevError)
		assert.Equal(t, "claude_mid", res.Workers[0].Binding)
	})
}

// TestSpawnAsksJevNothing: from a spawn that would ask, each one change that
// takes the ask away asks nothing, and the decision says which: routing or
// Jev off keeps jev_disabled, a codex coder jev_no_change, an override names
// its binding, a packet without what Jev needs gives its own reason; a
// service with no Jev asks no one; a failed preflight asks before nothing
// opens, so it asks nothing either; and a packet that is not UTF-8 is never
// sent, its failure recorded (A57).
func TestSpawnAsksJevNothing(t *testing.T) {
	t.Parallel()
	routingOff := strings.Replace(jevOn, "[routing]\nenabled = true\n", "[routing]\nenabled = false\n", 1)
	jevOff := strings.Replace(jevOn, "[routing.jev]\nenabled = true\n", "[routing.jev]\nenabled = false\n", 1)
	codex := strings.Replace(jevOn, "groups.coder]\n", "groups.coder]\nagent = { command = [\"codex\"] }\n", 1)
	userOverride := jevOn + "\n[routing.overrides]\ncoder = \"claude_strong\"\n"
	require.NotEqual(t, jevOn, routingOff)
	require.NotEqual(t, jevOn, jevOff)
	require.NotEqual(t, jevOn, codex)
	unavailable := jevTask()
	unavailable.LookupStatus = routing.LookupUnavailable
	empty := jevTask()
	empty.WorkerAssignment = ""
	tests := map[string]struct {
		config  string
		req     workforce.SpawnRequest
		noJev   bool
		reason  routing.Reason
		binding string
		applied bool
	}{
		"routing off":           {config: routingOff, req: workforce.SpawnRequest{Task: jevTask()}, reason: routing.ReasonJevDisabled, binding: "claude_mid"},
		"Jev off":               {config: jevOff, req: workforce.SpawnRequest{Task: jevTask()}, reason: routing.ReasonJevDisabled, binding: "claude_mid", applied: true},
		"a codex coder":         {config: codex, req: workforce.SpawnRequest{Task: jevTask()}, reason: routing.ReasonJevNoChange, binding: "codex_strong", applied: true},
		"the user's override":   {config: userOverride, req: workforce.SpawnRequest{Task: jevTask()}, binding: "claude_strong", applied: true},
		"the boss's --model":    {req: workforce.SpawnRequest{Model: "claude_mid", Task: jevTask()}, binding: "claude_mid", applied: true},
		"no packet":             {req: workforce.SpawnRequest{}, reason: routing.ReasonTaskNotFound, binding: "claude_mid", applied: true},
		"a lookup unavailable":  {req: workforce.SpawnRequest{Task: unavailable}, reason: routing.ReasonLookupUnavailable, binding: "claude_mid", applied: true},
		"an empty assignment":   {req: workforce.SpawnRequest{Task: empty}, reason: routing.ReasonInsufficientAssignment, binding: "claude_mid", applied: true},
		"a service with no Jev": {noJev: true, req: workforce.SpawnRequest{Task: jevTask()}, reason: routing.ReasonJevDisabled, binding: "claude_mid", applied: true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f, rec, j := jevFixture(t, cmp.Or(tc.config, jevOn), jevAnswer(routing.ComplexityDemanding, 0.9, 0.95), nil)
			if tc.noJev {
				f.svc.Jev = nil
			}
			req := tc.req
			req.Group, req.Count = "coder", 1

			_, err := f.svc.Spawn(t.Context(), req)
			require.NoError(t, err)
			assert.Empty(t, j.all(), "Jev is not asked")
			d := rec.all()[0].Decision
			require.NotNil(t, d)
			assert.Equal(t, tc.reason, d.FallbackReason)
			assert.Equal(t, tc.binding, d.SelectedBinding)
			assert.Equal(t, tc.applied, d.Applied)
			assert.Empty(t, d.JevError)
			assert.Empty(t, d.JevModelReturned)
		})
	}
	t.Run("a failed preflight", func(t *testing.T) {
		t.Parallel()
		f, _, j := jevFixture(t, jevOn, jevAnswer(routing.ComplexityDemanding, 0.9, 0.95), nil)
		f.svc.Preflight = func(context.Context, string) error { return assert.AnError }

		_, err := f.svc.Spawn(t.Context(), workforce.SpawnRequest{Group: "coder", Count: 1, Task: jevTask()})
		require.ErrorIs(t, err, workforce.ErrPreflight)
		assert.Empty(t, j.all(), "no call billed for a spawn that opens nothing")
	})
	t.Run("a packet that is not UTF-8", func(t *testing.T) {
		t.Parallel()
		f, rec, j := jevFixture(t, jevOn, jevAnswer(routing.ComplexityDemanding, 0.9, 0.95), nil)
		task := jevTask()
		task.Requirements = "ask \xff once"

		res, err := f.svc.Spawn(t.Context(), workforce.SpawnRequest{Group: "coder", Count: 1, Task: task})
		require.NoError(t, err)
		assert.Empty(t, j.all(), "no byte of it replaced and sent")
		d := rec.all()[0].Decision
		require.NotNil(t, d)
		assert.Equal(t, routing.ReasonJevFailure, d.FallbackReason)
		assert.Equal(t, "invalid routing: the task packet is not UTF-8", d.JevError)
		assert.Equal(t, "claude_mid", res.Workers[0].Binding)
	})
}

// TestUpAsksJevNothing: team up hands routing no packet, so no group it
// fills asks Jev, whatever [routing.jev] says (A57).
func TestUpAsksJevNothing(t *testing.T) {
	t.Parallel()
	f, rec, j := jevFixture(t, jevOn, jevAnswer(routing.ComplexityDemanding, 0.9, 0.95), nil)

	res, err := f.svc.Up(t.Context(), workforce.UpRequest{})
	require.NoError(t, err)
	assert.Empty(t, j.all())
	require.NotEmpty(t, res.Workers)
	for _, rt := range rec.all() {
		if rt.Group == "coder" {
			require.NotNil(t, rt.Decision)
			assert.Equal(t, routing.ReasonTaskNotFound, rt.Decision.FallbackReason)
		}
	}
}
