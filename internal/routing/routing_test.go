package routing_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/routing"
)

// rulesSHA256 is the sha256 of the user's rules v1 as given (2026-10-10). A
// change to the rules changes it, deliberately: the rules are the spec.
const rulesSHA256 = "2f94988c3aaa7a5e601a0b9e152cb9037a1346743efe1913f9bd4e2b1c95c6fd"

// rulesFile is the embedded rules, as committed.
const rulesFile = "rules/jev-routing-v1.json"

var now = time.Date(2026, 10, 21, 12, 0, 0, 0, time.FixedZone("CDT", -5*3600))

// policy applies d to the rules v1.
func policy(t *testing.T, d routing.Deployment) routing.Policy {
	t.Helper()
	p, err := routing.New(routing.V1(), d)
	require.NoError(t, err)
	return p
}

//go:fix inline

// TestV1IsTheUsersRules: the embedded rules are the user's file byte for
// byte, and decode to its seven bindings and seven roles.
func TestV1IsTheUsersRules(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(rulesFile)
	require.NoError(t, err)
	sum := sha256.Sum256(raw)
	assert.Equal(t, rulesSHA256, hex.EncodeToString(sum[:]))

	r := routing.V1()
	assert.Equal(t, 1, r.SchemaVersion)
	assert.Equal(t, "draft_unvalidated", r.Status)
	assert.Equal(t, []string{"claude_mid", "claude_premium", "claude_small", "claude_strong", "codex_premium", "codex_small", "codex_strong"}, r.BindingNames())
	assert.Equal(t, []string{"alex_debugger", "boss", "clerk", "code_review", "coder", "pre_check", "researcher"}, r.RoleNames())
	assert.Equal(t, []string{routing.Claude, routing.Codex, ""}, []string{r.Provider("claude_mid"), r.Provider("codex_premium"), r.Provider("claude_huge")})
}

// TestRoleFor: each worker type of the team model has its role, qa
// pre_check's, and every role it names is one of the rules'; anything else
// is refused.
func TestRoleFor(t *testing.T) {
	t.Parallel()
	want := map[string]string{
		"boss": "boss", "coder": "coder", "precheck": "pre_check", "qa": "pre_check",
		"review": "code_review", "research": "researcher", "librarian": "clerk", "debugger": "alex_debugger",
	}
	for group, role := range want {
		got, err := routing.RoleFor(group)
		require.NoError(t, err, group)
		assert.Equal(t, role, got, group)
		assert.Contains(t, routing.V1().RoleNames(), got, "%s's role is one of the rules'", group)
	}
	for _, group := range []string{"", "coders", "Coder", "pre_check", "clerk", "codex", "coder ", "-coder", "cöder", "coder\xff"} {
		_, err := routing.RoleFor(group)
		require.ErrorIs(t, err, routing.ErrUnknownRole, "%q", group)
		assert.Contains(t, err.Error(), "librarian", "the error lists the types that have a role")
	}
}

