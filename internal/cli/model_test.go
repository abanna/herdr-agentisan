package cli_test

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/herdr/herdrtest"
	"github.com/abanna/herdr-agentisan/internal/report"
)

// TestReportStatuslinePushesTheModel: `report statusline` pushes the model
// and its model_at beside ctx, or alone before Claude has measured a context
// (A56), and its debug log names what it pushed: a model-only report logs no
// ctx at all, never a zero.
func TestReportStatuslinePushesTheModel(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		stdin   string
		ctx     string
		logs    []string
		notLogs []string
	}{
		"beside ctx": {
			stdin: `{"model":{"id":"claude-opus-5-5","display_name":"Opus"},"context_window":{"used_percentage":42.6}}`,
			ctx:   "43", logs: []string{`"ctx":43`, `"model":"claude-opus-5-5"`},
		},
		"alone": {
			stdin: `{"model":{"id":"claude-opus-5-5","display_name":"Opus"}}`,
			logs:  []string{`"model":"claude-opus-5-5"`}, notLogs: []string{`"ctx"`},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := herdrtest.Start(t, paneHerdr("w14:p1"))
			var logs bytes.Buffer
			before := time.Now()

			out, err := runReport(reportCtx(t, srv.Path, "w14:p1", &logs), tc.stdin+"\n")
			require.NoError(t, err)
			assert.Empty(t, out)

			reqs := srv.Requests()
			require.Len(t, reqs, 2)
			require.Equal(t, "pane.report_metadata", reqs[1].Method)
			var params struct {
				Tokens map[string]string `json:"tokens"`
				TTLMs  uint64            `json:"ttl_ms"`
			}
			require.NoError(t, json.Unmarshal(reqs[1].Params, &params))
			assert.Equal(t, uint64(180000), params.TTLMs)
			assert.Equal(t, "claude-opus-5-5", params.Tokens[report.ModelKey])
			ctx, hasCtx := params.Tokens[report.CtxKey]
			assert.Equal(t, tc.ctx != "", hasCtx)
			assert.Equal(t, tc.ctx, ctx)
			_, hasCtxAt := params.Tokens[report.CtxAtKey]
			assert.Equal(t, hasCtx, hasCtxAt, "ctx_at rides ctx, and a model alone sets none (A61)")
			at, ok := report.ParseModelAt(params.Tokens[report.ModelAtKey])
			require.True(t, ok, "model_at is Unix milliseconds")
			assert.False(t, at.Before(before.Truncate(time.Millisecond)), "stamped by the command's own clock")
			assert.False(t, at.After(time.Now()))
			for _, l := range tc.logs {
				assert.Contains(t, logs.String(), l)
			}
			for _, l := range tc.notLogs {
				assert.NotContains(t, logs.String(), l)
			}
		})
	}
}
