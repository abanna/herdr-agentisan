package report_test

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/herdr/herdrtest"
	"github.com/abanna/herdr-agentisan/internal/report"
)

// TestValidModelClasses: a model is kept only when a snapshot row may carry
// it (A39, A56): printable UTF-8 of at most 80 code points that is not a
// flag. Anything else is no model, never a cut-down one.
func TestValidModelClasses(t *testing.T) {
	t.Parallel()

	for _, m := range []string{
		"claude-sonnet-5-5", "claude-opus-5-5[1m]", "gpt-5.5", "opus", "Opus 4.1 (1M context)", "a",
		strings.Repeat("m", report.MaxValueChars), strings.Repeat("é", report.MaxValueChars), "m-",
	} {
		assert.Equal(t, m, report.ValidModel(m), "%q", m)
	}
	for _, m := range []string{
		"", "-", "-m", "--model", strings.Repeat("m", report.MaxValueChars+1), strings.Repeat("é", report.MaxValueChars+1),
		"\x1b[31mopus", "opus\n", "op\tus", "opus\x00", "op\xffus", "op\xe2\x82", "\ufffd", "op\u200bus", "op\u202eus",
		"op\u00a0us", "op\u2028us", "op\ue000us", "\ufeffopus", "opus\r\n", "opus\r",
	} {
		assert.Empty(t, report.ValidModel(m), "%q", m)
	}
}

// TestParseModelAtClasses: a model_at is read only as FormatModelAt writes
// it, whole milliseconds, so a damaged value is no time at all and its model
// is never taken as current.
func TestParseModelAtClasses(t *testing.T) {
	t.Parallel()
	at := time.Unix(1_761_066_000, 900_999_999)
	assert.Equal(t, "1761066000900", report.FormatModelAt(at), "milliseconds, truncated")
	got, ok := report.ParseModelAt(report.FormatModelAt(at))
	require.True(t, ok)
	assert.Equal(t, time.Unix(1_761_066_000, 900_000_000), got)
	got, ok = report.ParseModelAt("1")
	require.True(t, ok)
	assert.Equal(t, time.UnixMilli(1), got)
	got, ok = report.ParseModelAt("9223372036854775807")
	require.True(t, ok)
	assert.Equal(t, int64(9223372036854775807), got.UnixMilli())

	for _, v := range []string{
		"", " ", "0", "-1", "+1761066000900", "01761066000900", " 1761066000900", "1761066000900 ",
		"1761066000.9", "1.7e12", "0x68f7", "1_761_066_000_900", "١٧٦١٠٦٦٠٠٠٩٠٠", "9223372036854775808", "claude-opus-5-5",
		"1761066000900\n", "1761066000900\r\n", "\ufeff1761066000900", "1761066000900\x00", "\xff1761066000900", "１７６１０６６０００９００",
		"1761066000900\xe2\x82", "\ufffd1761066000900",
	} {
		_, ok := report.ParseModelAt(v)
		assert.False(t, ok, "%q", v)
	}
}