// TestEveryRoleAndProviderResolvesToItsDefault: every role under each
// provider gets the rules' default binding, its model id and the role's
// effort (or the binding's own when the role has none), and a role whose
// one default runs under the other provider is blocked, never switched.
func TestEveryRoleAndProviderResolvesToItsDefault(t *testing.T) {
	t.Parallel()
	p := policy(t, routing.Deployment{})
	tests := []struct {
		role, provider, binding, model, effort string
		selection                              routing.Selection
	}{
		{"boss", routing.Claude, "claude_strong", "claude-opus-5-5", "high", routing.SelectExplicit},
		{"coder", routing.Claude, "claude_mid", "claude-sonnet-5-5", "high", routing.SelectJevComplexity},
		{"coder", routing.Codex, "codex_strong", "gpt-6.1-sol", "high", routing.SelectJevComplexity},
		{"clerk", routing.Claude, "claude_small", "claude-haiku-5-5", "low", routing.SelectFixed},
		{"clerk", routing.Codex, "codex_small", "gpt-6-luna", "low", routing.SelectFixed},
		{"researcher", routing.Claude, "claude_small", "claude-haiku-5-5", "low", routing.SelectFixed},
		{"researcher", routing.Codex, "codex_small", "gpt-6-luna", "low", routing.SelectFixed},
		{"pre_check", routing.Claude, "claude_mid", "claude-sonnet-5-5", "high", routing.SelectFixed},
		{"pre_check", routing.Codex, "codex_strong", "gpt-6.1-sol", "high", routing.SelectFixed},
		{"code_review", routing.Codex, "codex_strong", "gpt-6.1-sol", "high", routing.SelectFixed},
		{"alex_debugger", routing.Claude, "claude_strong", "claude-opus-5-5", "high", routing.SelectExplicit},
		{"alex_debugger", routing.Codex, "codex_strong", "gpt-6.1-sol", "high", routing.SelectExplicit},
	}
	for _, tc := range tests {
		d, err := p.Resolve(routing.Request{Role: tc.role, Provider: tc.provider}, now)
		require.NoError(t, err, "%s under %s", tc.role, tc.provider)
		assert.Equal(t, []string{tc.binding, tc.model, tc.effort}, []string{d.SelectedBinding, d.ModelID, d.Effort}, "%s under %s", tc.role, tc.provider)
		assert.Equal(t, tc.selection, d.Selection)
		assert.Equal(t, []string{tc.role, tc.provider}, []string{d.Role, d.Provider})
		assert.Nil(t, d.Override)
		assert.False(t, d.Applied, "routing is off unless the user turns it on")
	}
	// No fallback to the other provider (the user, 2026-10-22): the refusal
	// names what the role allows instead.
	for _, tc := range []struct{ role, provider, allows string }{
		{"boss", routing.Codex, "under codex it allows codex_strong, as an override"},
		{"code_review", routing.Claude, "it allows only codex_strong, none under claude"},
	} {
		_, err := p.Resolve(routing.Request{Role: tc.role, Provider: tc.provider}, now)
		require.ErrorIs(t, err, routing.ErrBlocked, "%s under %s", tc.role, tc.provider)
		assert.Contains(t, err.Error(), "never changes the provider")
		assert.Contains(t, err.Error(), tc.allows)
	}
	d, err := policy(t, routing.Deployment{Enabled: true}).Resolve(routing.Request{Role: "coder", Provider: routing.Claude}, now)
	require.NoError(t, err)
	assert.True(t, d.Applied, "a decision is applied once the user turns routing on")
}

// TestResolveOverrides: the boss's --model names an allowed binding by name
// or model id, --effort picks among the binding's efforts, and the user's
// override, the only way to a premium binding, stands over the boss's.
func TestResolveOverrides(t *testing.T) {
	t.Parallel()
	p := policy(t, routing.Deployment{Overrides: map[string]routing.RoleOverride{"alex_debugger": {"claude_premium"}}})
	tests := map[string]struct {
		req                    routing.Request
		binding, model, effort string
		override               routing.Override
		reason                 routing.Reason
	}{
		"the boss names a binding": {
			req: routing.Request{Role: "coder", Provider: routing.Claude, Model: "claude_strong"}, binding: "claude_strong", model: "claude-opus-5-5", effort: "high",
			override: routing.Override{Binding: "claude_strong", By: "boss"},
		},
		"the boss names a model id": {
			req: routing.Request{Role: "coder", Provider: routing.Codex, Model: "gpt-6.1-sol"}, binding: "codex_strong", model: "gpt-6.1-sol", effort: "high",
			override: routing.Override{Binding: "codex_strong", By: "boss"},
		},
		"the boss names a binding and an effort": {
			req: routing.Request{Role: "boss", Provider: routing.Claude, Model: "claude_strong", Effort: "xhigh"}, binding: "claude_strong", model: "claude-opus-5-5", effort: "xhigh",
			override: routing.Override{Binding: "claude_strong", By: "boss", Effort: "xhigh"},
		},
		"the boss names only an effort, and the coder keeps its reason": {
			req: routing.Request{Role: "coder", Provider: routing.Codex, Effort: "xhigh"}, binding: "codex_strong", model: "gpt-6.1-sol", effort: "xhigh",
			override: routing.Override{Effort: "xhigh"}, reason: routing.ReasonTaskNotFound,
		},
		"the user's premium override": {
			req: routing.Request{Role: "alex_debugger", Provider: routing.Claude}, binding: "claude_premium", model: "claude-fable-5-1", effort: "high",
			override: routing.Override{Binding: "claude_premium", By: "user"},
		},
		"the boss names what the user chose": {
			req: routing.Request{Role: "alex_debugger", Provider: routing.Claude, Model: "claude_premium", Effort: "xhigh"}, binding: "claude_premium", model: "claude-fable-5-1", effort: "xhigh",
			override: routing.Override{Binding: "claude_premium", By: "user", Effort: "xhigh"},
		},
	}
	for name, tc := range tests {
		d, err := p.Resolve(tc.req, now)
		require.NoError(t, err, name)
		assert.Equal(t, []string{tc.binding, tc.model, tc.effort}, []string{d.SelectedBinding, d.ModelID, d.Effort}, name)
		require.NotNil(t, d.Override, name)
		assert.Equal(t, tc.override, *d.Override, name)
		assert.Equal(t, tc.reason, d.FallbackReason, name)
	}
}

