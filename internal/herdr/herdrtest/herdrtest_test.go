package herdrtest_test

import (
	"bufio"
	"fmt"
	"net"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/herdr/herdrtest"
)

// ok answers every request.
func ok(herdrtest.Request) herdrtest.Reply { return herdrtest.Reply{Result: map[string]any{}} }

// fatalTB stands in for a test's T so a test can watch Settle fail: Fatalf
// records the message and ends the goroutine, as testing.T's does. Settle
// calls nothing else on it.
type fatalTB struct {
	testing.TB
	mu  sync.Mutex
	msg string
}

func (f *fatalTB) Helper() {}

func (f *fatalTB) Fatalf(format string, args ...any) {
	f.mu.Lock()
	f.msg = fmt.Sprintf(format, args...)
	f.mu.Unlock()
	runtime.Goexit()
}

// settleFailure runs srv.Settle and returns how it failed the test, or ""
// if it settled. It may be called from any goroutine.
func settleFailure(t *testing.T, srv *herdrtest.Server) string {
	t.Helper()
	tb := &fatalTB{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.Settle(tb)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Error("Settle neither settled nor failed")
		return "timed out"
	}
	tb.mu.Lock()
	defer tb.mu.Unlock()
	return tb.msg
}

func dial(t *testing.T, srv *herdrtest.Server) net.Conn {
	t.Helper()
	var d net.Dialer
	conn, err := d.DialContext(t.Context(), "unix", srv.Path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// TestSettleWaitsForEveryRequestSentBeforeIt: once Settle returns, every
// request whose connection was made before it is in Requests, whether its line
// was still arriving or its connection not yet accepted.
func TestSettleWaitsForEveryRequestSentBeforeIt(t *testing.T) {
	t.Parallel()
	const request = `{"id":"1","method":"workspace.report_metadata"}` + "\n"
	tests := map[string]struct {
		send func(t *testing.T, srv *herdrtest.Server)
		want int
	}{
		"no connection at all": {send: func(*testing.T, *herdrtest.Server) {}, want: 0},
		"a request still being written": {want: 1, send: func(t *testing.T, srv *herdrtest.Server) {
			conn := dial(t, srv)
			_, err := conn.Write([]byte(request[:20]))
			require.NoError(t, err)
			go func() {
				time.Sleep(50 * time.Millisecond)
				_, _ = conn.Write([]byte(request[20:]))
			}()
		}},
		"a hundred connections at once": {want: 100, send: func(t *testing.T, srv *herdrtest.Server) {
			errs := make(chan error, 100)
			var wg sync.WaitGroup
			for range 100 {
				wg.Go(func() {
					var d net.Dialer
					conn, err := d.DialContext(t.Context(), "unix", srv.Path)
					if err == nil {
						_, err = conn.Write([]byte(request))
						_ = conn.Close()
					}
					errs <- err
				})
			}
			wg.Wait() // every request written; many not yet accepted
			close(errs)
			for err := range errs {
				require.NoError(t, err)
			}
		}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := herdrtest.Start(t, ok)
			tc.send(t, srv)

			srv.Settle(t)
			assert.Len(t, srv.Requests(), tc.want, "Settle returned before every request was read")
		})
	}
}

// TestSettleWhileTheServerCloses: a Close at any point during Settle, before
// its dial, while its connection waits to be accepted or after it has been
// read, still settles, and nothing lands after: Close waits for every
// connection it accepted and drops the rest, as a herdr that exits does.
func TestSettleWhileTheServerCloses(t *testing.T) {
	t.Parallel()
	for range 50 {
		srv := herdrtest.Start(t, ok)
		_, err := dial(t, srv).Write([]byte(`{"id":"1","method":"ping"}` + "\n"))
		require.NoError(t, err)
		go srv.Close()
		require.Empty(t, settleFailure(t, srv))
		settled := srv.Requests()
		srv.Close() // returns once the first Close has
		assert.Equal(t, settled, srv.Requests(), "a request landed after Settle")
	}
}

// TestSettleIsNotARequest: Settle's own connection never reaches the handler
// or Requests, an open stream does not hold it up, and on a closed server,
// whose path is gone, it returns at once.
func TestSettleIsNotARequest(t *testing.T) {
	t.Parallel()
	var handled atomic.Int32
	lines := make(chan []byte)
	srv := herdrtest.Start(t, func(herdrtest.Request) herdrtest.Reply {
		handled.Add(1)
		return herdrtest.Reply{Result: herdrtest.SubscriptionStarted(), Stream: lines}
	})
	conn := dial(t, srv)
	_, err := conn.Write([]byte(`{"id":"1","method":"events.subscribe","params":{}}` + "\n"))
	require.NoError(t, err)
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	_, err = bufio.NewReader(conn).ReadBytes('\n')
	require.NoError(t, err, "the subscription is acknowledged and stays open")

	srv.Settle(t)
	assert.Len(t, srv.Requests(), 1)
	assert.Equal(t, int32(1), handled.Load())

	srv.Close()
	srv.Settle(t) // dialling the path, now gone, would fail the test
	assert.Len(t, srv.Requests(), 1)
}

// TestSettleTwiceAtOnce: two Settles on one server at the same time both
// settle, each after the request sent before them, and neither is recorded.
func TestSettleTwiceAtOnce(t *testing.T) {
	t.Parallel()
	srv := herdrtest.Start(t, ok)
	_, err := dial(t, srv).Write([]byte(`{"id":"1","method":"ping"}` + "\n"))
	require.NoError(t, err)

	failures := make([]string, 2)
	var wg sync.WaitGroup
	for i := range failures {
		wg.Go(func() { failures[i] = settleFailure(t, srv) })
	}
	wg.Wait()
	assert.Equal(t, []string{"", ""}, failures)
	assert.Len(t, srv.Requests(), 1)
}

// TestRequestLines is what the server records of a connection, which carries
// one request, as herdr reads it: the first line, decoded as JSON. A line
// that does not decode is read and dropped, and holds Settle up no more than
// one that does.
func TestRequestLines(t *testing.T) {
	t.Parallel()
	const ping = `{"id":"1","method":"ping"}`
	tests := map[string]struct {
		send string
		want []string // "id method" of each request recorded
	}{
		"a request line":                  {send: ping + "\n", want: []string{"1 ping"}},
		"no final newline":                {send: ping, want: []string{"1 ping"}},
		"a CRLF line":                     {send: ping + "\r\n", want: []string{"1 ping"}},
		"two lines, the first read":       {send: ping + "\n" + `{"id":"2","method":"pane.list"}` + "\n", want: []string{"1 ping"}},
		"a 1 MiB line":                    {send: `{"id":"1","method":"ping","params":{"pad":"` + strings.Repeat("x", 1<<20) + `"}}` + "\n", want: []string{"1 ping"}},
		"a non-ASCII method":              {send: `{"id":"1","method":"◆ ping"}` + "\n", want: []string{"1 ◆ ping"}},
		"params at the depth limit":       {send: nested(9999) + "\n", want: []string{"1 ping"}},
		"params one past the depth limit": {send: nested(10000) + "\n"},
		"invalid UTF-8 in a string":       {send: "{\"id\":\"1\",\"method\":\"p\xffing\"}\n", want: []string{"1 p\ufffding"}},
		"a truncated multibyte sequence":  {send: "{\"id\":\"1\",\"method\":\"p\xe2\x97\"}\n", want: []string{"1 p\ufffd\ufffd"}},
		"a lone surrogate escape":         {send: `{"id":"1","method":"p\ud800"}` + "\n", want: []string{"1 p\ufffd"}},
		"an escaped NUL":                  {send: `{"id":"1","method":"pi\u0000ng"}` + "\n", want: []string{"1 pi\x00ng"}},
		"a raw NUL in a string":           {send: "{\"id\":\"1\",\"method\":\"pi\x00ng\"}\n"},
		"a BOM before the line":           {send: "\ufeff" + ping + "\n"},
		"UTF-16":                          {send: utf16le(ping) + "\n"},
		"not JSON":                        {send: "ping\n"},
		"an empty line":                   {send: "\n"},
		"nothing sent":                    {send: ""},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var handled atomic.Int32
			srv := herdrtest.Start(t, func(r herdrtest.Request) herdrtest.Reply {
				handled.Add(1)
				return ok(r)
			})
			conn, isUnix := dial(t, srv).(*net.UnixConn)
			require.True(t, isUnix)
			_, err := conn.Write([]byte(tc.send))
			require.NoError(t, err)
			require.NoError(t, conn.CloseWrite())

			srv.Settle(t)
			var got []string
			for _, r := range srv.Requests() {
				got = append(got, r.ID+" "+r.Method)
			}
			assert.Equal(t, tc.want, got)
			assert.Equal(t, int32(len(tc.want)), handled.Load(), "only a recorded request is answered")
		})
	}
}

// nested is a ping whose params are n arrays deep, so the request is n+1
// deep: encoding/json decodes at most 10000.
func nested(n int) string {
	return `{"id":"1","method":"ping","params":` + strings.Repeat("[", n) + strings.Repeat("]", n) + `}`
}

// utf16le is s, which must be ASCII, encoded as UTF-16LE.
func utf16le(s string) string {
	var b strings.Builder
	for _, r := range s {
		b.WriteByte(byte(r))
		b.WriteByte(0)
	}
	return b.String()
}
