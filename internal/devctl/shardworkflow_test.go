package devctl_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/abanna/herdr-agentisan/internal/devctl"
)

const shardCoverageRoot = "bin/covdata"

type shardStep struct {
	If    string
	Run   string
	Shell string
	Uses  string
	With  map[string]string
}

type shardJob struct {
	If    string
	Needs []string
	Steps []shardStep
}

// Coverage can stay well above the floor with a shard missing or duplicated.
// Check the workflow and its local entry point independently of coverage.
func TestWorkflowCoversEveryTestShard(t *testing.T) {
	t.Parallel()
	var ci struct{ Jobs map[string]shardJob }
	var taskfile struct {
		Tasks map[string]struct {
			Deps []string
			Env  map[string]struct{ Sh string }
			Cmds []yaml.Node
		}
	}
	readShardYAML(t, ".github/workflows/ci.yml", &ci)
	readShardYAML(t, "Taskfile.yml", &taskfile)

	shards := devctl.Shards()
	selector := regexp.MustCompile(`\bgo run \./cmd/devctl test-packages ([\w-]+)`)
	selected := make(map[string][]string)
	for id, job := range ci.Jobs {
		for _, step := range job.Steps {
			for _, match := range selector.FindAllStringSubmatch(step.Run, -1) {
				selected[match[1]] = append(selected[match[1]], id)
			}
		}
	}
	var jobs, tasks, directories []string
	for _, shard := range shards {
		job := "test-" + shard
		task := "test:unit:" + shard
		artifact := "covdata-" + shard
		dir := shardCoverageRoot + "/" + artifact
		jobs = append(jobs, job)
		tasks = append(tasks, task)
		directories = append(directories, dir)
		assert.Equal(t, []string{job}, selected[shard], "CI selector for %s", shard)
		delete(selected, shard)

		local := taskfile.Tasks[task]
		match := selector.FindAllStringSubmatch(local.Env["AGENTISAN_SHARD_PACKAGES"].Sh, -1)
		assert.Equal(t, [][]string{{"go run ./cmd/devctl test-packages " + shard, shard}}, match, "Task selector for %s", shard)
		step := findShardStep(t, ci.Jobs[job].Steps, func(s shardStep) bool { return strings.Contains(s.Run, "go test -race") })
		assert.Equal(t, "bash", step.Shell, "%s relies on Bash errexit", job)
		assert.Contains(t, step.Run, "-test.gocoverdir=\"$PWD/"+dir+"\"")
		upload := findShardStep(t, ci.Jobs[job].Steps, func(s shardStep) bool { return strings.HasPrefix(s.Uses, "actions/upload-artifact@") })
		assert.Equal(t, artifact, upload.With["name"])
		assert.Equal(t, dir+"/", upload.With["path"])
		assert.Equal(t, "error", upload.With["if-no-files-found"])
		t.Run(shard, func(t *testing.T) { checkShardSelector(t, step.Run, shard) })
	}
	assert.Empty(t, selected, "unknown selectors")
	assert.ElementsMatch(t, tasks, taskfile.Tasks["test:unit"].Deps, "Task aggregate deps")

	aggregate := ci.Jobs["test"]
	assert.ElementsMatch(t, jobs, aggregate.Needs, "coverage job needs")
	assert.Equal(t, "${{ !cancelled() }}", aggregate.If, "coverage job runs after failed dependencies")
	require.NotEmpty(t, aggregate.Steps)
	assert.Equal(t, "exit 1", aggregate.Steps[0].Run, "require step must fail before download or merge")
	assert.Equal(t, "${{ !cancelled() && (contains(needs.*.result, 'failure') || contains(needs.*.result, 'skipped') || contains(needs.*.result, 'cancelled')) }}", aggregate.Steps[0].If)
	download := findShardStep(t, aggregate.Steps, func(s shardStep) bool { return strings.HasPrefix(s.Uses, "actions/download-artifact@") })
	assert.Equal(t, "covdata-*", download.With["pattern"])
	assert.Equal(t, shardCoverageRoot, download.With["path"])

	merge := findShardStep(t, aggregate.Steps, func(s shardStep) bool { return strings.Contains(s.Run, "go tool covdata textfmt") })
	checkShardMergeInputs(t, "CI", merge.Run, shards, directories)
	var localCommands []string
	for _, cmd := range taskfile.Tasks["cover"].Cmds {
		if cmd.Kind == yaml.ScalarNode {
			localCommands = append(localCommands, cmd.Value)
			continue
		}
		var block struct{ Cmd string }
		require.NoError(t, cmd.Decode(&block))
		localCommands = append(localCommands, block.Cmd)
	}
	localMerge := strings.Join(localCommands, "\n")
	checkShardMergeInputs(t, "Task", localMerge, shards, directories)
	ciGuard, _, _ := strings.Cut(merge.Run, "go tool covdata textfmt")
	taskGuard, _, _ := strings.Cut(localMerge, "go tool covdata textfmt")
	assert.Equal(t, strings.TrimSpace(ciGuard), strings.TrimSpace(taskGuard))
	t.Run("coverage guards", func(t *testing.T) { checkShardArtifacts(t, merge.Run, directories) })
}