// TestResolveRefuses: an unknown role, a provider that is not one, an
// override the rules or the user do not allow, an effort the binding does
// not take and a packet that breaks its rules are each refused with their
// sentinel, never answered with another binding.
func TestResolveRefuses(t *testing.T) {
	t.Parallel()
	user := policy(t, routing.Deployment{Overrides: map[string]routing.RoleOverride{"pre_check": {"claude_mid"}, "researcher": {"codex_small"}}})
	found := func(edit func(*routing.Task)) *routing.Task {
		task := &routing.Task{LookupStatus: routing.LookupFound, WorkerAssignment: "fix the bug"}
		edit(task)
		return task
	}
	tests := map[string]struct {
		req     routing.Request
		wantErr error
		wantIn  string
	}{
		"a worker type, not a role":                 {req: routing.Request{Role: "qa", Provider: routing.Claude}, wantErr: routing.ErrUnknownRole},
		"no role":                                   {req: routing.Request{Provider: routing.Claude}, wantErr: routing.ErrUnknownRole},
		"no provider":                               {req: routing.Request{Role: "coder"}, wantErr: routing.ErrProviderMismatch},
		"a provider's case variant":                 {req: routing.Request{Role: "coder", Provider: "Claude"}, wantErr: routing.ErrProviderMismatch},
		"an effort's case variant":                  {req: routing.Request{Role: "coder", Provider: routing.Claude, Effort: "HIGH"}, wantErr: routing.ErrOverrideRefused},
		"a provider that is neither":                {req: routing.Request{Role: "coder", Provider: "aider"}, wantErr: routing.ErrProviderMismatch},
		"a premium binding from the boss":           {req: routing.Request{Role: "alex_debugger", Provider: routing.Codex, Model: "codex_premium"}, wantErr: routing.ErrOverrideRefused, wantIn: "only the user chooses"},
		"a premium model id from the boss":          {req: routing.Request{Role: "alex_debugger", Provider: routing.Claude, Model: "claude-fable-5-1"}, wantErr: routing.ErrOverrideRefused, wantIn: "only the user chooses"},
		"a codex premium model id from the boss":    {req: routing.Request{Role: "alex_debugger", Provider: routing.Codex, Model: "gpt-6-astra"}, wantErr: routing.ErrOverrideRefused, wantIn: "only the user chooses"},
		"a binding the role does not allow":         {req: routing.Request{Role: "clerk", Provider: routing.Claude, Model: "claude_strong"}, wantErr: routing.ErrOverrideRefused, wantIn: "claude_small, codex_small"},
		"a model id the role does not allow":        {req: routing.Request{Role: "clerk", Provider: routing.Claude, Model: "claude-sonnet-5-5"}, wantErr: routing.ErrOverrideRefused, wantIn: "claude_small, codex_small"},
		"a model id the boss's role does not allow": {req: routing.Request{Role: "boss", Provider: routing.Claude, Model: "claude-haiku-5-5"}, wantErr: routing.ErrOverrideRefused, wantIn: "claude_strong, codex_strong"},
		"an alias, not a model id":                  {req: routing.Request{Role: "coder", Provider: routing.Claude, Model: "Opus 5.5"}, wantErr: routing.ErrOverrideRefused},
		"a short name":                              {req: routing.Request{Role: "coder", Provider: routing.Claude, Model: "opus"}, wantErr: routing.ErrOverrideRefused},
		"a binding of the other provider":           {req: routing.Request{Role: "coder", Provider: routing.Claude, Model: "codex_strong"}, wantErr: routing.ErrProviderMismatch, wantIn: "boss's --model"},
		"an effort the binding does not take":       {req: routing.Request{Role: "clerk", Provider: routing.Claude, Effort: "high"}, wantErr: routing.ErrOverrideRefused, wantIn: "takes low"},
		"an effort no client takes":                 {req: routing.Request{Role: "coder", Provider: routing.Claude, Effort: "turbo"}, wantErr: routing.ErrOverrideRefused},
		"the boss against the user's choice":        {req: routing.Request{Role: "pre_check", Provider: routing.Claude, Model: "codex_strong"}, wantErr: routing.ErrOverrideRefused, wantIn: "sets pre_check to claude_mid"},
		"a type running neither":                    {req: routing.Request{Role: "coder", Provider: "aider", Type: "coder"}, wantErr: routing.ErrProviderMismatch, wantIn: `the worker type coder runs "aider"`},
		"a packet's user_model_override":            {req: routing.Request{Role: "coder", Provider: routing.Claude, Task: found(func(t *routing.Task) { t.UserModelOverride = new("claude_premium") })}, wantErr: routing.ErrOverrideRefused},
		"a packet for another role":                 {req: routing.Request{Role: "coder", Provider: routing.Claude, Task: found(func(t *routing.Task) { t.RequestedRole = "pre_check" })}, wantErr: routing.ErrInvalid},
		"a packet preferring the other provider":    {req: routing.Request{Role: "coder", Provider: routing.Claude, Task: found(func(t *routing.Task) { t.ProviderPreference = routing.Codex })}, wantErr: routing.ErrProviderMismatch},
		"a packet with no lookup status":            {req: routing.Request{Role: "coder", Provider: routing.Claude, Task: found(func(t *routing.Task) { t.LookupStatus = "" })}, wantErr: routing.ErrInvalid},
		"a packet with another lookup status":       {req: routing.Request{Role: "coder", Provider: routing.Claude, Task: found(func(t *routing.Task) { t.LookupStatus = "Found" })}, wantErr: routing.ErrInvalid},
	}
	for name, tc := range tests {
		_, err := user.Resolve(tc.req, now)
		require.ErrorIs(t, err, tc.wantErr, name)
		assert.Contains(t, err.Error(), tc.wantIn, name)
	}
}

