// Package herdr is a client for herdr's socket API.
//
// The wire format is newline-delimited JSON over a unix socket, one request
// per connection: herdr closes the connection after answering anything that
// is not a subscription, and keeps a subscription's open to stream its events
// (Subscribe). Request and response shapes follow `herdr api schema --json`
// (protocol 22). Only the methods this plugin calls are modelled.
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
	// ErrEventsLost means a subscriber fell further behind than herdr's
	// event history reaches, and herdr ended its subscription. The caller
	// resubscribes and rereads the state it follows (ADR-001 A3). It is an
	// ErrAPI too: an APIError whose code is events_lost matches both.
	ErrEventsLost = errors.New("herdr dropped events the subscriber had not read")
)

// herdr's error codes the client gives a sentinel of their own.
const (
	// codePaneNotFound is the code for an id that names no pane.
	codePaneNotFound = "pane_not_found"
	// codeEventsLost is the code for a subscriber that fell behind.
	codeEventsLost = "events_lost"
)

// maxResponseBytes bounds one response line. The largest response this client
// asks for is pane.list, which the daemon's team poll reads every 3 s: about
// 20 KB for a realistic layout, and up to about 12 KB per pane at herdr's
// token limits. The cap leaves room for that and stops a misbehaving peer
// from growing the buffer without limit.
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
// and errors.Is(err, ErrEventsLost) its events_lost code, so callers never
// compare code strings themselves.
func (e *APIError) Is(target error) bool {
	if target == ErrPaneNotFound {
		return e.Code == codePaneNotFound
	}
	if target == ErrEventsLost {
		return e.Code == codeEventsLost
	}
	return false
}

// CallTimeout bounds every herdr call a Client makes unless its Timeout says
// otherwise. A plugin action holds one of herdr's in-flight slots until it
// exits, and a daemon runs for days, so neither may wait forever on a herdr
// that accepts the connection and never answers. A caller's earlier deadline
// still wins.
const CallTimeout = 5 * time.Second

// Client talks to one herdr socket. The zero value has no socket configured.
type Client struct {
	SocketPath string
	// Timeout bounds each call; zero means CallTimeout.
	Timeout time.Duration
	// Dialed, when set, runs on every connection once it is made and before
	// anything is written to it; an error ends the call with nothing sent.
	// The daemon uses it to prove the socket path still names its own
	// server: the path can be replaced between a check and the dial, and a
	// request must not reach the replacement (ADR-001 A3).
	Dialed func() error
}

// dialed runs the Dialed hook, if any, on a fresh connection.
func (c Client) dialed(method string) error {
	if c.Dialed == nil {
		return nil
	}
	if err := c.Dialed(); err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	return nil
}

// timeout is the bound for one call.
func (c Client) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return CallTimeout
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
	ctx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", c.SocketPath)
	if err != nil {
		return fmt.Errorf("%w: dial %s: %w", ErrUnavailable, c.SocketPath, err)
	}
	defer conn.Close() //nolint:errcheck // one-shot connection; the response is already read or the call failed
	if err := c.dialed(method); err != nil {
		return err
	}

	// The context bounds the whole exchange, not just the dial: a herdr that
	// accepts and never answers must not hold a plugin action open.
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()

	id := nextID()
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
	return decodeResponse(method, id, raw, out)
}

// nextID returns a request id unique within this process.
func nextID() string { return "agentisan-" + strconv.FormatUint(requestSeq.Add(1), 10) }

