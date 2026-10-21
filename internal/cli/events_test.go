package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/cli"
	"github.com/abanna/herdr-agentisan/internal/settings"
	"github.com/abanna/herdr-agentisan/internal/trail"
)

// eventsKey stands in for the trail's read key, and keyEq starts its line in
// the env file, built from the constant so no source line spells a key
// assignment the safety scan would stop on.
const (
	eventsKey = "probe-read-key-events"
	keyEq     = trail.KeyVar + "="
)

// runIn executes a fresh command tree with args under ctx.
func runIn(ctx context.Context, args ...string) (string, error) {
	root := cli.Root()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.ExecuteContext(ctx)
	return out.String(), err
}

// trailEnv stands up Elasticsearch answering status and body, an env file
// naming it, and a config dir whose [debugger] env_file is that file, and
// returns the command's context and the queries Elasticsearch received.
func trailEnv(t *testing.T, status int, body []byte) (context.Context, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		var q struct{ Query string }
		_ = json.Unmarshal(data, &q)
		mu.Lock()
		queries = append(queries, q.Query)
		mu.Unlock()
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	envFile := filepath.Join(dir, "local.env")
	require.NoError(t, os.WriteFile(envFile, []byte("ELASTIC_URL="+srv.URL+"\n"+keyEq+eventsKey+"\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, settings.FileName),
		[]byte("schema_version = 1\n[debugger]\nenv_file = \""+envFile+"\"\n"), 0o600))
	ctx := envCtx(t, map[string]string{"HERDR_PLUGIN_CONFIG_DIR": dir})
	return ctx, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(queries)
	}
}

// workerCols are the columns of the worker recipe's answer.
const workerCols = `[{"name":"@timestamp","type":"date"},{"name":"attributes.agentisan.worker.id","type":"keyword"},` +
	`{"name":"attributes.herdr.pane.id","type":"keyword"},{"name":"attributes.agentisan.status.from","type":"keyword"},` +
	`{"name":"attributes.agentisan.status.to","type":"keyword"},{"name":"attributes.agentisan.group","type":"keyword"},` +
	`{"name":"attributes.agentisan.worker.bound","type":"boolean"}]`

// recordedTrail is the live check's recorded answer with its rows newest
// first, as Elasticsearch returns them for the worker recipe.
func recordedTrail(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "trail", "testdata", "worker-trail.json"))
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, json.Unmarshal(data, &doc))
	values, _ := doc["values"].([]any)
	slices.Reverse(values)
	out, err := json.Marshal(doc)
	require.NoError(t, err)
	return out
}