// TestResolveFilters: a binding with no model id, switched off or not
// taking the effort is never chosen: a default that fails blocks the role,
// and an override that fails is refused.
func TestResolveFilters(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		d       routing.Deployment
		req     routing.Request
		wantErr error
		wantIn  string
	}{
		"a default with no model id": {
			d: routing.Deployment{Bindings: map[string]routing.Facts{"claude_mid": {ModelID: new("")}}}, req: routing.Request{Role: "coder", Provider: routing.Claude},
			wantErr: routing.ErrBlocked, wantIn: "unresolved",
		},
		"a default switched off": {
			d: routing.Deployment{Bindings: map[string]routing.Facts{"claude_small": {Enabled: new(false)}}}, req: routing.Request{Role: "clerk", Provider: routing.Claude},
			wantErr: routing.ErrBlocked, wantIn: "disabled",
		},
		"a default that no longer takes the role's effort": {
			d: routing.Deployment{Bindings: map[string]routing.Facts{"claude_mid": {Efforts: []string{"medium"}}}}, req: routing.Request{Role: "pre_check", Provider: routing.Claude},
			wantErr: routing.ErrBlocked, wantIn: "does not take effort high",
		},
		"the boss's binding with no model id": {
			d: routing.Deployment{Bindings: map[string]routing.Facts{"claude_strong": {ModelID: new("")}}}, req: routing.Request{Role: "coder", Provider: routing.Claude, Model: "claude_strong"},
			wantErr: routing.ErrOverrideRefused, wantIn: "boss's --model",
		},
		"the user's binding switched off": {
			d: routing.Deployment{Bindings: map[string]routing.Facts{"claude_premium": {Enabled: new(false)}}, Overrides: map[string]routing.RoleOverride{"alex_debugger": {"claude_premium"}}}, req: routing.Request{Role: "alex_debugger", Provider: routing.Claude},
			wantErr: routing.ErrOverrideRefused, wantIn: "user's [routing.overrides]",
		},
		"two bindings with the model id asked for": {
			d: routing.Deployment{Bindings: map[string]routing.Facts{"claude_mid": {ModelID: new("claude-opus-5-5")}}}, req: routing.Request{Role: "coder", Provider: routing.Claude, Model: "claude-opus-5-5"},
			wantErr: routing.ErrOverrideRefused, wantIn: "name the binding",
		},
	}
	for name, tc := range tests {
		_, err := policy(t, tc.d).Resolve(tc.req, now)
		require.ErrorIs(t, err, tc.wantErr, name)
		assert.Contains(t, err.Error(), tc.wantIn, name)
	}
	// The facts apply: a binding given a new id and efforts runs by them.
	d, err := policy(t, routing.Deployment{Bindings: map[string]routing.Facts{"claude_small": {ModelID: new("claude-haiku-5-5-20261001"), Efforts: []string{"medium", "low"}}}}).Resolve(routing.Request{Role: "clerk", Provider: routing.Claude}, now)
	require.NoError(t, err)
	assert.Equal(t, []string{"claude-haiku-5-5-20261001", "medium"}, []string{d.ModelID, d.Effort}, "a role with no effort of its own runs the binding's first")
}

