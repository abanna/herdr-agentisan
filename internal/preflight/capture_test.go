package preflight_test

import (
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
