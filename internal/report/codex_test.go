package report_test

import (
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/herdr/herdrtest"
	"github.com/abanna/herdr-agentisan/internal/report"
)

// window is the gpt-5 family's usable context window as Codex 0.161 records
// it: 272,000 tokens at the model's 95% headroom.
const window = 258400

// tokenCount is one rollout line as Codex writes it: an event_msg carrying a
// token_count whose last_token_usage holds last tokens and whose cumulative
// total has long passed the window, as a working session's does.
func tokenCount(last, window int64) string {
	return `{"timestamp":"2026-10-10T07:00:00.000Z","type":"event_msg","payload":{"type":"token_count","info":{` +
		`"total_token_usage":{"input_tokens":900000,"cached_input_tokens":800000,"output_tokens":9000,"reasoning_output_tokens":100,"total_tokens":909000},` +
		`"last_token_usage":{"input_tokens":` + strconv.FormatInt(last, 10) + `,"cached_input_tokens":0,"output_tokens":0,"reasoning_output_tokens":0,"total_tokens":` + strconv.FormatInt(last, 10) + `},` +
		`"model_context_window":` + strconv.FormatInt(window, 10) + `},"rate_limits":null}}`
}

// info is a token_count line around a hand-written info object.
func info(body string) string {
	return `{"type":"event_msg","payload":{"type":"token_count","info":` + body + `}}`
}

const (
	userMessage = `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}}`
	nullInfo    = `{"type":"event_msg","payload":{"type":"token_count","info":null,"rate_limits":{"primary":null}}}`
)

// lines is a rollout of ls, one per line.
func lines(ls ...string) string { return strings.Join(ls, "\n") + "\n" }