func readShardYAML(t *testing.T, name string, dst any) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", name))
	require.NoError(t, err)
	require.NoError(t, yaml.Unmarshal(data, dst), "%s", name)
}

func findShardStep(t *testing.T, steps []shardStep, matches func(shardStep) bool) shardStep {
	t.Helper()
	var found []shardStep
	for _, step := range steps {
		if matches(step) {
			found = append(found, step)
		}
	}
	require.Len(t, found, 1)
	return found[0]
}

func checkShardMergeInputs(t *testing.T, source, script string, shards, directories []string) {
	t.Helper()
	loop := regexp.MustCompile(`(?m)^for shard in ([\w -]+); do$`).FindStringSubmatch(script)
	require.Len(t, loop, 2, "%s coverage guard loop", source)
	assert.ElementsMatch(t, shards, strings.Fields(loop[1]), "%s coverage guard shards", source)
	inputs := regexp.MustCompile(`(?m)^go tool covdata textfmt -i=(\S+)`).FindStringSubmatch(script)
	require.Len(t, inputs, 2, "%s textfmt arguments", source)
	assert.ElementsMatch(t, directories, strings.Split(inputs[1], ","), "%s textfmt shards", source)
}

func checkShardSelector(t *testing.T, script, shard string) {
	dir := fakeShardGo(t)
	for _, tc := range []struct {
		name, packages, status string
		fails                  bool
	}{
		{"empty", "", "0", true},
		{"blank", "   ", "0", true},
		{"failed", "example.com/m/partial", "1", true},
		{"valid", "example.com/m/" + shard, "0", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := runShardShell(t, dir, script, "SELECT_PACKAGES="+tc.packages, "SELECT_STATUS="+tc.status)
			if tc.fails {
				assert.Error(t, err, "%s", out)
				assert.NotContains(t, out, "test operand ", "go test must not run")
				return
			}
			require.NoError(t, err, "%s", out)
			assert.Contains(t, out, "test operand "+tc.packages+"\n")
		})
	}
}

func checkShardArtifacts(t *testing.T, script string, directories []string) {
	dir := fakeShardGo(t)
	var files []string
	for _, path := range directories {
		path = filepath.Join(dir, path)
		require.NoError(t, os.MkdirAll(path, 0o700))
		for _, kind := range []string{"covcounters", "covmeta"} {
			file := filepath.Join(path, kind+".fixture")
			require.NoError(t, os.WriteFile(file, []byte("data"), 0o600))
			files = append(files, file)
		}
	}
	out, err := runShardShell(t, dir, script)
	require.NoError(t, err, "%s", out)
	require.Contains(t, out, "ran covdata")
	for _, file := range files {
		t.Run(filepath.Base(filepath.Dir(file))+"/"+filepath.Base(file), func(t *testing.T) {
			t.Cleanup(func() { require.NoError(t, os.WriteFile(file, []byte("data"), 0o600)) })
			for _, broken := range []string{"missing", "empty"} {
				t.Run(broken, func(t *testing.T) {
					if broken == "missing" {
						require.NoError(t, os.Remove(file))
					} else {
						require.NoError(t, os.WriteFile(file, nil, 0o600))
					}
					out, err := runShardShell(t, dir, script)
					assert.Error(t, err, "%s", out)
					assert.NotContains(t, out, "ran covdata")
					assert.Contains(t, out, strings.TrimPrefix(filepath.Base(filepath.Dir(file)), "covdata-"))
				})
			}
		})
	}
}

func fakeShardGo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go"), []byte(`#!/bin/sh
case "$1" in
  run) printf '%s\n' "$SELECT_PACKAGES"; exit "$SELECT_STATUS" ;;
  test) printf 'test operand %s\n' "$@" ;;
  tool) printf 'ran covdata\n' ;;
  *) exit 2 ;;
esac
`), 0o700))
	return dir
}

func runShardShell(t *testing.T, dir, script string, env ...string) (string, error) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "bash", "--noprofile", "--norc", "-eo", "pipefail", "-c", script)
	cmd.Dir = dir
	cmd.Env = append([]string{"PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH")}, env...)
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	return string(out), err
}
