package report_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/herdr/herdrtest"
	"github.com/abanna/herdr-agentisan/internal/report"
)

// claudeStop and claudePrompt are Claude Code's Stop and UserPromptSubmit
// payloads as its hooks reference shows them, with text as the final
// message or the prompt.
func claudeStop(text string) string {
	return `{"session_id":"abc123","transcript_path":"/home/u/.claude/projects/p/s.jsonl","cwd":"/w","permission_mode":"default",` +
		`"hook_event_name":"Stop","stop_hook_active":false,"last_assistant_message":` + strconv.Quote(text) + `,"background_tasks":[],"session_crons":[]}`
}

func claudePrompt(text string) string {
	return `{"session_id":"abc123","transcript_path":"/home/u/.claude/projects/p/s.jsonl","cwd":"/w","permission_mode":"default",` +
		`"hook_event_name":"UserPromptSubmit","prompt":` + strconv.Quote(text) + `}`
}

// codexStop is Codex 0.161's Stop payload (hooks/src/schema.rs,
// StopCommandInput); message is the raw JSON of last_assistant_message,
// which Codex sends as null when there is none.
func codexStop(message string) string {
	return `{"session_id":"019a","turn_id":"t1","transcript_path":"/r.jsonl","cwd":"/w","hook_event_name":"Stop",` +
		`"model":"gpt-5.5","permission_mode":"default","stop_hook_active":false,"last_assistant_message":` + message + `}`
}

