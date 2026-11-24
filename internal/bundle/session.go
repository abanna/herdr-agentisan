package bundle

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// What the client pins of the bundle 0.19.0 server and its MCP SDK 2.3.0.
const (
	ProtocolVersion = "2025-11-25"
	serverName      = "agentisan"
)

var minVersion = [3]int{0, 19, 0}

const (
	maxReply = 4 << 20 // bytes per stdout frame, including the newline
	// grace is how long Close waits for the server to end on its own once
	// its stdin is closed, and waitDelay how long a failed call waits to
	// learn how the server ended.
	grace     = 2 * time.Second
	waitDelay = 2 * time.Second
)

// Session is one running server; its calls are serial.
type Session struct {
	ctx     context.Context //nolint:containedctx // the one deadline of the session's life
	cmd     *exec.Cmd
	guard   *exec.Cmd
	life    *os.File
	in, out *os.File
	rd      *bufio.Reader
	exited  chan struct{} // closed once both processes are reaped
	waitErr error         // the reap's result, read after exited
	once    sync.Once
	killErr error
	nextID  int
	failed  error
}

// Open starts and handshakes with the server. The context bounds the session.
// Cancellation or a failed Open tears down the server group.
func Open(ctx context.Context, l Launch) (*Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, startFailure(ctx, err)
	}
	if len(l.Argv) == 0 {
		return nil, fmt.Errorf("%w: no command to start", ErrUnavailable)
	}
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		_, _ = inR.Close(), inW.Close()
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	guard, life, err := startSupervisor(ctx)
	if err != nil {
		_, _, _, _ = inR.Close(), inW.Close(), outR.Close(), outW.Close()
		return nil, startFailure(ctx, err)
	}
	s := &Session{ctx: ctx, guard: guard, life: life, in: inW, out: outR, rd: bufio.NewReaderSize(outR, maxReply), exited: make(chan struct{})}
	stop := context.AfterFunc(ctx, s.kill)
	cmd := exec.CommandContext(ctx, l.Argv[0], l.Argv[1:]...) // #nosec G204 -- argv is Locate's, or a test's
	s.cmd = cmd
	// Session cancellation must outlive exec's context watcher.
	cmd.Cancel = nil
	cmd.Dir, cmd.Env = l.Dir, l.Env
	cmd.Stdin, cmd.Stdout = inR, outW
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: guard.Process.Pid}
	start := l.start
	if start == nil {
		start = (*exec.Cmd).Start
	}
	startErr := start(cmd)
	_, _ = inR.Close(), outW.Close() // the server's ends; ours are in and out
	if startErr != nil {
		stop()
		s.kill()
		_ = guard.Wait()
		return nil, startFailure(ctx, fmt.Errorf("start %s: %w", l.Argv[0], startErr))
	}
	if ctx.Err() != nil {
		// Start may join the group after the cancellation signal. The guard is still unreaped.
		_ = syscall.Kill(-guard.Process.Pid, syscall.SIGKILL)
	}
	go func() {
		defer stop()
		s.waitErr = cmd.Wait()
		s.kill()
		_ = guard.Wait()
		close(s.exited)
	}()
	if err := s.handshake(l.ClientVersion); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

func startFailure(ctx context.Context, err error) error {
	if cerr := ctx.Err(); cerr != nil {
		return fmt.Errorf("%w: %w", ErrTimeout, cerr)
	}
	return fmt.Errorf("%w: %w", ErrUnavailable, err)
}

func (s *Session) handshake(clientVersion string) error {
	res, err := s.rpc("initialize", map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "herdr-agentisan", "version": clientVersion},
	})
	if errors.Is(err, ErrServerDied) {
		return fmt.Errorf("%w: before the handshake: %w", ErrUnavailable, err)
	}
	if err != nil {
		return err
	}
	var r struct {
		ProtocolVersion string `json:"protocolVersion"`
		Capabilities    struct {
			Tools *json.RawMessage `json:"tools"`
		} `json:"capabilities"`
		ServerInfo struct{ Name, Version string } `json:"serverInfo"`
	}
	if json.Unmarshal(res, &r) != nil || r.ProtocolVersion != ProtocolVersion || r.ServerInfo.Name != serverName || r.Capabilities.Tools == nil || !atLeast(r.ServerInfo.Version) {
		return fmt.Errorf("%w: protocol %q, server %q %q", ErrVersion, clip(r.ProtocolVersion), clip(r.ServerInfo.Name), clip(r.ServerInfo.Version))
	}
	if err := s.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}); err != nil {
		return s.broken(err)
	}
	return nil
}

// atLeast reports whether v is a major.minor.patch of at least minVersion.
func atLeast(v string) bool {
	parts := strings.Split(v, ".")
	if len(parts) != len(minVersion) {
		return false
	}
	newer := false
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return false
		}
		if !newer && n < minVersion[i] {
			return false
		}
		newer = newer || n > minVersion[i]
	}
	return true
}

// Execute calls the bundle's execute tool with dry_run=true and unwraps its result.
func (s *Session) Execute(operationID string, args map[string]any) (json.RawMessage, error) {
	res, err := s.rpc("tools/call", map[string]any{"name": "execute", "arguments": map[string]any{
		"operation_id": operationID, "arguments": args, "dry_run": true,
	}})
	if err != nil {
		return nil, err
	}
	out, err := decodeEnvelope(operationID, res)
	if errors.Is(err, ErrProtocol) {
		s.fail()
	}
	return out, err
}

