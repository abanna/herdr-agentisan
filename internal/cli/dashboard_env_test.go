package cli_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/daemon"
	"github.com/abanna/herdr-agentisan/internal/herdr/herdrtest"
)

// TestDashboardTakesItsHintsFromItsEnvironment: the pane the dashboard asks
// first, and the workspace it falls back on when herdr cannot be asked, come
// from the environment the command was given, never the process's own.
func TestDashboardTakesItsHintsFromItsEnvironment(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(sample)
	require.NoError(t, err)
	var line bytes.Buffer
	require.NoError(t, json.Compact(&line, raw))
	reply := `{"ok":true,"data":` + line.String() + "}\n"

	t.Run("HERDR_PANE_ID is asked first", func(t *testing.T) {
		t.Parallel()
		// w1:p1 is listed first and runs someone else; the dashboard's own
		// pane, w2:p1, is the one the environment names.
		srv := herdrtest.Start(t, func(r herdrtest.Request) herdrtest.Reply {
			switch r.Method {
			case "pane.list":
				return herdrtest.Reply{Result: map[string]any{"type": "pane_list", "panes": []map[string]any{
					{"pane_id": "w1:p1", "workspace_id": "w1", "focused": false},
					{"pane_id": "w2:p1", "workspace_id": "w2", "focused": true},
				}}}
			case "pane.process_info":
				var p struct {
					PaneID string `json:"pane_id"`
				}
				_ = json.Unmarshal(r.Params, &p)
				pid := os.Getpid()
				if p.PaneID == "w1:p1" {
					pid = 11
				}
				return herdrtest.Reply{Result: map[string]any{"type": "pane_process_info", "process_info": map[string]any{
					"pane_id": p.PaneID, "shell_pid": pid, "foreground_process_group_id": pid, "foreground_processes": []any{},
				}}}
			}
			return focusOK(r)
		})
		stateDir, err := os.MkdirTemp("", "cs")
		require.NoError(t, err)
		t.Cleanup(func() { _ = os.RemoveAll(stateDir) })
		paths, err := daemon.PathsFor(stateDir, srv.Path)
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(paths.Dir, 0o700))
		asked := fakeDaemonAt(t, paths.Socket, reply)

		s := startDashboardWith(t, envCtx(t, map[string]string{
			"HERDR_SOCKET_PATH": srv.Path, "HERDR_PLUGIN_STATE_DIR": stateDir,
			"HERDR_PANE_ID": "w2:p1", "HERDR_WORKSPACE_ID": "w7",
		}), "dashboard")
		s.waitFor("pee01")
		s.quit()
		for _, r := range srv.Requests() {
			if r.Method == "pane.process_info" {
				assert.JSONEq(t, `{"pane_id":"w2:p1"}`, string(r.Params), "the environment's pane is asked, and believed, first")
			}
		}
		assert.Contains(t, asked.String(), `"args":{"workspace_id":"w2"}`)
	})

	t.Run("HERDR_WORKSPACE_ID when herdr cannot be asked", func(t *testing.T) {
		t.Parallel()
		stateDir, err := os.MkdirTemp("", "cs")
		require.NoError(t, err)
		t.Cleanup(func() { _ = os.RemoveAll(stateDir) })
		gone := filepath.Join(stateDir, "gone.sock") // no herdr listens here
		paths, err := daemon.PathsFor(stateDir, gone)
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(paths.Dir, 0o700))
		asked := fakeDaemonAt(t, paths.Socket, reply)

		s := startDashboardWith(t, envCtx(t, map[string]string{
			"HERDR_SOCKET_PATH": gone, "HERDR_PLUGIN_STATE_DIR": stateDir,
			"HERDR_PANE_ID": "w2:p1", "HERDR_WORKSPACE_ID": "w7",
		}), "dashboard")
		s.waitFor("pee01")
		s.waitFor("herdr not answering")
		s.quit()
		assert.Contains(t, asked.String(), `"args":{"workspace_id":"w7"}`)
	})
}