// TestStatedClasses walks the lines a turn can state its item and stage in.
// Each value found is one CheckValue accepts, so herdr stores it as sent.
func TestStatedClasses(t *testing.T) {
	t.Parallel()
	const prompt, stop = report.EventPromptSubmit, report.EventStop
	long := strings.Repeat("x", 78) + " " + strings.Repeat("y", 10)
	tests := map[string]struct {
		event, text, item, stage string
	}{
		"a dispatch's item line":            {event: prompt, text: "item: NERD-5334\nBoss: next item for you", item: "NERD-5334"},
		"the first of two item lines":       {event: prompt, text: "item: NERD-1\nitem: NERD-2", item: "NERD-1"},
		"an item line in bold":              {event: prompt, text: "**item:** NERD-1", item: "NERD-1"},
		"a prompt with no item line":        {event: prompt, text: "#39 merged. Open 3a."},
		"item: inside a sentence":           {event: prompt, text: "the item: NERD-1 is next"},
		"item: behind a list marker":        {event: prompt, text: "- item: NERD-1"},
		"a prompt states no stage":          {event: prompt, text: "BOSS: done"},
		"Item: is not the prefix":           {event: prompt, text: "Item: NERD-1"},
		"an item behind a message header":   {event: prompt, text: "[msg 01JB2X from ha-boss] item: NERD-1\nbuild it", item: "NERD-1"},
		"behind a returned message header":  {event: prompt, text: "[msg 01J from term_w2:p1, originally to ha-coder-1] item: NERD-1", item: "NERD-1"},
		"behind the daemon's own header":    {event: prompt, text: "[agentisan] originally to term_w2:p1: item: NERD-1", item: "NERD-1"},
		"a bold item behind a header":       {event: prompt, text: "[msg 01J from ha-boss] **item:** NERD-1", item: "NERD-1"},
		"a header, the item on line 2":      {event: prompt, text: "[msg 01J from ha-boss] build it\nitem: NERD-1", item: "NERD-1"},
		"a header on a later line":          {event: prompt, text: "go\n[msg 01J from ha-boss] item: NERD-1"},
		"another bracket before the item":   {event: prompt, text: "[note] item: NERD-1"},
		"a daemon body's own bracket":       {event: prompt, text: "[agentisan] notices: item: NERD-1"},
		"a header id holding a space":       {event: prompt, text: "[msg 01 J from ha-boss] item: NERD-1"},
		"a header id holding a bracket":     {event: prompt, text: "[msg 01] from ha-boss] item: NERD-1"},
		"a header with no id":               {event: prompt, text: "[msg  from ha-boss] item: NERD-1"},
		"a header that never closes":        {event: prompt, text: "[msg 01J from ha-boss item: NERD-1"},
		"the daemon's header, unclosed":     {event: prompt, text: "[agentisan] originally to ha-coder-1 item: NERD-1"},
		"a sender holding a bracket":        {event: prompt, text: "[msg 01J from a] b] item: NERD-1"},
		"a sender holding a bare bracket":   {event: prompt, text: "[msg 01J from a]b] item: NERD-1", item: "NERD-1"},
		"a final message keeps its header":  {event: stop, text: "[msg 01J from ha-boss] item: NERD-1"},
		"a non-ASCII sender":                {event: prompt, text: "[msg 01J from rédacteur-1] item: NERD-1", item: "NERD-1"},
		"a Latin-1 byte in the sender":      {event: prompt, text: "[msg 01J from caf\xe9] item: NERD-1", item: "NERD-1"},
		"a cut multibyte sender":            {event: prompt, text: "[msg 01J from x\xe5\xaf] item: NERD-1", item: "NERD-1"},
		"a NUL in the sender":               {event: prompt, text: "[msg 01J from a\x00b] item: NERD-1", item: "NERD-1"},
		"a header in another case":          {event: prompt, text: "[Msg 01J from ha-boss] item: NERD-1"},
		"a BOM before a header":             {event: prompt, text: "\ufeff[msg 01J from ha-boss] item: NERD-1"},
		"a BOM behind a header":             {event: prompt, text: "[msg 01J from ha-boss] \ufeffitem: NERD-1", item: "NERD-1"},
		"a header, then CRLF":               {event: prompt, text: "[msg 01J from ha-boss] item: NERD-1\r\nbuild it", item: "NERD-1"},
		"an empty prompt":                   {event: prompt, text: ""},
		"a header and no body":              {event: prompt, text: "[msg 01J from ha-boss] "},
		"80 characters behind a header":     {event: prompt, text: "[msg 01J from ha-boss] item: " + strings.Repeat("é", 80), item: strings.Repeat("é", 80)},
		"a header behind a header":          {event: prompt, text: "[msg 01J from ha-boss] [msg 01K from x] item: NERD-1"},
		"a header, then Item:":              {event: prompt, text: "[msg 01J from ha-boss] Item: NERD-1"},
		"a header, then a list marker":      {event: prompt, text: "[msg 01J from ha-boss] - item: NERD-1"},
		"a worker's closing line":           {event: stop, text: "Done.\n\nBOSS: 1b updated c44380c, 554", stage: "1b updated c44380c, 554"},
		"the boss's own line":               {event: stop, text: "STATUS: waiting on CI", stage: "waiting on CI"},
		"a status in bold":                  {event: stop, text: "**BOSS:** blocked: 1Password agent", stage: "blocked: 1Password agent"},
		"a status in code ticks":            {event: stop, text: "`BOSS: PR #49 open`", stage: "PR #49 open"},
		"trailing blank lines and CRLF":     {event: stop, text: "Done.\r\n\r\nBOSS: x\r\n\r\n", stage: "x"},
		"the item before the status":        {event: stop, text: "Done.\n\nitem: NERD-5334\nSTATUS: waiting", item: "NERD-5334", stage: "waiting"},
		"the item after the status":         {event: stop, text: "STATUS: waiting\nitem: NERD-5334", item: "NERD-5334", stage: "waiting"},
		"the last of two closing items":     {event: stop, text: "item: A\nitem: B\nSTATUS: s", item: "B", stage: "s"},
		"an item in an earlier paragraph":   {event: stop, text: "item: A\n\nSTATUS: s", stage: "s"},
		"a status that is not the last":     {event: stop, text: "BOSS: x\nand then more"},
		"a status in an earlier paragraph":  {event: stop, text: "BOSS: x\n\nThanks."},
		"a closing item and no status":      {event: stop, text: "Done.\nitem: NERD-1", item: "NERD-1"},
		"a prefix with nothing after it":    {event: stop, text: "BOSS:   "},
		"a lower-case prefix":               {event: stop, text: "boss: x"},
		"no final message":                  {event: stop, text: ""},
		"white space inside":                {event: stop, text: "BOSS: a\tb  c", stage: "a b c"},
		"an escape and a bidi override":     {event: stop, text: "BOSS: red\x1b[31m ‮evil", stage: "red[31m evil"},
		"exactly 80 characters":             {event: stop, text: "BOSS: " + strings.Repeat("é", 80), stage: strings.Repeat("é", 80)},
		"81 characters":                     {event: stop, text: "BOSS: " + strings.Repeat("é", 81), stage: strings.Repeat("é", 79) + "…"},
		"a cut that would end on a space":   {event: stop, text: "BOSS: " + long, stage: strings.Repeat("x", 78) + "…"},
		"an item line past 80 characters":   {event: prompt, text: "item: " + strings.Repeat("審", 90), item: strings.Repeat("審", 79) + "…"},
		"only markup after the prefix":      {event: stop, text: "BOSS: ** ``"},
		"a status alone after a blank line": {event: stop, text: "BOSS: old\n\nBOSS: new", stage: "new"},
		"a status inside a closing fence":   {event: stop, text: "Done.\n\n```\nSTATUS: x\n```"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			item, stage := report.Stated(tc.event, tc.text)
			assert.Equal(t, tc.item, item)
			assert.Equal(t, tc.stage, stage)
			for _, v := range []string{item, stage} {
				if v != "" {
					require.NoError(t, report.CheckValue(v), "herdr stores %q as sent", v)
				}
			}
		})
	}
}

