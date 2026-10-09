// Package plugin is the plugin's domain: what herdr hands a plugin process
// (its runtime environment and manifest) and what the plugin does with it.
//
// Commands in internal/cli parse and render; the rules live here, so an
// action, an event hook and a test all see the same behaviour.
package plugin

import (
	"encoding/json"
	"errors"
	"fmt"
)

// ErrInvalidEnv means herdr's runtime variables were present but unusable.
var ErrInvalidEnv = errors.New("invalid herdr plugin environment")

// Env is the runtime environment herdr injects into every plugin process.
// Outside herdr every field is empty; that is not an error, because the
// binary can also be run by hand.
type Env struct {
	// InHerdr is true when HERDR_ENV=1.
	InHerdr bool
	// SocketPath and BinPath are how the plugin talks back to herdr.
	SocketPath string
	BinPath    string
	// PluginID, Root, ConfigDir and StateDir describe the plugin itself.
	// Root is a managed checkout that may be replaced: keep config in
	// ConfigDir and runtime state in StateDir, never in Root.
	PluginID  string
	Root      string
	ConfigDir string
	StateDir  string
	// ActionID is set for manifest actions; EventName for startup and event
	// hooks.
	ActionID  string
	EventName string
	// Context is HERDR_PLUGIN_CONTEXT_JSON, nil when absent.
	Context *InvocationContext
	// Event is HERDR_PLUGIN_EVENT_JSON, nil when absent.
	Event *Event
}

// InvocationContext is where herdr was focused when it invoked the plugin.
// Every field is nullable in herdr's schema; null decodes to "". Unknown
// fields are ignored on purpose: herdr adds fields between versions, and a
// plugin that rejected them would break on every herdr upgrade.
type InvocationContext struct {
	InvocationSource  string `json:"invocation_source"`
	CorrelationID     string `json:"correlation_id"`
	WorkspaceID       string `json:"workspace_id"`
	WorkspaceLabel    string `json:"workspace_label"`
	WorkspaceCwd      string `json:"workspace_cwd"`
	TabID             string `json:"tab_id"`
	TabLabel          string `json:"tab_label"`
	FocusedPaneID     string `json:"focused_pane_id"`
	FocusedPaneCwd    string `json:"focused_pane_cwd"`
	FocusedPaneAgent  string `json:"focused_pane_agent"`
	FocusedPaneStatus string `json:"focused_pane_status"`
	Worktree          string `json:"worktree"`
	SelectedText      string `json:"selected_text"`
	ClickedURL        string `json:"clicked_url"`
	LinkHandlerID     string `json:"link_handler_id"`
}

// Event is an event-hook payload: {"event": <kind>, "data": {...}}. Data is
// kept raw because its shape depends on the kind.
type Event struct {
	Kind string          `json:"event"`
	Data json.RawMessage `json:"data"`
}

// EnvFrom reads the runtime environment through lookup (os.LookupEnv in
// production). Taking the lookup rather than reading the process environment
// keeps tests from ever seeing a developer's live herdr variables.
func EnvFrom(lookup func(string) (string, bool)) (Env, error) {
	get := func(k string) string {
		v, _ := lookup(k)
		return v
	}
	env := Env{
		InHerdr:    get("HERDR_ENV") == "1",
		SocketPath: get("HERDR_SOCKET_PATH"),
		BinPath:    get("HERDR_BIN_PATH"),
		PluginID:   get("HERDR_PLUGIN_ID"),
		Root:       get("HERDR_PLUGIN_ROOT"),
		ConfigDir:  get("HERDR_PLUGIN_CONFIG_DIR"),
		StateDir:   get("HERDR_PLUGIN_STATE_DIR"),
		ActionID:   get("HERDR_PLUGIN_ACTION_ID"),
		EventName:  get("HERDR_PLUGIN_EVENT"),
	}
	if raw := get("HERDR_PLUGIN_CONTEXT_JSON"); raw != "" {
		var c InvocationContext
		if err := json.Unmarshal([]byte(raw), &c); err != nil {
			return Env{}, fmt.Errorf("%w: HERDR_PLUGIN_CONTEXT_JSON: %w", ErrInvalidEnv, err)
		}
		env.Context = &c
	}
	if raw := get("HERDR_PLUGIN_EVENT_JSON"); raw != "" {
		var e Event
		if err := json.Unmarshal([]byte(raw), &e); err != nil {
			return Env{}, fmt.Errorf("%w: HERDR_PLUGIN_EVENT_JSON: %w", ErrInvalidEnv, err)
		}
		env.Event = &e
	}
	return env, nil
}
