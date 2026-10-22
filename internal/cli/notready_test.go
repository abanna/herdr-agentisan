package cli_test

import (
	"bufio"
	"net"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/daemon"
)

// notReadyDaemon serves e's plugin socket, answering not_ready to the first
// n asks and ok after, and counts the asks.
func notReadyDaemon(t *testing.T, e daemonEnv, n int32) *atomic.Int32 {
	t.Helper()
	p, err := daemon.PathsFor(e.stateDir, e.herdrSock)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(p.Dir, 0o700))
	ln, err := net.Listen("unix", p.Socket)
	require.NoError(t, err)
	var asks atomic.Int32
	done := make(chan struct{})
	t.Cleanup(func() { _ = ln.Close(); <-done })
	go func() {
		defer close(done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = bufio.NewReader(conn).ReadString('\n')
			reply := `{"ok":true,"data":{"project":"ha"}}`
			if asks.Add(1) <= n {
				reply = `{"ok":false,"error":{"code":"not_ready","message":"the team poll has not reconciled the workers rows since the daemon started"}}`
			}
			_, _ = conn.Write([]byte(reply + "\n"))
			_ = conn.Close()
		}
	}()
	return &asks
}

// TestTeamStatusWaitsOutNotReady: `team status` from a plain shell, with no
// HERDR_PANE_ID, against a daemon still before its first reconcile (A35).
// It asks again until the daemon answers. A daemon that never does gets one
// not_ready refusal once the window ends, never before it and not on the
// op's budget, and no ask after it (QA #48, N2b; A52).
func TestTeamStatusWaitsOutNotReady(t *testing.T) {
	t.Parallel()
	t.Run("ready after 5 asks", func(t *testing.T) {
		t.Parallel()
		e := newDaemonEnv(t)
		asks := notReadyDaemon(t, e, 5)
		out, _, err := runOut(t, e.lookup(map[string]string{}), "team", "status", "--project", "ha")
		require.NoError(t, err)
		oneJSONObject(t, out)
		assert.JSONEq(t, `{"ok":true,"data":{"project":"ha"}}`, out)
		assert.Equal(t, int32(6), asks.Load())
	})
	t.Run("never ready", func(t *testing.T) {
		t.Parallel()
		e := newDaemonEnv(t)
		asks := notReadyDaemon(t, e, 1<<30)
		wait := daemon.OpBudget(daemon.OpTeamStatus, []byte(`{"project":"ha"}`))
		type result struct {
			out  string
			err  error
			took time.Duration
		}
		got := make(chan result, 1)
		start := time.Now()
		go func() {
			out, _, err := runOut(t, e.lookup(map[string]string{}), "team", "status", "--project", "ha")
			got <- result{out, err, time.Since(start)}
		}()
		// A client that lost its overall deadline would ask forever: this
		// timer, not the test binary's, is what catches it.
		var r result
		select {
		case r = <-got:
		case <-time.After(wait + 30*time.Second):
			t.Fatalf("team status still asking after %v (op budget %v)", time.Since(start), wait)
		}
		require.Error(t, r.err, "a refusal exits 1")
		v := oneJSONObject(t, r.out)
		assert.Equal(t, false, v["ok"])
		require.IsType(t, map[string]any{}, v["error"])
		assert.Equal(t, "not_ready", v["error"].(map[string]any)["code"])
		assert.GreaterOrEqual(t, r.took, daemon.NotReadyWindow, "asked for the whole window")
		assert.Less(t, r.took, daemon.NotReadyWindow+5*time.Second, "refused at the window's end, not past it by an exchange")
		n := asks.Load()
		time.Sleep(300 * time.Millisecond)
		assert.Equal(t, n, asks.Load(), "no ask after it gave up")
	})
}