// itemHerdr is a herdr whose one pane, w14:p1, carries item (none if ""),
// and that accepts every report.
func itemHerdr(item string) herdrtest.Handler {
	return func(r herdrtest.Request) herdrtest.Reply {
		if r.Method == "pane.list" {
			pane := map[string]any{"pane_id": "w14:p1", "tokens": map[string]string{}}
			if item != "" {
				pane["tokens"] = map[string]string{"item": item}
			}
			return herdrtest.Reply{Result: map[string]any{"type": "pane_list", "panes": []any{pane}}}
		}
		return okReply(r)
	}
}

// TestTurnPushesTheTokenContract: a turn's stated values go out under the
// item and stage contract (A5), a stage with its stage_at from the pane's
// clock (A42), source agentisan and the 24 h TTL, from either agent's
// payload. A turn that states both pushes both and asks herdr
// nothing else; the pane has no lineage, so the report trusts HERDR_PANE_ID.
func TestTurnPushesTheTokenContract(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		payload string
		want    report.TurnResult
		tokens  string
	}{
		"a Claude worker's Stop": {
			payload: claudeStop("Rebased.\n\nBOSS: 1b updated c44380c, 554"),
			want:    report.TurnResult{Stage: "1b updated c44380c, 554", PaneID: "w14:p1"},
			tokens:  `{"stage":"1b updated c44380c, 554","stage_at":"1761066000"}`,
		},
		"a Codex boss's Stop": {
			payload: codexStop(strconv.Quote("item: NERD-5334\nSTATUS: waiting on QA")),
			want:    report.TurnResult{Item: "NERD-5334", Stage: "waiting on QA", PaneID: "w14:p1"},
			tokens:  `{"item":"NERD-5334","stage":"waiting on QA","stage_at":"1761066000"}`,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := herdrtest.Start(t, itemHerdr(""))
			pane := report.Pane{SocketPath: srv.Path, PaneID: "w14:p1", Now: reportedAt}

			got, err := report.Turn(t.Context(), herdr.Client{SocketPath: srv.Path}, pane, strings.NewReader(tc.payload))
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)

			reqs := srv.Requests()
			require.Equal(t, []string{reportM}, methods(reqs))
			assert.JSONEq(t, `{"pane_id":"w14:p1","source":"agentisan","tokens":`+tc.tokens+`,"ttl_ms":86400000}`, string(reqs[0].Params))
		})
	}
}

