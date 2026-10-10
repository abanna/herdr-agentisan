package settings_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/settings"
)

func TestParseSecretRef(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		value any
		want  settings.SecretRef
		ok    bool
	}{
		"env name":                    {value: map[string]any{"env": "GITHUB_TOKEN"}, want: settings.SecretRef{Env: "GITHUB_TOKEN"}, ok: true},
		"env name lower, underscore":  {value: map[string]any{"env": "_my_token_2"}, want: settings.SecretRef{Env: "_my_token_2"}, ok: true},
		"env name starts with digit":  {value: map[string]any{"env": "2FA"}},
		"env name with a dash":        {value: map[string]any{"env": "MY-TOKEN"}},
		"env name empty":              {value: map[string]any{"env": ""}},
		"env name with a space":       {value: map[string]any{"env": "MY TOKEN"}},
		"env name with non-ASCII":     {value: map[string]any{"env": "TOKÉN"}},
		"env name with a NUL":         {value: map[string]any{"env": "TOKEN\x00"}},
		"env name like an option":     {value: map[string]any{"env": "-TOKEN"}},
		"op vault/item/field":         {value: map[string]any{"op": "op://Vault/Item/field"}, want: settings.SecretRef{Op: "op://Vault/Item/field"}, ok: true},
		"op with a section":           {value: map[string]any{"op": "op://Vault/Item/section/field"}, want: settings.SecretRef{Op: "op://Vault/Item/section/field"}, ok: true},
		"op names with spaces":        {value: map[string]any{"op": "op://Private Vault/Linear API/credential"}, want: settings.SecretRef{Op: "op://Private Vault/Linear API/credential"}, ok: true},
		"op names with non-ASCII":     {value: map[string]any{"op": "op://Coffre/Café/mot"}, want: settings.SecretRef{Op: "op://Coffre/Café/mot"}, ok: true},
		"op with a NUL":               {value: map[string]any{"op": "op://Vault/Item/field\x00"}},
		"op with a newline":           {value: map[string]any{"op": "op://Vault/Item/field\nmore"}},
		"op with a tab":               {value: map[string]any{"op": "op://Vault/Item\t/field"}},
		"op with two segments":        {value: map[string]any{"op": "op://Vault/Item"}},
		"op with an empty segment":    {value: map[string]any{"op": "op://Vault//field"}},
		"op with a trailing slash":    {value: map[string]any{"op": "op://Vault/Item/field/"}},
		"op with no path":             {value: map[string]any{"op": "op://"}},
		"op with another scheme":      {value: map[string]any{"op": "https://Vault/Item/field"}},
		"op without a scheme":         {value: map[string]any{"op": "Vault/Item/field"}},
		"op of the wrong type":        {value: map[string]any{"op": int64(1)}},
		"env of the wrong type":       {value: map[string]any{"env": true}},
		"an extra key":                {value: map[string]any{"env": "GITHUB_TOKEN", "value": plainSecret}},
		"both keys at once":           {value: map[string]any{"env": "GITHUB_TOKEN", "op": "op://Vault/Item/field"}},
		"neither key":                 {value: map[string]any{}},
		"one key the ref doesn't use": {value: map[string]any{"value": plainSecret}},
		"a plain string":              {value: plainSecret},
		"a number":                    {value: int64(1234)},
		"an array":                    {value: []any{plainSecret}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := settings.ParseSecretRef(tc.value)
			if !tc.ok {
				require.ErrorIs(t, err, settings.ErrSecretValue)
				assert.NotContains(t, err.Error(), plainSecret, "an error must never echo a secret")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestSecretRefString(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "env:GITHUB_TOKEN", settings.SecretRef{Env: "GITHUB_TOKEN"}.String())
	assert.Equal(t, "op://Vault/Item/field", settings.SecretRef{Op: "op://Vault/Item/field"}.String())
	assert.Empty(t, settings.SecretRef{}.String())
}

var errOpFailed = errors.New("op: not signed in")

// fakeOp answers op:// references from a map, failing for any other.
func fakeOp(values map[string]string) func(context.Context, string) (string, error) {
	return func(_ context.Context, ref string) (string, error) {
		v, ok := values[ref]
		if !ok {
			return "", errOpFailed
		}
		return v, nil
	}
}

func TestSecretRefResolve(t *testing.T) {
	t.Parallel()

	const ref = "op://Vault/Item/field"
	tests := map[string]struct {
		ref     settings.SecretRef
		env     map[string]string
		op      func(context.Context, string) (string, error)
		want    string
		wantErr []error
	}{
		"env set":           {ref: settings.SecretRef{Env: "TOKEN"}, env: map[string]string{"TOKEN": plainSecret}, want: plainSecret},
		"env unset":         {ref: settings.SecretRef{Env: "TOKEN"}, wantErr: []error{settings.ErrSecretUnavailable}},
		"env empty":         {ref: settings.SecretRef{Env: "TOKEN"}, env: map[string]string{"TOKEN": ""}, wantErr: []error{settings.ErrSecretUnavailable}},
		"op succeeds":       {ref: settings.SecretRef{Op: ref}, op: fakeOp(map[string]string{ref: plainSecret}), want: plainSecret},
		"op fails":          {ref: settings.SecretRef{Op: ref}, op: fakeOp(nil), wantErr: []error{settings.ErrSecretUnavailable, errOpFailed}},
		"op returns empty":  {ref: settings.SecretRef{Op: ref}, op: fakeOp(map[string]string{ref: ""}), wantErr: []error{settings.ErrSecretUnavailable}},
		"no op reader":      {ref: settings.SecretRef{Op: ref}, wantErr: []error{settings.ErrSecretUnavailable}},
		"empty reference":   {ref: settings.SecretRef{}, wantErr: []error{settings.ErrSecretUnavailable}},
		"both kinds of ref": {ref: settings.SecretRef{Env: "TOKEN", Op: ref}, env: map[string]string{"TOKEN": plainSecret}, wantErr: []error{settings.ErrSecretUnavailable}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := tc.ref.Resolve(t.Context(), envOf(tc.env), tc.op)
			if tc.wantErr != nil {
				for _, want := range tc.wantErr {
					require.ErrorIs(t, err, want)
				}
				assert.Empty(t, got)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestCheckSecrets resolves every secret in the resolved tree, reports each
// as set or missing, never carries a value, and fails when any is missing.
func TestCheckSecrets(t *testing.T) {
	t.Parallel()

	resolved := settings.Resolved{
		Values: map[string]any{
			"color": "#123456",
			"integrations": map[string]any{
				"github": map[string]any{"token": settings.SecretRef{Env: "GITHUB_TOKEN"}, "url": "https://github.com"},
				"linear": map[string]any{"token": settings.SecretRef{Op: "op://Vault/Linear/credential"}},
			},
		},
		Sources: map[string]string{
			"color":                     settings.LayerProject,
			"integrations.github.token": settings.LayerShared,
			"integrations.github.url":   settings.LayerShared,
			"integrations.linear.token": settings.LayerProject,
		},
	}
	tests := map[string]struct {
		env         map[string]string
		op          map[string]string
		noSecrets   bool
		wantMissing []string
	}{
		"no secrets at all": {noSecrets: true},
		"all set": {
			env: map[string]string{"GITHUB_TOKEN": plainSecret},
			op:  map[string]string{"op://Vault/Linear/credential": plainSecret},
		},
		"env missing": {
			op:          map[string]string{"op://Vault/Linear/credential": plainSecret},
			wantMissing: []string{"integrations.github.token"},
		},
		"both missing": {
			wantMissing: []string{"integrations.github.token", "integrations.linear.token"},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if tc.noSecrets {
				got, err := settings.CheckSecrets(t.Context(), settings.Resolved{
					Values:  map[string]any{"color": "#123456"},
					Sources: map[string]string{"color": settings.LayerShared},
				}, envOf(tc.env), fakeOp(tc.op))
				require.NoError(t, err)
				assert.Empty(t, got)
				return
			}
			got, err := settings.CheckSecrets(t.Context(), resolved, envOf(tc.env), fakeOp(tc.op))

			require.Len(t, got, 2)
			assert.Equal(t, "integrations.github.token", got[0].Path)
			assert.Equal(t, settings.SecretRef{Env: "GITHUB_TOKEN"}, got[0].Ref)
			assert.Equal(t, "integrations.linear.token", got[1].Path)
			var missing []string
			for _, s := range got {
				if s.Err != nil {
					require.ErrorIs(t, s.Err, settings.ErrSecretUnavailable)
					assert.NotContains(t, s.Err.Error(), plainSecret)
					missing = append(missing, s.Path)
				}
			}
			assert.Equal(t, tc.wantMissing, missing)
			if tc.wantMissing == nil {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, settings.ErrSecretUnavailable)
		})
	}
}
