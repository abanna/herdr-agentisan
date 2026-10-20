package telemetry_test

import (
	"maps"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"text/template"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/abanna/herdr-agentisan/internal/settings"
)

// The collector that carries the events into Elasticsearch ships as a config
// (docs/telemetry.md), not code: these tests are what keep it, the task that
// runs it and the page that documents it saying the same thing.
var (
	collectorPath = filepath.Join("..", "..", "docs", "config", "otelcol-agentisan.yaml")
	samplePath    = filepath.Join("..", "..", "docs", "config", "herdr-agentisan.example.toml")
	taskfilePath  = filepath.Join("..", "..", "Taskfile.yml")
	telemetryPath = filepath.Join("..", "..", "docs", "telemetry.md")
)

// The collector's container and the variables it reads.
const (
	collectorName = "herdr-agentisan-otelcol"
	urlVar        = "HERDR_AGENTISAN_ES_URL"
	writeVar      = "HERDR_AGENTISAN_ES_WRITE_KEY"
)

func loadYAML(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, yaml.Unmarshal(raw, &doc))
	return doc
}

// at walks m down keys, failing unless every step is a table.
func at(t *testing.T, m map[string]any, keys ...string) map[string]any {
	t.Helper()
	for i, k := range keys {
		next, ok := m[k].(map[string]any)
		require.Truef(t, ok, "%s is not a table", strings.Join(keys[:i+1], "."))
		m = next
	}
	return m
}

func keysOf(m map[string]any) []string {
	return slices.Sorted(maps.Keys(m))
}

// TestCollectorReceivesWhereTheSamplePoints: the receiver is OTLP/HTTP only,
// on loopback, since it is plaintext and unauthenticated, and on the
// endpoint the sample config sends the events to.
func TestCollectorReceivesWhereTheSamplePoints(t *testing.T) {
	t.Parallel()
	cfg := loadYAML(t, collectorPath)

	assert.Equal(t, []string{"otlp"}, keysOf(at(t, cfg, "receivers")))
	assert.Equal(t, []string{"protocols"}, keysOf(at(t, cfg, "receivers", "otlp")))
	assert.Equal(t, []string{"http"}, keysOf(at(t, cfg, "receivers", "otlp", "protocols")), "no gRPC: the daemon speaks OTLP/HTTP")
	httpRecv := at(t, cfg, "receivers", "otlp", "protocols", "http")
	assert.Equal(t, []string{"endpoint"}, keysOf(httpRecv), "no TLS or auth settings to drift")
	endpoint, _ := httpRecv["endpoint"].(string)
	host, _, err := net.SplitHostPort(endpoint)
	require.NoError(t, err)
	ip := net.ParseIP(host)
	require.NotNilf(t, ip, "the receiver's host %q is not an IP, so it could resolve off loopback", host)
	assert.True(t, ip.IsLoopback(), "the receiver listens on loopback only")

	sample, err := os.ReadFile(samplePath)
	require.NoError(t, err)
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, settings.FileName), sample, 0o600))
	want, err := settings.TelemetryEndpoint(dir, "")
	require.NoError(t, err)
	assert.Equal(t, want, endpoint, "the sample's [telemetry] endpoint is this collector")
}

