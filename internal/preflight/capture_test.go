package preflight_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/preflight"
)

// TestCaptureKeepsTheFirstBytes: a run keeps the first MaxOutput bytes of
// what tdaddy prints, across writes, and drops the rest, so a flood costs
// no more memory than the cap. Every write still takes all of p, so tdaddy
// never blocks on a pipe nobody reads.
func TestCaptureKeepsTheFirstBytes(t *testing.T) {
	t.Parallel()
	const capacity = preflight.MaxOutput
	for name, sizes := range map[string][]int{
		"under the cap":               {10, 20},
		"exactly the cap":             {capacity},
		"one byte past the cap":       {capacity, 1},
		"across the cap in one write": {capacity - 3, 10, 5},
		"one write far past the cap":  {3 * capacity},
		"empty writes":                {0, capacity, 0, 1},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := preflight.NewCapture()
			var all []byte
			for i, n := range sizes {
				// A byte's value follows its write and its place in it, so
				// the kept bytes show which writes they came from, in what
				// order, and which end of the write that crosses the cap.
				p := make([]byte, n)
				for j := range p {
					p[j] = byte('a' + (7*i+j)%26)
				}
				took, err := c.Write(p)
				require.NoError(t, err)
				assert.Equal(t, n, took, "write %d takes all of p", i)
				all = append(all, p...)
			}
			want := all[:min(len(all), capacity)]
			require.Len(t, c.String(), len(want))
			assert.Equal(t, string(want), c.String())
		})
	}
}

// TestCaptureSkipsLeadingBlanks: the blanks tdaddy prints before anything
// else are not kept and never count toward MaxOutput, so a reason after any
// number of them is kept. Blanks after the first byte kept are kept, and
// everything kept still counts (NERD-5343, Draft 20).
func TestCaptureSkipsLeadingBlanks(t *testing.T) {
	t.Parallel()
	const capacity = preflight.MaxOutput
	blanks := func(n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = " \n\t\r\v\f"[i%6]
		}
		return string(b)
	}
	text := func(n int) string { return strings.Repeat("x", n) }
	for name, tc := range map[string]struct {
		writes []string
		want   string
	}{
		"blanks a byte at a time, then text": {writes: append(strings.Split(blanks(capacity+10), ""), "the reason\n"), want: "the reason\n"},
		"blanks and text in one write":       {writes: []string{blanks(capacity) + "the reason\n"}, want: "the reason\n"},
		"blanks past the cap, then more text than it holds": {
			writes: []string{blanks(2 * capacity), text(capacity + 1)}, want: text(capacity),
		},
		"blanks after the text are kept": {writes: []string{"\n\n", "a\n", "\n  \n", "b"}, want: "a\n\n  \nb"},
		"a text write that starts with blanks keeps them": {
			writes: []string{"a", "  \n b"}, want: "a  \n b",
		},
		"nothing but blanks":         {writes: []string{blanks(3 * capacity)}, want: ""},
		"a Unicode space is not one": {writes: []string{"\u00a0reason"}, want: "\u00a0reason"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := preflight.NewCapture()
			for i, w := range tc.writes {
				took, err := c.Write([]byte(w))
				require.NoError(t, err)
				assert.Equal(t, len(w), took, "write %d takes all of p", i)
			}
			assert.Equal(t, tc.want, c.String())
		})
	}
}
