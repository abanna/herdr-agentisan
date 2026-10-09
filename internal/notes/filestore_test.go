package notes_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nerds-run/go-agents/internal/notes"
)

func newFileStore(t *testing.T) (*notes.FileStore, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "nested", "notes.json")
	return notes.NewFileStore(path), path
}

// TestFileStorePersistsAcrossInstances is the reason this type exists: the
// CLI is one process per command, so a note must survive the store going away.
func TestFileStorePersistsAcrossInstances(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	s, path := newFileStore(t)

	created, err := s.Create(ctx, notes.Draft{Title: "durable", Body: "survives"})
	require.NoError(t, err)

	// A completely separate store over the same file, as a second CLI
	// invocation would construct.
	reopened := notes.NewFileStore(path)
	got, err := reopened.Get(ctx, created.ID)
	require.NoError(t, err)
	assert.Equal(t, "durable", got.Title)
	assert.Equal(t, created.ID, got.ID)
}

func TestFileStoreMissingFileIsAnEmptyStore(t *testing.T) {
	t.Parallel()

	// The first `notes add` on a clean machine must not fail, and neither
	// must the first `notes list`.
	s, _ := newFileStore(t)
	got, err := s.List(t.Context())
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestFileStoreCreatesItsDirectory(t *testing.T) {
	t.Parallel()

	s, path := newFileStore(t)
	_, err := s.Create(t.Context(), notes.Draft{Title: "makes dirs"})
	require.NoError(t, err)

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.False(t, info.IsDir())
}

func TestFileStoreRoundTrip(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	s, _ := newFileStore(t)

	a, err := s.Create(ctx, notes.Draft{Title: "first"})
	require.NoError(t, err)
	b, err := s.Create(ctx, notes.Draft{Title: "second"})
	require.NoError(t, err)

	all, err := s.List(ctx)
	require.NoError(t, err)
	assert.Len(t, all, 2)

	require.NoError(t, s.Delete(ctx, a.ID))
	require.ErrorIs(t, s.Delete(ctx, a.ID), notes.ErrNotFound)

	remaining, err := s.List(ctx)
	require.NoError(t, err)
	require.Len(t, remaining, 1)
	assert.Equal(t, b.ID, remaining[0].ID)
}

func TestFileStoreValidatesLikeMemStore(t *testing.T) {
	t.Parallel()

	s, _ := newFileStore(t)
	_, err := s.Create(t.Context(), notes.Draft{Title: "   "})
	require.ErrorIs(t, err, notes.ErrInvalid)
}

func TestFileStoreGetMissing(t *testing.T) {
	t.Parallel()

	s, _ := newFileStore(t)
	_, err := s.Get(t.Context(), "absent")
	require.ErrorIs(t, err, notes.ErrNotFound)
}

func TestFileStoreRejectsCorruptFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "notes.json")
	require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o600))

	// Corruption must surface as an error, never as an empty store — the
	// latter would silently look like "you have no notes".
	_, err := notes.NewFileStore(path).List(t.Context())
	require.Error(t, err)
	assert.ErrorContains(t, err, "parse note store")
}

func TestFileStoreEmptyFileIsAnEmptyStore(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "notes.json")
	require.NoError(t, os.WriteFile(path, nil, 0o600))

	got, err := notes.NewFileStore(path).List(t.Context())
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestFileStoreWritesArePrivate(t *testing.T) {
	t.Parallel()

	s, path := newFileStore(t)
	_, err := s.Create(t.Context(), notes.Draft{Title: "private"})
	require.NoError(t, err)

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(),
		"the note store may hold personal content; it must not be world-readable")
}

func TestFileStoreLeavesNoTempFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "notes.json")
	s := notes.NewFileStore(path)

	for range 3 {
		_, err := s.Create(t.Context(), notes.Draft{Title: "churn"})
		require.NoError(t, err)
	}

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, 1, "atomic rename should leave only the store file behind")
}

func TestDefaultPathIsAbsolute(t *testing.T) {
	t.Parallel()

	got, err := notes.DefaultPath()
	require.NoError(t, err)
	assert.True(t, filepath.IsAbs(got))
	assert.Equal(t, "notes.json", filepath.Base(got))
}
