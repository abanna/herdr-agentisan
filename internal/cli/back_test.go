package cli_test

import (
	"bufio"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/cli"
	"github.com/abanna/herdr-agentisan/internal/daemon"
	"github.com/abanna/herdr-agentisan/internal/herdr/herdrtest"
	"github.com/abanna/herdr-agentisan/internal/plugin"
)

// fakeDaemon answers every request on the daemon socket of e's herdr with
// reply, as the daemon's back op would, and passes on each request line.
func fakeDaemon(t *testing.T, e daemonEnv, reply string) <-chan string {
	t.Helper()
	p, err := daemon.PathsFor(e.stateDir, e.herdrSock)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(p.Dir, 0o700))
	ln, err := net.Listen("unix", p.Socket)
	require.NoError(t, err)
	got := make(chan string, 8)
	done := make(chan struct{})
	t.Cleanup(func() {
		_ = ln.Close()
		<-done
	})
	go func() {
		defer close(done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			line, _ := bufio.NewReader(conn).ReadString('\n')
			got <- line
			_, _ = conn.Write([]byte(reply + "\n"))
			_ = conn.Close()
		}
	}()
	return got
}

// errBadArgs marks a command line cobra rejects, with an error of its own.
var errBadArgs = errors.New("bad arguments")

// TestBackCommand: `back` resolves the daemon socket as `daemon health` does,
// sends the back op and renders where it went. When it went nowhere it fails;
// run as a herdr action it also shows a toast saying why.
func TestBackCommand(t *testing.T) {
	t.Parallel()
	const went = `{"ok":true,"data":{"to":"w1:p1","from":"w1:p2"}}`
	const none = `{"ok":false,"error":{"code":"no_history","message":"no earlier pane to go back to"}}`

	tests := map[string]struct {
		reply  string // the daemon's answer; "" for no daemon
		env    map[string]string
		args   []string
		out    string
		is     error
		toasts int
	}{
		"went back":                {reply: went, out: "back to w1:p1 from w1:p2\n"},
		"went back, as JSON":       {reply: went, args: []string{"--json"}, out: "{\n  \"to\": \"w1:p1\",\n  \"from\": \"w1:p2\"\n}\n"},
		"no daemon, by hand":       {is: daemon.ErrUnavailable},
		"no daemon, as an action":  {env: map[string]string{"HERDR_PLUGIN_ACTION_ID": "back"}, is: daemon.ErrUnavailable, toasts: 1},
		"no history, as an action": {reply: none, env: map[string]string{"HERDR_PLUGIN_ACTION_ID": "back"}, is: daemon.ErrNoHistory, toasts: 1},
		"no history, by hand":      {reply: none, is: daemon.ErrNoHistory},
		"state dir from the flag":  {reply: went, env: map[string]string{"HERDR_PLUGIN_STATE_DIR": ""}, args: []string{"--state-dir", "STATE"}, out: "back to w1:p1 from w1:p2\n"},
		"no state dir":             {env: map[string]string{"HERDR_PLUGIN_STATE_DIR": ""}, is: daemon.ErrNoStateDir},
		"no herdr":                 {env: map[string]string{"HERDR_SOCKET_PATH": ""}, is: daemon.ErrHerdrGone},
		"a positional argument":    {reply: went, args: []string{"now"}, is: errBadArgs},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir, err := os.MkdirTemp("", "dc")
			require.NoError(t, err)
			t.Cleanup(func() { _ = os.RemoveAll(dir) })
			e := daemonEnv{herdrSock: filepath.Join(dir, "h.sock"), stateDir: filepath.Join(dir, "state")}
			herdr := herdrtest.StartAt(t, e.herdrSock, toastReply(true, "shown"))
			var sent <-chan string
			if tc.reply != "" {
				sent = fakeDaemon(t, e, tc.reply)
			}
			args := []string{"back"}
			for _, a := range tc.args {
				if a == "STATE" {
					a = e.stateDir
				}
				args = append(args, a)
			}

			out, err := runCtx(cli.WithLookupEnv(t.Context(), e.lookup(tc.env)), args...)
			if tc.is == errBadArgs {
				require.ErrorContains(t, err, "unknown command \"now\"")
				assert.Empty(t, sent, "a rejected command line never reaches the daemon")
			} else if tc.is != nil {
				require.ErrorIs(t, err, tc.is)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tc.out, out)
				require.Len(t, sent, 1)
				assert.JSONEq(t, `{"v":1,"op":"back","args":{}}`, <-sent)
			}
			reqs := herdr.Requests()
			require.Len(t, reqs, tc.toasts)
			for _, r := range reqs {
				assert.Equal(t, "notification.show", r.Method)
				assert.Contains(t, string(r.Params), plugin.BackTitle)
			}
		})
	}
}
