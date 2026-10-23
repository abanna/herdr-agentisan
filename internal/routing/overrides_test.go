package routing_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/routing"
)

// TestOverrideVersionsKeepTheString: a string override keeps the version it
// had before an override could hold one binding per provider (the live
// configuration's, pinned from 71d3620), so no decision recorded earlier
// changes its name; a one-element list is the string, and a list's order
// never changes its version (A63).
func TestOverrideVersionsKeepTheString(t *testing.T) {
	t.Parallel()
	live := policy(t, routing.Deployment{Enabled: true, Overrides: map[string]routing.RoleOverride{"coder": {"claude_strong"}}}).Version()
	assert.Equal(t, "v1-f0225362e568", live)
	both := policy(t, routing.Deployment{Enabled: true, Overrides: map[string]routing.RoleOverride{"coder": {"claude_strong", "codex_strong"}}}).Version()
	reversed := policy(t, routing.Deployment{Enabled: true, Overrides: map[string]routing.RoleOverride{"coder": {"codex_strong", "claude_strong"}}}).Version()
	assert.Equal(t, both, reversed, "a list's order is no fact")
	assert.NotEqual(t, live, both, "a binding for codex is another deployment")
	codex := policy(t, routing.Deployment{Enabled: true, Overrides: map[string]routing.RoleOverride{"coder": {"codex_strong"}}}).Version()
	assert.NotEqual(t, live, codex)
	assert.NotEqual(t, both, codex)
}

// TestUserOverridesBindTheirOwnProvider: the user's override of a role binds
// only workers of its binding's provider; a worker of another provider
// decides as if the user had set none, and a list sets one binding per
// provider (A63).
func TestUserOverridesBindTheirOwnProvider(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 23, 14, 0, 0, 0, time.UTC)
	tests := map[string]struct {
		overrides map[string]routing.RoleOverride
		role      string
		provider  string
		binding   string
		by        string
	}{
		"a claude coder under claude_strong":   {overrides: map[string]routing.RoleOverride{"coder": {"claude_strong"}}, role: "coder", provider: routing.Claude, binding: "claude_strong", by: "user"},
		"a codex coder under claude_strong":    {overrides: map[string]routing.RoleOverride{"coder": {"claude_strong"}}, role: "coder", provider: routing.Codex, binding: "codex_strong"},
		"a claude coder under the list":        {overrides: map[string]routing.RoleOverride{"coder": {"codex_strong", "claude_strong"}}, role: "coder", provider: routing.Claude, binding: "claude_strong", by: "user"},
		"a codex coder under the list":         {overrides: map[string]routing.RoleOverride{"coder": {"codex_strong", "claude_strong"}}, role: "coder", provider: routing.Codex, binding: "codex_strong", by: "user"},
		"a claude boss under codex_strong":     {overrides: map[string]routing.RoleOverride{"boss": {"codex_strong"}}, role: "boss", provider: routing.Claude, binding: "claude_strong"},
		"a codex boss under codex_strong":      {overrides: map[string]routing.RoleOverride{"boss": {"codex_strong"}}, role: "boss", provider: routing.Codex, binding: "codex_strong", by: "user"},
		"a codex qa under a claude pre_check":  {overrides: map[string]routing.RoleOverride{"pre_check": {"claude_mid"}}, role: "pre_check", provider: routing.Codex, binding: "codex_strong"},
		"a claude researcher under codex":      {overrides: map[string]routing.RoleOverride{"researcher": {"codex_small"}}, role: "researcher", provider: routing.Claude, binding: "claude_small"},
		"a codex debugger under claude only":   {overrides: map[string]routing.RoleOverride{"alex_debugger": {"claude_premium"}}, role: "alex_debugger", provider: routing.Codex, binding: "codex_strong"},
		"a claude debugger's premium override": {overrides: map[string]routing.RoleOverride{"alex_debugger": {"claude_premium"}}, role: "alex_debugger", provider: routing.Claude, binding: "claude_premium", by: "user"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			d, err := policy(t, routing.Deployment{Enabled: true, Overrides: tc.overrides}).Resolve(routing.Request{Role: tc.role, Provider: tc.provider}, now)
			require.NoError(t, err)
			assert.Equal(t, tc.provider, d.Provider)
			assert.Equal(t, tc.binding, d.SelectedBinding)
			if tc.by == "" {
				assert.Nil(t, d.Override, "no override applies to this provider")
				return
			}
			require.NotNil(t, d.Override)
			assert.Equal(t, routing.Override{Binding: tc.binding, By: tc.by}, *d.Override)
		})
	}
	_, err := policy(t, routing.Deployment{Enabled: true}).Resolve(routing.Request{Role: "boss", Provider: routing.Codex}, now)
	require.ErrorIs(t, err, routing.ErrBlocked, "the boss under codex with no override for codex is blocked, as A50 says")
}