// TestJevReasons: a coder takes its default with the reason that keeps Jev
// out of it, each logged apart; a role that does not ask Jev, and a coder
// whose binding the boss named, carry none.
func TestJevReasons(t *testing.T) {
	t.Parallel()
	p := policy(t, routing.Deployment{})
	task := func(status, assignment string) *routing.Task {
		return &routing.Task{LookupStatus: status, WorkerAssignment: assignment}
	}
	tests := map[string]struct {
		req  routing.Request
		want routing.Reason
	}{
		"no packet":                  {req: routing.Request{Role: "coder", Provider: routing.Claude}, want: routing.ReasonTaskNotFound},
		"the task not found":         {req: routing.Request{Role: "coder", Provider: routing.Claude, Task: task(routing.LookupNotFound, "fix it")}, want: routing.ReasonTaskNotFound},
		"the lookup unavailable":     {req: routing.Request{Role: "coder", Provider: routing.Claude, Task: task(routing.LookupUnavailable, "fix it")}, want: routing.ReasonLookupUnavailable},
		"an empty assignment":        {req: routing.Request{Role: "coder", Provider: routing.Claude, Task: task(routing.LookupFound, "")}, want: routing.ReasonInsufficientAssignment},
		"a blank assignment":         {req: routing.Request{Role: "coder", Provider: routing.Codex, Task: task(routing.LookupFound, " \t\n")}, want: routing.ReasonInsufficientAssignment},
		"an assignment":              {req: routing.Request{Role: "coder", Provider: routing.Claude, Task: task(routing.LookupFound, "fix it")}, want: routing.ReasonJevDisabled},
		"a fixed role":               {req: routing.Request{Role: "clerk", Provider: routing.Claude, Task: task(routing.LookupFound, "tidy")}, want: routing.ReasonNone},
		"an explicit role":           {req: routing.Request{Role: "boss", Provider: routing.Claude}, want: routing.ReasonNone},
		"the boss named the binding": {req: routing.Request{Role: "coder", Provider: routing.Claude, Model: "claude_strong", Task: task(routing.LookupFound, "fix it")}, want: routing.ReasonNone},
	}
	for name, tc := range tests {
		d, err := p.Resolve(tc.req, now)
		require.NoError(t, err, name)
		assert.Equal(t, tc.want, d.FallbackReason, name)
	}
}

