package dashboard_test

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/colorprofile"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/dashboard"
	"github.com/abanna/herdr-agentisan/internal/snapshot"
)

// sgrBefore is the attributes of the SGR sequences right before each place
// text is drawn in out, padding aside, one set per place: the plain attribute numbers, such
// as "1" for bold, "2" faint, "3" italic, with each colour's own parameters
// left out, so a colour's "38;2;…" never reads as faint; and "fg" the
// colour's own "38;2;r;g;b".
func sgrBefore(t *testing.T, out, text string) []map[string]bool {
	t.Helper()
	var sets []map[string]bool
	for _, m := range regexp.MustCompile(`((?:\x1b\[[0-9;:]*m)+) *`+regexp.QuoteMeta(text)).FindAllStringSubmatch(out, -1) {
		set := map[string]bool{}
		for _, seq := range regexp.MustCompile(`\x1b\[([0-9;:]*)m`).FindAllStringSubmatch(m[1], -1) {
			params := strings.Split(seq[1], ";")
			for i := 0; i < len(params); i++ {
				switch p := params[i]; {
				case (p == "38" || p == "48" || p == "58") && i+1 < len(params) && params[i+1] == "2" && i+4 < len(params):
					if p == "38" {
						set[strings.Join(params[i:i+5], ";")] = true
					}
					i += 4
				case (p == "38" || p == "48" || p == "58") && i+1 < len(params) && params[i+1] == "5":
					i += 2
				default:
					set[p] = true
				}
			}
		}
		sets = append(sets, set)
	}
	return sets
}

// staleSnapshot is a boss and a group whose stages are stale but for one:
// build_test reported three hours ago, an old "blocked", and "review",
// current.
func staleSnapshot() *snapshot.Snapshot {
	reported := at.Add(-3 * time.Hour)
	started := at.Add(-12 * time.Minute)
	boss := snapshot.Boss{Agent: snapshot.Agent{Name: "boss", PaneID: "w1:p1", Status: snapshot.StatusIdle, Stage: "waiting: QA", StageAt: &reported, StageStale: true}}
	return &snapshot.Snapshot{V: 1, At: at, Team: "t", Boss: &boss, Groups: []snapshot.Group{{Name: "coders", Agents: []snapshot.Agent{
		{Name: "pee01", PaneID: "w2:p1", Status: snapshot.StatusIdle, Stage: "build_test", StageAt: &reported, StageStale: true, StageStartedAt: &started},
		{Name: "pee02", PaneID: "w2:p2", Status: snapshot.StatusDone, Stage: "blocked: 1Password agent", StageStale: true},
		{Name: "pee03", PaneID: "w2:p3", Status: snapshot.StatusIdle, Stage: "review", StageAt: &started, StageStartedAt: &started},
	}}}}
}

// TestStaleStageStyle: under TrueColor a stale stage and its age are dim,
// faint and italic, which no current stage is, not even one that matches no
// colour rule and so is dim already; an old "blocked" takes no red and no
// bold (A42's F4). It holds in a row, on the boss line and in the group
// view. Not every terminal draws faint or italic, so the words say it too
// (TestStaleStageWords).
func TestStaleStageStyle(t *testing.T) {
	t.Parallel()
	const dim = "#6c7086"
	stale := func(t *testing.T, out, text string, n int) {
		t.Helper()
		sets := sgrBefore(t, out, text)
		require.Len(t, sets, n, "%q is drawn %d times", text, n)
		for _, set := range sets {
			assert.True(t, set["2"], "%q is faint: %v", text, set)
			assert.True(t, set["3"], "%q is italic: %v", text, set)
			assert.True(t, set[truecolor(dim)], "%q is dim: %v", text, set)
			assert.False(t, set["1"], "%q is not bold: %v", text, set)
		}
	}
	current := func(t *testing.T, out, text string) {
		t.Helper()
		sets := sgrBefore(t, out, text)
		require.NotEmpty(t, sets, "%q is drawn", text)
		for _, set := range sets {
			assert.False(t, set["2"], "%q is not faint: %v", text, set)
			assert.False(t, set["3"], "%q is not italic: %v", text, set)
		}
	}

	rows := dashboard.Render(dashboard.Frame{Snapshot: staleSnapshot(), Selection: dashboard.Selection{Group: -1}, Now: at}, 120, 24, dashboard.Options{Profile: colorprofile.TrueColor})
	stale(t, rows, "build_test", 1)
	stale(t, rows, "3h00m ago", 1)
	stale(t, rows, "blocked: 1Password agent", 1)
	stale(t, rows, "stale", 1)
	stale(t, rows, "waiting: QA", 1)
	stale(t, rows, " · 3h00m ago", 1)
	current(t, rows, "review")
	current(t, rows, "12m")

	view := dashboard.Render(dashboard.Frame{Snapshot: staleSnapshot(), Now: at, Group: "coders"}, 120, 40, dashboard.Options{Profile: colorprofile.TrueColor})
	stale(t, view, "build_test", 1)
	stale(t, view, "reported 3h00m ago", 1)
	stale(t, view, "blocked: 1Password agent", 1)
	stale(t, view, "stage stale", 1)
	current(t, view, "review")
	current(t, view, "in stage 12m")
}

// TestStaleStageWords: with no colour at all, the words say a stage is old:
// a row's time column reads "3h00m ago", or "stale" when no report gave a
// time, the boss line adds " · 3h00m ago", and the group view reads
// "reported 3h00m ago" or "stage stale". A current stage keeps its time in
// the stage, "12m" and "in stage 12m".
func TestStaleStageWords(t *testing.T) {
	t.Parallel()
	rows := dashboard.Render(dashboard.Frame{Snapshot: staleSnapshot(), Selection: dashboard.Selection{Group: -1}, Now: at}, 120, 24, plain)
	line := func(out, name string) string {
		for l := range strings.SplitSeq(out, "\n") {
			if strings.Contains(l, name) {
				return l
			}
		}
		return ""
	}
	assert.Regexp(t, `build_test +3h00m ago │$`, line(rows, "pee01"))
	assert.NotContains(t, line(rows, "pee01"), "12m", "a stale stage shows when it was reported, not how long it was held")
	assert.Regexp(t, `blocked: 1Password agent +stale │$`, line(rows, "pee02"))
	assert.Regexp(t, `review +12m │$`, line(rows, "pee03"))
	assert.Contains(t, line(rows, "boss ▸"), "waiting: QA · 3h00m ago")

	ahead := at.Add(time.Minute)
	clocks := staleSnapshot()
	clocks.Groups[0].Agents[0].StageAt = &ahead
	assert.Regexp(t, `build_test +stale │$`, line(dashboard.Render(dashboard.Frame{Snapshot: clocks, Selection: dashboard.Selection{Group: -1}, Now: at}, 120, 24, plain), "pee01"),
		"a stage_at ahead of the dashboard's clock gives no age, and the word stands in")

	view := dashboard.Render(dashboard.Frame{Snapshot: staleSnapshot(), Now: at, Group: "coders"}, 120, 40, plain)
	assert.Contains(t, view, "reported 3h00m ago")
	assert.Contains(t, view, "stage stale")
	assert.Contains(t, view, "in stage 12m")
	assert.Equal(t, 1, strings.Count(view, "in stage"), "only the current stage is in stage")
}