// decodeResponse decodes the response line raw to the request id into out.
func decodeResponse(method, id string, raw []byte, out any) error {
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
	PaneID      string `json:"pane_id"`
	WorkspaceID string `json:"workspace_id"`
	// Focused is true for the one pane that has the user's focus: the
	// focused pane of the active tab of the active workspace. Every other
	// pane, including the focused pane of a background tab, is false.
	Focused bool `json:"focused"`
	// Agent is the agent herdr detected in the pane, such as "claude"; ""
	// for a plain shell (null in herdr's schema).
	Agent string `json:"agent"`
	// AgentStatus is "idle", "working", "blocked", "done" or "unknown" in
	// herdr 0.9.3, kept as sent: a newer herdr may add one.
	AgentStatus string `json:"agent_status"`
	// Tokens are the pane's metadata tokens; nil when it has none.
	Tokens map[string]string `json:"tokens"`
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

// AgentInfo is one agent in an agent_info result, limited to the fields this
// plugin reads. Name is null in herdr's schema for an unnamed agent, which
// decodes to "".
type AgentInfo struct {
	// PaneID is the agent's current pane id, even when the target named it
	// by an alias.
	PaneID string `json:"pane_id"`
	Name   string `json:"name"`
}

// FocusAgent focuses the agent herdr knows as target (its name or pane id),
// switching workspace and tab as needed. herdr 0.9.3 answers agent.focus with
// an agent_info result; `herdr api schema` lists that type but does not map
// it to the method, so the type was read from herdr's handle_agent_focus.
func (c Client) FocusAgent(ctx context.Context, target string) (AgentInfo, error) {
	params := struct {
		Target string `json:"target"`
	}{Target: target}
	var out struct {
		Type  string     `json:"type"`
		Agent *AgentInfo `json:"agent"`
	}
	if err := c.Call(ctx, "agent.focus", params, &out); err != nil {
		return AgentInfo{}, err
	}
	if out.Type != "agent_info" {
		return AgentInfo{}, fmt.Errorf("%w: agent.focus: result type %q, want \"agent_info\"", ErrProtocol, out.Type)
	}
	if out.Agent == nil || out.Agent.PaneID == "" {
		return AgentInfo{}, fmt.Errorf("%w: agent.focus: result has no agent.pane_id", ErrProtocol)
	}
	return *out.Agent, nil
}

// PaneZoom is the result of pane.zoom, limited to the fields this plugin
// reads. Zoomed=false is not an error: Reason says why ("single_pane" for a
// pane alone in its tab).
type PaneZoom struct {
	// PaneID is the pane's current id, even when the request named an alias.
	PaneID  string `json:"pane_id"`
	Zoomed  bool   `json:"zoomed"`
	Changed bool   `json:"changed"`
	// Reason is null in herdr's schema when the zoom applied, which decodes
	// to "".
	Reason string `json:"reason"`
}

// ZoomPane zooms one pane on; an already zoomed pane stays zoomed. paneID is
// always sent as a string: herdr reads a null pane_id as the focused pane,
// which is never what a caller naming a pane means.
func (c Client) ZoomPane(ctx context.Context, paneID string) (PaneZoom, error) {
	return c.zoomPane(ctx, paneID, "on")
}

// UnzoomPane zooms one pane's tab off. herdr focuses the pane first whatever
// the mode (apply_pane_zoom, src/app/actions.rs:822 at v0.9.3). A tab not
// zoomed answers Reason "already_unzoomed" (actions.rs:843): no error.
func (c Client) UnzoomPane(ctx context.Context, paneID string) (PaneZoom, error) {
	return c.zoomPane(ctx, paneID, "off")
}

func (c Client) zoomPane(ctx context.Context, paneID, mode string) (PaneZoom, error) {
	params := struct {
		PaneID string `json:"pane_id"`
		Mode   string `json:"mode"`
	}{PaneID: paneID, Mode: mode}
	var out struct {
		Type string    `json:"type"`
		Zoom *PaneZoom `json:"zoom"`
	}
	if err := c.Call(ctx, "pane.zoom", params, &out); err != nil {
		return PaneZoom{}, err
	}
	if out.Type != "pane_zoom" {
		return PaneZoom{}, fmt.Errorf("%w: pane.zoom: result type %q, want \"pane_zoom\"", ErrProtocol, out.Type)
	}
	if out.Zoom == nil || out.Zoom.PaneID == "" {
		return PaneZoom{}, fmt.Errorf("%w: pane.zoom: result has no zoom.pane_id", ErrProtocol)
	}
	return *out.Zoom, nil
}

// FocusPane focuses one pane, switching workspace and tab, and returns it
// (src/app/api/panes.rs:484-500 at v0.9.3). If focus changed, herdr streams
// pane.focused after answering (src/server/headless.rs:467, api.rs:842-873).
func (c Client) FocusPane(ctx context.Context, paneID string) (PaneInfo, error) {
	params := struct {
		PaneID string `json:"pane_id"`
	}{PaneID: paneID}
	var out struct {
		Type string    `json:"type"`
		Pane *PaneInfo `json:"pane"`
	}
	if err := c.Call(ctx, "pane.focus", params, &out); err != nil {
		return PaneInfo{}, err
	}
	if out.Type != "pane_info" {
		return PaneInfo{}, fmt.Errorf("%w: pane.focus: result type %q, want \"pane_info\"", ErrProtocol, out.Type)
	}
	if out.Pane == nil || out.Pane.PaneID == "" {
		return PaneInfo{}, fmt.Errorf("%w: pane.focus: result has no pane.pane_id", ErrProtocol)
	}
	return *out.Pane, nil
}

// PluginPopup opens one of a plugin's manifest [[panes]] entrypoints as a
// herdr popup: a session-modal terminal over the active pane that leaves the
// tab layout alone and closes when its command exits.
type PluginPopup struct {
	PluginID   string `json:"plugin_id"`
	Entrypoint string `json:"entrypoint"`
	// Width and Height are the popup's outer size, in cells ("80") or a
	// percentage of the terminal ("92%"). Empty leaves the manifest's size,
	// or herdr's default of half the terminal.
	Width  string `json:"width,omitempty"`
	Height string `json:"height,omitempty"`
}

// OpenPluginPopup asks herdr to run a plugin pane in a popup. herdr runs the
// manifest's argv itself; the plugin starts no process. A popup has no pane
// id, so herdr answers a bare ok.
func (c Client) OpenPluginPopup(ctx context.Context, p PluginPopup) error {
	params := struct {
		PluginPopup
		Placement string `json:"placement"`
	}{PluginPopup: p, Placement: "popup"}
	var out struct {
		Type string `json:"type"`
	}
	if err := c.Call(ctx, "plugin.pane.open", params, &out); err != nil {
		return err
	}
	if out.Type != "ok" {
		return fmt.Errorf("%w: plugin.pane.open: result type %q, want \"ok\"", ErrProtocol, out.Type)
	}
	return nil
}