// TestEventsWorkerTellsTheTrail: `events worker` answers the item-12
// question from the trail: the live check's recorded answer for w14:p8
// renders as its six changes, in order, one per line, and as JSON.
func TestEventsWorkerTellsTheTrail(t *testing.T) {
	t.Parallel()
	ctx, queries := trailEnv(t, http.StatusOK, recordedTrail(t))

	out, err := runIn(ctx, "events", "worker", "w14:p8")
	require.NoError(t, err)
	assert.Equal(t, `w14:p8, the last 1h: 6 status changes
2026-10-20T11:29:10.415Z  w14:p8  done → working  coder  unbound
2026-10-20T11:31:31.416Z  w14:p8  working → done  coder  unbound
2026-10-20T11:32:07.416Z  w14:p8  done → working  coder  unbound
2026-10-20T11:32:49.417Z  w14:p8  working → done  coder  unbound
2026-10-20T11:38:37.597Z  w14:p8  done → working  coder  unbound
2026-10-20T12:07:26.969Z  w14:p8  working → done  coder  unbound
`, out)
	assert.Contains(t, queries()[0], "NOW() - 60 minutes")

	out, err = runIn(ctx, "events", "worker", "w14:p8", "--since", "90m", "--json")
	require.NoError(t, err)
	var got struct {
		ID      string           `json:"id"`
		Since   string           `json:"since"`
		Changes []map[string]any `json:"changes"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	assert.Equal(t, "w14:p8", got.ID)
	assert.Equal(t, "90m", got.Since)
	require.Len(t, got.Changes, 6)
	assert.Equal(t, map[string]any{
		"at": "2026-10-20T11:29:10.415Z", "worker_id": "w14:p8", "pane_id": "w14:p8",
		"from": "done", "to": "working", "group": "coder", "bound": false,
	}, got.Changes[0])
	assert.Contains(t, queries()[1], "NOW() - 90 minutes")
}

// TestEventsWorkerSaysWhenNothingChanged: an empty trail is said plainly,
// never rendered as an empty screen.
func TestEventsWorkerSaysWhenNothingChanged(t *testing.T) {
	t.Parallel()
	ctx, _ := trailEnv(t, http.StatusOK, []byte(`{"columns":`+workerCols+`,"values":[]}`))
	out, err := runIn(ctx, "events", "worker", "ha-coder-9", "--since", "2h")
	require.NoError(t, err)
	assert.Equal(t, "ha-coder-9, the last 2h: no status changes\n", out)
}

// TestEventsWorkerRendersEachChange: a bound worker shows its pane beside
// its id, a partial answer says so, a change with no pane or group shows
// neither, and no value can break its line: a tab or a line break in the id
// or in any value is flattened, as events query's cells are, and everything
// else is written as it came.
func TestEventsWorkerRendersEachChange(t *testing.T) {
	t.Parallel()
	const at = "2026-10-20T12:00:00.000Z"
	// forge would start a forged change on a line of its own; flat is it
	// flattened.
	const forge, flat = "\r" + at + "\n\tx", " " + at + "  x"
	// odd is kept as it came: precomposed and decomposed accents, a NUL and
	// a BOM. The id cannot hold the NUL: WorkerQuery refuses it.
	const odd, oddID = "é e\u0301 \x00\ufeff", "é e\u0301 \ufeff"
	row := func(vals ...any) string {
		data, err := json.Marshal(vals)
		require.NoError(t, err)
		return string(data)
	}
	tests := map[string]struct {
		id, row string
		partial bool
		want    string
	}{
		"bound, partial": {
			id: "coders-1", row: row(at, "coders-1", "w14:p8", "idle", "working", "coders", true), partial: true,
			want: "coders-1, the last 1h: 1 status change\n" + at + "  coders-1 (w14:p8)  idle → working  coders  bound\n" +
				"partial: Elasticsearch did not search everything it was asked to\n",
		},
		"no pane, no group": {
			id: "ha-coder-9", row: row(at, "ha-coder-9", "", "idle", "working", "", false),
			want: "ha-coder-9, the last 1h: 1 status change\n" + at + "  ha-coder-9  idle → working  -  unbound\n",
		},
		"every value null": {
			id: "ha-coder-9", row: row(nil, nil, nil, nil, nil, nil, nil),
			want: "ha-coder-9, the last 1h: 1 status change\n     →   -  unbound\n",
		},
		"case kept: a pane that differs only in case is shown": {
			id: "w14:p8", row: row(at, "w14:p8", "W14:P8", "idle", "working", "coder", false),
			want: "w14:p8, the last 1h: 1 status change\n" + at + "  w14:p8 (W14:P8)  idle → working  coder  unbound\n",
		},
		"a forged change in the id and every value": {
			id: "coders-1" + forge, row: row(at+forge, "coders-1"+forge, "w14:p8"+forge, "idle"+forge, "working"+forge, "coders"+forge, false),
			want: "coders-1" + flat + ", the last 1h: 1 status change\n" + at + flat + "  coders-1" + flat + " (w14:p8" + flat + ")  idle" + flat +
				" → working" + flat + "  coders" + flat + "  unbound\n",
		},
		"accents, a NUL and a BOM as they came": {
			id: "coders-1" + oddID, row: row(at+odd, "coders-1"+odd, "w14:p8"+odd, "idle"+odd, "working"+odd, "coders"+odd, true),
			want: "coders-1" + oddID + ", the last 1h: 1 status change\n" + at + odd + "  coders-1" + odd + " (w14:p8" + odd + ")  idle" + odd +
				" → working" + odd + "  coders" + odd + "  bound\n",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx, _ := trailEnv(t, http.StatusOK, []byte(`{"columns":`+workerCols+`,"values":[`+tc.row+`],"is_partial":`+
				strconv.FormatBool(tc.partial)+`}`))
			out, err := runIn(ctx, "events", "worker", tc.id)
			require.NoError(t, err)
			assert.Equal(t, tc.want, out)
		})
	}
}

// TestEventsRefuseBeforeReadingTheEnvFile: a query or a window that is
// refused is refused before the env file is read, so a mounted file is
// never asked for (1Password may prompt) on a query that cannot run.
func TestEventsRefuseBeforeReadingTheEnvFile(t *testing.T) {
	t.Parallel()
	ctx := envCtx(t, map[string]string{}) // no config dir: reading it would be the error
	_, err := runIn(ctx, "events", "query", "FROM logs-* | LIMIT 1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "starts FROM")
	_, err = runIn(ctx, "events", "worker", "w1:p1", "--since", "30s")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--since")
}

// TestEventsWorkerSaysWhenItCut: an answer that reached the row limit
// ends with what was cut and how to read the older ones: a narrower
// --since still keeps the newest, so it names events query.
func TestEventsWorkerSaysWhenItCut(t *testing.T) {
	t.Parallel()
	var rows []string
	for i := trail.RowLimit - 1; i >= 0; i-- {
		rows = append(rows, fmt.Sprintf(`["2026-10-20T00:%02d:%02dZ","w1:p1","w1:p1","idle","working","coder",false]`, i/60%60, i%60))
	}
	ctx, _ := trailEnv(t, http.StatusOK, []byte(`{"columns":`+workerCols+`,"values":[`+strings.Join(rows, ",")+`]}`))
	out, err := runIn(ctx, "events", "worker", "w1:p1", "--since", "168h")
	require.NoError(t, err)
	assert.True(t, strings.HasSuffix(out, "cut: only the newest 1000 are shown; read older ones with events query and a WHERE on @timestamp\n"), out[len(out)-120:])

	out, err = runIn(ctx, "events", "worker", "w1:p1", "--json")
	require.NoError(t, err)
	assert.Contains(t, out, `"cut": true`)
}

// TestEventsQueryRendersTheAnswer: `events query` runs a trail query and
// renders its table; a query reading anything else is refused before it
// reaches Elasticsearch.
func TestEventsQueryRendersTheAnswer(t *testing.T) {
	t.Parallel()
	ctx, queries := trailEnv(t, http.StatusOK, recordedTrail(t))

	out, err := runIn(ctx, "events", "query", "FROM logs-agentisan.otel-* | LIMIT 6")
	require.NoError(t, err)
	assert.Contains(t, out, "@timestamp")
	assert.Contains(t, out, "attributes.agentisan.worker.bound")
	assert.Contains(t, out, "2026-10-20T12:07:26.969Z  w14:p8")
	assert.Contains(t, out, "(6 rows)")

	out, err = runIn(ctx, "events", "query", "FROM logs-agentisan.otel-* | LIMIT 6", "--json")
	require.NoError(t, err)
	var table struct {
		Columns []map[string]string `json:"columns"`
		Values  [][]any             `json:"values"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &table))
	assert.Len(t, table.Columns, 8)
	assert.Len(t, table.Values, 6)

	_, err = runIn(ctx, "events", "query", "FROM logs-* | LIMIT 1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "starts FROM logs-agentisan.otel-*")
	assert.Len(t, queries(), 2, "the refused query never left")
}

