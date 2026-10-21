package herdr

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// The calls the workforce commands make (ADR-001 D11, A23): open a worker's
// tab or its team's workspace, start its agent and watch it come up, and
// close its pane. Every shape is herdr 0.9.3's (src/api/schema/*.rs,
// src/app/api/*.rs).

// Sentinel errors for herdr error codes the workforce branches on. Each is an
// ErrAPI too: an APIError with that code matches both.
var (
	// ErrPaneBusy means agent.start found the pane taken: it already hosts
	// an agent or a launch still pending (src/app/agents.rs:187-188), or no
	// shell is at its foreground, one still starting or a program in its
	// place (agent_pane_busy, src/app/agents.rs:194-196).
	ErrPaneBusy = errors.New("herdr pane is not an available shell")
	// ErrNameTaken means another terminal already has the agent name
	// (agent_name_taken).
	ErrNameTaken = errors.New("herdr agent name is taken")
	// ErrAgentNotFound means no agent answers to the target
	// (agent_not_found).
	ErrAgentNotFound = errors.New("herdr agent not found")
	// ErrWaitTimeout means pane.wait_for_output saw no match in time
	// (timeout, src/api/wait.rs:114-126).
	ErrWaitTimeout = errors.New("herdr wait timed out")
)

// workforceCodes maps the codes above to their sentinels; APIError.Is reads
// it.
var workforceCodes = map[string]error{
	"agent_pane_busy":  ErrPaneBusy,
	"agent_name_taken": ErrNameTaken,
	"agent_not_found":  ErrAgentNotFound,
	"timeout":          ErrWaitTimeout,
}

// Created is a new tab, or a new workspace's first tab, and the pane herdr
// opened in it, with its terminal.
type Created struct {
	WorkspaceID string
	TabID       string
	RootPane    PaneInfo
}

// TabCreate opens a tab in a workspace, never focused: a worker's tab must not
// pull the user away from what they are looking at.
type TabCreate struct {
	WorkspaceID string
	Cwd         string
	Label       string
	// Env is set in the tab's shell.
	Env map[string]string
}

// tabInfo is the tab part of a tab_created or workspace_created result.
type tabInfo struct {
	TabID       string `json:"tab_id"`
	WorkspaceID string `json:"workspace_id"`
}

// CreateTab opens one tab and returns it with its root pane: tab.create,
// answered with tab_created {tab, root_pane}.
func (c Client) CreateTab(ctx context.Context, t TabCreate) (Created, error) {
	params := struct {
		WorkspaceID string            `json:"workspace_id"`
		Cwd         string            `json:"cwd,omitempty"`
		Label       string            `json:"label,omitempty"`
		Focus       bool              `json:"focus"`
		Env         map[string]string `json:"env,omitempty"`
	}{WorkspaceID: t.WorkspaceID, Cwd: t.Cwd, Label: t.Label, Env: t.Env}
	var out struct {
		Type     string    `json:"type"`
		Tab      *tabInfo  `json:"tab"`
		RootPane *PaneInfo `json:"root_pane"`
	}
	if err := c.Call(ctx, "tab.create", params, &out); err != nil {
		return Created{}, err
	}
	return created("tab.create", "tab_created", out.Type, out.Tab, out.RootPane)
}

// WorkspaceCreate opens a workspace, not focused: herdr switches to it only
// when no workspace is active (src/app/creation.rs:155).
type WorkspaceCreate struct {
	Cwd   string
	Label string
	// Env is set in the first tab's shell.
	Env map[string]string
}

// CreateWorkspace opens one workspace and returns its first tab and root
// pane: workspace.create, answered with workspace_created {workspace, tab,
// root_pane}.
func (c Client) CreateWorkspace(ctx context.Context, w WorkspaceCreate) (Created, error) {
	params := struct {
		Cwd   string            `json:"cwd,omitempty"`
		Label string            `json:"label,omitempty"`
		Focus bool              `json:"focus"`
		Env   map[string]string `json:"env,omitempty"`
	}{Cwd: w.Cwd, Label: w.Label, Env: w.Env}
	var out struct {
		Type     string    `json:"type"`
		Tab      *tabInfo  `json:"tab"`
		RootPane *PaneInfo `json:"root_pane"`
	}
	if err := c.Call(ctx, "workspace.create", params, &out); err != nil {
		return Created{}, err
	}
	return created("workspace.create", "workspace_created", out.Type, out.Tab, out.RootPane)
}

