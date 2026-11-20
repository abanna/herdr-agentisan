package notice

import "github.com/abanna/herdr-agentisan/internal/team"

// Delivery is how a team's boss gets its notices and messages (ADR-001
// A74).
type Delivery int

// The deliveries (A74).
const (
	DeliverTyped Delivery = iota // the inbox, typed by the dispatcher (A30, A44)
	DeliverBoard                 // posts on the team's board (A74)
	DeliverHeld                  // the inbox, neither typed nor posted until the boss's agent is known
)

// ClaudeAgent is pane.list's agent for Claude (herdr 0.9.5).
const ClaudeAgent = "claude"

// BossAgent is the agent herdr detects on the pane in project's boss place
// that bears boss's name in m, or "": m nil, no such pane, or it shows none.
func BossAgent(m *team.Model, project, boss string) string {
	if m == nil {
		return ""
	}
	for _, p := range m.Projects {
		if p.Name != project {
			continue
		}
		for _, b := range p.Boss {
			if b.Pane.Name == boss && b.Pane.Agent != "" {
				return b.Pane.Agent
			}
		}
	}
	return ""
}

// DeliveryOf is a team's delivery: board says its key is board, last is
// the agent last seen on its boss's pane ("" when none since the start).
// !board: typed. "": held. claude: board. Anything else: typed.
func DeliveryOf(board bool, last string) Delivery {
	switch {
	case !board:
		return DeliverTyped
	case last == "":
		return DeliverHeld
	case last == ClaudeAgent:
		return DeliverBoard
	}
	return DeliverTyped
}
