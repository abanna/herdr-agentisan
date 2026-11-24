//go:build linux

package dashboard_test

import (
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

// bytewisePTY forces fragmented input while keeping the PTY's descriptor.
type bytewisePTY struct{ *os.File }

func (pty bytewisePTY) Read(p []byte) (int, error) {
	return pty.File.Read(p[:min(len(p), 1)])
}

func TestRunSelectsPaletteFromTerminal(t *testing.T) {
	t.Parallel()

	const (
		darkText  = "38;2;238;242;255"
		lightText = "38;2;23;35;59"
	)
	for _, tc := range []struct {
		name, reply, want string
		fromLight, split  bool
	}{
		{name: "no answer", want: darkText},
		{name: "dark", reply: "1e1e/1e1e/2e2e", want: darkText},
		{name: "light", reply: "ffff/ffff/ffff", want: lightText},
		{name: "near black with 16-bit channels", reply: "00ee/00ee/00ee", want: darkText, fromLight: true},
		{name: "fragmented 16-bit reply", reply: "00ee/00ee/00ee", want: darkText, fromLight: true, split: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			master, slave := openPTY(t, 120, 40)
			var out syncBuffer
			go func() { _, _ = io.Copy(&out, master) }()
			cfg := config(&script{steps: []func() (snapshot.Snapshot, error){serve(fixture(t, "full"))}}, nil)
			cfg.Options = dashboard.Options{}
			env := tea.WithEnvironment([]string{"TERM=xterm-256color", "COLORTERM=truecolor"})
			var input io.Reader = slave
			if tc.split {
				input = bytewisePTY{slave}
			}
			done := make(chan error, 1)
			go func() { done <- dashboard.Run(t.Context(), cfg, input, slave, env) }()

			require.Eventually(t, func() bool {
				return strings.Contains(out.String(), darkText)
			}, 5*time.Second, 10*time.Millisecond, "the first frame uses dark before a reply")
			require.Eventually(t, func() bool {
				return strings.Contains(out.String(), ansi.RequestBackgroundColor)
			}, 5*time.Second, 10*time.Millisecond, "request the terminal background")
			start := 0
			if tc.fromLight {
				_, err := io.WriteString(master, "\x1b]11;rgb:ffff/ffff/ffff\a")
				require.NoError(t, err)
				require.Eventually(t, func() bool {
					return strings.Contains(out.String(), lightText)
				}, 5*time.Second, 10*time.Millisecond, "establish the light palette before the regression reply")
				start = len(out.String())
			}
			if tc.reply != "" {
				_, err := fmt.Fprintf(master, "\x1b]11;rgb:%s\x1b\\", tc.reply)
				require.NoError(t, err)
			}
			require.Eventually(t, func() bool {
				return strings.Contains(out.String()[start:], tc.want)
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