// TestParseStatuslineClasses walks the model beside ctx in Claude's
// statusline JSON (A56). The model is .model.id; a model of any other shape
// is no model and never silences ctx, and a statusline with only one of them
// gives that one.
func TestParseStatuslineClasses(t *testing.T) {
	t.Parallel()

	withID := func(id, pct string) string {
		return `{"model":{"id":` + id + `,"display_name":"Opus"},"context_window":{"used_percentage":` + pct + `}}`
	}
	tests := map[string]struct {
		in   string
		want report.Reading
		err  error
	}{
		"ctx and the model":        {in: withID(`"claude-opus-5-5"`, "42.6"), want: report.Reading{Ctx: 43, HasCtx: true, Model: "claude-opus-5-5"}},
		"a 1M-context id":          {in: withID(`"claude-opus-5-5[1m]"`, "1"), want: report.Reading{Ctx: 1, HasCtx: true, Model: "claude-opus-5-5[1m]"}},
		"a non-ASCII id":           {in: withID(`"modèle-5"`, "5"), want: report.Reading{Ctx: 5, HasCtx: true, Model: "modèle-5"}},
		"the model before any ctx": {in: `{"model":{"id":"claude-sonnet-5-5","display_name":"Sonnet"}}`, want: report.Reading{Model: "claude-sonnet-5-5"}},
		"a null context_window":    {in: `{"model":{"id":"claude-haiku-5-5"},"context_window":null}`, want: report.Reading{Model: "claude-haiku-5-5"}},
		"a null percentage":        {in: withID(`"claude-opus-5-5"`, "null"), want: report.Reading{Model: "claude-opus-5-5"}},
		"display_name only":        {in: statusline("42.6"), want: report.Reading{Ctx: 43, HasCtx: true}},
		"a null model":             {in: `{"model":null,"context_window":{"used_percentage":5}}`, want: report.Reading{Ctx: 5, HasCtx: true}},
		"the model a string":       {in: `{"model":"opus","context_window":{"used_percentage":5}}`, want: report.Reading{Ctx: 5, HasCtx: true}},
		"the model an array":       {in: `{"model":["opus"],"context_window":{"used_percentage":5}}`, want: report.Reading{Ctx: 5, HasCtx: true}},
		"a null id":                {in: withID("null", "5"), want: report.Reading{Ctx: 5, HasCtx: true}},
		"an id that is a number":   {in: withID("5", "5"), want: report.Reading{Ctx: 5, HasCtx: true}},
		"an id that is an object":  {in: withID(`{"name":"opus"}`, "5"), want: report.Reading{Ctx: 5, HasCtx: true}},
		"an empty id":              {in: withID(`""`, "5"), want: report.Reading{Ctx: 5, HasCtx: true}},
		"an id that is a flag":     {in: withID(`"--model"`, "5"), want: report.Reading{Ctx: 5, HasCtx: true}},
		"an id with an escape":     {in: withID(`"\u001b[31mopus"`, "5"), want: report.Reading{Ctx: 5, HasCtx: true}},
		"an id of 81 characters":   {in: withID(`"`+strings.Repeat("m", 81)+`"`, "5"), want: report.Reading{Ctx: 5, HasCtx: true}},
		"an id of invalid UTF-8":   {in: withID(`"op`+"\xff"+`us"`, "5"), want: report.Reading{Ctx: 5, HasCtx: true}},
		"a lone surrogate id":      {in: withID(`"\ud800"`, "5"), want: report.Reading{Ctx: 5, HasCtx: true}},

		"neither":                    {in: `{"model":{"display_name":"Opus"}}`, err: report.ErrNoContext},
		"an unusable id and no ctx":  {in: `{"model":{"id":"-x"}}`, err: report.ErrNoContext},
		"a malformed ctx, a model":   {in: withID(`"claude-opus-5-5"`, `"42%"`), err: report.ErrMalformed},
		"ctx out of range, a model":  {in: withID(`"claude-opus-5-5"`, "100.4"), err: report.ErrMalformed},
		"context_window not object":  {in: `{"model":{"id":"claude-opus-5-5"},"context_window":42}`, err: report.ErrMalformed},
		"truncated after the model":  {in: `{"model":{"id":"claude-opus-5-5"}`, err: report.ErrMalformed},
		"two objects with the model": {in: withID(`"a"`, "1") + withID(`"b"`, "2"), err: report.ErrMalformed},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := report.ParseStatusline(strings.NewReader(tc.in))
			if tc.err != nil {
				require.ErrorIs(t, err, tc.err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestParseContextIgnoresTheModel: ParseContext keeps its contract, the
// percentage alone, so a statusline with only a model is still no context.
func TestParseContextIgnoresTheModel(t *testing.T) {
	t.Parallel()
	_, err := report.ParseContext(strings.NewReader(`{"model":{"id":"claude-opus-5-5"}}`))
	require.ErrorIs(t, err, report.ErrNoContext)
	got, err := report.ParseContext(strings.NewReader(`{"model":{"id":"claude-opus-5-5"},"context_window":{"used_percentage":7}}`))
	require.NoError(t, err)
	assert.Equal(t, 7, got)
}

// TestStatuslinePushesTheModel is the model's token contract (A56): model
// and model_at, in Unix milliseconds from the pane's clock, in the same
// report as ctx with its 180 s TTL, or alone before Claude has measured a
// context.
func TestStatuslinePushesTheModel(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		in     string
		want   report.Result
		tokens string
	}{
		"beside ctx": {
			in:     `{"model":{"id":"claude-opus-5-5","display_name":"Opus"},"context_window":{"used_percentage":42.6}}`,
			want:   report.Result{Ctx: 43, HasCtx: true, Model: "claude-opus-5-5", PaneID: "w14:p1"},
			tokens: `{"ctx":"43","model":"claude-opus-5-5","model_at":"1761066000900"}`,
		},
		"alone": {
			in:     `{"model":{"id":"claude-sonnet-5-5","display_name":"Sonnet"}}`,
			want:   report.Result{Model: "claude-sonnet-5-5", PaneID: "w14:p1"},
			tokens: `{"model":"claude-sonnet-5-5","model_at":"1761066000900"}`,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := herdrtest.Start(t, okReply)
			pane := report.Pane{SocketPath: srv.Path, PaneID: "w14:p1", Now: reportedAt}

			got, err := report.Statusline(t.Context(), herdr.Client{SocketPath: srv.Path}, pane, strings.NewReader(tc.in))
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)

			reqs := srv.Requests()
			require.Len(t, reqs, 1)
			assert.Equal(t, "pane.report_metadata", reqs[0].Method)
			assert.JSONEq(t, `{"pane_id":"w14:p1","source":"agentisan","tokens":`+tc.tokens+`,"ttl_ms":180000}`, string(reqs[0].Params))
		})
	}
}

// TestStatuslineStampsEveryReport: each report carries its own model_at, so
// a model reported again is always newer than the agent that reports it.
func TestStatuslineStampsEveryReport(t *testing.T) {
	t.Parallel()
	srv := herdrtest.Start(t, okReply)
	at := reportedAt()
	pane := report.Pane{SocketPath: srv.Path, PaneID: "w14:p1", Now: func() time.Time { return at }}
	in := `{"model":{"id":"claude-opus-5-5"}}`

	_, err := report.Statusline(t.Context(), herdr.Client{SocketPath: srv.Path}, pane, strings.NewReader(in))
	require.NoError(t, err)
	at = at.Add(61 * time.Second)
	_, err = report.Statusline(t.Context(), herdr.Client{SocketPath: srv.Path}, pane, strings.NewReader(in))
	require.NoError(t, err)

	reqs := srv.Requests()
	require.Len(t, reqs, 2)
	assert.Contains(t, string(reqs[0].Params), `"model_at":"1761066000900"`)
	assert.Contains(t, string(reqs[1].Params), `"model_at":"1761066061900"`)
}

// TestStatuslineDoesNotCallHerdrWithAModelBesideBadInput: a model never
// rescues malformed input, and an unusable model is no model to report.
func TestStatuslineDoesNotCallHerdrWithAModelBesideBadInput(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		in   string
		want error
	}{
		"a malformed percentage": {in: `{"model":{"id":"claude-opus-5-5"},"context_window":{"used_percentage":"42%"}}`, want: report.ErrMalformed},
		"an unusable model":      {in: `{"model":{"id":"--model"}}`, want: report.ErrNoContext},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := herdrtest.Start(t, okReply)

			_, err := report.Statusline(t.Context(), herdr.Client{SocketPath: srv.Path},
				report.Pane{SocketPath: srv.Path, PaneID: "w1:p1"}, strings.NewReader(tc.in))
			require.ErrorIs(t, err, tc.want)
			assert.Empty(t, srv.Requests())
		})
	}
}