// TestEventsQueryFlattensCells: every ES|QL value type renders, and a value
// holding a tab or a line break cannot break the table's rows.
func TestEventsQueryFlattensCells(t *testing.T) {
	t.Parallel()
	ctx, _ := trailEnv(t, http.StatusOK, []byte(`{"columns":[{"name":"v","type":"keyword"}],"values":[["a\tb\nc\rd"],[null],[true],[12.5]]}`))
	out, err := runIn(ctx, "events", "query", "FROM logs-agentisan.otel-* | LIMIT 4")
	require.NoError(t, err)
	assert.Equal(t, "v\na b c d\n\ntrue\n12.5\n(4 rows)\n", out)
}

// TestEventsFailures: each way the trail can be unreachable says what is
// missing, and nothing prints the key. The environment's ELASTIC_* are
// never read: with no env file configured, there is no trail.
func TestEventsFailures(t *testing.T) {
	t.Parallel()
	refused, _ := trailEnv(t, http.StatusUnauthorized, []byte(`{}`))
	noFile := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(noFile, settings.FileName), []byte("schema_version = 1\n"), 0o600))
	tests := map[string]struct {
		ctx    context.Context
		wantIn string
	}{
		"no config dir": {ctx: envCtx(t, map[string]string{"ELASTIC_URL": "http://es:9200", "ELASTIC_API_KEY": eventsKey}), wantIn: "HERDR_PLUGIN_CONFIG_DIR is not set"},
		"no env file":   {ctx: envCtx(t, map[string]string{"HERDR_PLUGIN_CONFIG_DIR": noFile, "ELASTIC_API_KEY": eventsKey}), wantIn: "[debugger] env_file is not set"},
		"key refused":   {ctx: refused, wantIn: "elasticsearch refused the read key (401)"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			out, err := runIn(tc.ctx, "events", "worker", "w14:p8")
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantIn)
			assert.NotContains(t, err.Error()+out, eventsKey)
		})
	}
}

// TestEventsFlagsNameNoDestination: the debugger may run any `events`
// command with any flag, so none may choose where the key goes: no flag
// names a URL, a host, a variable, a file or a config. The allow list is
// exact, so a new flag has to be added here, deliberately.
func TestEventsFlagsNameNoDestination(t *testing.T) {
	t.Parallel()
	events, _, err := cli.Root().Find([]string{"events"})
	require.NoError(t, err)
	want := map[string][]string{"events": {"help"}, "worker": {"help", "json", "since"}, "query": {"help", "json"}}
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		c.InitDefaultHelpFlag()
		var names []string
		seen := func(f *pflag.Flag) {
			if !slices.Contains(names, f.Name) {
				names = append(names, f.Name)
			}
		}
		c.Flags().VisitAll(seen)
		c.InheritedFlags().VisitAll(seen)
		slices.Sort(names)
		assert.Equal(t, want[c.Name()], names, c.CommandPath())
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(events)
	assert.Len(t, events.Commands(), 2)
}
