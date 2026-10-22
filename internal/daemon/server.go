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
	// codeTimeout answers an op whose own budget ended.
	codeTimeout = "timeout"
	// codeFailed answers an op error no code of its own names.
	codeFailed = "failed"
)

// Client-side sentinel errors.
var (
	// ErrUnavailable means the daemon socket could not be reached or closed
	// without answering.
	ErrUnavailable = errors.New("daemon is unavailable")
	// ErrNotSent means an exchange ended before any of its request was
	// written: its dial failed. The daemon certainly did not run the op, so
	// it is ErrUnavailable too. An exchange that fails after writing never
	// says this: the daemon may have run the op, and may never answer (A52).
	ErrNotSent = errors.New("nothing was sent to the daemon")
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

// Is lets errors.Is match the sentinel a refusal code stands for, so callers
// never compare codes themselves.
func (e *RequestError) Is(target error) bool {
	switch e.Code {
	case codeNoHistory:
		return target == ErrNoHistory
	case codeHerdrFailed:
		return target == ErrHerdrCall
	case codeNotReady:
		return target == ErrNotReady
	case codeNoTeam:
		return target == ErrNoTeam
	}
	return false
}

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
	// StoreVersion is the state database's schema version (user_version).
	StoreVersion int `json:"store_version"`
	// FocusRows is how many rows the focus history holds, or -1 when the
	// state database could not be read.
	FocusRows int `json:"focus_rows"`
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
	ln   net.Listener
	path string
	// health reports the daemon's health when asked.
	health func() HealthInfo
	// back runs the back op.
	back func() (BackResult, error)
	// snapshot is the selected team's last snapshot the team poll built,
	// encoded.
	snapshot func(Selector) (json.RawMessage, error)
	// ops are the ops that take args and a budget of their own: the
	// workforce's (A23). They run under opsCtx, which the daemon cancels
	// before it closes the socket, never under the connection: an op whose
	// client went still finishes.
	ops    map[string]op
	opsCtx context.Context
	log    zerolog.Logger
	wg     sync.WaitGroup
}

// op is a socket op with args and a budget of its own.
type op struct {
	// budget bounds the op, given its args.
	budget func(args json.RawMessage) time.Duration
	run    func(ctx context.Context, args json.RawMessage) (any, error)
	// code is the refusal code for one of run's errors.
	code func(error) string
}

// listen binds the daemon socket, replacing a stale one: only the lock holder
// calls it. The socket is the owner's only.
func listen(opsCtx context.Context, path string, health func() HealthInfo, back func() (BackResult, error), snapshot func(Selector) (json.RawMessage, error), ops map[string]op, log zerolog.Logger) (*server, error) {
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
	s := &server{ln: ln, path: path, health: health, back: back, snapshot: snapshot, ops: ops, opsCtx: opsCtx, log: log}
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
	// An op with a budget of its own gets it, and an exchange more to
	// answer in; reading the request keeps the 5 s.
	resp := s.dispatch(line, err, func(budget time.Duration) { _ = conn.SetDeadline(time.Now().Add(budget + ioTimeout)) })
	raw, _ := json.Marshal(resp)
	if _, err := conn.Write(append(raw, '\n')); err != nil {
		s.log.Debug().Err(err).Msg("daemon socket: write answer")
	}
}

func (s *server) dispatch(line []byte, readErr error, extend func(time.Duration)) response {
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
	if o, ok := s.ops[req.Op]; ok {
		return s.run(req.Op, o, req.Args, extend)
	}
	switch req.Op {
	case "health":
		h := s.health()
		h.UptimeSeconds = int64(time.Since(h.StartedAt).Seconds())
		data, _ := json.Marshal(h)
		return response{OK: true, Data: data}
	case "back":
		res, err := s.back()
		if err != nil {
			s.log.Info().Err(err).Msg("back refused")
			return refuse(backCode(err), err.Error())
		}
		data, _ := json.Marshal(res)
		return response{OK: true, Data: data}
	case "snapshot":
		// The cached bytes: the op never calls herdr, so it is cheap however
		// often a dashboard asks (D3). Args it does not know are ignored.
		var sel Selector
		if len(req.Args) > 0 && string(req.Args) != "null" {
			if err := json.Unmarshal(req.Args, &sel); err != nil {
				return refuse(codeBadRequest, "snapshot args: "+err.Error())
			}
		}
		data, err := s.snapshot(sel)
		if err != nil {
			return refuse(snapshotCode(err), err.Error())
		}
		return response{OK: true, Data: data}
	default:
		return refuse(codeUnknownOp, fmt.Sprintf("unknown op %q", req.Op))
	}
}