// TestCollectorWritesOnlyTheAgentisanStream: one logs pipeline, whose
// processor puts every record's resource in the agentisan dataset, so the
// exporter routes it to logs-agentisan.otel-default.
func TestCollectorWritesOnlyTheAgentisanStream(t *testing.T) {
	t.Parallel()
	cfg := loadYAML(t, collectorPath)

	assert.Equal(t, []string{"resource/agentisan"}, keysOf(at(t, cfg, "processors")))
	attrs, ok := at(t, cfg, "processors", "resource/agentisan")["attributes"].([]any)
	require.True(t, ok, "resource/agentisan has an attributes list")
	assert.Equal(t, []any{
		map[string]any{"key": "data_stream.dataset", "value": "agentisan", "action": "upsert"},
		map[string]any{"key": "data_stream.namespace", "value": "default", "action": "upsert"},
	}, attrs, "upsert, so a sender's own resource cannot pick another stream")

	assert.Equal(t, []string{"logs"}, keysOf(at(t, cfg, "service", "pipelines")), "logs only: the key can write no traces stream")
	assert.Equal(t, map[string]any{
		"receivers":  []any{"otlp"},
		"processors": []any{"resource/agentisan"},
		"exporters":  []any{"elasticsearch"},
	}, at(t, cfg, "service", "pipelines", "logs"))

	assert.Equal(t, []string{"metrics"}, keysOf(at(t, cfg, "service", "telemetry")))
	assert.Equal(t, map[string]any{"level": "none"}, at(t, cfg, "service", "telemetry", "metrics"),
		"the collector's own metrics would bind :8888, which another collector on the host network may hold")
}

// TestCollectorTakesElasticsearchFromTheEnvironment: the URL and the key are
// references to the two variables and nothing else, so no credential can be
// written into the file, and no setting that would log a document is on.
func TestCollectorTakesElasticsearchFromTheEnvironment(t *testing.T) {
	t.Parallel()
	cfg := loadYAML(t, collectorPath)

	assert.Equal(t, []string{"elasticsearch"}, keysOf(at(t, cfg, "exporters")))
	es := at(t, cfg, "exporters", "elasticsearch")
	assert.Equal(t, []string{"api_key", "endpoint", "mapping"}, keysOf(es),
		"no user, password, headers, auth, logs_index or include_source_on_error")
	assert.Equal(t, "${env:"+urlVar+"}", es["endpoint"])
	assert.Equal(t, "${env:"+writeVar+"}", es["api_key"])
	assert.Equal(t, map[string]any{"allowed_modes": []any{"otel"}}, es["mapping"])
	assert.NotContains(t, cfg, "extensions", "no authenticator extension carries a credential either")
}

// render expands c as Task does before running it: its only actions are
// {{.ROOT_DIR}} and the {{"{{"}} escapes for docker's own braces.
func render(t *testing.T, c, root string) string {
	t.Helper()
	tmpl, err := template.New("cmd").Option("missingkey=error").Parse(c)
	require.NoError(t, err)
	var b strings.Builder
	require.NoError(t, tmpl.Execute(&b, map[string]string{"ROOT_DIR": root}))
	return b.String()
}

// dockerRuns returns task's commands that run a container.
func dockerRuns(t *testing.T, task string) (runs []string, all []string) {
	t.Helper()
	tasks := at(t, loadYAML(t, taskfilePath), "tasks")
	cmds, ok := at(t, tasks, task)["cmds"].([]any)
	require.Truef(t, ok, "task %s has cmds", task)
	for _, c := range cmds {
		s, ok := c.(string)
		require.Truef(t, ok, "task %s runs only shell commands", task)
		all = append(all, s)
		if strings.HasPrefix(s, "docker run ") {
			runs = append(runs, s)
		}
	}
	return runs, all
}

var envFlag = regexp.MustCompile(`(?:^|\s)-e\s+(\S+)`)