// TestTurnANewItemClearsTheStage: an item stated with no stage is checked
// against the pane's item. A new one clears the stage, which belonged to the
// old item, and its stage_at, by reporting them empty, which herdr takes as
// a clear; the same one only renews its TTL, so the stage stays, and it
// never writes stage_at, which would make an old stage look reported now.
func TestTurnANewItemClearsTheStage(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		carried, payload, pane string
		cleared                bool
		tokens                 map[string]string
	}{
		"a dispatch of another item":      {carried: "NERD-5260", payload: claudePrompt("item: NERD-5334\nRead the Stage 3."), cleared: true, tokens: map[string]string{"item": "NERD-5334", "stage": "", "stage_at": ""}},
		"a pane with no item yet":         {payload: claudePrompt("item: NERD-5334"), cleared: true, tokens: map[string]string{"item": "NERD-5334", "stage": "", "stage_at": ""}},
		"a dispatch of the same item":     {carried: "NERD-5334", payload: claudePrompt("item: NERD-5334\nFix QA's findings."), tokens: map[string]string{"item": "NERD-5334"}},
		"a closing item line, no status":  {carried: "NERD-1", payload: claudeStop("Moving on.\nitem: NERD-2"), cleared: true, tokens: map[string]string{"item": "NERD-2", "stage": "", "stage_at": ""}},
		"the same item, closing, no line": {carried: "NERD-2", payload: claudeStop("item: NERD-2"), tokens: map[string]string{"item": "NERD-2"}},
		// Compared byte for byte, as herdr stores a value: another spelling
		// is another item.
		"the item re-cased":        {carried: "NERD-5334", payload: claudePrompt("item: nerd-5334"), cleared: true, tokens: map[string]string{"item": "nerd-5334", "stage": "", "stage_at": ""}},
		"the item in NFD, not NFC": {carried: "caf\u00e9", payload: claudePrompt("item: cafe\u0301"), cleared: true, tokens: map[string]string{"item": "cafe\u0301", "stage": "", "stage_at": ""}},
		// With no lineage HERDR_PANE_ID is trusted, and may be an alias that
		// pane.list never shows: its item is unknown, so the stage stays.
		"a pane pane.list does not show": {carried: "NERD-5334", pane: "w9:p1", payload: claudePrompt("item: NERD-5334"), tokens: map[string]string{"item": "NERD-5334"}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := herdrtest.Start(t, itemHerdr(tc.carried))
			pane := report.Pane{SocketPath: srv.Path, PaneID: "w14:p1", Now: reportedAt}
			if tc.pane != "" {
				pane.PaneID = tc.pane
			}

			got, err := report.Turn(t.Context(), herdr.Client{SocketPath: srv.Path}, pane, strings.NewReader(tc.payload))
			require.NoError(t, err)
			assert.Equal(t, tc.cleared, got.Cleared)

			reqs := srv.Requests()
			require.Equal(t, []string{list, reportM}, methods(reqs))
			var p stageParams
			require.NoError(t, json.Unmarshal(reqs[1].Params, &p))
			assert.Equal(t, uint64(86_400_000), p.TTLMs)
			assert.Equal(t, tc.tokens, p.Tokens)
		})
	}
}

// claudeArgv is what each process of oursProcs runs under Claude: the hook,
// the shell Claude ran it through, claude, the pane shell and herdr.
var claudeArgv = map[int][]string{
	5000: {"herdr-agentisan", "report", "turn"}, 4999: {"/bin/sh", "-c", "herdr-agentisan report turn"},
	4998: {"/bin/sh", "-c", "herdr-agentisan report turn"}, 900: {"claude", "--name", "coders-1"},
	600: {"-zsh"}, 100: {"herdr", "server"},
}

// TestTurnChecksTheCodexHost: the payload is no proof of which agent ran the
// hook, so a lineage that holds Codex, or may, must pass CheckCodexHost: a
// hook the shared app-server ran, whose HERDR_PANE_ID names the pane that
// started it, reports nothing. Claude's lineage, with no Codex in it, lands
// on its pane.
func TestTurnChecksTheCodexHost(t *testing.T) {
	t.Parallel()
	daemon := map[int]report.Stat{
		5000: {PPID: 4000, Start: 900}, 4000: {PPID: 900, Start: 700}, 900: {PPID: 600, Start: 500}, 600: {PPID: 1, Start: 400},
	}
	daemonArgv := map[int][]string{
		5000: {"herdr-agentisan", "report", "turn"}, 4000: {"codex", "app-server", "--listen", "unix://"}, 900: {"codex"}, 600: {"-zsh"},
	}
	claudeUnreadable := map[int][]string{5000: claudeArgv[5000], 4999: claudeArgv[4999], 900: claudeArgv[900]}
	tests := map[string]struct {
		lineage *report.Lineage
		thread  string
		sandbox bool
		want    error
	}{
		"Claude":                          {lineage: report.ReadLineage(5000, newProcTable(oursProcs()).stat).WithCmdline(argv(claudeArgv))},
		"an embedded Codex":               {lineage: codexLineage(embeddedProcs(), embeddedArgv)},
		"the shared app-server daemon":    {lineage: codexLineage(daemon, daemonArgv), want: report.ErrCodexDaemon},
		"an ancestor's cmdline unread":    {lineage: report.ReadLineage(5000, newProcTable(oursProcs()).stat).WithCmdline(argv(claudeUnreadable)), want: report.ErrHostUnverified},
		"a lineage with no cmdline":       {lineage: ours(), want: report.ErrHostUnverified},
		"a Codex tool command, sandboxed": {lineage: codexLineage(embeddedProcs(), embeddedArgv), thread: "019a", sandbox: true, want: report.ErrCodexSandboxed},
		"CODEX_THREAD_ID, no Codex":       {lineage: report.ReadLineage(5000, newProcTable(oursProcs()).stat).WithCmdline(argv(claudeArgv)), thread: "019a", want: report.ErrNoCodexHost},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := herdrtest.Start(t, fakeHerdr([]fakePane{{id: "w15:p4", shell: 600, pgid: 900, fg: []uint32{899, 900}}}))
			pane := report.Pane{SocketPath: srv.Path, PaneID: "w15:p4", Lineage: tc.lineage, CodexThread: tc.thread, CodexNetworkSandboxed: tc.sandbox}

			got, err := report.Turn(t.Context(), herdr.Client{SocketPath: srv.Path}, pane, strings.NewReader(claudeStop("BOSS: done")))
			if tc.want != nil {
				require.ErrorIs(t, err, tc.want)
				assert.Empty(t, srv.Requests(), "nothing reaches herdr")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, report.TurnResult{Stage: "done", PaneID: "w15:p4"}, got)
			assert.Equal(t, []string{"w15:p4"}, reportedTo(t, srv.Requests()))
		})
	}
}

