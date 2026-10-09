package cli_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nerds-run/go-agents/internal/cli"
	"github.com/nerds-run/go-agents/internal/notes"
)

// run executes the command tree against store with the given args and returns
// combined stdout. Cobra writes both streams to the buffer we set.
func run(t *testing.T, store notes.Store, args ...string) (string, error) {
	t.Helper()

	root := cli.Root()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)

	ctx := cli.WithStore(t.Context(), store)
	err := root.ExecuteContext(ctx)
	return out.String(), err
}

func TestNotesAddThenList(t *testing.T) {
	t.Parallel()
	store := notes.NewMemStore()

	out, err := run(t, store, "notes", "add", "first note", "--body", "the body")
	require.NoError(t, err)
	assert.Contains(t, out, "created ")

	out, err = run(t, store, "notes", "list")
	require.NoError(t, err)
	assert.Contains(t, out, "first note")
	assert.Contains(t, out, "ID")
}

func TestNotesListEmpty(t *testing.T) {
	t.Parallel()

	out, err := run(t, notes.NewMemStore(), "notes", "list")
	require.NoError(t, err)
	assert.Contains(t, out, "no notes")
}

func TestNotesListJSON(t *testing.T) {
	t.Parallel()
	store := notes.NewMemStore()
	_, err := store.Create(t.Context(), notes.Draft{Title: "json me"})
	require.NoError(t, err)

	out, err := run(t, store, "notes", "list", "--json")
	require.NoError(t, err)

	var got []notes.Note
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	require.Len(t, got, 1)
	assert.Equal(t, "json me", got[0].Title)
}

func TestNotesGetAndRemove(t *testing.T) {
	t.Parallel()
	store := notes.NewMemStore()
	n, err := store.Create(t.Context(), notes.Draft{Title: "target", Body: "payload"})
	require.NoError(t, err)

	out, err := run(t, store, "notes", "get", n.ID)
	require.NoError(t, err)
	assert.Contains(t, out, "target")
	assert.Contains(t, out, "payload")

	_, err = run(t, store, "notes", "rm", n.ID)
	require.NoError(t, err)

	_, err = run(t, store, "notes", "get", n.ID)
	require.ErrorIs(t, err, notes.ErrNotFound)
}

func TestNotesAddRejectsEmptyTitle(t *testing.T) {
	t.Parallel()

	_, err := run(t, notes.NewMemStore(), "notes", "add", "   ")
	require.ErrorIs(t, err, notes.ErrInvalid)
}

func TestNotesGetMissingIsNotFound(t *testing.T) {
	t.Parallel()

	_, err := run(t, notes.NewMemStore(), "notes", "get", "nope")
	require.ErrorIs(t, err, notes.ErrNotFound)
}

func TestArgCountsAreEnforced(t *testing.T) {
	t.Parallel()

	tests := map[string][]string{
		"add needs a title": {"notes", "add"},
		"get needs an id":   {"notes", "get"},
		"rm needs an id":    {"notes", "rm"},
		"list takes none":   {"notes", "list", "extra"},
	}
	for name, args := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := run(t, notes.NewMemStore(), args...)
			require.Error(t, err)
		})
	}
}

// TestEveryCommandIsDocumented guards the help text agents read to discover
// the tool: a command with no Short renders as a blank line in `--help`.
func TestEveryCommandIsDocumented(t *testing.T) {
	t.Parallel()

	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		assert.NotEmptyf(t, c.Short, "command %q has no Short description", c.CommandPath())
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(cli.Root())
}

func TestRootHelpListsNotes(t *testing.T) {
	t.Parallel()

	out, err := run(t, notes.NewMemStore(), "--help")
	require.NoError(t, err)
	assert.True(t, strings.Contains(out, "notes"), "root help should advertise the notes command")
}

// TestTwoTreesDoNotShareFlagState guards against binding --store to a
// package-level variable: cobra writes to the address it is given, so a global
// would make two trees in one process overwrite each other's flag. This
// surfaced as cross-test contamination only when the suite ran in parallel.
func TestTwoTreesDoNotShareFlagState(t *testing.T) {
	t.Parallel()

	a, b := cli.Root(), cli.Root()
	a.SetOut(&bytes.Buffer{})
	b.SetOut(&bytes.Buffer{})

	require.NoError(t, a.PersistentFlags().Set("store", "/tmp/a.json"))
	require.NoError(t, b.PersistentFlags().Set("store", "/tmp/b.json"))

	assert.Equal(t, "/tmp/a.json", a.PersistentFlags().Lookup("store").Value.String(),
		"setting the second tree's --store must not clobber the first")
	assert.Equal(t, "/tmp/b.json", b.PersistentFlags().Lookup("store").Value.String())
}