// TestDecisionCarriesEveryAuditField: a decision encodes every audit field
// the rules list, null where it has nothing, plus Jev's error, the
// selection, whether it was applied and the filters left unchecked; the
// assignment is there as a digest, never as text.
func TestDecisionCarriesEveryAuditField(t *testing.T) {
	t.Parallel()
	p := policy(t, routing.Deployment{})
	task := &routing.Task{
		TaskID: new("NERD-5289"), LookupStatus: routing.LookupFound, Title: "routing",
		Requirements: "route each worker", AcceptanceCriteria: []string{"defaults resolve"}, WorkerAssignment: "the secret plan",
	}
	d, err := p.Resolve(routing.Request{Role: "coder", Provider: routing.Claude, Task: task}, now)
	require.NoError(t, err)
	raw, err := json.Marshal(d)
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(raw, &got))
	audit := []string{"task_id", "task_lookup_status", "assignment_digest", "role", "provider", "selected_binding", "model_id", "effort", "policy_version", "jev_model_returned", "answers", "fallback_reason", "override", "timestamp"}
	assert.ElementsMatch(t, append(audit, "jev_error", "selection", "applied", "unchecked"), slices.Collect(maps.Keys(got)))
	assert.Empty(t, got["jev_error"])
	assert.Equal(t, "NERD-5289", got["task_id"])
	assert.Equal(t, routing.LookupFound, got["task_lookup_status"])
	assert.Regexp(t, `^sha256:[0-9a-f]{64}$`, got["assignment_digest"])
	assert.Equal(t, "2026-10-21T17:00:00Z", got["timestamp"], "the time is UTC")
	assert.Nil(t, got["answers"])
	assert.Nil(t, got["override"])
	assert.Equal(t, []any{"tool_compatibility", "context_headroom", "availability", "spending_limits"}, got["unchecked"])
	assert.NotContains(t, string(raw), "the secret plan")
	assert.NotContains(t, string(raw), "route each worker")
	assert.Equal(t, p.Version(), d.PolicyVersion)

	// No packet: no task id, no digest, the task counted as not found.
	d, err = p.Resolve(routing.Request{Role: "coder", Provider: routing.Claude}, now)
	require.NoError(t, err)
	assert.Nil(t, d.TaskID)
	assert.Equal(t, []string{routing.LookupNotFound, ""}, []string{d.TaskLookupStatus, d.AssignmentDigest})
}

// TestAssignmentDigest: the digest follows the assignment, the
// requirements and the acceptance criteria, and nothing else.
func TestAssignmentDigest(t *testing.T) {
	t.Parallel()
	p := policy(t, routing.Deployment{})
	digest := func(edit func(*routing.Task)) string {
		task := &routing.Task{LookupStatus: routing.LookupFound, WorkerAssignment: "a", Requirements: "r", AcceptanceCriteria: []string{"c"}}
		edit(task)
		d, err := p.Resolve(routing.Request{Role: "coder", Provider: routing.Claude, Task: task}, now)
		require.NoError(t, err)
		return d.AssignmentDigest
	}
	base := digest(func(*routing.Task) {})
	for name, edit := range map[string]func(*routing.Task){
		"the title":         func(t *routing.Task) { t.Title = "other" },
		"the task id":       func(t *routing.Task) { t.TaskID = new("NERD-1") },
		"the lookup":        func(t *routing.Task) { t.LookupStatus = routing.LookupUnavailable },
		"previous attempts": func(t *routing.Task) { t.PreviousAttempts = "tried" },
	} {
		assert.Equal(t, base, digest(edit), "%s is not the assignment", name)
	}
	for name, edit := range map[string]func(*routing.Task){
		"the assignment":       func(t *routing.Task) { t.WorkerAssignment = "b" },
		"the requirements":     func(t *routing.Task) { t.Requirements = "s" },
		"a criterion":          func(t *routing.Task) { t.AcceptanceCriteria = []string{"d"} },
		"no criteria":          func(t *routing.Task) { t.AcceptanceCriteria = nil },
		"a criterion moved":    func(t *routing.Task) { t.AcceptanceCriteria = []string{"", "c"} },
		"text moved between":   func(t *routing.Task) { t.WorkerAssignment, t.Requirements = "ar", "" },
		"a criterion moved up": func(t *routing.Task) { t.Requirements, t.AcceptanceCriteria = "c", []string{"r"} },
	} {
		assert.NotEqual(t, base, digest(edit), "%s changes the digest", name)
	}
	// Each field's length is set off from its bytes: without the ':' these
	// two would both hash fifteen 1s, then the criterion.
	assert.NotEqual(t, digest(func(t *routing.Task) { t.WorkerAssignment, t.Requirements = "1", "11111111111" }),
		digest(func(t *routing.Task) { t.WorkerAssignment, t.Requirements = "11111111111", "1" }))
	// Bytes that are not UTF-8 are hashed as they are, never folded into one
	// replacement character.
	assert.NotEqual(t, digest(func(t *routing.Task) { t.WorkerAssignment = "\xff" }), digest(func(t *routing.Task) { t.WorkerAssignment = "\xfe" }))
}
