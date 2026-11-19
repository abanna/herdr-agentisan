package store_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"os"
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
	"NoticeAdmission", "ErrNoNotice", "NoticeMeta",
	"GitHubNoticeCaptureID", "NoticeEnvelope", "NoticeReceipt",
	"ErrNoticeQuota", "ErrNoticePendingPerProject", "ErrNoticePendingInStore", "ErrNoticePendingPerPR",
	"ErrNoticeBytesPerProject", "ErrNoticeBytesInStore", "ErrNoticeIdentitiesPerProject",
	"ErrNoticeIdentitiesInStore", "ErrNoticeAdmissionRate",
}

// ledgerTables are the tables 0009 and 0010 add. Only the ledger names them
// in SQL.
var ledgerTables = []string{"notice_identities", "notice_payloads", "notice_counts", "notice_store_counts"}

// The ledger's own files, relative to the module root: B0a.1's helper, the
// ledger, its quotas and its metadata queries may use its names, and only
// the ledger, its quotas and its metadata queries may name its tables.
const (
	ledgerHelper = "internal/store/boardnotice.go"
	ledgerFile   = "internal/store/noticeledger.go"
	quotaFile    = "internal/store/noticequota.go"
	metaFile     = "internal/store/noticemeta.go"
)

var (
	ledgerNameFiles  = []string{ledgerHelper, ledgerFile, quotaFile, metaFile}
	ledgerTableFiles = []string{ledgerFile, quotaFile, metaFile}
)

// ledgerUses parses every non-test Go file under root/dir. It returns, by
// file relative to root, each use of a ledger name (an identifier spelled
// so, a selector, a method, an interface's method or a dot-import's bare
// name alike) and each string literal naming a ledger table, in any case, as
// SQL reads it, once for each table it names, with that table's name. It
// also returns each of those literals that contains
// REPLACE, in any case and in any file, the ledger's own included, and how
// many files it parsed.
func ledgerUses(t *testing.T, root, dir string) (names, tables map[string][]string, replaces []string, files int) {
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
				if n.Kind != token.STRING {
					break
				}
				lit, pos := strings.ToLower(n.Value), fset.Position(n.Pos()).String()
				named := false
				for _, table := range ledgerTables {
					if strings.Contains(lit, table) {
						tables[rel] = append(tables[rel], pos+" "+table)
						named = true
					}
				}
				if named && strings.Contains(lit, "replace") {
					replaces = append(replaces, pos)
				}
			}
			return true
		})
		return nil
	})
	require.NoError(t, err)
	return names, tables, replaces, files
}

// TestLedgerHasNoProductionCaller (D1): the ledger is dormant. No non-test
// file in internal/ or cmd/ but the ledger's own (boardnotice.go,
// noticeledger.go, noticequota.go and noticemeta.go) uses any of its names,
// its quotas' errors and its metadata's included, and none but
// noticeledger.go, noticequota.go and noticemeta.go names its tables in SQL,
// 0010's totals included, so nothing in production admits, reads or builds a
// notice for it, or reaches its rows around it.
//
// No non-test Go string, the ledger's own included, names a ledger table
// with REPLACE in it either: on a connection without recursive_triggers, a
// REPLACE's hidden delete leaves 0010's totals one too high for each row it
// replaces. The scan sees only a single string literal that holds both a
// ledger table's name and REPLACE. It misses a REPLACE built from separate
// literals or constants, and any .sql migration; B0b's guard triggers close
// both at the schema level.
func TestLedgerHasNoProductionCaller(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..")
	names, tables := map[string][]string{}, map[string][]string{}
	var replaces []string
	parsed := 0
	for _, dir := range []string{"internal", "cmd"} {
		n, tb, r, files := ledgerUses(t, root, dir)
		maps.Copy(names, n)
		maps.Copy(tables, tb)
		replaces = append(replaces, r...)
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
	inQuotas := map[string]bool{}
	for _, use := range slices.Concat(names[quotaFile], tables[quotaFile]) {
		inQuotas[use[strings.LastIndexByte(use, ' ')+1:]] = true
	}
	for _, name := range []string{"ErrNoticeQuota", "ErrNoticeAdmissionRate", "notice_counts"} {
		assert.True(t, inQuotas[name], "the scan finds %s in %s", name, quotaFile)
	}
	inMeta := map[string]bool{}
	for _, use := range slices.Concat(names[metaFile], tables[metaFile]) {
		inMeta[use[strings.LastIndexByte(use, ' ')+1:]] = true
	}
	for _, name := range []string{"PendingNotices", "NoticeStats", "NoticeMeta", "notice_payloads"} {
		assert.True(t, inMeta[name], "the scan finds %s in %s", name, metaFile)
	}

	for _, file := range ledgerNameFiles {
		delete(names, file)
	}
	for _, file := range ledgerTableFiles {
		delete(tables, file)
	}
	assert.Empty(t, names, "the notice ledger is dormant until the board's activation (NERD-5533 B1)")
	assert.Empty(t, tables, "only the ledger names its tables in SQL")
	assert.Empty(t, replaces, "no Go string names a ledger table with REPLACE in it")
}

// TestLedgerScanFindsAReplace (D1-R): D1's REPLACE scan, run over one
// non-test file, reports a REPLACE on a ledger table in capitals, in lower
// case and in a raw string alike, an UPDATE OR REPLACE as an INSERT OR
// REPLACE, and one on 0010's notice_counts as on 0009's notice_identities,
// so D1 cannot pass by seeing none.
func TestLedgerScanFindsAReplace(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dir := filepath.Join(root, "internal", "fixture")
	require.NoError(t, os.MkdirAll(dir, 0o750))
	file := filepath.Join(dir, "fixture.go")
	src := "package fixture\n\nconst (\n" +
		"\tupper = \"INSERT OR REPLACE INTO notice_identities (capture_id) VALUES (?)\"\n" +
		"\tlower = \"insert or replace into notice_identities (capture_id) values (?)\"\n" +
		"\traw   = \x60INSERT OR REPLACE INTO notice_identities (capture_id) VALUES (?)\x60\n" +
		"\tmoved = \"UPDATE OR REPLACE notice_identities SET project = ? WHERE capture_id = ?\"\n" +
		"\tcount = \"INSERT OR REPLACE INTO notice_counts (project, identities) VALUES (?, ?)\"\n" +
		")\n"
	require.NoError(t, os.WriteFile(file, []byte(src), 0o600))

	_, tables, replaces, files := ledgerUses(t, root, "internal")
	require.Equal(t, 1, files)
	require.Len(t, tables["internal/fixture/fixture.go"], 5, "the scan finds each literal naming a ledger table")
	assert.Equal(t, []string{file + ":4:10", file + ":5:10", file + ":6:10", file + ":7:10", file + ":8:10"}, replaces)
}
