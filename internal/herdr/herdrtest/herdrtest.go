// Package herdrtest runs an in-process stand-in for the herdr socket, so
// tests exercise the real client over a real unix socket without reaching a
// live herdr. A developer's shell inside herdr has HERDR_SOCKET_PATH set;
// tests must dial this server instead, never the environment's socket.
package herdrtest

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// Request is one decoded request line as the server received it.
type Request struct {
	ID     string          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// ErrorBody is herdr's error payload.
type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Reply is what a handler sends back. Exactly one of Result, Error or Raw is
// normally set; Raw is written verbatim to test malformed responses. ID
// overrides the echoed request id when non-nil. Unterminated omits the
// trailing newline; Silent closes the connection without writing anything.
//
// Stream, when non-nil, keeps the connection open after the reply, as herdr
// does for events.subscribe: the server writes every item received on it,
// verbatim, until the channel is closed or the server closes, and then closes
// the connection. Items are whole lines, newline included, unless a test
// means to send a partial one.
type Reply struct {
	Result       any
	Error        *ErrorBody
	Raw          []byte
	ID           *string
	Unterminated bool
	Silent       bool
	Stream       <-chan []byte
}

// Handler answers one request.
type Handler func(Request) Reply

// Server is a running fake herdr socket.
type Server struct {
	// Path is the unix socket to dial.
	Path string
	// socket is the socket file StartAt bound at Path (Settle).
	socket os.FileInfo

	mu       sync.Mutex
	requests []Request
	// unread counts the connections accepted whose request line is not yet
	// read and recorded (Settle).
	unread int

	ln    net.Listener
	wg    sync.WaitGroup
	close sync.Once
	// done is closed by Close, ending every open stream.
	done chan struct{}
}

// Requests returns every request received so far, in arrival order.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

// settleTimeout bounds Settle.
const settleTimeout = 5 * time.Second

// Settle waits until every request sent to s before it was called is in
// Requests, so a test can take Requests as a baseline that a request still in
// flight will not land after. A client can write its request, and even
// return, before the server has accepted the connection or read the line, so
// a client having returned proves nothing.
//
// Settle dials a connection of its own and sends nothing on it. The listener
// accepts connections in the order they were made, so once the server has
// read the end of that connection, it has accepted every connection made
// before it; Settle then waits until it has read and recorded each of them.
// Settle's own connection is never recorded. Streams already open do not hold
// it up: a connection counts as read once its request line is. One that has
// sent nothing and stays open does, for up to settleTimeout.
//
// It dials Path, and a test can remove or replace what Path names while s is
// still open. Settle then fails the test rather than settle a server it never
// reached: once the server has closed the settle connection, Path must still
// name the socket StartAt bound. A socket file is never bound twice, so short
// of a test linking that file back in meanwhile, Path naming it after the
// connection proves the connection reached s. On a server that is closed
// Settle returns once Close has, since Close waits for every connection it
// accepted.
func (s *Server) Settle(t testing.TB) {
	t.Helper()
	if s.closed() {
		return
	}
	deadline := time.Now().Add(settleTimeout)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	// Each failure below is the test's only if s is still open: a Close
	// meanwhile unlinks Path and resets a connection it never accepted, and
	// has waited for every connection it did.
	fail := func(format string, args ...any) {
		t.Helper()
		if !s.closed() {
			t.Fatalf("herdrtest: settle: "+format, args...)
		}
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", s.Path)
	if err != nil {
		fail("dial %s, which may no longer name this server: %v", s.Path, err)
		return
	}
	defer conn.Close() //nolint:errcheck // nothing was sent; the server has closed it already
	_ = conn.SetDeadline(deadline)
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		t.Fatalf("herdrtest: settle: dialled a %T, not a unix connection", conn)
	}
	// An empty request: the server reads EOF, records nothing and closes the
	// connection, which ends the copy.
	if err := uc.CloseWrite(); err != nil {
		fail("close write: %v", err)
		return
	}
	if _, err := io.Copy(io.Discard, uc); err != nil {
		fail("the server did not read the settle connection: %v", err)
		return
	}
	if now, err := os.Stat(s.Path); err != nil || !os.SameFile(now, s.socket) {
		fail("%s no longer names this server's socket", s.Path)
		return
	}
	for {
		s.mu.Lock()
		unread := s.unread
		s.mu.Unlock()
		if unread == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("herdrtest: settle: %d connections still unread after %v", unread, settleTimeout)
		}
		time.Sleep(time.Millisecond)
	}
}

// closed reports whether Close has been called, and if so waits for it to
// finish.
func (s *Server) closed() bool {
	select {
	case <-s.done:
		s.Close() // returns once the first call's Close has
		return true
	default:
		return false
	}
}

