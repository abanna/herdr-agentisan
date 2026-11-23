package store_test

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// The poll owns batch notice delivery. Store.Post stays independent, and
// every dispatcher claim goes through claimTyped, which publishes the
// in-flight boss before it unlocks the route.
func TestBoardNoticeDeliveryCallers(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..")
	names := []string{"Claim", "claim", "claimTyped", "PostAll", "postAll", "FitID", "Entries", "SettleEntries", "Tail"}
	got := map[string][]string{}
	for _, dir := range []string{"cmd", "internal"} {
		uses, _, _, _ := productionUses(t, root, dir, names, nil)
		for path, matches := range uses {
			for _, match := range matches {
				got[path] = append(got[path], match[strings.LastIndexByte(match, ' ')+1:])
			}
			slices.Sort(got[path])
		}
	}
	assert.Equal(t, map[string][]string{
		"internal/daemon/messages.go":      {"Claim", "claim", "claimTyped"},
		"internal/daemon/boarddelivery.go": {"claim", "claim", "claimTyped"},
		"internal/daemon/notices.go":       {"Entries", "FitID", "FitID", "PostAll", "SettleEntries", "Tail"},
		"internal/store/messages.go":       {"Claim"},
		"internal/store/board.go":          {"FitID", "PostAll", "postAll", "postAll"},
		"internal/notice/batch.go":         {"Entries", "SettleEntries", "Tail"},
		"internal/notice/inbox.go":         {"Tail"},
	}, got)
}
