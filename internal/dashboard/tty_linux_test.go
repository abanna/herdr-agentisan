//go:build linux

package dashboard_test

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/abanna/herdr-agentisan/internal/dashboard"
	"github.com/abanna/herdr-agentisan/internal/snapshot"
)

// openPTY opens a pseudo-terminal sized cols x rows and returns its master
// (the test's side) and its slave (the program's terminal).
func openPTY(t *testing.T, cols, rows uint16) (*os.File, *os.File) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("no pseudo-terminals here: %v", err)
	}
	t.Cleanup(func() { _ = master.Close() })
	mfd := int(master.Fd()) //nolint:gosec // a file descriptor fits an int
	require.NoError(t, unix.IoctlSetPointerInt(mfd, unix.TIOCSPTLCK, 0))
	n, err := unix.IoctlGetInt(mfd, unix.TIOCGPTN)
	require.NoError(t, err)
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = slave.Close() })
	require.NoError(t, unix.IoctlSetWinsize(int(slave.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Col: cols, Row: rows})) //nolint:gosec // a file descriptor fits an int
	return master, slave
}

// TestRunOnATerminal runs the program on a real terminal, as herdr's pane
// gives it one: it takes the terminal's size (not the 80x24 fallback), the
// whole screen and mouse clicks, and quits on q.
func TestRunOnATerminal(t *testing.T) {
	t.Parallel()

	master, slave := openPTY(t, 120, 40)
	var out syncBuffer
	go func() { _, _ = io.Copy(&out, master) }()
	src := &script{steps: []func() (snapshot.Snapshot, error){serve(fixture(t, "full"))}}

	done := make(chan error, 1)
	go func() { done <- dashboard.Run(t.Context(), config(src, &recorder{}), slave, slave) }()
	// pee09's stage is drawn whole only when the terminal is wide enough:
	// 120 columns, read from the terminal itself, not the 80x24 fallback.
	require.Eventually(t, func() bool { return strings.Contains(out.String(), "stale pane id resolver") }, 5*time.Second, 10*time.Millisecond)
	require.Contains(t, out.String(), "\x1b[?1049h", "the alternate screen")
	require.Contains(t, out.String(), "\x1b[?1002h", "mouse cell motion")

	_, err := master.Write([]byte("q"))
	require.NoError(t, err)
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("q did not quit")
	}
}

func TestRunDetectsNoColorSelection(t *testing.T) {
	t.Parallel()

	master, slave := openPTY(t, 120, 40)
	var out syncBuffer
	go func() { _, _ = io.Copy(&out, master) }()
	src := &script{steps: []func() (snapshot.Snapshot, error){serve(fixture(t, "full"))}}
	cfg := config(src, &recorder{})
	cfg.Options = dashboard.Options{}
	env := tea.WithEnvironment([]string{"TERM=xterm-256color", "COLORTERM=truecolor", "NO_COLOR=1"})

	done := make(chan error, 1)
	go func() { done <- dashboard.Run(t.Context(), cfg, slave, slave, env) }()
	require.Eventually(t, func() bool {
		return strings.Contains(ansi.Strip(out.String()), "│▸◐ pee01")
	}, 5*time.Second, 10*time.Millisecond, "NO_COLOR should select a pointer with default program options")

	_, err := master.Write([]byte("q"))
	require.NoError(t, err)
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("q did not quit")
	}
}

func TestRunSelectsPaletteFromTerminal(t *testing.T) {
	t.Parallel()

	const darkText = "38;2;238;242;255"
	for _, tc := range []struct {
		name, reply, want string
	}{
		{name: "no answer", want: darkText},
		{name: "dark", reply: "1e1e/1e1e/2e2e", want: darkText},
		{name: "light", reply: "ffff/ffff/ffff", want: "38;2;23;35;59"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			master, slave := openPTY(t, 120, 40)
			var out syncBuffer
			go func() { _, _ = io.Copy(&out, master) }()
			cfg := config(&script{steps: []func() (snapshot.Snapshot, error){serve(fixture(t, "full"))}}, nil)
			cfg.Options = dashboard.Options{}
			env := tea.WithEnvironment([]string{"TERM=xterm-256color", "COLORTERM=truecolor"})
			done := make(chan error, 1)
			go func() { done <- dashboard.Run(t.Context(), cfg, slave, slave, env) }()

			require.Eventually(t, func() bool {
				return strings.Contains(out.String(), darkText)
			}, 5*time.Second, 10*time.Millisecond, "the first frame uses dark before a reply")
			require.Eventually(t, func() bool {
				return strings.Contains(out.String(), ansi.RequestBackgroundColor)
			}, 5*time.Second, 10*time.Millisecond, "request the terminal background")
			if tc.reply != "" {
				_, err := fmt.Fprintf(master, "\x1b]11;rgb:%s\x1b\\", tc.reply)
				require.NoError(t, err)
			}
			require.Eventually(t, func() bool {
				return strings.Contains(out.String(), tc.want)
			}, 5*time.Second, 10*time.Millisecond, "use the palette for the terminal background")
			_, err := master.Write([]byte("q"))
			require.NoError(t, err)
			select {
			case err := <-done:
				require.NoError(t, err)
			case <-time.After(5 * time.Second):
				t.Fatal("q did not quit")
			}
		})
	}
}

