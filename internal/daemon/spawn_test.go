package daemon_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/daemon"
	"github.com/abanna/herdr-agentisan/internal/herdr"
)

// helperEnv switches the test binary into a helper process: ExecSpawn
// re-execs it the way `daemon start` execs `daemon run`.
const helperEnv = "DAEMON_TEST_HELPER"

func TestMain(m *testing.M) {
	switch os.Getenv(helperEnv) {
	case "":
		os.Exit(m.Run())
	case "identity":
		helperIdentity()
	case "exit3":
		os.Exit(3)
	case "modes":
		helperModes()
	default:
		fmt.Fprintln(os.Stderr, "unknown helper", os.Getenv(helperEnv))
		os.Exit(2)
	}
}

// helperIdentity writes what the spawned process sees of itself to the file
// DAEMON_TEST_OUT names: pid, session, process group and where its stdio
// points.
func helperIdentity() {
	out := map[string]any{"pid": os.Getpid(), "sid": sessionOf("self"), "pgid": syscall.Getpgrp(), "marker": os.Getenv("DAEMON_TEST_MARKER")}
	for fd := range 3 {
		target, _ := os.Readlink("/proc/self/fd/" + strconv.Itoa(fd))
		out["fd"+strconv.Itoa(fd)] = target
	}
	raw, _ := json.Marshal(out)
	_ = os.WriteFile(os.Getenv("DAEMON_TEST_OUT"), raw, 0o600)
	os.Exit(0)
}

// helperModes runs a daemon under umask 000 against a minimal herdr and
// reports the permissions of everything it created. umask is process-wide,
// which is why this runs in its own process.
func helperModes() {
	syscall.Umask(0)
	root := os.Getenv("DAEMON_TEST_DIR")
	stateDir := filepath.Join(root, "state") // Run creates it
	herdrSock := filepath.Join(root, "h.sock")
	ln, err := net.Listen("unix", herdrSock)
	if err != nil {
		os.Exit(4)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			var req struct {
				ID string `json:"id"`
			}
			_ = json.NewDecoder(conn).Decode(&req)
			_, _ = fmt.Fprintf(conn, `{"id":%q,"result":{"type":"pong","version":"0.9.3","protocol":22}}`+"\n", req.ID)
			_ = conn.Close()
		}
	}()

	p, err := daemon.PathsFor(stateDir, herdrSock)
	if err != nil {
		os.Exit(5)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- daemon.Run(ctx, daemon.Options{
			StateDir: stateDir, HerdrSocket: herdrSock, Herdr: herdr.Client{SocketPath: herdrSock},
			LockWait: time.Second, Poll: 20 * time.Millisecond, Logger: zerolog.Nop(),
		})
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := daemon.Health(ctx, p.Socket); err == nil {
			break
		}
		if time.Now().After(deadline) {
			os.Exit(6)
		}
		time.Sleep(10 * time.Millisecond)
	}
	w, err := daemon.NewLogWriter(p.Log, 100)
	if err != nil {
		os.Exit(7)
	}
	_ = w.Close()

	modes := map[string]string{}
	for name, path := range map[string]string{
		"dir": stateDir, "server dir": p.Dir, "lock": p.Lock, "socket": p.Socket, "log": p.Log,
		"state.db": p.DB, "state.db-wal": p.DB + "-wal", "state.db-shm": p.DB + "-shm",
	} {
		st, err := os.Stat(path)
		if err != nil {
			modes[name] = "missing"
			continue
		}
		modes[name] = fmt.Sprintf("%o", st.Mode().Perm())
	}
	cancel()
	<-done
	raw, _ := json.Marshal(modes)
	_, _ = os.Stdout.Write(raw)
	os.Exit(0)
}

// sessionOf reads a process's session id (field 6 of /proc/<pid>/stat,
// index 3 after the comm field's last ')'), or -1.
func sessionOf(pid string) int {
	raw, err := os.ReadFile("/proc/" + pid + "/stat")
	if err != nil {
		return -1
	}
	fields := strings.Fields(string(raw[strings.LastIndexByte(string(raw), ')')+1:]))
	if len(fields) < 4 {
		return -1
	}
	sid, err := strconv.Atoi(fields[3])
	if err != nil {
		return -1
	}
	return sid
}

