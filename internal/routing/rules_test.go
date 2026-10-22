package routing_test

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/routing"
)

// rulesDoc is the user's rules as a tree a test can change.
func rulesDoc(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(rulesFile)
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, json.Unmarshal(raw, &doc))
	return doc
}

// at walks a path of keys down a rules tree.
func at(doc map[string]any, path ...string) map[string]any {
	for _, k := range path {
		doc = doc[k].(map[string]any)
	}
	return doc
}

// TestParseRulesRefuses: rules that break a rule of their own are refused
// with ErrInvalid, the rule named, so a change to the user's file can never
// quietly route by something the plugin does not understand.
func TestParseRulesRefuses(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		edit   func(doc map[string]any)
		wantIn string
	}{
		"a top-level key it does not know": {func(d map[string]any) { d["fallback"] = "x" }, "unknown field"},
		"a role key it does not know":      {func(d map[string]any) { at(d, "roles", "coder")["escalate"] = true }, "unknown field"},
		"a binding key it does not know":   {func(d map[string]any) { at(d, "bindings", "claude_mid")["tools"] = []any{} }, "unknown field"},
		"another schema version":           {func(d map[string]any) { d["schema_version"] = 2 }, "schema_version is 2"},
		"no schema version":                {func(d map[string]any) { delete(d, "schema_version") }, "schema_version is 0"},
		"another routing owner":            {func(d map[string]any) { d["routing_owner"] = "boss" }, "routing_owner"},
		"no bindings":                      {func(d map[string]any) { d["bindings"] = map[string]any{} }, "need bindings and roles"},
		"no roles":                         {func(d map[string]any) { d["roles"] = map[string]any{} }, "need bindings and roles"},
		"a binding name out of the alphabet": {func(d map[string]any) {
			at(d, "bindings")["Claude_mid"] = at(d, "bindings", "claude_mid")
		}, "a name must match"},
		"a non-ASCII binding name":   {func(d map[string]any) { at(d, "bindings")["claude_mïd"] = at(d, "bindings", "claude_mid") }, "a name must match"},
		"a binding name like a flag": {func(d map[string]any) { at(d, "bindings")["-claude_mid"] = at(d, "bindings", "claude_mid") }, "a name must match"},
		"a binding of no provider": {func(d map[string]any) {
			at(d, "bindings")["gemini_mid"] = map[string]any{"model_id": "g", "efforts": []any{"high"}}
		}, "must start with claude_ or codex_"},
		"a model id that starts with a dash": {func(d map[string]any) { at(d, "bindings", "claude_mid")["model_id"] = "--bare" }, "not a model id"},
		"a model id with a space":            {func(d map[string]any) { at(d, "bindings", "claude_mid")["model_id"] = "claude sonnet" }, "not a model id"},
		"no efforts":                         {func(d map[string]any) { at(d, "bindings", "claude_mid")["efforts"] = []any{} }, "efforts is empty"},
		"an effort no client takes":          {func(d map[string]any) { at(d, "bindings", "claude_mid")["efforts"] = []any{"turbo"} }, `effort "turbo"`},
		"an effort twice":                    {func(d map[string]any) { at(d, "bindings", "claude_strong")["efforts"] = []any{"high", "high"} }, "listed twice"},
		"an effort only codex takes":         {func(d map[string]any) { at(d, "bindings", "claude_mid")["efforts"] = []any{"high", "minimal"} }, `effort "minimal" is not one claude takes`},
		"an effort only claude takes":        {func(d map[string]any) { at(d, "bindings", "codex_strong")["efforts"] = []any{"max"} }, `effort "max" is not one codex takes`},
		"a role name out of the alphabet": {func(d map[string]any) {
			at(d, "roles")["Coder"] = at(d, "roles", "coder")
		}, "a name must match"},
		"a non-ASCII role name":              {func(d map[string]any) { at(d, "roles")["cöder"] = at(d, "roles", "coder") }, "a name must match"},
		"a selection it does not know":       {func(d map[string]any) { at(d, "roles", "coder")["selection"] = "jev" }, `selection "jev"`},
		"an empty allowed list":              {func(d map[string]any) { at(d, "roles", "clerk")["allowed"] = []any{} }, "allowed is empty"},
		"an allowed binding that is not one": {func(d map[string]any) { at(d, "roles", "clerk")["allowed"] = []any{"claude_small", "claude_tiny"} }, `"claude_tiny"`},
		"an allowed binding twice": {func(d map[string]any) {
			at(d, "roles", "clerk")["allowed"] = []any{"claude_small", "codex_small", "claude_small"}
		}, "twice"},
		"premium allowed where premium_allowed is false": {func(d map[string]any) {
			at(d, "roles", "boss")["allowed"] = []any{"claude_strong", "codex_strong", "claude_premium"}
		}, "premium_allowed is false"},
		"both default and defaults":    {func(d map[string]any) { at(d, "roles", "boss")["defaults"] = map[string]any{"claude": "claude_strong"} }, "exactly one"},
		"neither default nor defaults": {func(d map[string]any) { delete(at(d, "roles", "boss"), "default") }, "exactly one"},
		"a default outside allowed":    {func(d map[string]any) { at(d, "roles", "boss")["default"] = "claude_mid" }, "not in allowed"},
		"a premium default": {func(d map[string]any) {
			at(d, "roles", "alex_debugger", "defaults")["claude"] = "claude_premium"
		}, "is premium"},
		"a default of the other provider": {func(d map[string]any) {
			at(d, "roles", "coder", "defaults")["claude"] = "codex_strong"
		}, "the claude default codex_strong is a codex binding"},
		"a default effort its default does not take": {func(d map[string]any) { at(d, "roles", "coder")["default_effort"] = "xhigh" }, `default_effort "xhigh"`},
	}
	for name, tc := range tests {
		doc := rulesDoc(t)
		tc.edit(doc)
		raw, err := json.Marshal(doc)
		require.NoError(t, err)
		_, err = routing.ParseRules(raw)
		require.ErrorIs(t, err, routing.ErrInvalid, name)
		assert.Contains(t, err.Error(), tc.wantIn, name)
	}
	raw, err := os.ReadFile(rulesFile)
	require.NoError(t, err)
	for name, doc := range map[string][]byte{
		"not JSON":             []byte("schema_version = 1\n"),
		"two documents":        append(append([]byte{}, raw...), raw...),
		"a document then junk": append(append([]byte{}, raw...), '}'),
		"an array":             []byte("[]"),
		"empty":                {},
		"a BOM first":          append([]byte("\xef\xbb\xbf"), raw...),
		"a NUL first":          append([]byte{0}, raw...),
	} {
		_, err := routing.ParseRules(doc)
		require.ErrorIs(t, err, routing.ErrInvalid, name)
	}
}

