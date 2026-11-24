package cli_test

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/cli"
	"github.com/abanna/herdr-agentisan/internal/recall"
	"github.com/abanna/herdr-agentisan/internal/settings"
	"github.com/abanna/herdr-agentisan/internal/trail"
)

const (
	recallMemory = "---\nname: alpha-rule\ndescription: \"Alpha, the memory rule\"\n---\n\nAlpha body.\n"
	recallRules  = "# Rulings\n\n- First ruling, one line.\n- Second ruling,\n  two lines.\n"
)

// recallEnv writes a memory directory with one file and a rulings file, and a
// shared config naming them; it returns the command's context, the config dir
// and the rulings file's path.
func recallEnv(t *testing.T) (context.Context, string, string) {
	t.Helper()
	root, cfg := t.TempDir(), t.TempDir()
	mem := filepath.Join(root, "memory")
	require.NoError(t, os.Mkdir(mem, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(mem, "alpha-rule.md"), []byte(recallMemory), 0o600))
	rulings := filepath.Join(root, "rulings.md")
	require.NoError(t, os.WriteFile(rulings, []byte(recallRules), 0o600))
	shared := "schema_version = 1\n[recall]\nmemory_dirs = [\"" + mem + "\"]\n[recall.bullet_files]\nrul = \"" + rulings + "\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(cfg, settings.FileName), []byte(shared), 0o600))
	env := map[string]string{trail.ConfigDirEnv: cfg}
	ctx := cli.WithLookupEnv(context.Background(), func(k string) (string, bool) { v, ok := env[k]; return v, ok })
	return ctx, cfg, rulings
}

func libraryOf(t *testing.T, cfg string) recall.Library {
	t.Helper()
	src, err := settings.RecallSources(cfg, func(string) (string, bool) { return "", false })
	require.NoError(t, err)
	lib, err := recall.Load(src)
	require.NoError(t, err)
	return lib
}

// TestRecallListPrintsEverySectionWithItsPlace: one line each, the ID to ask
// for, the file and lines it is read from, and its heading.
func TestRecallListPrintsEverySectionWithItsPlace(t *testing.T) {
	t.Parallel()
	ctx, _, rulings := recallEnv(t)

	out, err := runIn(ctx, "recall", "list")
	require.NoError(t, err)
	assert.Regexp(t, regexp.MustCompile(`(?m)^mem:alpha-rule\s+\S+alpha-rule\.md:1-6\s+Alpha, the memory rule$`), out)
	assert.Regexp(t, regexp.MustCompile(`(?m)^rul:[0-9a-f]{8}\s+`+regexp.QuoteMeta(rulings)+`:3\s+First ruling, one line\.$`), out)
	assert.Regexp(t, regexp.MustCompile(`(?m)^rul:[0-9a-f]{8}\s+`+regexp.QuoteMeta(rulings)+`:4-5\s+Second ruling,$`), out)
	assert.Contains(t, out, "3 sections")
}

// TestRecallShowPrintsSectionsWordForWord: under a header with the file and
// lines, in the order asked, exactly as the file has them.
func TestRecallShowPrintsSectionsWordForWord(t *testing.T) {
	t.Parallel()
	ctx, cfg, rulings := recallEnv(t)
	lib := libraryOf(t, cfg)
	second := lib.Sections[2]

	out, err := runIn(ctx, "recall", "show", second.ID, "mem:alpha-rule")
	require.NoError(t, err)
	want := "== " + second.ID + "  " + rulings + ":4-5\n- Second ruling,\n  two lines.\n" +
		"\n" +
		"== mem:alpha-rule  " + lib.Sections[0].File + ":1-6\n" + strings.TrimSuffix(recallMemory, "\n") + "\n"
	assert.Equal(t, want, out)
}

// TestRecallShowRefusesAnIDThatIsGone: a removed or edited section is named
// and nothing else is printed, so no answer is part of one.
func TestRecallShowRefusesAnIDThatIsGone(t *testing.T) {
	t.Parallel()
	ctx, _, _ := recallEnv(t)
	out, err := runIn(ctx, "recall", "show", "mem:alpha-rule", "rul:00000000")
	require.ErrorIs(t, err, recall.ErrUnknownID)
	assert.Contains(t, err.Error(), "rul:00000000")
	assert.Contains(t, err.Error(), "recall list", "it says where to look")
	assert.NotContains(t, out, "Alpha body")
}

// TestRecallNeedsItsSources: with no config dir, or none of its keys set, the
// command says what to set; a source it cannot read fails it whole.
func TestRecallNeedsItsSources(t *testing.T) {
	t.Parallel()
	t.Run("no config dir", func(t *testing.T) {
		t.Parallel()
		ctx := cli.WithLookupEnv(context.Background(), func(string) (string, bool) { return "", false })
		_, err := runIn(ctx, "recall", "list")
		require.Error(t, err)
		assert.Contains(t, err.Error(), trail.ConfigDirEnv)
	})
	t.Run("no [recall] table", func(t *testing.T) {
		t.Parallel()
		cfg := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(cfg, settings.FileName), []byte("schema_version = 1\n"), 0o600))
		ctx := cli.WithLookupEnv(context.Background(), func(k string) (string, bool) { return cfg, k == trail.ConfigDirEnv })
		_, err := runIn(ctx, "recall", "show", "mem:x")
		require.ErrorIs(t, err, recall.ErrNoSources)
		assert.Contains(t, err.Error(), "recall.memory_dirs")
		assert.Contains(t, err.Error(), "recall.bullet_files")
	})
	t.Run("a source that is gone", func(t *testing.T) {
		t.Parallel()
		ctx, _, rulings := recallEnv(t)
		require.NoError(t, os.Remove(rulings))
		_, err := runIn(ctx, "recall", "list")
		require.ErrorIs(t, err, recall.ErrSource)
		assert.Contains(t, err.Error(), rulings)
	})
	t.Run("show with no ID", func(t *testing.T) {
		t.Parallel()
		ctx, _, _ := recallEnv(t)
		_, err := runIn(ctx, "recall", "show")
		require.Error(t, err)
	})
}
