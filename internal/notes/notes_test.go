package notes_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nerds-run/go-agents/internal/notes"
)

func TestDraftValidate(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		draft     notes.Draft
		wantErr   error
		wantTitle string
	}{
		"trims surrounding whitespace": {
			draft:     notes.Draft{Title: "  hello  ", Body: "  world  "},
			wantTitle: "hello",
		},
		"empty title is rejected": {
			draft:   notes.Draft{Title: "", Body: "b"},
			wantErr: notes.ErrInvalid,
		},
		"whitespace-only title is rejected after trimming": {
			draft:   notes.Draft{Title: "   \t\n ", Body: "b"},
			wantErr: notes.ErrInvalid,
		},
		"title at the limit is accepted": {
			draft:     notes.Draft{Title: strings.Repeat("a", notes.MaxTitleLen)},
			wantTitle: strings.Repeat("a", notes.MaxTitleLen),
		},
		"title over the limit is rejected": {
			draft:   notes.Draft{Title: strings.Repeat("a", notes.MaxTitleLen+1)},
			wantErr: notes.ErrInvalid,
		},
		"body over the limit is rejected": {
			draft:   notes.Draft{Title: "ok", Body: strings.Repeat("b", notes.MaxBodyLen+1)},
			wantErr: notes.ErrInvalid,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			err := tc.draft.Validate()
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantTitle, tc.draft.Title)
		})
	}
}

func TestMemStoreCreateAndGet(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	s := notes.NewMemStore()

	created, err := s.Create(ctx, notes.Draft{Title: "first", Body: "body"})
	require.NoError(t, err)
	assert.NotEmpty(t, created.ID)
	assert.False(t, created.CreatedAt.IsZero())

	got, err := s.Get(ctx, created.ID)
	require.NoError(t, err)
	assert.Equal(t, created, got)
}

func TestMemStoreGetMissing(t *testing.T) {
	t.Parallel()

	_, err := notes.NewMemStore().Get(t.Context(), "nope")
	require.ErrorIs(t, err, notes.ErrNotFound)
}

func TestMemStoreCreateRejectsInvalid(t *testing.T) {
	t.Parallel()

	_, err := notes.NewMemStore().Create(t.Context(), notes.Draft{Title: " "})
	require.ErrorIs(t, err, notes.ErrInvalid)
}

func TestMemStoreListIsNewestFirst(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	s := notes.NewMemStore()
	for _, title := range []string{"a", "b", "c"} {
		_, err := s.Create(ctx, notes.Draft{Title: title})
		require.NoError(t, err)
	}

	got, err := s.List(ctx)
	require.NoError(t, err)
	require.Len(t, got, 3)

	for i := 1; i < len(got); i++ {
		prev, cur := got[i-1], got[i]
		assert.Falsef(t, cur.CreatedAt.After(prev.CreatedAt),
			"index %d (%s) is newer than the entry before it", i, cur.CreatedAt)
	}
}

func TestMemStoreListEmptyIsNotNil(t *testing.T) {
	t.Parallel()

	// The API marshals this directly; a nil slice would emit `null` instead
	// of `[]` and break clients that iterate the result unconditionally.
	got, err := notes.NewMemStore().List(t.Context())
	require.NoError(t, err)
	assert.NotNil(t, got)
	assert.Empty(t, got)
}

func TestMemStoreDelete(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	s := notes.NewMemStore()
	n, err := s.Create(ctx, notes.Draft{Title: "doomed"})
	require.NoError(t, err)

	require.NoError(t, s.Delete(ctx, n.ID))

	_, err = s.Get(ctx, n.ID)
	require.ErrorIs(t, err, notes.ErrNotFound)

	// Deleting twice must report not-found rather than succeeding silently.
	require.ErrorIs(t, s.Delete(ctx, n.ID), notes.ErrNotFound)
}

func TestMemStoreHonoursCancelledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	s := notes.NewMemStore()
	_, err := s.Create(ctx, notes.Draft{Title: "x"})
	require.ErrorIs(t, err, context.Canceled)
}

func TestMemStoreConcurrentCreate(t *testing.T) {
	t.Parallel()

	const writers = 50
	ctx := t.Context()
	s := notes.NewMemStore()

	var wg sync.WaitGroup
	wg.Add(writers)
	for i := range writers {
		go func() {
			defer wg.Done()
			_, err := s.Create(ctx, notes.Draft{Title: "concurrent", Body: string(rune('a' + i%26))})
			assert.NoError(t, err)
		}()
	}
	wg.Wait()

	got, err := s.List(ctx)
	require.NoError(t, err)
	assert.Len(t, got, writers, "every concurrent create should be stored under a distinct ID")
}
