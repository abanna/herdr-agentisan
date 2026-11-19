package notice_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/abanna/herdr-agentisan/internal/notice"
	"github.com/abanna/herdr-agentisan/internal/team"
)

// bossModel is a model of project ha whose boss place holds one pane, named
// name and showing agent; a group pane elsewhere and another project's boss
// both bear ha-boss and show claude, and neither counts.
func bossModel(name, agent string) *team.Model {
	return &team.Model{Projects: []team.Project{
		{
			Name: "ha", Boss: []team.Worker{{Pane: team.Pane{ID: "w14:p1", Name: name, Agent: agent}}},
			Groups: []team.Group{{Name: "coder", Workers: []team.Worker{{Pane: team.Pane{ID: "w14:p2", Name: "ha-boss", Agent: "claude"}}}}},
		},
		{Name: "other", Boss: []team.Worker{{Pane: team.Pane{ID: "w15:p1", Name: "ha-boss", Agent: "claude"}}}},
	}}
}

// TestDeliveryRemembersTheBossAgent is A74's rule, every cell: for a tabs
// team whose key is board, the agent on the pane in its boss place that
// bears the boss's name, remembered across polls that name none. The row is
// this poll's model, the column the memory before it; the memory is folded
// here as the daemon's inbox folds it (remember), which its own tests pin.
// With the key typed, every cell is typed.
func TestDeliveryRemembersTheBossAgent(t *testing.T) {
	t.Parallel()
	const (
		board = notice.DeliverBoard
		typed = notice.DeliverTyped
		held  = notice.DeliverHeld
	)
	rows := []struct {
		name  string
		model *team.Model
		agent string // BossAgent's answer
		// want is the delivery after the poll, from a memory of "" (never
		// seen), claude, and codex (another agent).
		want [3]notice.Delivery
	}{
		{"the named pane shows claude", bossModel("ha-boss", "claude"), "claude", [3]notice.Delivery{board, board, board}},
		{"the named pane shows another agent", bossModel("ha-boss", "codex"), "codex", [3]notice.Delivery{typed, typed, typed}},
		{"the named pane shows no agent", bossModel("ha-boss", ""), "", [3]notice.Delivery{held, board, typed}},
		{"no pane bears the name", bossModel("", "claude"), "", [3]notice.Delivery{held, board, typed}},
		{"no model", nil, "", [3]notice.Delivery{held, board, typed}},
	}
	before := [3]string{"", notice.ClaudeAgent, "codex"}
	for _, r := range rows {
		got := notice.BossAgent(r.model, "ha", "ha-boss")
		assert.Equal(t, r.agent, got, "%s: the boss's agent", r.name)
		for i, last := range before {
			memory := last
			if got != "" {
				memory = got
			}
			wantMemory := last
			if r.agent != "" {
				wantMemory = r.agent
			}
			assert.Equal(t, wantMemory, memory, "%s, last %q: the memory", r.name, last)
			assert.Equal(t, r.want[i], notice.DeliveryOf(true, memory), "%s, last %q: board", r.name, last)
			assert.Equal(t, typed, notice.DeliveryOf(false, memory), "%s, last %q: the key typed", r.name, last)
		}
	}
	assert.Empty(t, notice.BossAgent(bossModel("ha-boss", "claude"), "nope", "ha-boss"), "a project the model lacks")
	assert.Empty(t, notice.BossAgent(&team.Model{}, "ha", "ha-boss"), "an empty model")
	two := bossModel("ha-boss", "")
	two.Projects[0].Boss = append(two.Projects[0].Boss, team.Worker{Pane: team.Pane{ID: "w14:p3", Name: "ha-boss", Agent: "codex"}})
	assert.Equal(t, "codex", notice.BossAgent(two, "ha", "ha-boss"), "the first named pane that shows an agent")
}
