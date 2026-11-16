package store_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ledgerNames are the notice ledger's exported names, and B0a.1's, which
// nothing outside internal/store may use until the board's activation
// (NERD-5533 B1) names its one wiring site. Each later slice adds its own.
var ledgerNames = []string{
	"AdmitNotice", "PendingNotice", "PendingNotices", "NoticeStats", "PruneNoticePayloads",
	"NoticeAdmission", "ErrNoNotice",
	"GitHubNoticeCaptureID", "NoticeEnvelope", "NoticeReceipt",
}

// ledgerUses parses every non-test Go file under dir and returns each use of
// a ledger name, as file:line: any identifier spelled so, a selector, a
// method, an interface's method or a dot-import's bare name alike. It also
// returns how many files it parsed.
func ledgerUses(t *testing.T, dir string, skip func(path string) bool) (uses []string, files int) {
	t.Helper()
	fset := token.NewFileSet()
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" || skip(path) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		files++
		ast.Inspect(f, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && slices.Contains(ledgerNames, id.Name) {
				uses = append(uses, fset.Position(id.Pos()).String()+" "+id.Name)
			}
			return true
		})
		return nil
	})
	require.NoError(t, err)
	return uses, files
}

// TestLedgerHasNoProductionCaller (D1): the ledger is dormant. No non-test
// file in internal/ or cmd/, outside internal/store, uses any of its names,
// so nothing in production admits, reads or builds a notice for it.
func TestLedgerHasNoProductionCaller(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..")
	store := filepath.Join(root, "internal", "store")
	var outside []string
	parsed := 0
	for _, dir := range []string{filepath.Join(root, "internal"), filepath.Join(root, "cmd")} {
		uses, files := ledgerUses(t, dir, func(path string) bool { return path == store })
		outside = append(outside, uses...)
		parsed += files
	}
	assert.Empty(t, outside, "the notice ledger is dormant until the board's activation (NERD-5533 B1)")
	assert.Greater(t, parsed, 100, "the scan reached the module's packages")

	// The scan finds the names where they are: it cannot pass by seeing
	// nothing.
	inside, _ := ledgerUses(t, store, func(string) bool { return false })
	found := map[string]bool{}
	for _, use := range inside {
		found[use[strings.LastIndexByte(use, ' ')+1:]] = true
	}
	for _, name := range []string{"AdmitNotice", "PendingNotice", "NoticeAdmission", "ErrNoNotice", "GitHubNoticeCaptureID", "NoticeEnvelope", "NoticeReceipt"} {
		assert.True(t, found[name], "the scan finds %s in internal/store", name)
	}
}
