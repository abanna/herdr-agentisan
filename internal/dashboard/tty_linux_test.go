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
	// The header spells the counts out only when the terminal is wide
	// enough: 120 columns, read from the terminal itself.
	require.Eventually(t, func() bool { return strings.Contains(out.String(), "◐6 working") }, 5*time.Second, 10*time.Millisecond)
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
