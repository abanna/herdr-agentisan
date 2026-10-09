// Package herdr is a client for herdr's socket API.
//
// The wire format is newline-delimited JSON over a unix socket, one request
// per connection: herdr closes the connection after answering anything that
// is not a subscription. Request and response shapes follow `herdr api schema
// --json` (protocol 22). Only the methods this plugin calls are modelled.
package herdr

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync/atomic"
	"time"
)

// Sentinel errors. Callers branch on these with errors.Is.
var (
	// ErrNoSocket means no socket path was configured. Outside herdr,
	// HERDR_SOCKET_PATH is unset.
	ErrNoSocket = errors.New("herdr socket path is not set")
	// ErrUnavailable means the socket could not be reached or the connection
	// failed mid-call.
	ErrUnavailable = errors.New("herdr is unavailable")
	// ErrProtocol means herdr answered with something the schema does not allow.
	ErrProtocol = errors.New("herdr protocol violation")
	// ErrAPI means herdr answered with an error body; see APIError.
	ErrAPI = errors.New("herdr returned an error")
	// ErrPaneNotFound means herdr has no pane by the id it was given. It is an
	// ErrAPI too: an APIError whose code is pane_not_found matches both.
	ErrPaneNotFound = errors.New("herdr pane not found")
)

// codePaneNotFound is herdr's error code for an id that names no pane.
const codePaneNotFound = "pane_not_found"

// maxResponseBytes bounds one response line. The largest response this client
// asks for is a few hundred bytes; the cap stops a misbehaving peer from
// growing the buffer without limit.
const maxResponseBytes = 1 << 20

// APIError is herdr's {"error":{"code","message"}} body. It unwraps to ErrAPI.
type APIError struct {
	Method  string
	Code    string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("herdr %s: %s: %s", e.Method, e.Code, e.Message)
}

// Unwrap lets errors.Is(err, ErrAPI) match.
func (e *APIError) Unwrap() error { return ErrAPI }

// Is lets errors.Is(err, ErrPaneNotFound) match herdr's pane_not_found code,
// so callers never compare code strings themselves.
func (e *APIError) Is(target error) bool {
	return target == ErrPaneNotFound && e.Code == codePaneNotFound
}

// Client talks to one herdr socket. The zero value has no socket configured.
type Client struct {
	SocketPath string
}

// requestSeq makes request ids unique within a process.
var requestSeq atomic.Uint64

type request struct {
	ID     string `json:"id"`
	Method string `json:"method"`
	Params any    `json:"params"`
}

