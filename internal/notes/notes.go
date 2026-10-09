// Package notes is the domain the CLI and the REST API both drive.
//
// It owns validation and storage behind an interface so a handler or a command
// never talks to a concrete store. Swapping the in-memory store for Postgres
// means implementing Store, not editing callers.
package notes

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Domain errors. Callers map these onto transport status codes; they must not
// re-derive intent from error strings.
var (
	// ErrNotFound is returned when no note has the requested ID.
	ErrNotFound = errors.New("note not found")
	// ErrInvalid is returned when input fails validation.
	ErrInvalid = errors.New("invalid note")
)

// MaxBodyLen bounds a note body so an unbounded request cannot exhaust memory.
const MaxBodyLen = 4096

// MaxTitleLen bounds a note title.
const MaxTitleLen = 200

// Note is a single stored note.
type Note struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

// Draft is the caller-supplied half of a Note; the store owns ID and CreatedAt.
type Draft struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

// Validate normalises the draft and reports whether it is usable.
// It trims surrounding whitespace first, so a title of only spaces is empty.
func (d *Draft) Validate() error {
	d.Title = strings.TrimSpace(d.Title)
	d.Body = strings.TrimSpace(d.Body)

	switch {
	case d.Title == "":
		return fmt.Errorf("%w: title must not be empty", ErrInvalid)
	case len(d.Title) > MaxTitleLen:
		return fmt.Errorf("%w: title exceeds %d bytes", ErrInvalid, MaxTitleLen)
	case len(d.Body) > MaxBodyLen:
		return fmt.Errorf("%w: body exceeds %d bytes", ErrInvalid, MaxBodyLen)
	}
	return nil
}

// Store persists notes. Implementations must be safe for concurrent use.
type Store interface {
	Create(ctx context.Context, d Draft) (Note, error)
	Get(ctx context.Context, id string) (Note, error)
	List(ctx context.Context) ([]Note, error)
	Delete(ctx context.Context, id string) error
}

// MemStore is an in-memory Store. It is the default so the service runs with
// no external dependency; it is not durable and is not meant for production.
type MemStore struct {
	mu    sync.RWMutex
	notes map[string]Note
	now   func() time.Time
	newID func() string
}

// NewMemStore returns an empty MemStore.
func NewMemStore() *MemStore {
	return &MemStore{
		notes: make(map[string]Note),
		now:   time.Now,
		newID: uuid.NewString,
	}
}

var _ Store = (*MemStore)(nil)

// Create validates d and stores it under a freshly generated ID.
func (s *MemStore) Create(ctx context.Context, d Draft) (Note, error) {
	if err := ctx.Err(); err != nil {
		return Note{}, fmt.Errorf("create note: %w", err)
	}
	if err := d.Validate(); err != nil {
		return Note{}, err
	}

	n := Note{
		ID:        s.newID(),
		Title:     d.Title,
		Body:      d.Body,
		CreatedAt: s.now().UTC(),
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.notes[n.ID] = n
	return n, nil
}

// Get returns the note with the given ID, or ErrNotFound.
func (s *MemStore) Get(ctx context.Context, id string) (Note, error) {
	if err := ctx.Err(); err != nil {
		return Note{}, fmt.Errorf("get note: %w", err)
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	n, ok := s.notes[id]
	if !ok {
		return Note{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return n, nil
}

// List returns every note, newest first. Ties break on ID so the order is
// total and the output is reproducible.
func (s *MemStore) List(ctx context.Context) ([]Note, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("list notes: %w", err)
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]Note, 0, len(s.notes))
	for _, n := range s.notes {
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
func (s *MemStore) Delete(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("delete note: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.notes[id]; !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	delete(s.notes, id)
	return nil
}