// TestParseCodexHookModelClasses: the hook's model is a string ValidModel
// keeps; any other shape is no model, never a malformed payload, so it
// cannot silence ctx.
func TestParseCodexHookModelClasses(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		model string
		want  string
	}{
		"a model":               {model: `"gpt-5.5"`, want: "gpt-5.5"},
		"a non-ASCII model":     {model: `"modèle"`, want: "modèle"},
		"absent":                {},
		"null":                  {model: "null"},
		"a number":              {model: "5"},
		"an object":             {model: `{"slug":"gpt-5.5"}`},
		"an array":              {model: `["gpt-5.5"]`},
		"empty":                 {model: `""`},
		"a flag":                {model: `"-m"`},
		"an escape":             {model: `"\u001b]0;x\u0007"`},
		"81 characters":         {model: `"` + strings.Repeat("m", 81) + `"`},
		"invalid UTF-8":         {model: `"gpt` + "\xff" + `"`},
		"a lone surrogate":      {model: `"\udc00"`},
		"a truncated multibyte": {model: `"gpt` + "\xe2\x82" + `"`},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			in := `{"transcript_path":"/r.jsonl"}`
			if tc.model != "" {
				in = `{"transcript_path":"/r.jsonl","model":` + tc.model + `}`
			}
			got, err := report.ParseCodexHook(strings.NewReader(in))
			require.NoError(t, err)
			assert.Equal(t, report.CodexHook{TranscriptPath: "/r.jsonl", Model: tc.want}, got)
		})
	}
}

