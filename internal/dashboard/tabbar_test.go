package dashboard_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"math"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/daemon"
	"github.com/abanna/herdr-agentisan/internal/dashboard"
	"github.com/abanna/herdr-agentisan/internal/snapshot"
)


// prs are PRs numbered nums, every one with the states of pr.
func prs(pr snapshot.PR, nums ...int) []snapshot.PR {
	out := make([]snapshot.PR, 0, len(nums))
	for _, n := range nums {
		pr.Number = n
		out = append(out, pr)
	}
	return out
}

var (
	ready   = snapshot.PR{CI: snapshot.CISuccess, Codex: snapshot.CodexApproved, Merge: snapshot.MergeClean}
	pending = snapshot.PR{CI: snapshot.CIPending, Codex: snapshot.CodexPending, Merge: snapshot.MergeBlocked}
)

// TestTabBarLine: herdr-status.sh's line, "PRs … · slots u/t · issues n",
// each PR with the header's one mark (A29), within the 80 characters herdr
// shows (A41). What the snapshot does not report is left out; a zero it
// reports is shown.
func TestTabBarLine(t *testing.T) {
	t.Parallel()

	// The live tab bar's line at 2026-10-20T21:50Z: 81 characters, so herdr
	// showed "issues 1".
	today := &snapshot.Project{
		PRs:   append(prs(snapshot.PR{CI: snapshot.CISuccess, Codex: snapshot.CodexPending, Merge: snapshot.MergeClean}, 928), prs(ready, 982, 984, 988, 990, 992, 993, 994, 995)...),
		Slots: &snapshot.Slots{Used: 0, Total: 2}, Issues: new(13),
	}
	tests := map[string]struct {
		project *snapshot.Project
		want    string
	}{
		"every mark": {
			project: &snapshot.Project{PRs: []snapshot.PR{
				{Number: 1, CI: snapshot.CIFailure, Codex: snapshot.CodexApproved, Merge: snapshot.MergeClean},
				{Number: 2, CI: snapshot.CIPending, Codex: snapshot.CodexApproved, Merge: snapshot.MergeClean},
				{Number: 3, CI: snapshot.CIUnknown, Codex: snapshot.CodexApproved, Merge: snapshot.MergeClean},
				{Number: 4, CI: snapshot.CISuccess, Codex: snapshot.CodexApproved, Merge: snapshot.MergeClean},
				{Number: 5, CI: snapshot.CISuccess, Codex: snapshot.CodexFindings, Merge: snapshot.MergeClean},
				{Number: 6, CI: snapshot.CISuccess, Codex: snapshot.CodexUnknown, Merge: snapshot.MergeClean},
				{Number: 7, CI: snapshot.CISuccess, Codex: snapshot.CodexApproved, Merge: snapshot.MergeBlocked},
				{Number: 8, CI: snapshot.CISuccess, Codex: snapshot.CodexPending, Merge: snapshot.MergeClean},
			}},
			want: "PRs #1✗ #2⏳ #3? #4✓ #5💬 #6? #7… #8…",
		},
		"herdr-status.sh's order":   {project: &snapshot.Project{PRs: prs(ready, 886), Issues: new(14), Slots: &snapshot.Slots{Used: 2, Total: 2}}, want: "PRs #886✓ · slots 2/2 · issues 14"},
		"no PR open and zeros":      {project: &snapshot.Project{PRs: []snapshot.PR{}, Issues: new(0), Slots: &snapshot.Slots{Used: 0, Total: 0}}, want: "PRs none · slots 0/0 · issues 0"},
		"PRs not reported":          {project: &snapshot.Project{Issues: new(3), Slots: &snapshot.Slots{Used: 1, Total: 2}}, want: "slots 1/2 · issues 3"},
		"slots not reported":        {project: &snapshot.Project{PRs: prs(ready, 1), Issues: new(3)}, want: "PRs #1✓ · issues 3"},
		"issues not reported":       {project: &snapshot.Project{PRs: prs(ready, 1), Slots: &snapshot.Slots{Used: 1, Total: 2}}, want: "PRs #1✓ · slots 1/2"},
		"nothing reported":          {project: &snapshot.Project{Color: "#bd93f9"}, want: ""},
		"no project state":          {want: ""},
		"today's line, too long":    {project: today, want: "PRs 9 · slots 0/2 · issues 13"},
		"one character past the 80": {project: &snapshot.Project{PRs: prs(pending, 1001, 1002, 1003, 1004, 1005, 101, 102, 103), Slots: &snapshot.Slots{Used: 0, Total: 2}, Issues: new(130)}, want: "PRs 8 · slots 0/2 · issues 130"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := dashboard.TabBarLine(snapshot.Snapshot{V: 1, Team: "agentisan", Project: tc.project})
			assert.Equal(t, tc.want, got)
			assert.LessOrEqual(t, utf8.RuneCountInString(got), dashboard.TabBarWidth)
		})
	}
}

