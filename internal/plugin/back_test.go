package plugin_test

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/daemon"
	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/plugin"
)

// toasts records the notifications Back asks herdr to show.
type toasts struct {
	got []herdr.Notification
	err error
}

func (n *toasts) ShowNotification(_ context.Context, x herdr.Notification) (herdr.NotificationResult, error) {
	n.got = append(n.got, x)
	return herdr.NotificationResult{Shown: n.err == nil}, n.err
}

// TestBackTellsTheUserWhyNothingMoved: a key press must not silently do
// nothing. Run as a herdr action, a Back that went nowhere shows a toast
// saying why; by hand, the command prints the error. Either way the error,
// with its sentinel, is returned.
func TestBackTellsTheUserWhyNothingMoved(t *testing.T) {
	t.Parallel()
	down := fmt.Errorf("%w: dial unix: no such file or directory", daemon.ErrUnavailable)
	none := &daemon.RequestError{Code: "no_history", Message: "no earlier pane to go back to"}
	herdrFailed := &daemon.RequestError{Code: "herdr_failed", Message: "herdr did not complete the move: focus w1:p2: pane_not_found"}
	stateFailed := &daemon.RequestError{Code: "state_failed", Message: "back: database is locked"}

	tests := map[string]struct {
		action   string
		err      error
		toastErr error
		is       error  // the sentinel the returned error matches
		toast    string // in the toast body; "" for no toast
	}{
		"went back, as an action":          {action: "back"},
		"went back, by hand":               {},
		"daemon not running, as an action": {action: "back", err: down, is: daemon.ErrUnavailable, toast: "daemon is not running"},
		"daemon not running, by hand":      {err: down, is: daemon.ErrUnavailable},
		"no history, as an action":         {action: "back", err: none, is: daemon.ErrNoHistory, toast: "No earlier pane"},
		"no history, by hand":              {err: none, is: daemon.ErrNoHistory},
		"herdr failed, as an action":       {action: "back", err: herdrFailed, is: daemon.ErrHerdrCall, toast: "pane_not_found"},
		"state failed, as an action":       {action: "back", err: stateFailed, is: daemon.ErrRequest, toast: "database is locked"},
		"the toast fails too":              {action: "back", err: none, toastErr: herdr.ErrUnavailable, is: herdr.ErrUnavailable, toast: "No earlier pane"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			want := daemon.BackResult{To: "w1:p1", From: "w1:p2"}
			n := &toasts{err: tc.toastErr}
			goBack := func(context.Context) (daemon.BackResult, error) {
				if tc.err != nil {
					return daemon.BackResult{}, tc.err
				}
				return want, nil
			}

			got, err := plugin.Back(t.Context(), plugin.Env{ActionID: tc.action}, goBack, n)
			if tc.err == nil {
				require.NoError(t, err)
				assert.Equal(t, want, got)
				assert.Empty(t, n.got, "a Back that moved shows no toast")
				return
			}
			require.ErrorIs(t, err, tc.is)
			require.ErrorIs(t, err, tc.err, "the cause is kept")
			if tc.toast == "" {
				assert.Empty(t, n.got, "by hand, the error is printed instead")
				return
			}
			require.Len(t, n.got, 1)
			assert.Equal(t, plugin.BackTitle, n.got[0].Title)
			assert.Contains(t, n.got[0].Body, tc.toast)
		})
	}
}

// TestKeySnippetBindsBack keeps the documented [[keys.command]] (sample and
// README) from rotting. It decodes the sample strictly (keySnippet), and
// names the action by its qualified id, plugin and action joined by a dot
// (src/app/api/plugins/mod.rs:640-643).
func TestKeySnippetBindsBack(t *testing.T) {
	t.Parallel()
	sample, cfg := readKeySnippet(t)
	require.Len(t, cfg.Keys.Command, 1)
	bind := cfg.Keys.Command[0]

	m, err := plugin.LoadManifest(filepath.Join("..", "..", plugin.ManifestFile))
	require.NoError(t, err)
	var action *plugin.Action
	for i := range m.Actions {
		if m.Actions[i].ID == plugin.BackAction {
			action = &m.Actions[i]
		}
	}
	require.NotNil(t, action, "the manifest declares no %q action", plugin.BackAction)
	assert.Equal(t, []string{"bin/herdr-agentisan", "back"}, action.Command)

	assert.Equal(t, "prefix+b", bind.Key)
	assert.Equal(t, "plugin_action", bind.Type)
	assert.Equal(t, m.ID+"."+action.ID, bind.Command)
	assert.NotEmpty(t, bind.Description)
	assert.Nil(t, bind.Width, "popup sizes apply to type popup only")
	assert.Nil(t, bind.Height, "popup sizes apply to type popup only")

	assert.Equal(t, settings(sample), settings(readmeKeys(t)), "the README binds the keys as the sample does")
}

// settings is a TOML text's lines that are neither blank nor comments.
func settings(text string) []string {
	var out []string
	for line := range strings.Lines(text) {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			out = append(out, line)
		}
	}
	return out
}