// TestParseRulesKeepsAnUnresolvedID: a binding with no model id parses,
// unresolved, and resolving to it is refused, never "the latest".
func TestParseRulesKeepsAnUnresolvedID(t *testing.T) {
	t.Parallel()
	doc := rulesDoc(t)
	at(doc, "bindings", "claude_small")["model_id"] = ""
	raw, err := json.Marshal(doc)
	require.NoError(t, err)
	r, err := routing.ParseRules(raw)
	require.NoError(t, err)
	p, err := routing.New(r, routing.Deployment{})
	require.NoError(t, err)
	_, err = p.Resolve(routing.Request{Role: "clerk", Provider: routing.Claude}, now)
	require.ErrorIs(t, err, routing.ErrBlocked)
	assert.Contains(t, err.Error(), "unresolved")
}

// TestNewChecksTheDeployment: a fact or an override the rules do not allow
// is refused, naming the [routing] key it is in; a binding left unresolved
// and an empty deployment are allowed.
func TestNewChecksTheDeployment(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		d       routing.Deployment
		wantErr error
		wantIn  string
	}{
		"a binding the rules do not have":   {d: routing.Deployment{Bindings: map[string]routing.Facts{"claude_huge": {}}}, wantErr: routing.ErrInvalid, wantIn: "routing.bindings.claude_huge"},
		"a model id that is a flag":         {d: routing.Deployment{Bindings: map[string]routing.Facts{"claude_mid": {ModelID: new("-c")}}}, wantErr: routing.ErrInvalid, wantIn: "routing.bindings.claude_mid"},
		"a model id with a control":         {d: routing.Deployment{Bindings: map[string]routing.Facts{"claude_mid": {ModelID: new("claude\x1b[2J")}}}, wantErr: routing.ErrInvalid},
		"a model id past 128 bytes":         {d: routing.Deployment{Bindings: map[string]routing.Facts{"claude_mid": {ModelID: new(strings.Repeat("a", 129))}}}, wantErr: routing.ErrInvalid},
		"a model id with a space":           {d: routing.Deployment{Bindings: map[string]routing.Facts{"claude_mid": {ModelID: new("claude sonnet")}}}, wantErr: routing.ErrInvalid},
		"a non-ASCII model id":              {d: routing.Deployment{Bindings: map[string]routing.Facts{"claude_mid": {ModelID: new("claude-sönnet")}}}, wantErr: routing.ErrInvalid},
		"a model id with a NUL":             {d: routing.Deployment{Bindings: map[string]routing.Facts{"claude_mid": {ModelID: new("claude\x00")}}}, wantErr: routing.ErrInvalid},
		"no efforts":                        {d: routing.Deployment{Bindings: map[string]routing.Facts{"claude_mid": {Efforts: []string{}}}}, wantErr: routing.ErrInvalid, wantIn: "efforts is empty"},
		"an effort no client takes":         {d: routing.Deployment{Bindings: map[string]routing.Facts{"claude_mid": {Efforts: []string{"High"}}}}, wantErr: routing.ErrInvalid},
		"an effort only codex takes":        {d: routing.Deployment{Bindings: map[string]routing.Facts{"claude_mid": {Efforts: []string{"minimal"}}}}, wantErr: routing.ErrInvalid, wantIn: "not one claude takes"},
		"an effort only claude takes":       {d: routing.Deployment{Bindings: map[string]routing.Facts{"codex_strong": {Efforts: []string{"high", "max"}}}}, wantErr: routing.ErrInvalid, wantIn: "not one codex takes"},
		"a role the rules do not have":      {d: routing.Deployment{Overrides: map[string]string{"qa": "claude_mid"}}, wantErr: routing.ErrUnknownRole, wantIn: "routing.overrides.qa"},
		"a binding the role does not allow": {d: routing.Deployment{Overrides: map[string]string{"clerk": "claude_strong"}}, wantErr: routing.ErrOverrideRefused, wantIn: "routing.overrides.clerk"},
		"a premium binding for the boss":    {d: routing.Deployment{Overrides: map[string]string{"boss": "claude_premium"}}, wantErr: routing.ErrOverrideRefused},
		"no binding at all":                 {d: routing.Deployment{Overrides: map[string]string{"coder": ""}}, wantErr: routing.ErrOverrideRefused},
	}
	for name, tc := range tests {
		_, err := routing.New(routing.V1(), tc.d)
		require.ErrorIs(t, err, tc.wantErr, name)
		assert.Contains(t, err.Error(), tc.wantIn, name)
	}
	_, err := routing.New(routing.V1(), routing.Deployment{Bindings: map[string]routing.Facts{"claude_mid": {ModelID: new("--dangerously-skip")}}})
	require.ErrorIs(t, err, routing.ErrInvalid)
	assert.NotContains(t, err.Error(), "dangerously", "a model id is never echoed, as settings.CheckModel's model is not")
	for name, d := range map[string]routing.Deployment{
		"nothing":                           {},
		"an unresolved id":                  {Bindings: map[string]routing.Facts{"codex_premium": {ModelID: new("")}}},
		"a model id at 128 bytes":           {Bindings: map[string]routing.Facts{"claude_mid": {ModelID: new(strings.Repeat("a", 128))}}},
		"a context suffix":                  {Bindings: map[string]routing.Facts{"claude_strong": {ModelID: new("claude-opus-5-5[1m]")}}},
		"every effort codex takes":          {Bindings: map[string]routing.Facts{"codex_strong": {Efforts: []string{"minimal", "low", "medium", "high", "xhigh"}}}},
		"every effort claude takes":         {Bindings: map[string]routing.Facts{"claude_mid": {Efforts: []string{"low", "medium", "high", "xhigh", "max"}}}},
		"a premium choice for the debugger": {Overrides: map[string]string{"alex_debugger": "codex_premium"}},
	} {
		_, err := routing.New(routing.V1(), d)
		require.NoError(t, err, name)
	}
}