// run runs one op with a budget of its own.
func (s *server) run(name string, o op, args json.RawMessage, extend func(time.Duration)) response {
	budget := o.budget(args)
	extend(budget)
	ctx, cancel := context.WithTimeout(s.opsCtx, budget)
	defer cancel()
	res, err := o.run(ctx, args)
	if err != nil {
		s.log.Info().Err(err).Str("op", name).Msg("op refused")
		code := o.code(err)
		// An op whose own budget ended while a call failed is a timeout, the
		// error saying what it waited on; a code that says more than that a
		// call failed, such as handoff_timeout or herdr_gone, stands.
		if errors.Is(ctx.Err(), context.DeadlineExceeded) && callFailed[code] {
			return refuse(codeTimeout, fmt.Sprintf("%s ran past its %s budget: %v", name, budget, err))
		}
		return refuse(code, err.Error())
	}
	data, err := json.Marshal(res)
	if err != nil {
		return refuse(codeFailed, fmt.Sprintf("encode the %s answer: %v", name, err))
	}
	s.log.Info().Str("op", name).Msg("op done")
	return response{OK: true, Data: data}
}

// callFailed are the codes that say only that a call failed.
var callFailed = map[string]bool{codeHerdrFailed: true, codeStoreFailed: true, codeFailed: true}

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

// exchangeFailed is ErrUnavailable for a failed exchange, naming the
// context's own error when the context is what ended it, so a caller can
// tell a deadline or a cancel from a daemon that went. The connection's only
// deadlines are the context's, so a socket timeout is the context's deadline
// even when the read notices it a moment before the context does.
func exchangeFailed(ctx context.Context, op string, err error) error {
	ctxErr := ctx.Err()
	if _, ok := ctx.Deadline(); ok && ctxErr == nil && errors.Is(err, os.ErrDeadlineExceeded) {
		ctxErr = context.DeadlineExceeded
	}
	if ctxErr != nil {
		return fmt.Errorf("%w: %s: %w", ErrUnavailable, op, ctxErr)
	}
	return fmt.Errorf("%w: %s: %w", ErrUnavailable, op, err)
}

// call sends one request with no args and returns the data of an ok answer,
// within one exchange.
func call(ctx context.Context, socket, op string) (json.RawMessage, error) {
	return callArgs(ctx, socket, op, struct{}{})
}

// callArgs sends one request with args, encoded as one JSON object, and
// returns the data of an ok answer, within one exchange.
func callArgs(ctx context.Context, socket, op string, args any) (json.RawMessage, error) {
	rawArgs, err := json.Marshal(args)
	if err != nil {
		return nil, fmt.Errorf("encode %s args: %w", op, err)
	}
	return callWith(ctx, socket, op, rawArgs, ioTimeout)
}

// callWith sends one request with args and returns the data of an ok
// answer, within timeout. A failed dial is ErrNotSent; a failure after it
// is ErrUnavailable alone, since some of the request may have been written.
func callWith(ctx context.Context, socket, op string, args json.RawMessage, timeout time.Duration) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNotSent, exchangeFailed(ctx, op, err))
	}
	defer conn.Close() //nolint:errcheck // one-shot connection
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	// A cancel, which has no deadline, must end the exchange too: a
	// dashboard quitting never waits out a wedged daemon.
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()
	raw, _ := json.Marshal(request{V: protocolVersion, Op: op, Args: args})
	if _, err := conn.Write(append(raw, '\n')); err != nil {
		return nil, exchangeFailed(ctx, op, err)
	}
	line, err := readMessage(conn)
	if err != nil {
		return nil, exchangeFailed(ctx, op, err)
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
