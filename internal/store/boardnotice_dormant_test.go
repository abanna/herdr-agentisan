package store_test

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// The batch APIs have no production caller until the wiring slice. Only the
// declarations and AppendTo's read of Tail may mention their names.
func TestBoardNoticeBatchHasNoProductionCaller(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..")
	names := []string{"PostAll", "FitID", "Entries", "SettleAll", "Tail"}
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
		"internal/store/board.go":  {"FitID", "PostAll"},
		"internal/notice/batch.go": {"Entries", "SettleAll", "Tail"},
		"internal/notice/inbox.go": {"Tail"},
	}, got)
}
