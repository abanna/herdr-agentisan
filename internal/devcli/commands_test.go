package devcli_test

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/devcli"
)

// runDev executes the devctl tree with args and returns its output.
func runDev(t *testing.T, args ...string) (string, error) {
	t.Helper()

	root := devcli.Root()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)

	err := root.ExecuteContext(t.Context())
	return out.String(), err
}

func TestDocsParityCommandPasses(t *testing.T) {
	out, err := runDev(t, "docs-parity")
	require.NoError(t, err)
	assert.Contains(t, out, "agree")
}

func TestCoverageCommandFailsBelowTheFloor(t *testing.T) {
	// Write a profile this test controls rather than depending on the repo's
	// coverage.out: a gate test that skips when a file is absent is a gate
	// that can vanish on a clean checkout.
	path := filepath.Join(t.TempDir(), "low.out")
	require.NoError(t, os.WriteFile(path, []byte(lowCoverageProfile), 0o600))

	_, err := runDev(t, "coverage", "--profile", path, "--min", "90")
	require.Error(t, err, "a profile under the floor must fail")
	assert.ErrorContains(t, err, "below the")
}

func TestCoverageCommandPassesAboveTheFloor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "high.out")
	require.NoError(t, os.WriteFile(path, []byte(lowCoverageProfile), 0o600))

	// A floor of 0 is met by any profile, exercising the success path end to
	// end: the command runs `go tool cover`, parses the total, compares and
	// reports. Real parsing accuracy is asserted in TestParseCoverageTotal.
	out, err := runDev(t, "coverage", "--profile", path, "--min", "0")
	require.NoError(t, err)
	assert.Contains(t, out, "ok")
}

// lowCoverageProfile is a minimal valid coverage profile. `go tool cover`
// resolves it against real sources, so the exact percentage it reports is not
// the point — only that the command parses a total and applies the floor.
const lowCoverageProfile = `mode: atomic
github.com/abanna/herdr-agentisan/internal/devcli/coverage.go:10.20,12.2 1 1
github.com/abanna/herdr-agentisan/internal/devcli/coverage.go:14.20,16.2 1 0
`

func TestCoverageCommandRejectsAMissingProfile(t *testing.T) {
	_, err := runDev(t, "coverage", "--profile", "does-not-exist.out")
	require.Error(t, err)
	assert.ErrorContains(t, err, "not found")
}

func TestCheckCoverageFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "summary.txt")
	require.NoError(t, os.WriteFile(path, []byte("total:\t(statements)\t88.0%\n"), 0o600))

	res, err := devcli.CheckCoverageFile(path, 75)
	require.NoError(t, err)
	assert.InDelta(t, 88.0, res.Percent, 0.001)
	assert.True(t, res.Meets())
}

func TestCheckCoverageFileMissing(t *testing.T) {
	t.Parallel()

	_, err := devcli.CheckCoverageFile(filepath.Join(t.TempDir(), "absent"), 75)
	require.Error(t, err)
}

// TestParityReportStringNamesEveryDirection guards the failure message: an
// agent reading it must be told which of the three sources to change.
func TestParityReportStringNamesEveryDirection(t *testing.T) {
	t.Parallel()

	rep := devcli.ParityReport{
		UndocumentedCIGates: []string{"go vet ./..."},
		DocumentedNotInCI:   []string{"go build ./..."},
		TaskGatesNotInCI:    []string{"lint: go tool golangci-lint run ./..."},
		CIGatesNotInTask:    []string{"go mod tidy"},
	}
	require.False(t, rep.OK())

	got := rep.String()
	for _, want := range []string{
		"docs parity FAILED",
		"AGENTS.md does not document them",
		"CI does not run them",
		"Taskfile gates whose command CI does not run",
		"`task check` is weaker than CI",
	} {
		assert.Containsf(t, got, want, "failure report should explain %q", want)
	}
}

func TestParityReportOKMessage(t *testing.T) {
	t.Parallel()

	rep := devcli.ParityReport{}
	assert.True(t, rep.OK())
	assert.Contains(t, rep.String(), "agree")
}

func TestCheckParityOnABrokenTree(t *testing.T) {
	t.Parallel()

	// A tree with no AGENTS.md must error rather than silently report parity.
	_, err := devcli.CheckParity(t.TempDir())
	require.Error(t, err)
	assert.ErrorContains(t, err, "AGENTS.md")
}

