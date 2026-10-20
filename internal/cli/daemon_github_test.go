package cli_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/cli"
	"github.com/abanna/herdr-agentisan/internal/daemon"
	"github.com/abanna/herdr-agentisan/internal/github/ghtest"
	"github.com/abanna/herdr-agentisan/internal/settings"
)

// TestDaemonRunFindsGhOnItsPATH: `daemon run` finds gh on the PATH herdr
// gave it, never this process's, and collects every configured repo with
// it (ADR-001 A29); with no gh there, or no PATH, it logs that and collects
// nothing. gh
// inherits the daemon's environment, so the test strips every GH_* and
// GITHUB_* variable from its own first, and the stand-in never sees a real
// token.
func TestDaemonRunFindsGhOnItsPATH(t *testing.T) {
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "GH_") || strings.HasPrefix(k, "GITHUB_") {
			t.Setenv(k, "")
			require.NoError(t, os.Unsetenv(k))
		}
	}
	f := ghtest.New(t)
	t.Setenv("FAKE_GH_DIR", f.Dir)
	cfg := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(cfg, settings.FileName),
		[]byte("schema_version = 1\n[projects.app]\nrepo = \"/x\"\ngithub = \"acme/app\"\n"), 0o600))

	for name, path := range map[string]string{"gh on its PATH": filepath.Dir(f.Bin), "no gh on its PATH": t.TempDir(), "no PATH": "-"} {
		t.Run(name, func(t *testing.T) {
			e := newDaemonEnv(t)
			var logs bytes.Buffer
			ctx, cancel := context.WithCancel(daemonCtx(t, e, nil, &logs))
			defer cancel()
			env := map[string]string{"HERDR_PLUGIN_CONFIG_DIR": cfg, "PATH": path}
			if path == "-" {
				delete(env, "PATH")
			}
			ctx = cli.WithLookupEnv(ctx, e.lookup(env))
			done := make(chan error, 1)
			go func() {
				_, err := runCtx(ctx, "daemon", "run")
				done <- err
			}()
			paths, err := daemon.PathsFor(e.stateDir, e.herdrSock)
			require.NoError(t, err)
			log := func() string {
				raw, _ := os.ReadFile(paths.Log)
				return string(raw)
			}
			if name == "gh on its PATH" {
				require.Eventually(t, func() bool { return slices.Contains(f.Calls(t), "api") }, ready, 10*time.Millisecond)
				assert.Contains(t, f.Args(t, "pr"), "--repo=acme/app")
				assert.Contains(t, log(), "github: collecting")
			} else {
				require.Eventually(t, func() bool { return strings.Contains(log(), "gh is not on the daemon's PATH") }, ready, 10*time.Millisecond)
			}
			cancel()
			select {
			case err := <-done:
				require.NoError(t, err)
			case <-time.After(ready):
				t.Fatal("daemon run did not stop")
			}
		})
	}
	assert.Len(t, f.Calls(t), 2, "only the run with gh on its PATH ran it, once per call")
}