func selfExe(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	require.NoError(t, err)
	return exe
}

// TestExecSpawnDetaches: the spawned daemon is its own session leader with no
// terminal and stdio on /dev/null, and inherits the environment: the
// statusline-style background job of `daemon start` must not tie it to the
// shell or pane that started it.
func TestExecSpawnDetaches(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("reads sessions and fds from /proc")
	}
	out := filepath.Join(t.TempDir(), "out.json")
	t.Setenv(helperEnv, "identity")
	t.Setenv("DAEMON_TEST_OUT", out)
	t.Setenv("DAEMON_TEST_MARKER", "inherited")

	pid, err := daemon.ExecSpawn(selfExe(t))(t.Context())
	require.NoError(t, err)
	require.Positive(t, pid)

	var got map[string]any
	require.Eventually(t, func() bool {
		raw, err := os.ReadFile(out)
		return err == nil && json.Unmarshal(raw, &got) == nil
	}, 5*time.Second, 10*time.Millisecond)

	mySid := sessionOf("self")
	require.Positive(t, mySid)
	assert.EqualValues(t, pid, got["pid"])
	assert.EqualValues(t, pid, got["sid"], "setsid: the daemon leads its own session")
	assert.EqualValues(t, pid, got["pgid"])
	assert.NotEqualValues(t, mySid, got["sid"])
	assert.Equal(t, "inherited", got["marker"])
	for fd := range 3 {
		assert.Equal(t, os.DevNull, got["fd"+strconv.Itoa(fd)], "fd %d", fd)
	}
}

// TestExecSpawnFailures: a binary that cannot start is an error before any
// child exists; one that starts and exits non-zero is not ExecSpawn's error
// (Start sees it never answers health).
func TestExecSpawnFailures(t *testing.T) {
	t.Run("missing binary", func(t *testing.T) {
		t.Parallel()
		_, err := daemon.ExecSpawn(filepath.Join(t.TempDir(), "absent"))(t.Context())
		require.Error(t, err)
	})
	t.Run("not executable", func(t *testing.T) {
		t.Parallel()
		p := filepath.Join(t.TempDir(), "plain")
		require.NoError(t, os.WriteFile(p, []byte("#!/bin/sh\n"), 0o600))
		_, err := daemon.ExecSpawn(p)(t.Context())
		require.Error(t, err)
	})
	t.Run("a child that exits non-zero still spawned", func(t *testing.T) {
		t.Setenv(helperEnv, "exit3")
		pid, err := daemon.ExecSpawn(selfExe(t))(t.Context())
		require.NoError(t, err)
		assert.Positive(t, pid)
	})
}

// TestCreatedFilesKeepTheirModesUnderAnyUmask: the daemon creates its state
// dir 0700 and its lock, socket and log 0600, so a permissive umask cannot
// open them to other local users.
func TestCreatedFilesKeepTheirModesUnderAnyUmask(t *testing.T) {
	t.Parallel()
	dir, err := os.MkdirTemp("", "dm")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	cmd := execHelper(t, "modes", "DAEMON_TEST_DIR="+dir)
	raw, err := cmd.Output()
	require.NoError(t, err, "helper failed")
	var modes map[string]string
	require.NoError(t, json.Unmarshal(raw, &modes))
	assert.Equal(t, map[string]string{
		"dir": "700", "server dir": "700", "lock": "600", "socket": "600", "log": "600",
		"state.db": "600", "state.db-wal": "600", "state.db-shm": "600",
	}, modes)
}

func execHelper(t *testing.T, mode string, env ...string) *execCmd {
	t.Helper()
	c := newExecCmd(t.Context(), selfExe(t))
	c.Env = append(append(os.Environ(), helperEnv+"="+mode), env...)
	c.Env = filterEnv(c.Env, "DAEMON_TEST_OUT")
	return c
}

func filterEnv(env []string, drop string) []string {
	out := env[:0:0]
	for _, kv := range env {
		if !strings.HasPrefix(kv, drop+"=") {
			out = append(out, kv)
		}
	}
	return out
}