// codes maps stable ErrorCode values from agentisan operations/policy.py.
var codes = map[string]error{
	"unknown_operation": ErrUnknownOperation,
	"validation_failed": ErrRefused,
	"policy_denied":     ErrRefused,
	"operation_failed":  ErrOperation,
	"handler_error":     ErrOperation,
}

// decodeEnvelope reads the structured envelope or its JSON text fallback.
func decodeEnvelope(operationID string, res json.RawMessage) (json.RawMessage, error) {
	var tr struct {
		Content []struct{ Type, Text string } `json:"content"`
		Payload json.RawMessage               `json:"structuredContent"`
		IsError bool                          `json:"isError"`
	}
	if err := json.Unmarshal(res, &tr); err != nil {
		return nil, fmt.Errorf("%w: tools/call result: %w", ErrProtocol, err)
	}
	text := ""
	if len(tr.Content) == 1 && tr.Content[0].Type == "text" {
		text = tr.Content[0].Text
	}
	if len(tr.Payload) == 0 || string(tr.Payload) == "null" {
		tr.Payload = json.RawMessage(text)
	}
	var env struct {
		Status      string          `json:"status"`
		OperationID string          `json:"operation_id"`
		Result      json.RawMessage `json:"result"`
		Error       *struct{ Code, Message string }
	}
	if len(tr.Payload) == 0 || json.Unmarshal(tr.Payload, &env) != nil || env.Status == "" {
		return nil, fmt.Errorf("%w: no operation envelope in the reply", ErrProtocol)
	}
	switch {
	case !tr.IsError && env.Status == "ok" && env.OperationID == operationID:
		return env.Result, nil
	case env.Status == "error" && env.Error != nil:
		kind, known := codes[env.Error.Code]
		if !known {
			kind = ErrContract
		}
		return nil, fmt.Errorf("%w: %s: %s", kind, clip(env.Error.Code), clip(env.Error.Message))
	}
	return nil, fmt.Errorf("%w: status %q with isError=%t for operation %q", ErrContract, clip(env.Status), tr.IsError, clip(env.OperationID))
}

// frame is the part of a JSON-RPC message the client looks at.
type frame struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int
		Message string
	} `json:"error"`
}

// rpc reads one request's reply and ends the session on failure.
func (s *Session) rpc(method string, params any) (json.RawMessage, error) {
	if s.failed != nil {
		return nil, s.failed
	}
	s.nextID++
	id := s.nextID
	if err := s.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return nil, s.broken(err)
	}
	line, err := s.rd.ReadSlice('\n')
	if err != nil {
		return nil, s.broken(err)
	}
	var f frame
	switch err := json.Unmarshal(line, &f); {
	case err != nil:
		return nil, s.protocol("a line that is not JSON-RPC: " + clip(string(line)))
	case f.Method != "":
		return nil, s.protocol("a request or notification of the server's own: " + clip(f.Method))
	case string(f.ID) != strconv.Itoa(id):
		return nil, s.protocol("a reply to request " + clip(string(f.ID)) + ", not " + strconv.Itoa(id))
	case f.Error != nil:
		return nil, s.protocol(fmt.Sprintf("a JSON-RPC error: code %d: %s", f.Error.Code, clip(f.Error.Message)))
	}
	return f.Result, nil
}

func (s *Session) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode a request: %w", err)
	}
	if _, err := s.in.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("write to the server: %w", err)
	}
	return nil
}

func (s *Session) protocol(what string) error {
	s.fail()
	return fmt.Errorf("%w: %s", ErrProtocol, what)
}

// broken classifies failed I/O by deadline, frame limit, then server death.
func (s *Session) broken(err error) error {
	s.fail()
	if cerr := s.ctx.Err(); cerr != nil {
		return fmt.Errorf("%w: %w", ErrTimeout, cerr)
	}
	if errors.Is(err, bufio.ErrBufferFull) {
		return fmt.Errorf("%w: reply exceeds the %d MiB line limit", ErrProtocol, maxReply>>20)
	}
	select {
	case <-s.exited:
		if s.waitErr != nil {
			err = s.waitErr
		}
	case <-time.After(waitDelay):
	}
	return fmt.Errorf("%w: %w", ErrServerDied, err)
}

// fail ends the session: the group is killed and later calls fail.
func (s *Session) fail() {
	if s.failed == nil {
		s.failed = fmt.Errorf("%w: the session has ended", ErrServerDied)
	}
	s.kill()
}

// kill signals the group and closes both MCP pipes to free blocked I/O.
func (s *Session) kill() {
	s.once.Do(func() {
		_ = s.life.Close()
		if err := syscall.Kill(-s.guard.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			s.killErr = fmt.Errorf("kill the server's process group: %w", err)
		}
		_, _ = s.in.Close(), s.out.Close()
	})
}

// Close gives stdin EOF time to stop the server before group kill and is safe to repeat.
func (s *Session) Close() error {
	if s.failed == nil {
		s.failed = fmt.Errorf("%w: the session is closed", ErrServerDied)
	}
	_ = s.in.Close()
	select {
	case <-s.exited:
	case <-time.After(grace):
	}
	s.kill()
	<-s.exited
	return s.killErr
}