// TestCodexPushesTheModelAlone: a Codex with no count to report still
// reports its model (A56), with its model_at and the 24 h TTL, so a fresh or
// ephemeral session shows its model. A transcript it cannot read is
// malformed input and reports nothing, model included, as a subagent's
// hook does.
func TestCodexPushesTheModelAlone(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		hook func(t *testing.T) string
		want error
	}{
		"an ephemeral session": {hook: func(*testing.T) string { return hookJSON("", 1) }},
		"no token count yet": {hook: func(t *testing.T) string {
			return hookJSON(transcript(t, `{"type":"session_meta","payload":{}}`+"\n"), 1)
		}},
		"no usable context window": {hook: func(t *testing.T) string {
			return hookJSON(transcript(t, tokenCount(100, 12000)+"\n"), 1)
		}},
		"a relative transcript path": {hook: func(*testing.T) string { return hookJSON("r.jsonl", 1) }, want: report.ErrMalformed},
		"a missing transcript": {hook: func(t *testing.T) string {
			return hookJSON(filepath.Join(t.TempDir(), "gone.jsonl"), 1)
		}, want: report.ErrMalformed},
		"a directory as the transcript": {hook: func(t *testing.T) string { return hookJSON(t.TempDir(), 1) }, want: report.ErrMalformed},
		"a subagent's model": {hook: func(*testing.T) string {
			return `{"transcript_path":null,"agent_id":"a1","model":"gpt-5.5"}`
		}, want: report.ErrSubagent},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := herdrtest.Start(t, fakeHerdr([]fakePane{{id: "w15:p4", shell: 600, pgid: 899, fg: []uint32{899, 900}}}))
			pane := report.Pane{SocketPath: srv.Path, PaneID: "w15:p4", Lineage: codexLineage(embeddedProcs(), embeddedArgv), Now: reportedAt}

			got, err := report.Codex(t.Context(), herdr.Client{SocketPath: srv.Path}, pane, strings.NewReader(tc.hook(t)))
			if tc.want != nil {
				require.ErrorIs(t, err, tc.want)
				assert.Empty(t, srv.Requests(), "nothing reaches herdr")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, report.Result{Model: "gpt-5", PaneID: "w15:p4"}, got)
			reqs := srv.Requests()
			require.Equal(t, []string{process, reportM}, methods(reqs))
			assert.JSONEq(t, `{"pane_id":"w15:p4","source":"agentisan","tokens":{"model":"gpt-5","model_at":"1761066000900"},"ttl_ms":86400000}`, string(reqs[1].Params))
		})
	}
}
