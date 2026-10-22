package daemon_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/abanna/herdr-agentisan/internal/daemon"
	"github.com/abanna/herdr-agentisan/internal/report"
	"github.com/abanna/herdr-agentisan/internal/store"
)

// TestATypedMessageStatesItsItem: a dispatch sent with `send` and its item
// first states that item to the recipient's turn hook (ADR-001 A42),
// whichever header the daemon types it behind (A30). report turn reads the
// prompt TextOf makes as it reads one typed by hand, so the two cannot drift.
func TestATypedMessageStatesItsItem(t *testing.T) {
	t.Parallel()
	const id = "01JB2X5W8Q3R4S5T6V7W8X9Y0Z"
	const body = "item: NERD-5334\nreview #109, then Draft 27"
	tests := map[string]store.Message{
		"from a boss":            {ID: id, From: haBoss, Body: body},
		"from a pane":            {ID: id, From: notes, Body: body},
		"returned":               {ID: id, From: coder, Body: body, ReturnedFrom: "ha-coder-2"},
		"the daemon's":           {ID: id, From: store.Daemon, Body: body},
		"the daemon's, returned": {ID: id, From: store.Daemon, Body: body, ReturnedFrom: "ha-coder-1"},
	}
	for name, m := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			item, stage := report.Stated(report.EventPromptSubmit, daemon.TextOf(m))
			assert.Equal(t, "NERD-5334", item)
			assert.Empty(t, stage)
		})
	}
}
