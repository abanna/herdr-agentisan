package herdr

import (
	"context"
	"fmt"
)

// ListAgents returns every agent terminal herdr knows, with its pane and,
// when it has one, its name: the name agent.focus takes. A plain shell is
// not an agent terminal and is never listed (herdr 0.9.3
// src/app/agents.rs:366-376).
func (c Client) ListAgents(ctx context.Context) ([]AgentInfo, error) {
	var out struct {
		Type   string      `json:"type"`
		Agents []AgentInfo `json:"agents"`
	}
	if err := c.Call(ctx, "agent.list", struct{}{}, &out); err != nil {
		return nil, err
	}
	if out.Type != "agent_list" {
		return nil, fmt.Errorf("%w: agent.list: result type %q, want \"agent_list\"", ErrProtocol, out.Type)
	}
	if out.Agents == nil {
		return nil, fmt.Errorf("%w: agent.list: result has no agents", ErrProtocol)
	}
	for _, a := range out.Agents {
		if a.PaneID == "" {
			return nil, fmt.Errorf("%w: agent.list: an agent has no pane_id", ErrProtocol)
		}
	}
	return out.Agents, nil
}
