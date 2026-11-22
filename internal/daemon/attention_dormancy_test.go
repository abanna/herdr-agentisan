package daemon_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Remove this guard when the poll starts using the board alert in 3b-ii.
func TestBoardAttentionHasNoProductionCaller(t *testing.T) {
	t.Parallel()

	const tracker = "github.com/abanna/herdr-agentisan/internal/attention"
	const adapter = "github.com/abanna/herdr-agentisan/internal/daemon/attentioninput"
	root := filepath.Join("..", "..")
	projection := filepath.Join(root, "internal", "daemon", "attentioninput", "inputs.go")
	for _, dir := range []string{"cmd", "internal"} {
		parsed := 0
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() && entry.Name() == "testdata" {
				return filepath.SkipDir
			}
			if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
			if err != nil {
				return err
			}
			parsed++
			for _, spec := range file.Imports {
				importPath, err := strconv.Unquote(spec.Path.Value)
				if err != nil {
					return err
				}
				// The projection uses the tracker's input types.
				if importPath == adapter || (importPath == tracker && path != projection) {
					t.Errorf("%s imports %s before board alerts are wired", path, importPath)
				}
			}
			return nil
		})
		require.NoError(t, err)
		require.Positive(t, parsed, "production Go files in %s", dir)
	}
}