func created(method, want, typ string, tab *tabInfo, root *PaneInfo) (Created, error) {
	if typ != want {
		return Created{}, fmt.Errorf("%w: %s: result type %q, want %q", ErrProtocol, method, typ, want)
	}
	if tab == nil || tab.TabID == "" || tab.WorkspaceID == "" {
		return Created{}, fmt.Errorf("%w: %s: result has no tab.tab_id or tab.workspace_id", ErrProtocol, method)
	}
	if root == nil || root.PaneID == "" || root.TerminalID == "" {
		return Created{}, fmt.Errorf("%w: %s: result has no root_pane.pane_id or root_pane.terminal_id", ErrProtocol, method)
	}
	return Created{WorkspaceID: tab.WorkspaceID, TabID: tab.TabID, RootPane: *root}, nil
}

// RenameTab sets a tab's label: tab.rename, answered with tab_info.
func (c Client) RenameTab(ctx context.Context, tabID, label string) error {
	params := struct {
		TabID string `json:"tab_id"`
		Label string `json:"label"`
	}{TabID: tabID, Label: label}
	var out struct {
		Type string   `json:"type"`
		Tab  *tabInfo `json:"tab"`
	}
	if err := c.Call(ctx, "tab.rename", params, &out); err != nil {
		return err
	}
	if out.Type != "tab_info" || out.Tab == nil || out.Tab.TabID == "" {
		return fmt.Errorf("%w: tab.rename: result type %q with no tab, want \"tab_info\"", ErrProtocol, out.Type)
	}
	return nil
}

// Agent is one agent terminal as agent.start and agent.get report it,
// limited to what the readiness wait reads.
type Agent struct {
	PaneID string `json:"pane_id"`
	// TerminalID is stable for the terminal's life, where a pane id can be
	// reissued; the wait pins it.
	TerminalID string `json:"terminal_id"`
	// Name is omitted by herdr for an unnamed agent, which decodes to "".
	Name string `json:"name"`
	// Agent is the agent herdr detects, such as "claude"; "" before it has
	// detected one.
	Agent string `json:"agent"`
	// AgentStatus is "idle", "working", "blocked", "done" or "unknown".
	AgentStatus      string `json:"agent_status"`
	InteractiveReady bool   `json:"interactive_ready"`
	LaunchPending    bool   `json:"launch_pending"`
}

// AgentStart asks herdr to launch an agent in a shell pane.
type AgentStart struct {
	// Name is the agent's herdr name.
	Name string
	// Kind is herdr's agent kind, "claude" or "codex": herdr runs its own
	// executable for it from the shell's PATH.
	Kind   string
	PaneID string
	// Args follow the executable.
	Args []string
	// Timeout is herdr's own startup timeout: over 3 s, at most 300 s
	// (src/app/agents.rs:202-209). 0 sends none, so herdr's 30 s applies.
	Timeout time.Duration
}

// StartAgent types the agent's command line into the pane's shell and
// returns at once, before the agent is up: agent.start, answered with
// agent_started {agent, argv}, with the launch pending. ErrPaneBusy means the
// pane was taken (see ErrPaneBusy).
func (c Client) StartAgent(ctx context.Context, a AgentStart) (Agent, error) {
	params := struct {
		Name      string   `json:"name"`
		Kind      string   `json:"kind"`
		PaneID    string   `json:"pane_id"`
		Args      []string `json:"args,omitempty"`
		TimeoutMS int64    `json:"timeout_ms,omitempty"`
	}{Name: a.Name, Kind: a.Kind, PaneID: a.PaneID, Args: a.Args, TimeoutMS: a.Timeout.Milliseconds()}
	var out struct {
		Type  string `json:"type"`
		Agent *Agent `json:"agent"`
	}
	if err := c.Call(ctx, "agent.start", params, &out); err != nil {
		return Agent{}, err
	}
	return agentResult("agent.start", "agent_started", out.Type, out.Agent)
}