// TestOtelcolTaskRunsTheShippedConfig: both containers `task otelcol:up`
// runs, the validation and the collector, bind this config read-only from
// the checkout and are handed exactly the variables it reads, by name: a
// value never appears on a command line, and --env-file, which hands over a
// whole file of variables, is never used.
func TestOtelcolTaskRunsTheShippedConfig(t *testing.T) {
	t.Parallel()
	runs, all := dockerRuns(t, "otelcol:up")
	require.Len(t, runs, 2, "validate, then the collector")
	// Task stops at the first command that fails, so this order is what
	// keeps a missing variable or a broken config from removing a running
	// collector, and a collector that cannot start from passing.
	require.Len(t, all, 5, "guard, validate, remove, run, check")
	assert.Equal(t, runs[0], all[1], "validate before anything is removed")
	assert.True(t, strings.HasPrefix(all[2], "docker rm -f "+collectorName+" "), "then the old container goes")
	assert.Equal(t, runs[1], all[3], "then the new one runs")
	check := render(t, all[4], t.TempDir())
	assert.Contains(t, check, "docker inspect --format '{{.State.Running}} {{.RestartCount}}' "+collectorName,
		"and must be running, never having restarted: a crash loop is briefly running between attempts")
	assert.Contains(t, check, `!= "true 0"`)
	assert.Contains(t, runs[0], " validate --config=/etc/otelcol/agentisan.yaml")
	assert.Contains(t, runs[1], " -d --name "+collectorName+" --network host --restart unless-stopped ")

	pinned := regexp.MustCompile(` otel/opentelemetry-collector-contrib:\d+\.\d+\.\d+ `)
	for _, run := range runs {
		var vars []string
		for _, m := range envFlag.FindAllStringSubmatch(run, -1) {
			vars = append(vars, m[1])
		}
		assert.Equal(t, []string{urlVar, writeVar}, vars, "by name, never NAME=value")
		assert.Contains(t, run, ` --mount "type=bind,src={{.ROOT_DIR}}/docs/config/otelcol-agentisan.yaml,dst=/etc/otelcol/agentisan.yaml,readonly" `,
			"--mount refuses a missing file, where -v would create a directory in its place")
		assert.NotContains(t, run, " -v ")
		assert.Regexp(t, pinned, run, "a pinned image")
	}
	for _, c := range all {
		assert.NotContains(t, c, "--env-file")
	}
	assert.Contains(t, all[0], "${"+urlVar+":?", "refuses before Docker runs when the URL is unset or empty")
	assert.Contains(t, all[0], "${"+writeVar+":?", "and when the key is")

	_, down := dockerRuns(t, "otelcol:down")
	assert.Equal(t, []string{"docker rm -f " + collectorName}, down)
}

