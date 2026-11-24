//go:build linux

package dashboard_test

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/abanna/herdr-agentisan/internal/dashboard"
	"github.com/abanna/herdr-agentisan/internal/snapshot"
)

type qa4Run struct {
	master, slave *os.File
	out           *syncBuffer
	done          chan error
	keys          chan string
}

func qa4Start(t *testing.T) *qa4Run {
	t.Helper()
	master, slave := openPTY(t, 120, 40)
	out := &syncBuffer{}
	go func() { _, _ = io.Copy(out, master) }()
	cfg := config(&script{steps: []func() (snapshot.Snapshot, error){serve(fixture(t, "full"))}}, nil)
	cfg.Options = dashboard.Options{}
	env := tea.WithEnvironment([]string{"TERM=xterm-256color", "COLORTERM=truecolor"})
	ctx, cancel := context.WithCancel(t.Context())
	done, exited := make(chan error, 1), make(chan struct{})
	keys := make(chan string, 16)
	filter := tea.WithFilter(func(_ tea.Model, msg tea.Msg) tea.Msg {
		if key, ok := msg.(tea.KeyPressMsg); ok {
			select {
			case keys <- key.String():
			default:
			}
		}
		return msg
	})
	t.Cleanup(func() {
		cancel()
		<-exited
	})
	go func() {
		defer close(exited)
		done <- dashboard.Run(ctx, cfg, slave, slave, env, filter)
	}()
	require.Eventually(t, func() bool { return strings.Contains(out.String(), "38;2;238;242;255") }, 5*time.Second, 5*time.Millisecond)
	return &qa4Run{master, slave, out, done, keys}
}

func (r *qa4Run) send(t *testing.T, s string) {
	t.Helper()
	_, err := r.master.WriteString(s)
	require.NoError(t, err)
}

func (r *qa4Run) quits(t *testing.T, within time.Duration) time.Duration {
	t.Helper()
	start := time.Now()
	select {
	case err := <-r.done:
		require.NoError(t, err)
		return time.Since(start)
	case <-time.After(within):
		t.Fatalf("did not quit within %v", within)
		return 0
	}
}

// latency of the first new output after a key, as the user sees a keystroke.
func (r *qa4Run) latency(t *testing.T, key, decoded string) time.Duration {
	t.Helper()
	before := len(r.out.String())
	start := time.Now()
	r.send(t, key)
	select {
	case got := <-r.keys:
		require.Equal(t, decoded, got, "decoded key")
	case <-time.After(3 * time.Second):
		t.Fatalf("key %q was not decoded", key)
	}
	require.Eventually(t, func() bool { return len(r.out.String()) > before }, 3*time.Second, time.Millisecond, "no redraw after %q", key)
	return time.Since(start)
}

func TestQA4PTYBareEscapeAndAltLatency(t *testing.T) {
	r := qa4Start(t)
	help := r.latency(t, "?", "?")
	esc := r.latency(t, "\x1b", "esc") // closes help
	t.Logf("LATENCY ? %v | bare Esc %v", help, esc)
	require.Less(t, esc, 500*time.Millisecond)
	r.send(t, "q")
	r.quits(t, 5*time.Second)
}

func TestQA4PTYNeverEndingReplyThenTyping(t *testing.T) {
	r := qa4Start(t)
	r.send(t, "\x1b]11;rgb:1e1e/1e1e/")
	time.Sleep(300 * time.Millisecond)
	r.send(t, "q")
	r.quits(t, 2*time.Second)
}

func TestQA4PTYSplitIntroducerThenInput(t *testing.T) {
	r := qa4Start(t)
	r.send(t, "\x1b]")
	time.Sleep(40 * time.Millisecond)
	r.send(t, "11;rgb:00ee/")
	time.Sleep(40 * time.Millisecond)
	r.send(t, "00ee/00ee\x1b")
	time.Sleep(40 * time.Millisecond)
	r.send(t, "\\")
	time.Sleep(100 * time.Millisecond)
	r.send(t, "q")
	r.quits(t, 5*time.Second)
}

func TestQA4PTYRawModeAndHandBack(t *testing.T) {
	r := qa4Start(t)
	tio, err := unix.IoctlGetTermios(int(r.slave.Fd()), unix.TCGETS)
	require.NoError(t, err)
	t.Logf("RAW while running: ICANON=%v ECHO=%v", tio.Lflag&unix.ICANON != 0, tio.Lflag&unix.ECHO != 0)
	require.Zero(t, tio.Lflag&unix.ICANON, "raw while running")
	require.Zero(t, tio.Lflag&unix.ECHO)
	r.send(t, "q")
	took := r.quits(t, 5*time.Second)
	tio, err = unix.IoctlGetTermios(int(r.slave.Fd()), unix.TCGETS)
	require.NoError(t, err)
	t.Logf("RESTORED after exit (%v): ICANON=%v ECHO=%v", took, tio.Lflag&unix.ICANON != 0, tio.Lflag&unix.ECHO != 0)
	require.NotZero(t, tio.Lflag&unix.ICANON, "cooked again after exit")
	require.NotZero(t, tio.Lflag&unix.ECHO)

	// A leaked reader goroutine would steal the next typed bytes.
	r.send(t, "hand\n")
	time.Sleep(200 * time.Millisecond)
	fds := []unix.PollFd{{Fd: int32(r.slave.Fd()), Events: unix.POLLIN}} //nolint:gosec
	n, err := unix.Poll(fds, 500)
	require.NoError(t, err)
	t.Logf("HANDBACK readable after exit: %v", n == 1)
	require.Equal(t, 1, n, "typed bytes must stay in the terminal after exit")
	buf := make([]byte, 16)
	m, err := r.slave.Read(buf)
	require.NoError(t, err)
	require.Equal(t, "hand\n", string(buf[:m]))
}