// Start listens on a fresh socket and serves one request per connection, as
// herdr does. The directory comes from os.MkdirTemp rather than t.TempDir:
// unix socket paths are capped at 104-108 bytes, and t.TempDir embeds the
// full (sub)test name.
func Start(t testing.TB, handle Handler) *Server {
	t.Helper()

	dir, err := os.MkdirTemp("", "hd")
	if err != nil {
		t.Fatalf("herdrtest: temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return StartAt(t, filepath.Join(dir, "s.sock"), handle)
}

// StartAt serves at path, which must not exist yet. Closing a server and
// starting another at the same path is how a test restarts herdr: the socket
// file is new, so its inode changes, as it does across a herdr live handoff.
func StartAt(t testing.TB, path string, handle Handler) *Server {
	t.Helper()

	s := &Server{Path: path, done: make(chan struct{})}
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "unix", path)
	if err != nil {
		t.Fatalf("herdrtest: listen: %v", err)
	}
	s.ln = ln
	if s.socket, err = os.Stat(path); err != nil {
		_ = ln.Close()
		t.Fatalf("herdrtest: stat the socket: %v", err)
	}
	s.wg.Go(func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // listener closed
			}
			// Counted here, in accept order, not in serve, which may not
			// have started by the time Settle looks.
			s.mu.Lock()
			s.unread++
			s.mu.Unlock()
			s.wg.Go(func() { s.serve(conn, handle) })
		}
	})
	t.Cleanup(s.Close)
	return s
}

// Crash stops the server but leaves its socket file behind, as a herdr that
// was killed does: the file still names a socket, and nothing answers it.
func (s *Server) Crash() {
	if ul, ok := s.ln.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(false)
	}
	s.Close()
}

// Close stops the server and removes its socket file, as a herdr that exits
// does. Open streams end, so their subscribers see the connection close. It is
// safe to call more than once.
func (s *Server) Close() {
	s.close.Do(func() {
		close(s.done)
		_ = s.ln.Close() // a unix listener unlinks its socket file on close
		s.wg.Wait()
	})
}

func (s *Server) serve(conn net.Conn, handle Handler) {
	defer conn.Close() //nolint:errcheck // test server; nothing to report to

	line, err := bufio.NewReader(conn).ReadBytes('\n')
	req, ok := s.record(line, err)
	if !ok {
		return
	}

	reply := handle(req)
	if reply.Silent {
		return
	}
	out := reply.Raw
	if out == nil {
		id := req.ID
		if reply.ID != nil {
			id = *reply.ID
		}
		env := map[string]any{"id": id}
		if reply.Error != nil {
			env["error"] = reply.Error
		} else {
			env["result"] = reply.Result
		}
		out, _ = json.Marshal(env)
	}
	if !reply.Unterminated {
		out = append(out, '\n')
	}
	if _, err := conn.Write(out); err != nil || reply.Stream == nil {
		return
	}
	s.stream(conn, reply.Stream)
}

// record decodes and records the request line read from a connection, and
// reports whether there was one. Either way the connection is read.
func (s *Server) record(line []byte, readErr error) (Request, bool) {
	var req Request
	ok := (readErr == nil || errors.Is(readErr, net.ErrClosed) || len(line) > 0) &&
		json.Unmarshal(line, &req) == nil
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unread--
	if ok {
		s.requests = append(s.requests, req)
	}
	return req, ok
}

// streamWriteTimeout bounds one stream write, so a client that stops reading
// cannot hold Close up.
const streamWriteTimeout = 5 * time.Second

// stream writes each item from lines until lines is closed, the server
// closes or the client goes.
func (s *Server) stream(conn net.Conn, lines <-chan []byte) {
	for {
		select {
		case <-s.done:
			return
		case line, ok := <-lines:
			if !ok {
				return
			}
			_ = conn.SetWriteDeadline(time.Now().Add(streamWriteTimeout))
			if _, err := conn.Write(line); err != nil {
				return
			}
		}
	}
}

// SubscriptionStarted is the result herdr acknowledges events.subscribe with
// (src/api/server.rs, stream_subscriptions).
func SubscriptionStarted() map[string]any { return map[string]any{"type": "subscription_started"} }

// PaneFocused is the line herdr streams to a pane.focused subscriber when
// paneID, in workspaceID, takes focus: an event envelope, with no request id
// (src/api/schema/events.rs, EventEnvelope and EventData::PaneFocused).
func PaneFocused(paneID, workspaceID string) []byte {
	return line(map[string]any{
		"event": "pane_focused",
		"data":  map[string]any{"type": "pane_focused", "pane_id": paneID, "workspace_id": workspaceID},
	})
}

// EventsLost is the line herdr writes, just before it closes the stream, to a
// subscriber that fell more than 512 events behind (src/api/subscriptions.rs,
// subscription_events_after). id is the subscribe request's.
func EventsLost(id string) []byte {
	return line(map[string]any{
		"id": id,
		"error": ErrorBody{
			Code:    "events_lost",
			Message: "event subscription fell behind retained history; resubscribe and resync with session.snapshot",
		},
	})
}

func line(v any) []byte {
	raw, _ := json.Marshal(v)
	return append(raw, '\n')
}