// TestBossModelMeetsTheOverrideOfItsProvider: the boss's --model is held to
// the user's override for the worker's own provider only; with none for that
// provider the boss names any binding the role allows under it, premium
// still the user's alone (A63, A50).
func TestBossModelMeetsTheOverrideOfItsProvider(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 23, 14, 0, 0, 0, time.UTC)
	claudeStrong := map[string]routing.RoleOverride{"coder": {"claude_strong"}}
	both := map[string]routing.RoleOverride{"coder": {"claude_strong", "codex_strong"}}
	premium := map[string]routing.RoleOverride{"alex_debugger": {"claude_premium"}}
	tests := map[string]struct {
		overrides map[string]routing.RoleOverride
		role      string
		req       routing.Request
		binding   string
		by        string
		wantErr   error
	}{
		"codex, no codex override: the boss's binding": {overrides: claudeStrong, req: routing.Request{Role: "coder", Provider: routing.Codex, Model: "codex_strong"}, binding: "codex_strong", by: "boss"},
		"claude, another binding than the user's":      {overrides: claudeStrong, req: routing.Request{Role: "coder", Provider: routing.Claude, Model: "claude_mid"}, wantErr: routing.ErrOverrideRefused},
		"claude, the user's binding":                   {overrides: claudeStrong, req: routing.Request{Role: "coder", Provider: routing.Claude, Model: "claude_strong"}, binding: "claude_strong", by: "user"},
		"codex, a claude binding":                      {overrides: claudeStrong, req: routing.Request{Role: "coder", Provider: routing.Codex, Model: "claude_mid"}, wantErr: routing.ErrProviderMismatch},
		"codex, the list's codex binding":              {overrides: both, req: routing.Request{Role: "coder", Provider: routing.Codex, Model: "codex_strong"}, binding: "codex_strong", by: "user"},
		"codex debugger, premium from the boss":        {overrides: premium, req: routing.Request{Role: "alex_debugger", Provider: routing.Codex, Model: "codex_premium"}, wantErr: routing.ErrOverrideRefused},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			d, err := policy(t, routing.Deployment{Enabled: true, Overrides: tc.overrides}).Resolve(tc.req, now)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.binding, d.SelectedBinding)
			require.NotNil(t, d.Override)
			assert.Equal(t, routing.Override{Binding: tc.binding, By: tc.by}, *d.Override)
		})
	}
}

// TestNewRefusesAListThatIsNotOnePerProvider: a list sets at most one
// binding per provider and names one at least; each binding is held to the
// role as a string is. Each refusal names the override's key (A63).
func TestNewRefusesAListThatIsNotOnePerProvider(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		o       routing.RoleOverride
		role    string
		wantErr error
		wantIn  []string
	}{
		"two claude bindings":        {role: "coder", o: routing.RoleOverride{"claude_mid", "claude_strong"}, wantErr: routing.ErrOverrideRefused, wantIn: []string{"routing.overrides.coder", "claude_mid", "claude_strong", "claude"}},
		"one binding twice":          {role: "coder", o: routing.RoleOverride{"codex_strong", "codex_strong"}, wantErr: routing.ErrOverrideRefused, wantIn: []string{"routing.overrides.coder", "codex_strong"}},
		"an empty list":              {role: "coder", o: routing.RoleOverride{}, wantErr: routing.ErrOverrideRefused, wantIn: []string{"routing.overrides.coder", "names no binding"}},
		"a binding the role refuses": {role: "clerk", o: routing.RoleOverride{"claude_small", "codex_strong"}, wantErr: routing.ErrOverrideRefused, wantIn: []string{"routing.overrides.clerk", "codex_strong"}},
		"a premium for the boss":     {role: "boss", o: routing.RoleOverride{"codex_strong", "claude_premium"}, wantErr: routing.ErrOverrideRefused, wantIn: []string{"routing.overrides.boss"}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := routing.New(routing.V1(), routing.Deployment{Overrides: map[string]routing.RoleOverride{tc.role: tc.o}})
			require.ErrorIs(t, err, tc.wantErr)
			for _, s := range tc.wantIn {
				assert.Contains(t, err.Error(), s)
			}
		})
	}
	_, err := routing.New(routing.V1(), routing.Deployment{Overrides: map[string]routing.RoleOverride{"coder": {"claude_strong", "codex_strong"}, "clerk": {"codex_small"}}})
	require.NoError(t, err, "one binding per provider, and one alone")
}

// TestRulesCheckOverrides: CheckOverrides is the check New and settings run
// on one role's override: each binding allowed, one per provider, at least
// one (A63).
func TestRulesCheckOverrides(t *testing.T) {
	t.Parallel()
	r := routing.V1()
	require.NoError(t, r.CheckOverrides("coder", routing.RoleOverride{"claude_strong"}))
	require.NoError(t, r.CheckOverrides("coder", routing.RoleOverride{"codex_strong", "claude_mid"}))
	require.ErrorIs(t, r.CheckOverrides("coder", routing.RoleOverride{"claude_mid", "claude_strong"}), routing.ErrOverrideRefused)
	require.ErrorIs(t, r.CheckOverrides("coder", nil), routing.ErrOverrideRefused)
	require.ErrorIs(t, r.CheckOverrides("qa", routing.RoleOverride{"claude_mid"}), routing.ErrUnknownRole)
}

// TestPolicyUserOverride: the user's binding for a role under a provider,
// "" when the user set none for that provider.
func TestPolicyUserOverride(t *testing.T) {
	t.Parallel()
	p := policy(t, routing.Deployment{Overrides: map[string]routing.RoleOverride{"coder": {"claude_strong"}, "boss": {"claude_strong", "codex_strong"}}})
	assert.Equal(t, "claude_strong", p.UserOverride("coder", routing.Claude))
	assert.Empty(t, p.UserOverride("coder", routing.Codex))
	assert.Equal(t, "codex_strong", p.UserOverride("boss", routing.Codex))
	assert.Empty(t, p.UserOverride("clerk", routing.Claude))
}