// TestTabBarLineFillsTheWidth: PRs that fit exactly the 80 characters stay
// one by one. herdr counts characters, not cells, so ⏳, two cells wide,
// counts as one.
func TestTabBarLineFillsTheWidth(t *testing.T) {
	t.Parallel()
	s := snapshot.Snapshot{V: 1, Team: "agentisan", Project: &snapshot.Project{
		PRs:   prs(pending, 1001, 1002, 1003, 1004, 1005, 101, 102, 103),
		Slots: &snapshot.Slots{Used: 0, Total: 2}, Issues: new(13),
	}}
	const want = "PRs #1001⏳ #1002⏳ #1003⏳ #1004⏳ #1005⏳ #101⏳ #102⏳ #103⏳ · slots 0/2 · issues 13"
	require.Equal(t, dashboard.TabBarWidth, utf8.RuneCountInString(want), "the case sits on the edge")
	assert.Equal(t, want, dashboard.TabBarLine(s))
}

// TestTabBarLineIsCutAtTheWidth: a count line still too long is cut to the
// 80 characters, ending in "…", where herdr would cut it silently.
func TestTabBarLineIsCutAtTheWidth(t *testing.T) {
	t.Parallel()
	s := snapshot.Snapshot{V: 1, Team: "agentisan", Project: &snapshot.Project{
		PRs:   prs(ready, 1),
		Slots: &snapshot.Slots{Used: math.MaxInt, Total: math.MaxInt}, Issues: new(math.MaxInt),
	}}
	got := dashboard.TabBarLine(s)
	assert.Equal(t, dashboard.TabBarWidth, utf8.RuneCountInString(got))
	assert.True(t, strings.HasPrefix(got, "PRs 1 · slots 9223372036854775807/"), got)
	assert.True(t, strings.HasSuffix(got, "…"), got)
}

// listenAt is a unix listener at a fresh daemon socket path.
func listenAt(t *testing.T) (string, net.Listener) {
	t.Helper()
	sock := filepath.Join(shortDir(t), "daemon.sock")
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	return sock, ln
}

// answering is a daemon at a fresh socket that answers every request with
// reply, and passes on each request line.
func answering(t *testing.T, reply string) (string, <-chan string) {
	t.Helper()
	sock, ln := listenAt(t)
	got := make(chan string, 4)
	go func() {
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
	return sock, got
}

// TestTabBar: the line for each answer the daemon can give, asked for the
// team the selector names (A41). A workspace that is no team's is an empty
// line; a daemon that gave no snapshot is one short line saying why.
func TestTabBar(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("testdata", "full.json"))
	require.NoError(t, err)
	var full bytes.Buffer
	require.NoError(t, json.Compact(&full, raw))
	refusal := func(code string) string {
		return `{"ok":false,"error":{"code":"` + code + `","message":"m"}}`
	}
	tests := map[string]struct {
		reply string
		want  string
	}{
		"a team":                              {reply: `{"ok":true,"data":` + full.String() + `}`, want: "PRs #878⏳ #882💬 #884✗ #886✓ #887… · slots 2/2 · issues 14"},
		"no team there":                       {reply: refusal("no_team"), want: ""},
		"before the first poll":               {reply: refusal("not_ready"), want: dashboard.TabBarStarting},
		"a refusal":                           {reply: refusal("timeout"), want: dashboard.TabBarError},
		"not the protocol":                    {reply: `not json`, want: dashboard.TabBarError},
		"a newer snapshot":                    {reply: `{"ok":true,"data":{"v":2}}`, want: dashboard.TabBarError},
		"a snapshot that breaks the contract": {reply: `{"ok":true,"data":{"v":1,"at":"2026-10-20T10:00:00Z","team":"t","groups":[{"name":""}]}}`, want: dashboard.TabBarError},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			sock, asked := answering(t, tc.reply)
			sel := daemon.Selector{WorkspaceID: "w2"}
			assert.Equal(t, tc.want, dashboard.TabBar(t.Context(), sock, sel))
			var req struct {
				Op   string          `json:"op"`
				Args json.RawMessage `json:"args"`
			}
			require.NoError(t, json.Unmarshal([]byte(<-asked), &req))
			assert.Equal(t, "snapshot", req.Op)
			assert.JSONEq(t, `{"workspace_id":"w2"}`, string(req.Args), "the team the selector names")
		})
	}
}