// writeParityTree builds a minimal repository whose three gate sources can be
// varied independently, so a runner's visibility to the check can be asserted
// rather than assumed.
func writeParityTree(t *testing.T, agentsRow, taskCmd, ciRun string) string {
	t.Helper()
	root := t.TempDir()

	agents := "# AGENTS\n\n| Task | Command |\n|------|---------|\n" +
		"| Lint | `go vet ./...` |\n" + agentsRow + "\n"
	taskfile := "version: \"3\"\ntasks:\n  vet:\n    cmds:\n      - go vet ./...\n" + taskCmd
	ci := "name: CI\njobs:\n  gate:\n    steps:\n      - run: go vet ./...\n" + ciRun

	require.NoError(t, os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte(agents), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "Taskfile.yml"), []byte(taskfile), 0o600))
	wf := filepath.Join(root, ".github", "workflows")
	require.NoError(t, os.MkdirAll(wf, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(wf, "ci.yml"), []byte(ci), 0o600))
	return root
}

// TestStaleExemptionsFlagsEntriesWithNothingBehindThem: the minimal tree has
// only a `vet` task and one table row, so every real escape-hatch entry points
// at nothing and must be reported, naming which map to edit.
func TestStaleExemptionsFlagsEntriesWithNothingBehindThem(t *testing.T) {
	t.Parallel()

	stale, err := devcli.StaleExemptions(writeParityTree(t, "", "", ""))
	require.NoError(t, err)
	assert.Contains(t, stale, `nonGateTasks["build:cli"]: Taskfile.yml has no such task`)
	assert.Contains(t, stale, `localOnly["task run:cli"]: AGENTS.md does not document this command`)
	assert.True(t, slices.IsSorted(stale), "report order must be stable")
}

func TestStaleExemptionsOnABrokenTree(t *testing.T) {
	t.Parallel()

	_, err := devcli.StaleExemptions(t.TempDir())
	require.Error(t, err)
}

const (
	dockerGateCmd  = "docker run --rm prom/prometheus:v3.6.0 test rules x.yaml"
	dockerAgentRow = "| Alerts | `" + dockerGateCmd + "` |"
	dockerTaskCmd  = "  alerts:\n    cmds:\n      - " + dockerGateCmd + "\n"
	dockerCIRun    = "      - run: " + dockerGateCmd + "\n"
)

// TestParitySeesADockerRunner is the regression test for the change that made
// the alert-rule gate checkable at all. A runner missing from isGateCommand is
// dropped from the CI side of the comparison, so the gate then reports drift
// in two directions for a repository that is perfectly consistent — and the
// tempting "fix" is to add an entry to localOnly, which weakens the check.
func TestParitySeesADockerRunner(t *testing.T) {
	t.Parallel()

	rep, err := devcli.CheckParity(writeParityTree(t, dockerAgentRow, dockerTaskCmd, dockerCIRun))
	require.NoError(t, err)
	assert.Truef(t, rep.OK(), "a consistent docker gate must pass: %s", rep)
}

func TestParityCatchesADockerGateMissingFromCI(t *testing.T) {
	t.Parallel()

	rep, err := devcli.CheckParity(writeParityTree(t, dockerAgentRow, dockerTaskCmd, ""))
	require.NoError(t, err)
	require.False(t, rep.OK())
	assert.Contains(t, rep.DocumentedNotInCI, dockerGateCmd)
	assert.Contains(t, rep.TaskGatesNotInCI, "alerts: "+dockerGateCmd)
}

func TestParityCatchesADockerGateMissingFromTheDocs(t *testing.T) {
	t.Parallel()

	rep, err := devcli.CheckParity(writeParityTree(t, "", dockerTaskCmd, dockerCIRun))
	require.NoError(t, err)
	require.False(t, rep.OK())
	assert.Contains(t, rep.UndocumentedCIGates, dockerGateCmd)
}

func TestParityCatchesADockerGateMissingFromTheTaskfile(t *testing.T) {
	t.Parallel()

	rep, err := devcli.CheckParity(writeParityTree(t, dockerAgentRow, "", dockerCIRun))
	require.NoError(t, err)
	require.False(t, rep.OK())
	assert.Contains(t, rep.CIGatesNotInTask, dockerGateCmd)
}