// TestTurnReportsNothing: a subagent's hook, an event that is not a turn's
// edge, a turn that states nothing and a payload that is not a hook report
// nothing, and never call herdr.
func TestTurnReportsNothing(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		outside bool
		payload string
		want    error
	}{
		"outside a pane":               {outside: true, payload: claudeStop("BOSS: x"), want: report.ErrNotInPane},
		"a Claude SubagentStop":        {payload: `{"hook_event_name":"SubagentStop","agent_id":"def456","agent_type":"Explore","last_assistant_message":"BOSS: x"}`, want: report.ErrSubagent},
		"a Codex subagent's prompt":    {payload: `{"hook_event_name":"UserPromptSubmit","agent_id":"a1","agent_type":"worker","prompt":"item: NERD-1"}`, want: report.ErrSubagent},
		"a SubagentStop with no agent": {payload: `{"hook_event_name":"SubagentStop","last_assistant_message":"BOSS: x"}`, want: report.ErrMalformed},
		"another event":                {payload: `{"hook_event_name":"PostToolUse","last_assistant_message":"BOSS: x"}`, want: report.ErrMalformed},
		"an event re-cased":            {payload: `{"hook_event_name":"stop","last_assistant_message":"BOSS: x"}`, want: report.ErrMalformed},
		"no event":                     {payload: `{"last_assistant_message":"BOSS: x"}`, want: report.ErrMalformed},
		"a JSON null":                  {payload: `null`, want: report.ErrMalformed},
		"a turn with no status line":   {payload: claudeStop("Done, no line."), want: report.ErrNothingStated},
		"Codex's null final message":   {payload: codexStop("null"), want: report.ErrNothingStated},
		"a prompt with no item line":   {payload: claudePrompt("BOSS: ignored in a prompt"), want: report.ErrNothingStated},
		"not one JSON object":          {payload: `{"hook_event_name":"Stop"`, want: report.ErrMalformed},
		"a field of the wrong type":    {payload: `{"hook_event_name":"Stop","last_assistant_message":7}`, want: report.ErrMalformed},
		"over the size cap":            {payload: claudeStop(strings.Repeat("x", report.MaxTurnHookBytes) + "\nBOSS: x"), want: report.ErrMalformed},
		"an empty payload":             {payload: "", want: report.ErrMalformed},
		"a payload behind a BOM":       {payload: "\ufeff" + claudeStop("BOSS: x"), want: report.ErrMalformed},
		"JSON nested past the decoder": {payload: `{"x":` + strings.Repeat("[", 10001) + strings.Repeat("]", 10001) + `}`, want: report.ErrMalformed},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := herdrtest.Start(t, okReply)
			pane := report.Pane{SocketPath: srv.Path, PaneID: "w14:p1"}
			if tc.outside {
				pane = report.Pane{}
			}

			_, err := report.Turn(t.Context(), herdr.Client{SocketPath: srv.Path}, pane, strings.NewReader(tc.payload))
			require.ErrorIs(t, err, tc.want)
			assert.Empty(t, srv.Requests(), "nothing reaches herdr")
		})
	}
}