// TestTabBarWithNoDaemon: no socket, a socket nobody listens on, something
// else at the socket's path, and a socket the command may not reach are all
// a daemon that is down.
func TestTabBarWithNoDaemon(t *testing.T) {
	t.Parallel()
	down := func(t *testing.T, sock string) {
		t.Helper()
		assert.Equal(t, dashboard.TabBarDown, dashboard.TabBar(t.Context(), sock, daemon.Selector{}))
	}
	t.Run("no socket", func(t *testing.T) {
		t.Parallel()
		down(t, filepath.Join(shortDir(t), "daemon.sock"))
	})
	t.Run("nobody listening", func(t *testing.T) {
		t.Parallel()
		sock, ln := listenAt(t)
		ln.(*net.UnixListener).SetUnlinkOnClose(false)
		require.NoError(t, ln.Close())
		_, err := os.Stat(sock)
		require.NoError(t, err, "the socket file stays")
		down(t, sock)
	})
	t.Run("a directory there", func(t *testing.T) {
		t.Parallel()
		sock := filepath.Join(shortDir(t), "daemon.sock")
		require.NoError(t, os.Mkdir(sock, 0o700))
		down(t, sock)
	})
	t.Run("a file there", func(t *testing.T) {
		t.Parallel()
		sock := filepath.Join(shortDir(t), "daemon.sock")
		require.NoError(t, os.WriteFile(sock, []byte("{}\n"), 0o600))
		down(t, sock)
	})
	t.Run("a socket it may not write", func(t *testing.T) {
		t.Parallel()
		if os.Geteuid() == 0 {
			t.Skip("root connects to any socket")
		}
		sock, _ := answering(t, `{"ok":true,"data":{}}`)
		require.NoError(t, os.Chmod(sock, 0o500))
		down(t, sock)
	})
	t.Run("a directory it cannot search", func(t *testing.T) {
		t.Parallel()
		if os.Geteuid() == 0 {
			t.Skip("root searches any directory")
		}
		sock, _ := answering(t, `{"ok":true,"data":{}}`)
		dir := filepath.Dir(sock)
		require.NoError(t, os.Chmod(dir, 0o000))
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
		down(t, sock)
	})
}

// TestTabBarWithAWedgedDaemon: a daemon that takes the request and never
// answers is "not answering" once ctx's deadline passes, not a wait as long
// as the socket's own 5 s: herdr would kill the command first and blank its
// entry.
func TestTabBarWithAWedgedDaemon(t *testing.T) {
	t.Parallel()
	sock, ln := listenAt(t)
	held := make(chan net.Conn, 1)
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			held <- conn
		}
	}()
	t.Cleanup(func() {
		select {
		case conn := <-held:
			_ = conn.Close()
		default:
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	assert.Equal(t, dashboard.TabBarNotAnswering, dashboard.TabBar(ctx, sock, daemon.Selector{}))
	assert.Less(t, time.Since(start), 2*time.Second, "within herdr's default timeout")

	// A cancel, the command's process told to stop, ends the wait at once
	// too; nobody reads that line.
	ctx, cancel = context.WithCancel(t.Context())
	time.AfterFunc(50*time.Millisecond, cancel)
	start = time.Now()
	assert.Equal(t, dashboard.TabBarDown, dashboard.TabBar(ctx, sock, daemon.Selector{}))
	assert.Less(t, time.Since(start), 2*time.Second, "a cancel does not wait out the socket's 5 s")
}