// TestTelemetryDocRunsTheTasksContainer: the `docker run` docs/telemetry.md
// gives a user without Task is the one `task otelcol:up` runs, from the
// checkout's directory.
func TestTelemetryDocRunsTheTasksContainer(t *testing.T) {
	t.Parallel()
	runs, _ := dockerRuns(t, "otelcol:up")
	require.Len(t, runs, 2)

	page, err := os.ReadFile(telemetryPath)
	require.NoError(t, err)
	var doc string
	lines := strings.Split(string(page), "\n")
	for i := 0; i < len(lines); i++ {
		if !strings.HasPrefix(lines[i], "docker run -d ") {
			continue
		}
		require.Empty(t, doc, "the page shows one docker run -d")
		cmd := lines[i]
		for strings.HasSuffix(cmd, `\`) && i+1 < len(lines) {
			i++
			cmd = strings.TrimSuffix(cmd, `\`) + " " + lines[i]
		}
		doc = cmd
	}
	require.NotEmpty(t, doc, "docs/telemetry.md shows the container's docker run")
	doc = strings.ReplaceAll(doc, "$PWD", "{{.ROOT_DIR}}")
	assert.Equal(t, strings.Fields(runs[1]), strings.Fields(doc))
}

// fakeDocker is a stand-in docker for TestOtelcolUpStopsAtTheFirstFailure:
// it appends its first two arguments to $DOCKER_LOG, fails when its
// arguments contain $DOCKER_FAIL, and answers `inspect` with $DOCKER_INSPECT.
const fakeDocker = `#!/bin/sh
printf '%s %s\n' "$1" "$2" >> "$DOCKER_LOG"
if [ -n "$DOCKER_FAIL" ]; then
  case "$*" in *"$DOCKER_FAIL"*) echo "docker: failed" >&2; exit 1 ;; esac
fi
if [ "$1" = inspect ] && [ -n "$DOCKER_INSPECT" ]; then echo "$DOCKER_INSPECT"; fi
exit 0
`

// TestOtelcolUpStopsAtTheFirstFailure runs the commands `task otelcol:up`
// runs, rendered as Task renders them, in order, each under sh (Task runs
// them with its own POSIX shell) and stopping at the first that fails as
// Task does, against a stand-in docker: a missing variable stops before any
// docker call, a failed validate before the running collector is removed,
// and a collector that is not running 3 s on, or has restarted, fails the
// task. No case echoes a value.
func TestOtelcolUpStopsAtTheFirstFailure(t *testing.T) {
	t.Parallel()
	_, cmds := dockerRuns(t, "otelcol:up")
	given := map[string]string{urlVar: "http://127.0.0.1:1", writeVar: "guard-probe-value"}
	var (
		validate = "run --rm"
		remove   = "rm -f"
		start    = "run -d"
		running  = "inspect --format"
		all      = []string{validate, remove, start, running}
	)
	for name, tc := range map[string]struct {
		env       map[string]string
		fail      string
		inspect   string // what docker inspect prints 3 s on
		noDocker  bool
		wantCalls []string
		wantOut   string // "" for a clean start
	}{
		"a clean start":                {env: given, inspect: "true 0", wantCalls: all},
		"no container to remove":       {env: given, fail: "rm -f", inspect: "true 0", wantCalls: all},
		"no URL":                       {env: map[string]string{writeVar: given[writeVar]}, wantOut: urlVar},
		"an empty URL":                 {env: map[string]string{urlVar: "", writeVar: given[writeVar]}, wantOut: urlVar},
		"no key":                       {env: map[string]string{urlVar: given[urlVar]}, wantOut: writeVar},
		"an empty key":                 {env: map[string]string{urlVar: given[urlVar], writeVar: ""}, wantOut: writeVar},
		"a config that fails validate": {env: given, fail: "validate", wantCalls: []string{validate}, wantOut: "docker: failed"},
		"a run that fails":             {env: given, fail: "--restart", wantCalls: []string{validate, remove, start}, wantOut: "docker: failed"},
		"exited by 3 s on":             {env: given, inspect: "false 1", wantCalls: all, wantOut: "docker logs " + collectorName},
		"gone by 3 s on":               {env: given, wantCalls: all, wantOut: "docker logs " + collectorName},
		"crash-looping, running again": {env: given, inspect: "true 3", wantCalls: all, wantOut: "docker logs " + collectorName},
		"no docker at all":             {env: given, noDocker: true, wantOut: "docker"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			bin, dockerLog := t.TempDir(), filepath.Join(t.TempDir(), "calls")
			if !tc.noDocker {
				require.NoError(t, os.WriteFile(filepath.Join(bin, "docker"), []byte(fakeDocker), 0o700)) //nolint:gosec // an executable stand-in
			}
			// The check's `sleep 3`, made instant.
			require.NoError(t, os.WriteFile(filepath.Join(bin, "sleep"), []byte("#!/bin/sh\nexit 0\n"), 0o700)) //nolint:gosec // an executable stand-in
			env := []string{"PATH=" + bin, "DOCKER_LOG=" + dockerLog, "DOCKER_FAIL=" + tc.fail, "DOCKER_INSPECT=" + tc.inspect}
			for k, v := range tc.env {
				env = append(env, k+"="+v)
			}
			var out []byte
			var err error
			root := t.TempDir()
			for _, c := range cmds {
				cmd := exec.CommandContext(t.Context(), "sh", "-c", render(t, c, root))
				cmd.Env = env // only what the case sets, never the test's own
				var o []byte
				o, err = cmd.CombinedOutput()
				out = append(out, o...)
				if err != nil {
					break
				}
			}
			logged, _ := os.ReadFile(dockerLog)
			var calls []string
			if s := strings.TrimSpace(string(logged)); s != "" {
				calls = strings.Split(s, "\n")
			}
			assert.Equal(t, tc.wantCalls, calls, "docker calls, in order")
			if tc.wantOut == "" {
				assert.NoError(t, err, string(out))
			} else {
				require.Error(t, err, "the task stops")
				assert.Contains(t, string(out), tc.wantOut)
			}
			for _, v := range given {
				assert.NotContains(t, string(out), v, "a value is never echoed")
			}
		})
	}
}
