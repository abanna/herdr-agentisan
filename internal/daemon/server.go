package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

// The socket protocol (D6): one JSON request per connection,
// {"v":1,"op":…,"args":…}, answered with {"ok":true,"data":…} or
// {"ok":false,"error":{"code","message"}}.
const (
	protocolVersion = 1
	// maxMessage caps a request or response line.
	maxMessage = 1 << 20
	// ioTimeout bounds one exchange on either side.
	ioTimeout = 5 * time.Second
)

// Error codes the daemon answers with.
const (
	codeBadRequest         = "bad_request"
	codeUnsupportedVersion = "unsupported_version"
	codeUnknownOp          = "unknown_op"
)

// Client-side sentinel errors.
var (
	// ErrUnavailable means the daemon socket could not be reached or closed
	// without answering.
	ErrUnavailable = errors.New("daemon is unavailable")
	// ErrProtocol means the daemon answered with something the protocol does
	// not allow.
	ErrProtocol = errors.New("daemon protocol violation")
	// ErrRequest means the daemon refused the request; see RequestError.
	ErrRequest = errors.New("daemon refused the request")
	// ErrLog means the daemon log file could not be opened or rotated.
	ErrLog = errors.New("daemon log")
)

// RequestError is a daemon's {"ok":false} answer. It unwraps to ErrRequest.
type RequestError struct {
	Code    string
	Message string
}

func (e *RequestError) Error() string { return fmt.Sprintf("daemon: %s: %s", e.Code, e.Message) }

// Unwrap lets errors.Is(err, ErrRequest) match.
func (e *RequestError) Unwrap() error { return ErrRequest }

// HealthInfo is the health op's answer.
type HealthInfo struct {
	PID           int       `json:"pid"`
	Version       string    `json:"version"`
	Commit        string    `json:"commit"`
	StartedAt     time.Time `json:"started_at"`
	UptimeSeconds int64     `json:"uptime_s"`
	HerdrProtocol uint32    `json:"herdr_protocol"`
	HerdrSocket   string    `json:"herdr_socket"`
	// Herdr is the server the daemon belongs to.
	Herdr Identity `json:"herdr"`
}

type request struct {
	V    int             `json:"v"`
	Op   string          `json:"op"`
	Args json.RawMessage `json:"args,omitempty"`
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type response struct {
	OK    bool            `json:"ok"`
	Data  json.RawMessage `json:"data,omitempty"`
	Error *errorBody      `json:"error,omitempty"`
}

// server serves the daemon socket.
type server struct {
	ln     net.Listener
	path   string
	health HealthInfo
	log    zerolog.Logger
	wg     sync.WaitGroup
}

// listen binds the daemon socket, replacing a stale one: only the lock holder
// calls it. The socket is the owner's only.
func listen(path string, health HealthInfo, log zerolog.Logger) (*server, error) {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("remove stale socket %s: %w", path, err)
	}
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("chmod %s: %w", path, err)
	}
	s := &server{ln: ln, path: path, health: health, log: log}
	s.wg.Go(s.accept)
	return s, nil
}

func (s *server) accept() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return // closed
		}
		s.wg.Go(func() { s.handle(conn) })
	}
}

// close stops serving and removes the socket.
func (s *server) close() {
	_ = s.ln.Close()
	s.wg.Wait()
	_ = os.Remove(s.path)
}

func (s *server) handle(conn net.Conn) {
	defer conn.Close() //nolint:errcheck // one-shot connection; the answer is written or the client is gone
	_ = conn.SetDeadline(time.Now().Add(ioTimeout))
	line, err := readMessage(conn)
	resp := s.dispatch(line, err)
	raw, _ := json.Marshal(resp)
	if _, err := conn.Write(append(raw, '\n')); err != nil {
		s.log.Debug().Err(err).Msg("daemon socket: write answer")
	}
}

func (s *server) dispatch(line []byte, readErr error) response {
	if readErr != nil {
		return refuse(codeBadRequest, readErr.Error())
	}
	if len(bytes.TrimSpace(line)) == 0 {
		return refuse(codeBadRequest, "empty request")
	}
	dec := json.NewDecoder(bytes.NewReader(line))
	var req request
	if err := dec.Decode(&req); err != nil {
		return refuse(codeBadRequest, "request is not one JSON object: "+err.Error())
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return refuse(codeBadRequest, "request holds more than one JSON value")
	}
	if req.V != protocolVersion {
		return refuse(codeUnsupportedVersion, fmt.Sprintf("request version %d, this daemon speaks %d", req.V, protocolVersion))
	}
	switch req.Op {
	case "health":
		h := s.health
		h.UptimeSeconds = int64(time.Since(h.StartedAt).Seconds())
		data, _ := json.Marshal(h)
		return response{OK: true, Data: data}
	default:
		return refuse(codeUnknownOp, fmt.Sprintf("unknown op %q", req.Op))
	}
}

func refuse(code, message string) response {
	return response{Error: &errorBody{Code: code, Message: message}}
}

// readMessage reads one line of at most maxMessage bytes. The peer may close
// its write side instead of sending a newline.
func readMessage(r io.Reader) ([]byte, error) {
	line, err := bufio.NewReader(io.LimitReader(r, maxMessage+1)).ReadBytes('\n')
	if len(line) > maxMessage {
		return nil, fmt.Errorf("message exceeds %d bytes", maxMessage)
	}
	if err != nil && (!errors.Is(err, io.EOF) || len(line) == 0) {
		return line, err //nolint:wrapcheck // callers wrap with their own sentinel
	}
	return line, nil
}

// Health asks the daemon at socket for its health.
func Health(ctx context.Context, socket string) (HealthInfo, error) {
	data, err := call(ctx, socket, "health")
	if err != nil {
		return HealthInfo{}, err
	}
	var info HealthInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return HealthInfo{}, fmt.Errorf("%w: health: %w", ErrProtocol, err)
	}
	return info, nil
}

// call sends one request and returns the data of an ok answer.
func call(ctx context.Context, socket, op string) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, ioTimeout)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	defer conn.Close() //nolint:errcheck // one-shot connection
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	raw, _ := json.Marshal(request{V: protocolVersion, Op: op, Args: json.RawMessage(`{}`)})
	if _, err := conn.Write(append(raw, '\n')); err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrUnavailable, op, err)
	}
	line, err := readMessage(conn)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrUnavailable, op, err)
	}
	var resp response
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrProtocol, op, err)
	}
	if !resp.OK {
		if resp.Error == nil {
			return nil, fmt.Errorf("%w: %s: not ok and no error", ErrProtocol, op)
		}
		return nil, &RequestError{Code: resp.Error.Code, Message: resp.Error.Message}
	}
	if len(resp.Data) == 0 || string(resp.Data) == "null" {
		return nil, fmt.Errorf("%w: %s: ok without data", ErrProtocol, op)
	}
	return resp.Data, nil
}