type response struct {
	ID     string          `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// Call sends one request and decodes its result into out. params must
// marshal to a JSON object: the schema requires it even when empty.
func (c Client) Call(ctx context.Context, method string, params, out any) error {
	if c.SocketPath == "" {
		return ErrNoSocket
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", c.SocketPath)
	if err != nil {
		return fmt.Errorf("%w: dial %s: %w", ErrUnavailable, c.SocketPath, err)
	}
	defer conn.Close() //nolint:errcheck // one-shot connection; the response is already read or the call failed

	// The context bounds the whole exchange, not just the dial: a herdr that
	// accepts and never answers must not hold a plugin action open.
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()

	id := "agentisan-" + strconv.FormatUint(requestSeq.Add(1), 10)
	line, err := json.Marshal(request{ID: id, Method: method, Params: params})
	if err != nil {
		return fmt.Errorf("encode %s request: %w", method, err)
	}
	if _, err := conn.Write(append(line, '\n')); err != nil {
		return c.ioError(ctx, method, err)
	}

	raw, err := readLine(conn)
	if err != nil {
		if errors.Is(err, ErrProtocol) {
			return fmt.Errorf("%s: %w", method, err)
		}
		return c.ioError(ctx, method, err)
	}

	var resp response
	if err := json.Unmarshal(raw, &resp); err != nil {
		return fmt.Errorf("%w: %s: decode response: %w", ErrProtocol, method, err)
	}
	if resp.ID != id {
		return fmt.Errorf("%w: %s: response id %q does not match request id %q", ErrProtocol, method, resp.ID, id)
	}
	if resp.Error != nil {
		return &APIError{Method: method, Code: resp.Error.Code, Message: resp.Error.Message}
	}
	if len(resp.Result) == 0 || string(resp.Result) == "null" {
		return fmt.Errorf("%w: %s: response has neither result nor error", ErrProtocol, method)
	}
	if err := json.Unmarshal(resp.Result, out); err != nil {
		return fmt.Errorf("%w: %s: decode result: %w", ErrProtocol, method, err)
	}
	return nil
}

// ioError reports a failed exchange, preferring the context's own error when
// the context is what ended it.
func (c Client) ioError(ctx context.Context, method string, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("%s: %w", method, ctxErr)
	}
	return fmt.Errorf("%w: %s: %w", ErrUnavailable, method, err)
}

// readLine reads one newline-terminated line of at most maxResponseBytes.
// herdr closes the connection after answering, so EOF after a complete line
// is normal and EOF before any byte is a failure.
func readLine(r io.Reader) ([]byte, error) {
	br := bufio.NewReader(io.LimitReader(r, maxResponseBytes+1))
	line, err := br.ReadBytes('\n')
	if len(line) > maxResponseBytes {
		return nil, fmt.Errorf("%w: response exceeds %d bytes", ErrProtocol, maxResponseBytes)
	}
	if err != nil && (!errors.Is(err, io.EOF) || len(line) == 0) {
		return nil, err //nolint:wrapcheck // the caller wraps with the method and sentinel
	}
	return line, nil
}

// Pong is the result of ping.
type Pong struct {
	Version  string `json:"version"`
	Protocol uint32 `json:"protocol"`
}

// Ping checks herdr is reachable and reports its version and protocol.
func (c Client) Ping(ctx context.Context) (Pong, error) {
	var out struct {
		Type string `json:"type"`
		Pong
	}
	if err := c.Call(ctx, "ping", struct{}{}, &out); err != nil {
		return Pong{}, err
	}
	if out.Type != "pong" {
		return Pong{}, fmt.Errorf("%w: ping: result type %q, want \"pong\"", ErrProtocol, out.Type)
	}
	return out.Pong, nil
}

// Notification is the payload of notification.show. Optional fields are
// omitted when empty, because herdr validates the enums it is sent.
type Notification struct {
	Title string `json:"title"`
	Body  string `json:"body,omitempty"`
	// Sound is "none", "done" or "request".
	Sound string `json:"sound,omitempty"`
	// Position is "top-left", "top-right", "bottom-left" or "bottom-right".
	Position string `json:"position,omitempty"`
}

// NotificationResult reports whether herdr displayed the toast. Shown=false
// is not an error: Reason says why ("disabled", "rate_limited",
// "no_foreground_client", "busy").
type NotificationResult struct {
	Shown  bool   `json:"shown"`
	Reason string `json:"reason"`
}

// PaneMetadata is the payload of pane.report_metadata, limited to the token
// fields this plugin sends.
type PaneMetadata struct {
	PaneID string `json:"pane_id"`
	// Source names the writer. herdr keeps one value per key and the last
	// write wins across sources, so every key needs exactly one writer.
	Source string `json:"source"`
	// Tokens maps key to value. An empty value clears the key.
	Tokens map[string]string `json:"tokens,omitempty"`
	// TTLMillis expires the tokens. Zero is omitted, which herdr reads as
	// "never expires"; herdr rejects an explicit 0 and anything over 24 h.
	TTLMillis uint64 `json:"ttl_ms,omitempty"`
}

// ReportPaneMetadata sets tokens on a pane. herdr answers ok without applying
// a report it considers stale or blocked, so success means accepted, not
// necessarily displayed.
func (c Client) ReportPaneMetadata(ctx context.Context, m PaneMetadata) error {
	var out struct {
		Type string `json:"type"`
	}
	if err := c.Call(ctx, "pane.report_metadata", m, &out); err != nil {
		return err
	}
	if out.Type != "ok" {
		return fmt.Errorf("%w: pane.report_metadata: result type %q, want \"ok\"", ErrProtocol, out.Type)
	}
	return nil
}

// ShowNotification asks herdr to display a toast.
func (c Client) ShowNotification(ctx context.Context, n Notification) (NotificationResult, error) {
	var out struct {
		Type string `json:"type"`
		NotificationResult
	}
	if err := c.Call(ctx, "notification.show", n, &out); err != nil {
		return NotificationResult{}, err
	}
	if out.Type != "notification_show" {
		return NotificationResult{}, fmt.Errorf("%w: notification.show: result type %q, want \"notification_show\"", ErrProtocol, out.Type)
	}
	return out.NotificationResult, nil
}

// PaneInfo is one pane in pane.list, limited to the fields this plugin reads.
type PaneInfo struct {
	PaneID string `json:"pane_id"`
}

// ListPanes returns every pane in every workspace.
func (c Client) ListPanes(ctx context.Context) ([]PaneInfo, error) {
	var out struct {
		Type  string     `json:"type"`
		Panes []PaneInfo `json:"panes"`
	}
	if err := c.Call(ctx, "pane.list", struct{}{}, &out); err != nil {
		return nil, err
	}
	if out.Type != "pane_list" {
		return nil, fmt.Errorf("%w: pane.list: result type %q, want \"pane_list\"", ErrProtocol, out.Type)
	}
	if out.Panes == nil {
		return nil, fmt.Errorf("%w: pane.list: result has no panes", ErrProtocol)
	}
	for _, p := range out.Panes {
		if p.PaneID == "" {
			return nil, fmt.Errorf("%w: pane.list: a pane has no pane_id", ErrProtocol)
		}
	}
	return out.Panes, nil
}

// Process is one process in a pane's foreground job.
type Process struct {
	PID  uint32 `json:"pid"`
	Name string `json:"name"`
}

// ProcessInfo is the result of pane.process_info, limited to the processes
// this plugin matches on. herdr reports a pid it does not know as null, which
// decodes to 0: no process has pid 0.
type ProcessInfo struct {
	// PaneID is the pane's current id, even when the request named an alias.
	PaneID string `json:"pane_id"`
	// ShellPID is the process herdr spawned for the pane.
	ShellPID uint32 `json:"shell_pid"`
	// ForegroundProcessGroupID is the pane terminal's foreground job.
	ForegroundProcessGroupID uint32    `json:"foreground_process_group_id"`
	ForegroundProcesses      []Process `json:"foreground_processes"`
}

// PaneProcessInfo returns the processes running in one pane. paneID is always
// sent as a string: herdr reads a null pane_id as the focused pane, which is
// never what a caller naming a pane means.
func (c Client) PaneProcessInfo(ctx context.Context, paneID string) (ProcessInfo, error) {
	params := struct {
		PaneID string `json:"pane_id"`
	}{PaneID: paneID}
	var out struct {
		Type        string       `json:"type"`
		ProcessInfo *ProcessInfo `json:"process_info"`
	}
	if err := c.Call(ctx, "pane.process_info", params, &out); err != nil {
		return ProcessInfo{}, err
	}
	if out.Type != "pane_process_info" {
		return ProcessInfo{}, fmt.Errorf("%w: pane.process_info: result type %q, want \"pane_process_info\"", ErrProtocol, out.Type)
	}
	if out.ProcessInfo == nil || out.ProcessInfo.PaneID == "" {
		return ProcessInfo{}, fmt.Errorf("%w: pane.process_info: result has no process_info.pane_id", ErrProtocol)
	}
	return *out.ProcessInfo, nil
}