// transcript writes content to a rollout file and returns its path.
func transcript(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rollout-2026-10-10T07-00-00-0199.jsonl")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

// TestTranscriptContextMatchesTheTUI pins ctx to the number Codex's own
// "Context N% used" shows (codex-rs rust-v0.161.0 tui/src/token_usage.rs):
// 12,000 baseline tokens come off both sides, the REMAINING share is
// rounded, and used is 100 minus it.
func TestTranscriptContextMatchesTheTUI(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		content string
		want    int
		err     error
	}{
		"a working session":                 {content: lines(tokenCount(24610, window)), want: 5},
		"half the window":                   {content: lines(tokenCount(135200, window)), want: 50},
		"at the baseline":                   {content: lines(tokenCount(12000, window)), want: 0},
		"under the baseline":                {content: lines(tokenCount(3, window)), want: 0},
		"negative last usage":               {content: lines(tokenCount(-5, window)), want: 0},
		"the full window":                   {content: lines(tokenCount(window, window)), want: 100},
		"past the window":                   {content: lines(tokenCount(window+1000, window)), want: 100},
		"remaining 99.5 rounds up, used 0":  {content: lines(tokenCount(12001, 12200)), want: 0},
		"remaining 0.5 rounds up, used 99":  {content: lines(tokenCount(12199, 12200)), want: 99},
		"the latest token_count decides":    {content: lines(tokenCount(250000, window), tokenCount(24610, window)), want: 5},
		"later lines of other types":        {content: lines(tokenCount(24610, window), userMessage, userMessage), want: 5},
		"a null info falls back":            {content: lines(tokenCount(24610, window), nullInfo), want: 5},
		"a quoted token_count is not one":   {content: lines(tokenCount(24610, window), `{"type":"response_item","payload":{"type":"token_count","info":{}}}`), want: 5},
		"a torn last line is skipped":       {content: lines(tokenCount(24610, window), tokenCount(250000, window)[:120]), want: 5},
		"an unparsable count is skipped":    {content: lines(tokenCount(24610, window), info(`{"last_token_usage":{"total_tokens":1e999},"model_context_window":258400}`)), want: 5},
		"no last_token_usage is skipped":    {content: lines(tokenCount(24610, window), info(`{"model_context_window":258400}`)), want: 5},
		"CRLF line endings":                 {content: lines(tokenCount(24610, window) + "\r"), want: 5},
		"a compaction estimate counts":      {content: lines(info(`{"total_token_usage":{"input_tokens":5,"total_tokens":999999},"last_token_usage":{"input_tokens":0,"output_tokens":0,"total_tokens":135200},"model_context_window":258400}`)), want: 50},
		"overflow marker reports full":      {content: lines(info(`{"total_token_usage":{"total_tokens":258400},"last_token_usage":{"total_tokens":3},"model_context_window":258400}`)), want: 100},
		"a real total equal to W is not it": {content: lines(info(`{"total_token_usage":{"input_tokens":1,"total_tokens":258400},"last_token_usage":{"total_tokens":24610},"model_context_window":258400}`)), want: 5},

		"the largest window":                {content: lines(tokenCount(24610, math.MaxInt64)), want: 0},
		"the most negative last usage":      {content: lines(tokenCount(math.MinInt64, window)), want: 0},
		"many short lines after the count":  {content: lines(tokenCount(24610, window)) + strings.Repeat("{}\n", 100_000), want: 5},
		"a lone surrogate in the count":     {content: lines(strings.Replace(tokenCount(24610, window), `"rate_limits":null`, `"rate_limits":null,"note":"\ud800"`, 1)), want: 5},
		"invalid UTF-8 in the count":        {content: lines(strings.Replace(tokenCount(24610, window), `"rate_limits":null`, `"rate_limits":null,"note":"`+"\xff"+`"`, 1)), want: 5},
		"a NUL inside the latest count":     {content: lines(tokenCount(24610, window), strings.Replace(tokenCount(250000, window), `"type":"token_count"`, "\x00"+`"type":"token_count"`, 1)), want: 5},
		"nesting past the JSON depth limit": {content: lines(tokenCount(24610, window), `{"type":"token_count","deep":`+strings.Repeat("[", 10_001)+strings.Repeat("]", 10_001)+`}`), want: 5},

		"no window":                {content: lines(info(`{"last_token_usage":{"total_tokens":24610}}`)), err: report.ErrNoContext},
		"null window":              {content: lines(info(`{"last_token_usage":{"total_tokens":24610},"model_context_window":null}`)), err: report.ErrNoContext},
		"window at the baseline":   {content: lines(tokenCount(100, 12000)), err: report.ErrNoContext},
		"zero window":              {content: lines(tokenCount(100, 0)), err: report.ErrNoContext},
		"only null infos":          {content: lines(nullInfo, nullInfo), err: report.ErrNoContext},
		"no token_count at all":    {content: lines(userMessage), err: report.ErrNoContext},
		"an empty transcript":      {content: "", err: report.ErrNoContext},
		"a non-JSON transcript":    {content: lines("\x28\xb5\x2f\xfd compressed"), err: report.ErrNoContext},
		"no newline after a count": {content: tokenCount(24610, window), want: 5},
		"a byte-order mark first":  {content: "\xef\xbb\xbf" + lines(tokenCount(24610, window)), err: report.ErrNoContext},
		"UTF-16 encoded":           {content: "\xff\xfe{\x00\"\x00t\x00", err: report.ErrNoContext},
		"lone CR line framing":     {content: tokenCount(24610, window) + "\r" + tokenCount(250000, window) + "\r", err: report.ErrNoContext},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := report.TranscriptContext(transcript(t, tc.content))
			if tc.err != nil {
				require.ErrorIs(t, err, tc.err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestTranscriptContextReadsABoundedTail: only the last MaxTranscriptTail
// bytes are read, and a line cut by that boundary is never parsed, while one
// that starts exactly on it is.
func TestTranscriptContextReadsABoundedTail(t *testing.T) {
	t.Parallel()

	count := tokenCount(24610, window) + "\n"
	filler := func(n int) string { return strings.Repeat("x", n-1) + "\n" }

	tests := map[string]struct {
		content string
		err     error
	}{
		"the count starts on the boundary":     {content: filler(100) + count + filler(report.MaxTranscriptTail-len(count))},
		"the count straddles the boundary":     {content: filler(100) + count + filler(report.MaxTranscriptTail-len(count)+1), err: report.ErrNoContext},
		"the count is past the tail":           {content: count + filler(report.MaxTranscriptTail), err: report.ErrNoContext},
		"a tail of exactly MaxTranscriptTail":  {content: count + filler(report.MaxTranscriptTail-len(count))},
		"one long line over the whole tail":    {content: count + strings.Repeat("y", report.MaxTranscriptTail+10), err: report.ErrNoContext},
		"a short file read whole from offset0": {content: count},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := report.TranscriptContext(transcript(t, tc.content))
			if tc.err != nil {
				require.ErrorIs(t, err, tc.err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, 5, got)
		})
	}
}

// TestTranscriptContextFileClasses walks what transcript_path can name. Only
// an absolute path to a regular file, or a link to one, is read.
func TestTranscriptContextFileClasses(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	good := transcript(t, tokenCount(24610, window)+"\n")
	link := filepath.Join(dir, "link.jsonl")
	require.NoError(t, os.Symlink(good, link))

	tests := map[string]struct {
		path string
		err  error
	}{
		"a regular file":             {path: good},
		"a symlink to one":           {path: link},
		"a relative path":            {path: "rollout.jsonl", err: report.ErrMalformed},
		"a relative path with ..":    {path: "../../etc/passwd", err: report.ErrMalformed},
		"a drive-relative path":      {path: `C:rollout.jsonl`, err: report.ErrMalformed},
		"a UNC path":                 {path: `\\server\share\rollout.jsonl`, err: report.ErrMalformed},
		"an empty path":              {path: "", err: report.ErrMalformed},
		"a missing file":             {path: filepath.Join(dir, "gone.jsonl"), err: report.ErrMalformed},
		"a directory":                {path: dir, err: report.ErrMalformed},
		"a character device":         {path: os.DevNull, err: report.ErrMalformed},
		"a dangling symlink":         {path: filepath.Join(dir, "dangling"), err: report.ErrMalformed},
		"a NUL byte in the path":     {path: dir + "/a\x00b", err: report.ErrMalformed},
		"a path with spaces and UTF": {path: transcriptAt(t, "sessions/2026/10/10/rollout é .jsonl")},
	}
	if runtime.GOOS == "linux" { // APFS refuses a name that is not UTF-8
		tests["non-UTF-8 bytes in the name"] = struct {
			path string
			err  error
		}{path: transcriptAt(t, "rollout-\xff\xfe.jsonl")}
	}
	require.NoError(t, os.Symlink(filepath.Join(dir, "nowhere"), filepath.Join(dir, "dangling")))

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := report.TranscriptContext(tc.path)
			if tc.err != nil {
				require.ErrorIs(t, err, tc.err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, 5, got)
		})
	}
}

// TestTranscriptContextPermissionDenied: a transcript the hook may not read,
// or one under a directory it may not search, is malformed, not a hang.
func TestTranscriptContextPermissionDenied(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-0 file")
	}
	unreadable := transcriptAt(t, "rollout.jsonl")
	require.NoError(t, os.Chmod(unreadable, 0o000))
	hidden := transcriptAt(t, "closed/rollout.jsonl")
	require.NoError(t, os.Chmod(filepath.Dir(hidden), 0o000))
	t.Cleanup(func() { _ = os.Chmod(filepath.Dir(hidden), 0o700) })

	for _, path := range []string{unreadable, hidden} {
		_, err := report.TranscriptContext(path)
		require.ErrorIs(t, err, report.ErrMalformed, path)
	}
}

