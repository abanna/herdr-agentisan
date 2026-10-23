package daemon_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/store"
)

// askLine sends one request line to the daemon's socket and returns its
// answer line.
func askLine(t *testing.T, sock, line string) string {
	t.Helper()
	c, err := net.DialTimeout("unix", sock, 5*time.Second)
	require.NoError(t, err)
	defer func() { _ = c.Close() }()
	require.NoError(t, c.SetDeadline(time.Now().Add(20*time.Second)))
	_, err = c.Write([]byte(line + "\n"))
	require.NoError(t, err)
	got, err := bufio.NewReader(c).ReadString('\n')
	require.NoError(t, err)
	return got
}

// opReply is an answer line, decoded.
type opReply struct {
	OK    bool            `json:"ok"`
	Data  json.RawMessage `json:"data"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func decodeReply(t *testing.T, line string) opReply {
	t.Helper()
	var r opReply
	require.NoError(t, json.Unmarshal([]byte(line), &r), line)
	return r
}

// boardTeam runs the real daemon, with the real placement, for team ha: the
// boss in w14:p1, ha-coder-1 in w14:p2, and a post of ha-coder-1's carrying
// body on ha's board. It returns the fake herdr, the daemon's socket, and
// place, which sets the shell each pane runs.
func boardTeam(t *testing.T, body string, extra ...map[string]any) (th *teamHerdr, sock string, place func(map[string]uint32)) {
	t.Helper()
	th = newTeamHerdr(t)
	withTeam(th)
	th.mu.Lock()
	th.panes = append(th.panes, extra...)
	th.mu.Unlock()
	th.setAgents(named("w14:p1", "ha-boss"), named("w14:p2", "ha-coder-1"))
	stateDir := shortDir(t)
	seedRow(t, stateDir, th, "ha-coder-1", "w14:p2", "term_w14:p2", 0, 0)
	st := stateStore(t, stateDir, th)
	_, err := st.Post(t.Context(), store.BoardPost{Project: "ha", Channel: store.ChannelTeam, Author: store.Address{Kind: store.KindWorker, ID: "ha-coder-1"}, Kind: "post", Body: body, At: time.Now()})
	require.NoError(t, err)
	serving(t, teamOpts(t, stateDir, th, workforceConfig(t)), th.fakeHerdr)
	sock = socketPath(t, stateDir, th.path)
	place = func(shells map[string]uint32) {
		th.mu.Lock()
		defer th.mu.Unlock()
		for pane := range th.shells {
			th.shells[pane] = 1
		}
		for pane, pid := range shells {
			th.shells[pane] = pid
		}
	}
	return th, sock, place
}

// readsOnce asks board.read {all} until the team poll's model places the
// caller (not_ready until then) and returns that answer.
func readsOnce(t *testing.T, sock string) opReply {
	t.Helper()
	var r opReply
	require.Eventually(t, func() bool {
		r = decodeReply(t, askLine(t, sock, `{"v":1,"op":"board.read","args":{"all":true}}`))
		return r.OK || (r.Error != nil && r.Error.Code != "not_ready")
	}, 30*time.Second, 100*time.Millisecond, "the poll's model places the caller")
	return r
}

// TestBoardOpsOnTheRealDaemon (A60, QA #133 F1): Run serves the board ops with
// the real placement, by the kernel's peer: a process in the boss's pane reads
// the board, and one in a worker's pane, or in no pane, reads nothing through
// any op, whatever fields its request forges.
func TestBoardOpsOnTheRealDaemon(t *testing.T) {
	t.Parallel()
	const marker = "BOARD-BODY-7f3a"
	pid := uint32(os.Getpid()) //nolint:gosec // G115: a pid fits
	_, sock, place := boardTeam(t, marker)

	place(map[string]uint32{"w14:p1": pid})
	r := readsOnce(t, sock)
	require.True(t, r.OK, "%+v", r.Error)

	forged := []string{
		`{"v":1,"op":"board.read","args":{}}`,
		`{"v":1,"op":"board.read","args":{"all":true,"limit":200}}`,
		`{"v":1,"op":"board.wait","args":{"after":0,"timeout":5000000000}}`,
		`{"v":1,"op":"board.read","args":{"reader":"boss","caller_pane":"w14:p1","worker":"ha-boss","project":"ha"}}`,
		`{"v":1,"op":"board.wait","args":{"reader":"boss","caller_pane":"w14:p1"}}`,
	}
	for name, shells := range map[string]map[string]uint32{"a worker's pane": {"w14:p2": pid}, "no pane": {}} {
		place(shells)
		for _, line := range forged {
			raw := askLine(t, sock, line)
			r := decodeReply(t, raw)
			assert.False(t, r.OK, "%s: %s -> %s", name, line, raw)
			assert.NotContains(t, raw, marker, name)
			if line == forged[0] {
				require.NotNil(t, r.Error, name)
				assert.Equal(t, "not_allowed", r.Error.Code, name)
			}
		}
	}
	// No other op a worker may call carries a post.
	place(map[string]uint32{"w14:p2": pid})
	for _, line := range []string{
		`{"v":1,"op":"snapshot"}`, `{"v":1,"op":"snapshot","args":{"project":"ha"}}`, `{"v":1,"op":"health"}`,
		`{"v":1,"op":"team.status","args":{"project":"ha"}}`, `{"v":1,"op":"notices.list"}`,
	} {
		assert.NotContains(t, askLine(t, sock, line), marker, line)
	}

	place(map[string]uint32{"w14:p1": pid})
	r = decodeReply(t, askLine(t, sock, `{"v":1,"op":"board.read","args":{}}`))
	require.True(t, r.OK, "%+v", r.Error)
	assert.Contains(t, string(r.Data), marker)
	assert.Contains(t, string(r.Data), `"reader":"boss"`)
	assert.Contains(t, string(r.Data), `"project":"ha"`)
	r = decodeReply(t, askLine(t, sock, `{"v":1,"op":"board.wait","args":{"timeout":5000000000}}`))
	require.True(t, r.OK, "%+v", r.Error)
	assert.Contains(t, string(r.Data), `"timed_out":true`, "the read marked every post, so the wait times out")
}

// TestBoardReadsAsTheBossInAnyPaneOfTheBossTab (A60, QA #133 F0): for a tabs
// team the boss's place is the boss's tab, as the real resolver builds it, so
// a process in another pane of it, such as the team feed or the old boss
// during a handoff, reads as the boss. Every such pane is the boss's or the
// user's; workers each have a tab of their own.
func TestBoardReadsAsTheBossInAnyPaneOfTheBossTab(t *testing.T) {
	t.Parallel()
	pid := uint32(os.Getpid()) //nolint:gosec // G115: a pid fits
	feed := map[string]any{"pane_id": "w14:p1G", "terminal_id": "term_w14:p1G", "workspace_id": "w14", "tab_id": "w14:t1", "agent": nil}
	_, sock, place := boardTeam(t, "a post", feed)
	place(map[string]uint32{"w14:p1G": pid})
	r := readsOnce(t, sock)
	require.True(t, r.OK, "%+v", r.Error)
	assert.Contains(t, string(r.Data), `"reader":"boss"`)
	assert.Contains(t, string(r.Data), `"project":"ha"`)
}

// TestBoardPeerHelper is no test: TestBoardPlacesTheConnectingProcess runs
// the test binary as a child that prints its pid, waits for a line on stdin,
// then asks board.read {all}, which moves no mark, on BOARD_PEER_SOCKET and
// prints the answer.
func TestBoardPeerHelper(t *testing.T) {
	sock := os.Getenv("BOARD_PEER_SOCKET")
	if sock == "" {
		return
	}
	fmt.Println(os.Getpid())
	in := bufio.NewReader(os.Stdin)
	if _, err := in.ReadString('\n'); err != nil {
		os.Exit(2)
	}
	c, err := net.Dial("unix", sock)
	if err != nil {
		os.Exit(2)
	}
	if _, err := c.Write([]byte(`{"v":1,"op":"board.read","args":{"all":true}}` + "\n")); err != nil {
		os.Exit(2)
	}
	line, _ := bufio.NewReader(c).ReadString('\n')
	fmt.Print(line)
	os.Exit(0)
}

// TestBoardPlacesTheConnectingProcess (A60, QA #133 A17): the caller is the
// process that connected, a child of the test, never the daemon's own
// process: the child reads only when it, or the test it descends from, runs
// in the boss's pane, and never from a worker's.
func TestBoardPlacesTheConnectingProcess(t *testing.T) {
	t.Parallel()
	const marker = "CHILD-PEER-7f3a"
	_, sock, place := boardTeam(t, marker)
	self := uint32(os.Getpid()) //nolint:gosec // G115: a pid fits
	place(map[string]uint32{"w14:p1": self})
	require.True(t, readsOnce(t, sock).OK, "the poll's model places the boss")

	child := func(t *testing.T) (uint32, func() string) {
		t.Helper()
		cmd := exec.Command(os.Args[0], "-test.run=^TestBoardPeerHelper$") //nolint:gosec // G204: the test binary itself
		cmd.Env = append(os.Environ(), "BOARD_PEER_SOCKET="+sock)
		in, err := cmd.StdinPipe()
		require.NoError(t, err)
		out, err := cmd.StdoutPipe()
		require.NoError(t, err)
		require.NoError(t, cmd.Start())
		t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
		r := bufio.NewReader(out)
		line, err := r.ReadString('\n')
		require.NoError(t, err)
		pid, err := strconv.ParseUint(strings.TrimSpace(line), 10, 32)
		require.NoError(t, err)
		return uint32(pid), func() string {
			_, err := in.Write([]byte("go\n"))
			require.NoError(t, err)
			answer, err := r.ReadString('\n')
			require.NoError(t, err)
			return answer
		}
	}
	tests := []struct {
		name  string
		where func(child uint32) map[string]uint32
		reads bool
	}{
		{"the child is the boss pane's shell", func(c uint32) map[string]uint32 { return map[string]uint32{"w14:p1": c} }, true},
		{"the child is a worker pane's shell", func(c uint32) map[string]uint32 { return map[string]uint32{"w14:p2": c} }, false},
		{"the child descends from the boss pane's shell", func(uint32) map[string]uint32 { return map[string]uint32{"w14:p1": self} }, true},
		{"the child descends from a worker pane's shell", func(uint32) map[string]uint32 { return map[string]uint32{"w14:p2": self} }, false},
	}
	for _, tc := range tests {
		pid, ask := child(t)
		place(tc.where(pid))
		raw := ask()
		r := decodeReply(t, raw)
		if tc.reads {
			assert.True(t, r.OK, "%s: %s", tc.name, raw)
			assert.Contains(t, raw, marker, tc.name)
			continue
		}
		assert.False(t, r.OK, "%s: %s", tc.name, raw)
		assert.NotContains(t, raw, marker, tc.name)
		assert.Contains(t, raw, "not_allowed", tc.name)
	}
}