// TestTurnSurfacesHerdrErrors: a herdr that refuses the report, or the list
// a new item is checked against, fails the report with herdr's sentinel.
func TestTurnSurfacesHerdrErrors(t *testing.T) {
	t.Parallel()
	refuse := func(method string) herdrtest.Handler {
		return func(r herdrtest.Request) herdrtest.Reply {
			if r.Method == method {
				return herdrtest.Reply{Error: &herdrtest.ErrorBody{Code: "busy", Message: "try later"}}
			}
			return itemHerdr("NERD-1")(r)
		}
	}
	for name, tc := range map[string]struct{ method, payload string }{
		"the report refused": {method: reportM, payload: claudeStop("BOSS: x")},
		"the list refused":   {method: list, payload: claudePrompt("item: NERD-2")},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := herdrtest.Start(t, refuse(tc.method))
			pane := report.Pane{SocketPath: srv.Path, PaneID: "w14:p1"}
			_, err := report.Turn(t.Context(), herdr.Client{SocketPath: srv.Path}, pane, strings.NewReader(tc.payload))
			require.ErrorIs(t, err, herdr.ErrAPI)
		})
	}
}

// TestTurnBoundsTheCallByCallTimeout: the hook's context carries no deadline,
// so one herdr.CallTimeout bounds the whole report, as it does Statusline's
// and Stage's. An item alone on a stale HERDR_PANE_ID makes every call Turn
// can: resolving the pane (process_info, list, process_info), reading its
// item (list), and the report, and a budget per call would let them add up.
func TestTurnBoundsTheCallByCallTimeout(t *testing.T) {
	t.Parallel()

	rep := &deadlineReporter{}
	lineage := report.ReadLineage(5000, newProcTable(oursProcs()).stat).WithCmdline(argv(claudeArgv))
	start := time.Now()
	got, err := report.Turn(context.Background(), rep,
		report.Pane{SocketPath: "/x", PaneID: "w1:p1", Lineage: lineage}, strings.NewReader(claudeStop("item: NERD-1")))
	require.NoError(t, err)
	assert.Equal(t, report.TurnResult{Item: "NERD-1", PaneID: "w2:p1", Cleared: true}, got)

	require.Len(t, rep.deadlines, 5, "process_info, list, process_info, list, report")
	for i, d := range rep.deadlines {
		require.False(t, d.IsZero(), "call %d must run under a deadline", i)
		assert.Equal(t, rep.deadlines[0], d, "call %d must share the one report deadline", i)
	}
	assert.WithinDuration(t, start.Add(herdr.CallTimeout), rep.deadlines[0], time.Second)
}

// TestTurnLandsOnTheReportingPane: a turn report goes to the pane the hook's
// own lineage runs in, never to the pane a stale or reissued HERDR_PANE_ID
// now names (NERD-5268: never report for another pane), and nowhere when no
// pane runs it.
func TestTurnLandsOnTheReportingPane(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		panes []fakePane
		want  []string
		err   error
	}{
		"the id reissued to another pane": {
			panes: []fakePane{{id: "wZ:p1", shell: 700, pgid: 701}, {id: "wN:p2", shell: 600}},
			want:  []string{"wN:p2"},
		},
		"no pane runs the hook": {
			panes: []fakePane{{id: "wZ:p1", shell: 700, pgid: 701}},
			err:   report.ErrPaneUnresolved,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := herdrtest.Start(t, fakeHerdr(tc.panes))
			lineage := report.ReadLineage(5000, newProcTable(oursProcs()).stat).WithCmdline(argv(claudeArgv))
			pane := report.Pane{SocketPath: srv.Path, PaneID: "wZ:p1", Lineage: lineage}

			got, err := report.Turn(t.Context(), herdr.Client{SocketPath: srv.Path}, pane, strings.NewReader(claudeStop("BOSS: done")))
			if tc.err != nil {
				require.ErrorIs(t, err, tc.err)
				assert.Empty(t, reportedTo(t, srv.Requests()), "no pane gets the report")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want[0], got.PaneID)
			assert.Equal(t, tc.want, reportedTo(t, srv.Requests()))
		})
	}
}