func startPaletteTerminal(t *testing.T, env []string, opts ...tea.ProgramOption) (*os.File, *syncBuffer) {
	t.Helper()
	master, slave := openPTY(t, 120, 40)
	var out syncBuffer
	go func() { _, _ = io.Copy(&out, master) }()
	cfg := config(&script{steps: []func() (snapshot.Snapshot, error){serve(fixture(t, "full"))}}, nil)
	cfg.Options = dashboard.Options{}
	ctx, cancel := context.WithCancel(context.WithoutCancel(t.Context()))
	done := make(chan error, 1)
	go func() {
		done <- dashboard.Run(ctx, cfg, slave, slave, append([]tea.ProgramOption{tea.WithEnvironment(env)}, opts...)...)
	}()
	t.Cleanup(func() {
		defer cancel()
		_, err := master.Write([]byte("q"))
		require.NoError(t, err)
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Error("q did not quit")
		}
	})
	return master, &out
}

func waitBackgroundQuery(t *testing.T, out *syncBuffer, n int) {
	t.Helper()
	require.Eventually(t, func() bool {
		return strings.Count(out.String(), ansi.RequestBackgroundColor) >= n
	}, 6*time.Second, 10*time.Millisecond, "background query %d", n)
}

func replyBackground(t *testing.T, master *os.File, rgb string) {
	t.Helper()
	_, err := fmt.Fprintf(master, "\x1b]11;rgb:%s\x1b\\", rgb)
	require.NoError(t, err)
}

func TestRunFollowsBackgroundChanges(t *testing.T) {
	t.Parallel()

	const dark = "3030/3434/4646"
	const light = "efef/f1f1/f5f5"
	master, out := startPaletteTerminal(t, []string{"TERM=xterm-256color", "COLORTERM=truecolor"})
	for i, reply := range []struct{ rgb, text string }{
		{dark, "38;2;238;242;255"},
		{light, "38;2;23;35;59"},
		{dark, "38;2;238;242;255"},
	} {
		waitBackgroundQuery(t, out, i+1)
		start := 0
		if i > 0 {
			start = len(out.String())
		}
		replyBackground(t, master, reply.rgb)
		require.Eventually(t, func() bool {
			return strings.Contains(out.String()[start:], reply.text)
		}, 2*time.Second, 10*time.Millisecond, "palette after reply %d (%s)", i+1, reply.rgb)
	}
}

func TestRunQueriesBackgroundOnlyAtTrueColor(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		env  []string
	}{
		{"TrueColor", []string{"TERM=xterm-256color", "COLORTERM=truecolor"}},
		{"256", []string{"TERM=xterm-256color"}},
		{"16", []string{"TERM=xterm"}},
		{"NO_COLOR", []string{"TERM=xterm-256color", "COLORTERM=truecolor", "NO_COLOR=1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, out := startPaletteTerminal(t, tc.env)
			if tc.name == "TrueColor" {
				waitBackgroundQuery(t, out, 2)
				return
			}
			require.Eventually(t, func() bool { return strings.Contains(out.String(), "pee01") }, 5*time.Second, 10*time.Millisecond)
			require.Never(t, func() bool {
				return strings.Contains(out.String(), ansi.RequestBackgroundColor)
			}, 4*time.Second, 10*time.Millisecond)
		})
	}
}

func TestRunRepeatedBackgroundReplyKeepsFrame(t *testing.T) {
	t.Parallel()

	replies := make(chan struct{}, 2)
	filter := tea.WithFilter(func(_ tea.Model, msg tea.Msg) tea.Msg {
		if _, ok := msg.(tea.BackgroundColorMsg); ok {
			replies <- struct{}{}
		}
		return msg
	})
	master, out := startPaletteTerminal(t, []string{"TERM=xterm-256color", "COLORTERM=truecolor"}, filter)
	waitBackgroundQuery(t, out, 1)
	replyBackground(t, master, "efef/f1f1/f5f5")
	require.Eventually(t, func() bool {
		return strings.Contains(out.String(), "38;2;23;35;59")
	}, 2*time.Second, 10*time.Millisecond, "the first reply takes effect")
	waitBackgroundQuery(t, out, 2)

	before := out.String()
	replyBackground(t, master, "efef/f1f1/f5f5")
	require.Eventually(t, func() bool { return len(replies) == 2 }, 2*time.Second, 10*time.Millisecond, "both replies reach the model")
	require.Never(t, func() bool { return out.String() != before }, 200*time.Millisecond, 10*time.Millisecond, "a repeated reply should emit no redraw")
}
