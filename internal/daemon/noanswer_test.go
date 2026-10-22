package daemon_test

import (
	"bufio"
	"context"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/daemon"
)

// The answers a stand-in daemon writes.
const (
	notReadyLine = `{"ok":false,"error":{"code":"not_ready","message":"not yet"}}` + "\n"
	okLine       = `{"ok":true,"data":{"ok":true}}` + "\n"
)

// serveConns serves sock until the test ends, running handle on each
// connection with its count, from 1, and a channel closed when the test
// ends, which a handler holding its connection waits on.
func serveConns(t *testing.T, sock string, handle func(n int32, conn net.Conn, ended <-chan struct{})) *atomic.Int32 {
	t.Helper()
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	var conns atomic.Int32
	ended := make(chan struct{})
	var wg sync.WaitGroup
	t.Cleanup(func() {
		close(ended)
		_ = ln.Close()
		wg.Wait()
	})
	wg.Go(func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			n := conns.Add(1)
			wg.Go(func() {
				defer conn.Close() //nolint:errcheck // the test's own connection
				handle(n, conn, ended)
			})
		}
	})
	return &conns
}

// TestAnExchangeSaysWhetherItSent: a dial that finds no daemon sent nothing
// (ErrNotSent), and a daemon that took the connection and closed it, before
// or after the request was written, or without an answer, may have read the
// request and run the op, so it never says so (A52). Both are
// ErrUnavailable.
func TestAnExchangeSaysWhetherItSent(t *testing.T) {
	t.Parallel()
	t.Run("no daemon", func(t *testing.T) {
		t.Parallel()
		_, err := daemon.Workforce(t.Context(), shortDir(t)+"/none.sock", daemon.OpSend, struct{}{})
		require.ErrorIs(t, err, daemon.ErrUnavailable)
		assert.ErrorIs(t, err, daemon.ErrNotSent)
	})
	for name, read := range map[string]bool{
		"a daemon that reads the request and closes": true,
		"a daemon that closes at once":               false,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			sock := shortDir(t) + "/d.sock"
			conns := serveConns(t, sock, func(_ int32, conn net.Conn, _ <-chan struct{}) {
				if read {
					_, _ = bufio.NewReader(conn).ReadString('\n')
				}
			})
			_, err := daemon.Workforce(t.Context(), sock, daemon.OpSend, struct{}{})
			require.ErrorIs(t, err, daemon.ErrUnavailable)
			assert.NotErrorIs(t, err, daemon.ErrNotSent, "the request may have reached the daemon")
			assert.NotErrorIs(t, err, context.DeadlineExceeded, "found closed at once")
			assert.Equal(t, int32(1), conns.Load())
		})
	}
	// A request too big for the socket's buffers, to a daemon that closes
	// without reading: the write itself fails, at once or partway, and part
	// of the request may have reached the daemon.
	t.Run("a daemon that closes before the request is written", func(t *testing.T) {
		t.Parallel()
		sock := shortDir(t) + "/d.sock"
		serveConns(t, sock, func(int32, net.Conn, <-chan struct{}) {})
		_, err := daemon.Workforce(t.Context(), sock, daemon.OpSend, map[string]string{"body": strings.Repeat("x", 8<<20)})
		require.ErrorIs(t, err, daemon.ErrUnavailable)
		assert.NotErrorIs(t, err, daemon.ErrNotSent, "part of the request may have reached the daemon")
	})
}

// TestNotReadyWindow: Workforce asks a not_ready daemon again for
// NotReadyWindow, and the window ends only the waits between asks (A52).
// The op here has no budget of its own, so each ask gets just an exchange
// (5 s), where the window is twice that: a wait bounded by one ask's
// budget would end with the first refusals.
func TestNotReadyWindow(t *testing.T) {
	t.Parallel()

	// The daemon turns ready late in the window and takes the ask then past
	// the window's end, within the ask's own wait: the client waits for it
	// and has its answer, where a window that cut an ask short would print
	// not_ready for an op the daemon ran.
	t.Run("an ask the window began is never cut short by it", func(t *testing.T) {
		t.Parallel()
		sock := shortDir(t) + "/d.sock"
		start := time.Now()
		var held atomic.Int32
		conns := serveConns(t, sock, func(_ int32, conn net.Conn, ended <-chan struct{}) {
			_, _ = bufio.NewReader(conn).ReadString('\n')
			if time.Since(start) < daemon.NotReadyWindow-2*time.Second {
				_, _ = conn.Write([]byte(notReadyLine))
				return
			}
			held.Add(1)
			select {
			case <-time.After(time.Until(start.Add(daemon.NotReadyWindow + time.Second))):
				_, _ = conn.Write([]byte(okLine))
			case <-ended:
			}
		})
		data, err := daemon.Workforce(t.Context(), sock, "no-budget", struct{}{})
		require.NoError(t, err, "the ask begun in the window has its answer")
		assert.JSONEq(t, `{"ok":true}`, string(data))
		assert.GreaterOrEqual(t, time.Since(start), daemon.NotReadyWindow, "answered after the window ended")
		assert.Equal(t, int32(1), held.Load(), "the client waited on the one ask")
		assert.Greater(t, conns.Load(), int32(2), "asked again past one ask's wait")
	})

	// After a refusal, an ask the daemon took and never answered within its
	// whole wait may be running the op: no answer, not the earlier
	// not_ready, whose answer would be to ask again.
	t.Run("an ask taken after a refusal and never answered is no answer", func(t *testing.T) {
		t.Parallel()
		sock := shortDir(t) + "/d.sock"
		conns := serveConns(t, sock, func(n int32, conn net.Conn, ended <-chan struct{}) {
			_, _ = bufio.NewReader(conn).ReadString('\n')
			if n == 1 {
				_, _ = conn.Write([]byte(notReadyLine))
				return
			}
			<-ended
		})
		_, err := daemon.Workforce(t.Context(), sock, "no-budget", struct{}{})
		require.ErrorIs(t, err, daemon.ErrUnavailable)
		assert.NotErrorIs(t, err, daemon.ErrNotReady, "not the earlier refusal")
		assert.NotErrorIs(t, err, daemon.ErrNotSent, "the daemon took the ask")
		assert.ErrorIs(t, err, context.DeadlineExceeded, "its own wait ran out")
		assert.Equal(t, int32(2), conns.Load())
	})
}