// TestPolicyVersion: the version names the rules' schema and changes with
// the rules and the deployment, and only with them.
func TestPolicyVersion(t *testing.T) {
	t.Parallel()
	base := policy(t, routing.Deployment{}).Version()
	assert.Regexp(t, `^v1-[0-9a-f]{12}$`, base)
	assert.Equal(t, base, policy(t, routing.Deployment{}).Version(), "the same deployment, the same version")
	for name, d := range map[string]routing.Deployment{
		"routing on":      {Enabled: true},
		"a new model id":  {Bindings: map[string]routing.Facts{"claude_mid": {ModelID: new("claude-sonnet-5-6")}}},
		"a binding off":   {Bindings: map[string]routing.Facts{"claude_mid": {Enabled: new(false)}}},
		"a user override": {Overrides: map[string]string{"coder": "claude_strong"}},
	} {
		assert.NotEqual(t, base, policy(t, d).Version(), name)
	}
	raw, err := os.ReadFile(rulesFile)
	require.NoError(t, err)
	edited := bytes.Replace(raw, []byte("Fable current version is 5.1"), []byte("Fable current version is 5.2"), 1)
	require.NotEqual(t, raw, edited)
	other, err := routing.ParseRules(edited)
	require.NoError(t, err)
	p, err := routing.New(other, routing.Deployment{})
	require.NoError(t, err)
	assert.NotEqual(t, base, p.Version(), "other rules, the same deployment")
}