// TestTurnRefusesATerminal: run by hand in a pane, stdin is the terminal,
// which is refused unread, since reading it would wait for Ctrl-D; /dev/null
// stands in for it, and only the reason tells the refusal from an empty read.
func TestTurnRefusesATerminal(t *testing.T) {
	t.Parallel()
	dev, err := os.Open(os.DevNull)
	require.NoError(t, err)
	t.Cleanup(func() { _ = dev.Close() })
	srv := herdrtest.Start(t, okReply)

	_, err = report.Turn(t.Context(), herdr.Client{SocketPath: srv.Path}, report.Pane{SocketPath: srv.Path, PaneID: "w14:p1"}, dev)
	require.ErrorIs(t, err, report.ErrMalformed)
	assert.ErrorContains(t, err, "character device")
	assert.Empty(t, srv.Requests())
}

// TestTurnDrainsStdin: a payload bigger than a pipe buffer is read to its end
// on a path that reports nothing, so the agent's write completes. Over the
// size cap, what is left past the cap is itself more than a pipe buffer.
func TestTurnDrainsStdin(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		pane    report.Pane
		payload string
	}{
		"outside a pane":    {payload: claudeStop(strings.Repeat("x", 200<<10))},
		"over the size cap": {pane: report.Pane{SocketPath: "/nowhere", PaneID: "w1:p1"}, payload: claudeStop(strings.Repeat("x", report.MaxTurnHookBytes+256<<10))},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r, w, err := os.Pipe()
			require.NoError(t, err)
			wrote := make(chan error, 1)
			go func() {
				_, err := io.WriteString(w, tc.payload)
				wrote <- errors.Join(err, w.Close())
			}()
			_, err = report.Turn(t.Context(), herdr.Client{SocketPath: "/nowhere"}, tc.pane, r)
			require.Error(t, err)
			require.NoError(t, r.Close())
			require.NoError(t, <-wrote, "the writer finished before the hook closed stdin")
		})
	}
}

// TestTurnPayloadEncodings: a payload's text is JSON, which carries only
// UTF-8. Each byte that is not, and a lone surrogate, decodes as U+FFFD, which
// herdr stores as sent; a NUL is unprintable and dropped.
func TestTurnPayloadEncodings(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct{ message, stage string }{
		"an invalid byte":            {message: "BOSS: a\xffb", stage: "a\ufffdb"},
		"Latin-1 é":                  {message: "BOSS: caf\xe9", stage: "caf\ufffd"},
		"a cut multibyte sequence":   {message: "BOSS: x\xe5\xaf", stage: "x\ufffd\ufffd"},
		"a lone surrogate":           {message: `BOSS: a\ud800b`, stage: "a\ufffdb"},
		"an embedded NUL":            {message: `BOSS: a\u0000b`, stage: "ab"},
		"a lone CR inside the value": {message: `BOSS: a\rb`, stage: "a b"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := herdrtest.Start(t, okReply)
			pane := report.Pane{SocketPath: srv.Path, PaneID: "w14:p1"}
			payload := `{"hook_event_name":"Stop","last_assistant_message":"` + tc.message + `"}`

			got, err := report.Turn(t.Context(), herdr.Client{SocketPath: srv.Path}, pane, strings.NewReader(payload))
			require.NoError(t, err)
			assert.Equal(t, tc.stage, got.Stage)
			require.NoError(t, report.CheckValue(got.Stage))
		})
	}
}

// TestTurnSizeCap: a payload of exactly MaxTurnHookBytes is read whole and
// reports; one byte more reports nothing.
func TestTurnSizeCap(t *testing.T) {
	t.Parallel()
	payload := claudeStop("BOSS: x")
	exact := payload + strings.Repeat(" ", report.MaxTurnHookBytes-len(payload))
	for in, want := range map[string]error{exact: nil, exact + " ": report.ErrMalformed} {
		srv := herdrtest.Start(t, okReply)
		pane := report.Pane{SocketPath: srv.Path, PaneID: "w14:p1"}
		_, err := report.Turn(t.Context(), herdr.Client{SocketPath: srv.Path}, pane, strings.NewReader(in))
		if want == nil {
			require.NoError(t, err, "exactly the cap")
			continue
		}
		require.ErrorIs(t, err, want, "one past the cap")
	}
}
