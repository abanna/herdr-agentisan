package devcli_test

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/devcli"
)

func TestParseCoverageTotal(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		in      string
		want    float64
		wantErr bool
	}{
		"reads the total line": {
			in: "github.com/x/y/a.go:10:\tFoo\t100.0%\n" +
				"total:\t\t\t\t(statements)\t83.4%\n",
			want: 83.4,
		},
		"zero coverage is a real value, not an error": {
			in:   "total:\t(statements)\t0.0%\n",
			want: 0,
		},
		"missing total is an error, never a silent zero": {
			in:      "github.com/x/y/a.go:10:\tFoo\t100.0%\n",
			wantErr: true,
		},
		"empty input is an error": {
			in:      "",
			wantErr: true,
		},
		"unparseable percentage is an error": {
			in:      "total:\t(statements)\tnot-a-number%\n",
			wantErr: true,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, err := devcli.ParseCoverageTotal(strings.NewReader(tc.in))
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.InDelta(t, tc.want, got, 0.001)
		})
	}
}

func TestCoverageResultMeets(t *testing.T) {
	t.Parallel()

	assert.True(t, devcli.CoverageResult{Percent: 80, Threshold: 75}.Meets())
	assert.True(t, devcli.CoverageResult{Percent: 75, Threshold: 75}.Meets(), "exactly at the floor passes")
	assert.False(t, devcli.CoverageResult{Percent: 74.9, Threshold: 75}.Meets())
}

func TestCoverageResultStringReportsVerdict(t *testing.T) {
	t.Parallel()

	assert.Contains(t, devcli.CoverageResult{Percent: 90, Threshold: 75}.String(), "ok")
	assert.Contains(t, devcli.CoverageResult{Percent: 10, Threshold: 75}.String(), "FAIL")
}

// TestParityHoldsInThisRepo is the gate itself: it fails the build when
// AGENTS.md, Taskfile.yml and ci.yml stop agreeing.
func TestParityHoldsInThisRepo(t *testing.T) {
	t.Parallel()

	rep, err := devcli.CheckParity("../..")
	require.NoError(t, err)
	assert.Truef(t, rep.OK(), "%s", rep)
}

// TestNoStaleParityExemptionsInThisRepo fails when a task or table row is
// deleted but its escape-hatch entry is left behind. A dead entry is not
// harmless: it silently exempts whatever task is later added under that name.
func TestNoStaleParityExemptionsInThisRepo(t *testing.T) {
	t.Parallel()

	stale, err := devcli.StaleExemptions("../..")
	require.NoError(t, err)
	assert.Empty(t, stale)
}

func TestEveryDevCommandIsDocumented(t *testing.T) {
	t.Parallel()

	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		assert.NotEmptyf(t, c.Short, "command %q has no Short description", c.CommandPath())
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(devcli.Root())
}
