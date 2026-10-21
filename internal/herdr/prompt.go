package herdr

import (
	"context"
	"fmt"
)

// PromptAgent types text into the agent herdr knows as target, its name or
// pane id, then presses Enter, and returns the agent it typed into. herdr
// resolves the target and queues the input in one step, and answers only
// once the text, a 300 ms pause and Enter were written (0.9.5
// src/app/api/agents.rs:82-268).
//
// What failed matters to a caller that must never type twice. ErrNotSent
// means nothing was written. herdr's agent_not_found,
// agent_target_ambiguous (two agents have the name), agent_blocked,
// agent_not_ready and empty_agent_prompt (an *APIError's Code) refuse before
// any byte reaches the pane (0.9.5 validate_agent_prompt). Any other
// failure, herdr's timeout and agent_prompt_failed among them, may come
// after the text was typed.
//
// It never sets herdr's wait option: with it herdr would answer only once
// the agent reached a status, long after the call's timeout.
func (c Client) PromptAgent(ctx context.Context, target, text string) (AgentInfo, error) {
	params := struct {
		Target string `json:"target"`
		Text   string `json:"text"`
	}{Target: target, Text: text}
	var out struct {
		Type  string     `json:"type"`
		Agent *AgentInfo `json:"agent"`
	}
	if err := c.Call(ctx, "agent.prompt", params, &out); err != nil {
		return AgentInfo{}, err
	}
	if out.Type != "agent_prompted" {
		return AgentInfo{}, fmt.Errorf("%w: agent.prompt: result type %q, want \"agent_prompted\"", ErrProtocol, out.Type)
	}
	if out.Agent == nil || out.Agent.PaneID == "" {
		return AgentInfo{}, fmt.Errorf("%w: agent.prompt: result has no agent.pane_id", ErrProtocol)
	}
	return *out.Agent, nil
}
