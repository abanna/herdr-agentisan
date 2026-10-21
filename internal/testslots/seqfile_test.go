package testslots_test

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/testslots"
)

// seqLocks is a lock list served as Linux serves /proc/locks (v6.12
// fs/seq_file.c seq_read_iter, fs/locks.c locks_start): each read() call
// fills at most a page, taking lines while what it holds is less than the
// call asked for, and each fill starts again at the index where the last
// one stopped. Before each fill but a read's first, drop changes the list,
// as another process dropping a lock between two calls does. Each read
// starts from lines as they are here and meets the same changes in the same
// places: the worst case, where plain reads all skip the same line.
type seqLocks struct {
	lines []string
	page  int
	drop  func(lines []string) []string
}

func (l *seqLocks) open() (io.ReadCloser, error) {
	return &seqReader{l: l, lines: slices.Clone(l.lines)}, nil
}

type seqReader struct {
	l      *seqLocks
	lines  []string // the list as this read finds it
	index  int
	left   []byte // filled, not yet read
	filled bool
}

func (r *seqReader) Close() error { return nil }

func (r *seqReader) Read(p []byte) (int, error) {
	n := copy(p, r.left)
	r.left = r.left[n:]
	if len(r.left) > 0 || n == len(p) {
		return n, nil
	}
	if r.filled && r.l.drop != nil {
		r.lines = r.l.drop(r.lines)
	}
	r.filled = true
	var fill []byte
	for r.index < len(r.lines) {
		line := r.lines[r.index]
		if len(fill) > 0 && len(fill)+len(line) > r.l.page {
			break // the page is full
		}
		fill = append(fill, line...)
		r.index++
		if len(fill) >= len(p)-n {
			break // as much as the call asked for
		}
	}
	if len(fill) == 0 {
		if n > 0 {
			return n, nil
		}
		return 0, io.EOF
	}
	m := copy(p[n:], fill)
	r.left = fill[m:]
	return n + m, nil
}

// lockLine is a /proc/locks line for an exclusive flock on inode, 64 bytes
// with its newline, so a 4096-byte page holds 64 of them.
func lockLine(id int, inode uint64) string {
	line := fmt.Sprintf("%d: FLOCK  ADVISORY  WRITE 4242 00:2a:%d 0 EOF", id, inode)
	return line + strings.Repeat(" ", 63-len(line)) + "\n"
}

// TestReadHeldSurvivesASkippedPage: when a lock listed before the index a
// page begins at goes between two read() calls, the next page skips one
// line, so a read of the list misses a lock held all along, and a second
// plain read, whose pages begin in the same places, can miss it again. The
// three staggered reads still see it (NERD-5341). Here the held lock is the
// line a plain read's second page begins with, and another lock goes before
// every page after a read's first.
func TestReadHeldSurvivesASkippedPage(t *testing.T) {
	t.Parallel()
	const page = 4096
	l := &seqLocks{page: page}
	for i := range 300 {
		l.lines = append(l.lines, lockLine(i+1, uint64(1000+i)))
	}
	require.Len(t, l.lines[0], 64)
	held := testslots.File{Major: 0, Minor: 0x2a, Inode: 1064} // line 65, the second page's first
	l.drop = func(lines []string) []string {
		i := slices.IndexFunc(lines, func(line string) bool { return !strings.Contains(line, ":1064 ") })
		return slices.Delete(slices.Clone(lines), i, i+1)
	}

	for range 2 { // and so does the next, which meets the same change
		content, err := testslots.ReadWhole("locks", l.open, page)
		require.NoError(t, err)
		once, err := testslots.Held(content)
		require.NoError(t, err)
		assert.False(t, once[held], "one read, a page per call, skips it")
	}

	all, err := testslots.ReadHeld("locks", l.open, page)
	require.NoError(t, err)
	assert.True(t, all[held], "three staggered reads see it")
}

// failingReader fails its first read with err.
type failingReader struct{ err error }

func (f failingReader) Read([]byte) (int, error) { return 0, f.err }
func (failingReader) Close() error               { return nil }

// TestReadHeldFailsWhole: any of the three reads failing, to open, to read
// or with a line Held cannot read, is ErrLocks. A part of the list never
// reads as a free slot.
func TestReadHeldFailsWhole(t *testing.T) {
	t.Parallel()
	good := lockLine(1, 7)
	errBoom := errors.New("boom")
	tests := map[string]func(call int) (io.ReadCloser, error){
		"the second open fails": func(call int) (io.ReadCloser, error) {
			if call == 2 {
				return nil, errBoom
			}
			return io.NopCloser(strings.NewReader(good)), nil
		},
		"the third read fails": func(call int) (io.ReadCloser, error) {
			if call == 3 {
				return failingReader{errBoom}, nil
			}
			return io.NopCloser(strings.NewReader(good)), nil
		},
		"the third holds a line it cannot read": func(call int) (io.ReadCloser, error) {
			if call == 3 {
				return io.NopCloser(strings.NewReader(good + "1: FLOCK\n")), nil
			}
			return io.NopCloser(strings.NewReader(good)), nil
		},
	}
	for name, open := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			calls := 0
			_, err := testslots.ReadHeld("locks", func() (io.ReadCloser, error) {
				calls++
				return open(calls)
			}, 4096)
			require.ErrorIs(t, err, testslots.ErrLocks)
		})
	}
}

// TestReadWholeAsksForTheFirstSize: the first read() call asks for exactly
// the first size, which is what sets a read's pages apart from the others';
// the later ones ask for more than a page.
func TestReadWholeAsksForTheFirstSize(t *testing.T) {
	t.Parallel()
	for _, first := range []int{1, 1365, 2730, 4096} {
		r := &recordingReader{r: strings.NewReader(strings.Repeat(lockLine(1, 7), 200))}
		content, err := testslots.ReadWhole("locks", func() (io.ReadCloser, error) { return r, nil }, first)
		require.NoError(t, err)
		assert.Len(t, content, 200*64)
		require.NotEmpty(t, r.asked)
		assert.Equal(t, first, r.asked[0], "first %d", first)
		for _, n := range r.asked[1:] {
			assert.Greater(t, n, 4096)
		}
	}
}

// recordingReader records the size of every read() call made on it.
type recordingReader struct {
	r     io.Reader
	asked []int
}

func (r *recordingReader) Read(p []byte) (int, error) {
	r.asked = append(r.asked, len(p))
	return r.r.Read(p) //nolint:wrapcheck // passes strings.Reader's EOF through
}

func (*recordingReader) Close() error { return nil }
