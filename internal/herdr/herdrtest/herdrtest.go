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

	mu       sync.Mutex
	requests []Request

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
	s.wg.Go(func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // listener closed
			}
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
	if err != nil && !errors.Is(err, net.ErrClosed) && len(line) == 0 {
		return
	}
	var req Request
	if err := json.Unmarshal(line, &req); err != nil {
		return
	}
	s.mu.Lock()
	s.requests = append(s.requests, req)
	s.mu.Unlock()

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
