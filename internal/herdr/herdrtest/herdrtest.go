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
type Reply struct {
	Result       any
	Error        *ErrorBody
	Raw          []byte
	ID           *string
	Unterminated bool
	Silent       bool
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

	s := &Server{Path: path}
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

// Close stops the server and removes its socket file, as a herdr that exits
// does. It is safe to call more than once.
func (s *Server) Close() {
	s.close.Do(func() {
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
	_, _ = conn.Write(out)
}
