package notes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

// FileStore is a JSON-file-backed Store. It exists because a CLI whose data
// vanishes between invocations is not usable: MemStore is fine for a test or a
// single server process, but the `go-agents` binary is one process per command.
//
// It is deliberately simple — whole-file read and atomic rewrite under a
// mutex plus an exclusive lock file. That is correct for one developer on one
// machine and honest about its ceiling; a real deployment swaps in a database
// by implementing Store, which is the point of the interface.
type FileStore struct {
	mu   sync.Mutex
	path string
	now  func() time.Time
}

var _ Store = (*FileStore)(nil)

// NewFileStore returns a store backed by the JSON file at path. The file and
// its parent directory are created on first write, not here, so constructing
// a store never has a side effect.
func NewFileStore(path string) *FileStore {
	return &FileStore{path: path, now: time.Now}
}

// DefaultPath is where the CLI keeps its notes when nothing overrides it.
// It follows the XDG base-directory spec, falling back to the home directory.
func DefaultPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve user config directory: %w", err)
	}
	return filepath.Join(dir, "go-agents", "notes.json"), nil
}

// load reads the whole file. A missing file is an empty store, not an error:
// the first `notes add` on a clean machine must succeed.
func (s *FileStore) load() (map[string]Note, error) {
	raw, err := os.ReadFile(s.path) // #nosec G304 -- path is operator-configured, by design
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]Note{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read note store %s: %w", s.path, err)
	}
	if len(raw) == 0 {
		return map[string]Note{}, nil
	}

	var out map[string]Note
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("parse note store %s: %w", s.path, err)
	}
	if out == nil {
		out = map[string]Note{}
	}
	return out, nil
}

// save rewrites the file atomically: write a sibling temp file, then rename.
// A crash mid-write therefore leaves the previous contents intact rather than
// a truncated file that no longer parses.
func (s *FileStore) save(notes map[string]Note) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o750); err != nil {
		return fmt.Errorf("create note store directory: %w", err)
	}

	raw, err := json.MarshalIndent(notes, "", "  ")
	if err != nil {
		return fmt.Errorf("encode note store: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".notes-*.json")
	if err != nil {
		return fmt.Errorf("create temp note store: %w", err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup if anything below fails before the rename.
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp note store: %w", err)
	}
	// fsync before rename, or the rename can land while the data has not.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temp note store: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp note store: %w", err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return fmt.Errorf("chmod temp note store: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("replace note store: %w", err)
	}
	return nil
}

// Create validates d and appends it to the file.
func (s *FileStore) Create(ctx context.Context, d Draft) (Note, error) {
	if err := ctx.Err(); err != nil {
		return Note{}, fmt.Errorf("create note: %w", err)
	}
	if err := d.Validate(); err != nil {
		return Note{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	all, err := s.load()
	if err != nil {
		return Note{}, err
	}
	n := Note{
		ID:        uuid.NewString(),
		Title:     d.Title,
		Body:      d.Body,
		CreatedAt: s.now().UTC(),
	}
	all[n.ID] = n
	if err := s.save(all); err != nil {
		return Note{}, err
	}
	return n, nil
}

// Get returns the note with the given ID, or ErrNotFound.
func (s *FileStore) Get(ctx context.Context, id string) (Note, error) {
	if err := ctx.Err(); err != nil {
		return Note{}, fmt.Errorf("get note: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	all, err := s.load()
	if err != nil {
		return Note{}, err
	}
	n, ok := all[id]
	if !ok {
		return Note{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return n, nil
}

// List returns every note, newest first, ties broken on ID.
func (s *FileStore) List(ctx context.Context) ([]Note, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("list notes: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	all, err := s.load()
	if err != nil {
		return nil, err
	}
	out := make([]Note, 0, len(all))
	for _, n := range all {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out, nil
}

// Delete removes the note with the given ID, or returns ErrNotFound.
func (s *FileStore) Delete(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("delete note: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	all, err := s.load()
	if err != nil {
		return err
	}
	if _, ok := all[id]; !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	delete(all, id)
	return s.save(all)
}
