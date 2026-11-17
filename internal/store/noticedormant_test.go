package store_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ledgerNames are the notice ledger's exported names, and B0a.1's, which
// nothing outside the ledger's own files may use until the board's
// activation (NERD-5533 B1) names its one wiring site. Each later slice adds
// its own.
var ledgerNames = []string{
	"AdmitNotice", "PendingNotice", "PendingNotices", "NoticeStats", "PruneNoticePayloads",
	"NoticeAdmission", "ErrNoNotice",
	"GitHubNoticeCaptureID", "NoticeEnvelope", "NoticeReceipt",
}

// ledgerTables are the tables 0009 adds. Only the ledger names them in SQL.
var ledgerTables = []string{"notice_identities", "notice_payloads"}

// The ledger's own files, relative to the module root: B0a.1's helper and
// the ledger may use its names, and only the ledger may name its tables.
const (
	ledgerHelper = "internal/store/boardnotice.go"
	ledgerFile   = "internal/store/noticeledger.go"
)

// ledgerUses parses every non-test Go file under root/dir. It returns, by
// file relative to root, each use of a ledger name (an identifier spelled
// so, a selector, a method, an interface's method or a dot-import's bare
// name alike) and each string literal naming a ledger table, in any case, as
// SQL reads it. It also returns how many files it parsed.
func ledgerUses(t *testing.T, root, dir string) (names, tables map[string][]string, files int) {
	t.Helper()
	names, tables = map[string][]string{}, map[string][]string{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" {
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
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.Ident:
				if slices.Contains(ledgerNames, n.Name) {
					names[rel] = append(names[rel], fset.Position(n.Pos()).String()+" "+n.Name)
				}
			case *ast.BasicLit:
				if n.Kind == token.STRING && slices.ContainsFunc(ledgerTables, func(table string) bool {
					return strings.Contains(strings.ToLower(n.Value), table)
				}) {
					tables[rel] = append(tables[rel], fset.Position(n.Pos()).String())
				}
			}
			return true
		})
		return nil
	})
	require.NoError(t, err)
	return names, tables, files
}

// TestLedgerHasNoProductionCaller (D1): the ledger is dormant. No non-test
// file in internal/ or cmd/ but the ledger's own uses any of its names, and
// none but the ledger names its tables in SQL, so nothing in production
// admits, reads or builds a notice for it, or reaches its rows around it.
func TestLedgerHasNoProductionCaller(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..")
	names, tables := map[string][]string{}, map[string][]string{}
	parsed := 0
	for _, dir := range []string{"internal", "cmd"} {
		n, tb, files := ledgerUses(t, root, dir)
		maps.Copy(names, n)
		maps.Copy(tables, tb)
		parsed += files
	}
	assert.Greater(t, parsed, 100, "the scan reached the module's packages")

	// The scan finds the names and the tables where they are: it cannot pass
	// by seeing nothing.
	found := map[string]bool{}
	for _, use := range slices.Concat(names[ledgerHelper], names[ledgerFile]) {
		found[use[strings.LastIndexByte(use, ' ')+1:]] = true
	}
	for _, name := range []string{"AdmitNotice", "PendingNotice", "NoticeAdmission", "ErrNoNotice", "GitHubNoticeCaptureID", "NoticeEnvelope", "NoticeReceipt"} {
		assert.True(t, found[name], "the scan finds %s in the ledger's files", name)
	}
	assert.NotEmpty(t, tables[ledgerFile], "the scan finds the ledger's SQL")

	delete(names, ledgerHelper)
	delete(names, ledgerFile)
	delete(tables, ledgerFile)
	assert.Empty(t, names, "the notice ledger is dormant until the board's activation (NERD-5533 B1)")
	assert.Empty(t, tables, "only the ledger names its tables in SQL")
}