// GetAgent returns the agent herdr knows as target, its name or pane id:
// agent.get, answered with agent_info.
func (c Client) GetAgent(ctx context.Context, target string) (Agent, error) {
	params := struct {
		Target string `json:"target"`
	}{Target: target}
	var out struct {
		Type  string `json:"type"`
		Agent *Agent `json:"agent"`
	}
	if err := c.Call(ctx, "agent.get", params, &out); err != nil {
		return Agent{}, err
	}
	return agentResult("agent.get", "agent_info", out.Type, out.Agent)
}

func agentResult(method, want, typ string, a *Agent) (Agent, error) {
	if typ != want {
		return Agent{}, fmt.Errorf("%w: %s: result type %q, want %q", ErrProtocol, method, typ, want)
	}
	if a == nil || a.PaneID == "" || a.TerminalID == "" {
		return Agent{}, fmt.Errorf("%w: %s: result has no agent.pane_id or agent.terminal_id", ErrProtocol, method)
	}
	return *a, nil
}

// GetPane reads one pane: pane.get, answered with pane_info. An id herdr
// does not have is ErrPaneNotFound.
func (c Client) GetPane(ctx context.Context, paneID string) (PaneInfo, error) {
	return c.paneCall(ctx, "pane.get", paneID)
}

// ClosePane closes one pane, and its tab or workspace with it when it was
// the last there (A15): pane.close, answered with ok.
func (c Client) ClosePane(ctx context.Context, paneID string) error {
	return c.callOK(ctx, "pane.close", struct {
		PaneID string `json:"pane_id"`
	}{PaneID: paneID})
}

// SendKeys sends key presses to a pane, as herdr's key names ("enter",
// "esc", "ctrl+c"): pane.send_keys, answered with ok.
func (c Client) SendKeys(ctx context.Context, paneID string, keys []string) error {
	return c.callOK(ctx, "pane.send_keys", struct {
		PaneID string   `json:"pane_id"`
		Keys   []string `json:"keys"`
	}{PaneID: paneID, Keys: keys})
}

func (c Client) callOK(ctx context.Context, method string, params any) error {
	var out struct {
		Type string `json:"type"`
	}
	if err := c.Call(ctx, method, params, &out); err != nil {
		return err
	}
	if out.Type != "ok" {
		return fmt.Errorf("%w: %s: result type %q, want \"ok\"", ErrProtocol, method, out.Type)
	}
	return nil
}

// WaitForOutput waits until a line of the pane's visible screen matches the
// regular expression pattern (herdr's regex crate, line by line,
// src/api/subscriptions.rs:21-37) and returns that line: pane.wait_for_output,
// answered with output_matched. herdr trims each row's trailing white space
// and drops trailing blank rows, and answers the first matching line from the
// top: a pattern that needs trailing white space (`\$ $`) never matches, and
// an older matching line still on the screen ends the wait at once. herdr
// holds the call open until then, so the client's own timeout is added to
// timeout to cover the wait and herdr's own late answer; a shorter deadline
// in ctx still ends it. ErrWaitTimeout means no line matched within timeout.
func (c Client) WaitForOutput(ctx context.Context, paneID, pattern string, timeout time.Duration) (string, error) {
	params := struct {
		PaneID    string `json:"pane_id"`
		Source    string `json:"source"`
		Match     any    `json:"match"`
		TimeoutMS int64  `json:"timeout_ms"`
	}{
		PaneID: paneID, Source: "visible", TimeoutMS: max(timeout.Milliseconds(), 1),
		Match: struct {
			Type  string `json:"type"`
			Value string `json:"value"`
		}{Type: "regex", Value: pattern},
	}
	var out struct {
		Type        string  `json:"type"`
		MatchedLine *string `json:"matched_line"`
	}
	c.Timeout = timeout + c.timeout()
	if err := c.Call(ctx, "pane.wait_for_output", params, &out); err != nil {
		return "", err
	}
	if out.Type != "output_matched" || out.MatchedLine == nil {
		return "", fmt.Errorf("%w: pane.wait_for_output: result type %q with no matched_line, want \"output_matched\"", ErrProtocol, out.Type)
	}
	return *out.MatchedLine, nil
}