// transcriptAt writes a one-count rollout at rel under a temp dir.
func transcriptAt(t *testing.T, rel string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), rel)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(tokenCount(24610, window)+"\n"), 0o600))
	return path
}

// hookJSON is a PostToolUse payload as Codex 0.161 sends it, naming path as
// its transcript. pad grows tool_response to make the payload large.
func hookJSON(path string, pad int) string {
	tp := "null"
	if path != "" {
		tp = strconv.Quote(path)
	}
	return `{"session_id":"0199","turn_id":"1","transcript_path":` + tp + `,"cwd":"/w","hook_event_name":"PostToolUse",` +
		`"model":"gpt-5","permission_mode":"default","tool_name":"Bash","tool_input":{"command":"ls"},` +
		`"tool_response":"` + strings.Repeat("o", pad) + `","tool_use_id":"call_1"}`
}

// TestParseCodexHookClasses walks the payload shapes. Only transcript_path
// and agent_id matter; every other field is Codex's to add or change.
func TestParseCodexHookClasses(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		in   string
		want report.CodexHook
		err  error
	}{
		"a PostToolUse payload":      {in: hookJSON("/r.jsonl", 3), want: report.CodexHook{TranscriptPath: "/r.jsonl"}},
		"a null transcript":          {in: hookJSON("", 3)},
		"no transcript field":        {in: `{"hook_event_name":"Stop"}`},
		"a subagent's hook":          {in: `{"transcript_path":"/r.jsonl","agent_id":"a1","agent_type":"worker"}`, want: report.CodexHook{TranscriptPath: "/r.jsonl", AgentID: "a1"}},
		"a null agent_id":            {in: `{"transcript_path":"/r.jsonl","agent_id":null}`, want: report.CodexHook{TranscriptPath: "/r.jsonl"}},
		"invalid UTF-8 in the path":  {in: `{"transcript_path":"/r` + "\xff" + `.jsonl"}`, want: report.CodexHook{TranscriptPath: "/r\ufffd.jsonl"}},
		"top-level null":             {in: `null`},
		"transcript_path not string": {in: `{"transcript_path":42}`, err: report.ErrMalformed},
		"agent_id not string":        {in: `{"agent_id":{}}`, err: report.ErrMalformed},
		"empty input":                {in: "", err: report.ErrMalformed},
		"truncated JSON":             {in: `{"transcript_path":"/r`, err: report.ErrMalformed},
		"not an object":              {in: `["/r.jsonl"]`, err: report.ErrMalformed},
		"two objects":                {in: `{}{}`, err: report.ErrMalformed},
		"invalid UTF-8 outside JSON": {in: "\xff{}", err: report.ErrMalformed},
		"a byte-order mark":          {in: "\xef\xbb\xbf{}", err: report.ErrMalformed},
		"UTF-16 encoded":             {in: "\xff\xfe{\x00}\x00", err: report.ErrMalformed},
		"a raw NUL byte":             {in: "{}\x00", err: report.ErrMalformed},
		"nested past the JSON depth limit": {
			in: `{"tool_input":` + strings.Repeat("[", 10_001) + strings.Repeat("]", 10_001) + `,"transcript_path":"/r.jsonl"}`, err: report.ErrMalformed,
		},
		"a lone surrogate escape": {in: `{"transcript_path":"/r\ud800.jsonl"}`, want: report.CodexHook{TranscriptPath: "/r\ufffd.jsonl"}},
		"CRLF after the object":   {in: `{"transcript_path":"/r.jsonl"}` + "\r\n", want: report.CodexHook{TranscriptPath: "/r.jsonl"}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := report.ParseCodexHook(strings.NewReader(tc.in))
			if tc.err != nil {
				require.ErrorIs(t, err, tc.err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// countingReader counts what was read from it, to prove stdin was drained.
type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err //nolint:wrapcheck // a pass-through reader must return io.EOF unwrapped.
}

// TestParseCodexHookSizeCap pins the cap at its boundary and shows a payload
// over it is still read to its end, so its writer never meets a closed pipe.
func TestParseCodexHookSizeCap(t *testing.T) {
	t.Parallel()

	at := hookJSON("/r.jsonl", 0)
	at = hookJSON("/r.jsonl", report.MaxCodexHookBytes-len(at))
	require.Len(t, at, report.MaxCodexHookBytes)
	got, err := report.ParseCodexHook(strings.NewReader(at))
	require.NoError(t, err)
	assert.Equal(t, "/r.jsonl", got.TranscriptPath)

	over := &countingReader{r: strings.NewReader(at + strings.Repeat(" ", 4096))}
	_, err = report.ParseCodexHook(over)
	require.ErrorIs(t, err, report.ErrMalformed)
	assert.Equal(t, len(at)+4096, over.n, "the rest of an oversized payload is drained")
}

// TestParseCodexHookStdinConfigurations walks what stdin can be when Codex,
// or a person, runs the hook: a pipe with the payload, a pipe closed with
// nothing in it, a closed descriptor, and a character device standing in for
// a terminal, which is refused unread because a terminal would block.
func TestParseCodexHookStdinConfigurations(t *testing.T) {
	t.Parallel()
	pipe := func(t *testing.T, content string) *os.File {
		t.Helper()
		r, w, err := os.Pipe()
		require.NoError(t, err)
		go func() { _, _ = io.WriteString(w, content); _ = w.Close() }()
		t.Cleanup(func() { _ = r.Close() })
		return r
	}
	closed := func(t *testing.T) *os.File {
		t.Helper()
		r, w, err := os.Pipe()
		require.NoError(t, err)
		require.NoError(t, w.Close())
		require.NoError(t, r.Close())
		return r
	}
	dev, err := os.Open(os.DevNull)
	require.NoError(t, err)
	t.Cleanup(func() { _ = dev.Close() })

	got, err := report.ParseCodexHook(pipe(t, hookJSON("/r.jsonl", 1)))
	require.NoError(t, err)
	assert.Equal(t, "/r.jsonl", got.TranscriptPath)
	for name, in := range map[string]*os.File{"an empty pipe": pipe(t, ""), "a closed descriptor": closed(t), "a character device": dev} {
		_, err := report.ParseCodexHook(in)
		require.ErrorIs(t, err, report.ErrMalformed, name)
	}
}

// embeddedProcs is a pane running Codex embedded (--no-daemon): report (5000)
// -> sh -lc (4998) -> codex TUI (900) -> node launcher (899) -> pane shell
// (600) -> herdr (100).
func embeddedProcs() map[int]report.Stat {
	return map[int]report.Stat{
		5000: {PPID: 4998, Start: 900}, 4998: {PPID: 900, Start: 800}, 900: {PPID: 899, Start: 500},
		899: {PPID: 600, Start: 500}, 600: {PPID: 100, Start: 400}, 100: {PPID: 1, Start: 100},
	}
}

// argv is a fake /proc cmdline table; a pid missing from it is unreadable.
func argv(cmds map[int][]string) report.CmdlineFunc {
	return func(pid int) ([]string, error) {
		a, ok := cmds[pid]
		if !ok {
			return nil, report.ErrProcCmdline
		}
		return a, nil
	}
}

var embeddedArgv = map[int][]string{
	5000: {"herdr-agentisan", "report", "codex"}, 4998: {"/bin/sh", "-lc", "herdr-agentisan report codex"},
	900: {"/usr/lib/node_modules/@openai/codex/vendor/codex", "--no-daemon"}, 899: {"node", "/usr/bin/codex", "--no-daemon"},
	600: {"-zsh"}, 100: {"herdr", "server"},
}

func codexLineage(procs map[int]report.Stat, cmds map[int][]string) *report.Lineage {
	return report.ReadLineage(5000, newProcTable(procs).stat).WithCmdline(argv(cmds))
}

// TestCodexPushesTheTokenContract: from an embedded Codex, ctx lands on the
// pane the TUI runs in, under the same contract as Claude's.
func TestCodexPushesTheTokenContract(t *testing.T) {
	t.Parallel()
	srv := herdrtest.Start(t, fakeHerdr([]fakePane{{id: "w15:p4", shell: 600, pgid: 899, fg: []uint32{899, 900}}}))
	pane := report.Pane{SocketPath: srv.Path, PaneID: "w15:p4", Lineage: codexLineage(embeddedProcs(), embeddedArgv)}
	path := transcript(t, tokenCount(24610, window)+"\n")

	got, err := report.Codex(t.Context(), herdr.Client{SocketPath: srv.Path}, pane, strings.NewReader(hookJSON(path, 10)))
	require.NoError(t, err)
	assert.Equal(t, report.Result{Ctx: 5, PaneID: "w15:p4"}, got)

	reqs := srv.Requests()
	require.Equal(t, []string{process, reportM}, methods(reqs))
	assert.JSONEq(t, `{"pane_id":"w15:p4","source":"agentisan","tokens":{"ctx":"5"},"ttl_ms":180000}`, string(reqs[1].Params))
}

// TestCodexRefusesWhatItCannotPlace: nothing reaches herdr unless the hook
// provably runs in the pane's own Codex. Under the shared app-server daemon
// the hook inherits the daemon's environment, so HERDR_PANE_ID names the pane
// that first started the daemon, and while that TUI lives the lineage runs
// through it: without the guard, ctx would land on the wrong pane.
func TestCodexRefusesWhatItCannotPlace(t *testing.T) {
	t.Parallel()

	// report (5000) -> app-server (4000) -> pid-update-loop (3000) -> the
	// TUI that started it (900) -> its pane shell (600).
	daemon := map[int]report.Stat{
		5000: {PPID: 4000, Start: 900}, 4000: {PPID: 3000, Start: 700}, 3000: {PPID: 900, Start: 600},
		900: {PPID: 600, Start: 500}, 600: {PPID: 1, Start: 400},
	}
	daemonArgv := map[int][]string{
		5000: {"herdr-agentisan", "report", "codex"}, 4000: {"/home/u/.codex/packages/bin/codex", "app-server", "--listen", "unix://"},
		3000: {"codex", "app-server", "daemon", "pid-update-loop"}, 900: {"codex"}, 600: {"-zsh"},
	}
	viaNode := map[int][]string{5000: daemonArgv[5000], 4000: {"node", "/usr/lib/node_modules/@openai/codex/bin/codex.js", "app-server"}, 3000: {"code"}, 900: {"codex"}, 600: {"-zsh"}}
	loopOnly := map[int][]string{5000: daemonArgv[5000], 4000: {"codex-code-mode-host"}, 3000: daemonArgv[3000], 900: {"codex"}, 600: {"-zsh"}}
	// Only a process named exactly codex is the host: Codex's own helpers,
	// whose names start with it, are not (A25).
	helpersOnly := map[int][]string{
		5000: embeddedArgv[5000], 4998: {"/usr/lib/codex/codex-linux-sandbox", "--sandbox-policy", "{}", "--", "sh"},
		900: {"/opt/codex/bin/codex-code-mode-host"}, 899: {"node", "/usr/lib/node_modules/@openai/codex/bin/codex-cli.js"},
		600: {"/usr/local/bin/codexd"}, 100: {"herdr", "server"},
	}
	noCodex := map[int][]string{5000: embeddedArgv[5000], 4998: embeddedArgv[4998], 900: {"claude"}, 899: {"python3", "launch.py"}, 600: {"-zsh"}, 100: {"herdr", "server"}}
	unreadable := map[int][]string{5000: embeddedArgv[5000], 4998: embeddedArgv[4998], 900: embeddedArgv[900]}
	reused := newProcTable(embeddedProcs())
	reusedLineage := report.ReadLineage(5000, reused.stat).WithCmdline(func(pid int) ([]string, error) {
		if pid == 900 {
			reused.set(900, report.Stat{PPID: 1, Start: 5001}) // the TUI exits; its pid is reissued
		}
		return embeddedArgv[pid], nil
	})

	tests := map[string]struct {
		lineage *report.Lineage
		hook    string
		want    error
	}{
		"the shared app-server daemon":       {lineage: codexLineage(daemon, daemonArgv), want: report.ErrCodexDaemon},
		"the daemon's update loop":           {lineage: codexLineage(daemon, loopOnly), want: report.ErrCodexDaemon},
		"an IDE's app-server under node":     {lineage: codexLineage(daemon, viaNode), want: report.ErrCodexDaemon},
		"no lineage (off Linux)":             {lineage: nil, want: report.ErrHostUnverified},
		"a lineage with no cmdline reader":   {lineage: report.ReadLineage(5000, newProcTable(embeddedProcs()).stat), want: report.ErrHostUnverified},
		"an ancestor's cmdline unreadable":   {lineage: codexLineage(embeddedProcs(), unreadable), want: report.ErrHostUnverified},
		"no codex among the ancestors":       {lineage: codexLineage(embeddedProcs(), noCodex), want: report.ErrNoCodexHost},
		"only codex helpers, never codex":    {lineage: codexLineage(embeddedProcs(), helpersOnly), want: report.ErrNoCodexHost},
		"an ancestor's pid reused mid-check": {lineage: reusedLineage, want: report.ErrHostUnverified},
		"a hook inside a subagent":           {lineage: codexLineage(embeddedProcs(), embeddedArgv), hook: `{"transcript_path":"/r","agent_id":"a1"}`, want: report.ErrSubagent},
		"an ephemeral session":               {lineage: codexLineage(embeddedProcs(), embeddedArgv), hook: hookJSON("", 1), want: report.ErrNoContext},
		"an unreadable payload":              {lineage: codexLineage(embeddedProcs(), embeddedArgv), hook: `{`, want: report.ErrMalformed},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := herdrtest.Start(t, fakeHerdr([]fakePane{{id: "w15:p1", shell: 600, pgid: 900, fg: []uint32{900}}}))
			hook := tc.hook
			if hook == "" {
				hook = hookJSON(transcript(t, tokenCount(24610, window)+"\n"), 1)
			}
			pane := report.Pane{SocketPath: srv.Path, PaneID: "w15:p1", Lineage: tc.lineage}

			_, err := report.Codex(t.Context(), herdr.Client{SocketPath: srv.Path}, pane, strings.NewReader(hook))
			require.ErrorIs(t, err, tc.want)
			assert.Empty(t, srv.Requests(), "nothing reaches herdr")
		})
	}
}

// TestCodexDrainsStdinOnEveryPath: every path, including the ones that report
// nothing, reads stdin to its end, so Codex's write of the payload completes.
// Codex 0.161 tolerates a closed pipe anyway; this keeps the hook from
// depending on that. The payload is bigger than a pipe buffer, so a hook that
// stopped reading early would leave the writer blocked, then broken.
func TestCodexDrainsStdinOnEveryPath(t *testing.T) {
	t.Parallel()

	big := hookJSON("/r.jsonl", 200<<10)
	tests := map[string]struct {
		pane    report.Pane
		payload string
		want    error
	}{
		"outside a pane":         {pane: report.Pane{}, payload: big, want: report.ErrNotInPane},
		"inside a subagent":      {pane: report.Pane{SocketPath: "/nowhere", PaneID: "w1:p1"}, payload: strings.Replace(big, `"cwd"`, `"agent_id":"a1","cwd"`, 1), want: report.ErrSubagent},
		"on an unverified host":  {pane: report.Pane{SocketPath: "/nowhere", PaneID: "w1:p1"}, payload: big, want: report.ErrHostUnverified},
		"over the size cap":      {pane: report.Pane{SocketPath: "/nowhere", PaneID: "w1:p1"}, payload: hookJSON("/r.jsonl", report.MaxCodexHookBytes), want: report.ErrMalformed},
		"with nothing to report": {pane: report.Pane{SocketPath: "/nowhere", PaneID: "w1:p1", Lineage: codexLineage(embeddedProcs(), embeddedArgv)}, payload: hookJSON("", 200<<10), want: report.ErrNoContext},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r, w, err := os.Pipe()
			require.NoError(t, err)
			wrote := make(chan error, 1)
			go func() {
				_, err := io.WriteString(w, tc.payload)
				wrote <- errors.Join(err, w.Close())
			}()

			_, err = report.Codex(t.Context(), herdr.Client{SocketPath: "/nowhere"}, tc.pane, r)
			require.ErrorIs(t, err, tc.want)
			require.NoError(t, r.Close())
			require.NoError(t, <-wrote, "the writer finished before the hook closed stdin")
		})
	}
}
