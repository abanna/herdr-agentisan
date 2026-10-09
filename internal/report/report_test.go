package report_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/herdr/herdrtest"
	"github.com/abanna/herdr-agentisan/internal/plugin"
	"github.com/abanna/herdr-agentisan/internal/report"
)

// padded returns a valid statusline reporting pct whose length is exactly n
// bytes, so the size cap is tested at its boundary.
func padded(t *testing.T, pct string, n int) string {
	t.Helper()
	head := `{"context_window":{"used_percentage":` + pct + `},"pad":"`
	tail := `"}`
	require.Greater(t, n, len(head)+len(tail))
	return head + strings.Repeat("x", n-len(head)-len(tail)) + tail
}

func statusline(pct string) string {
	return `{"model":{"display_name":"Opus"},"context_window":{"used_percentage":` + pct + `,"remaining_percentage":57.4}}`
}

// TestParseContextClasses walks the shapes Claude's statusline JSON can take.
// Only a JSON number from 0 to 100 yields a value; everything else is a
// sentinel the caller turns into "report nothing".
func TestParseContextClasses(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		in   string
		want int
		err  error
	}{
		"rounds up":                           {in: statusline("42.6"), want: 43},
		"rounds down":                         {in: statusline("42.4"), want: 42},
		"x.5 rounds away from zero":           {in: statusline("42.5"), want: 43},
		"integer":                             {in: statusline("7"), want: 7},
		"zero":                                {in: statusline("0"), want: 0},
		"negative zero":                       {in: statusline("-0"), want: 0},
		"exactly 100":                         {in: statusline("100"), want: 100},
		"99.6 rounds to 100":                  {in: statusline("99.6"), want: 100},
		"exponent form":                       {in: statusline("4.26e1"), want: 43},
		"trailing newline from a here-string": {in: statusline("42.6") + "\n", want: 43},
		"unknown fields are ignored":          {in: `{"new_field":[1,2],"context_window":{"used_percentage":12,"extra":{}}}`, want: 12},
		"CRLF line ending":                    {in: statusline("42.6") + "\r\n", want: 43},
		"lone CR line ending":                 {in: statusline("42.6") + "\r", want: 43},
		"invalid UTF-8 inside a string":       {in: `{"model":{"display_name":"Op` + "\xff" + `us"},"context_window":{"used_percentage":5}}`, want: 5},
		"truncated multibyte in a string":     {in: `{"model":{"display_name":"` + "\xe2\x82" + `"},"context_window":{"used_percentage":5}}`, want: 5},
		"lone surrogate escape in a string":   {in: `{"model":{"display_name":"\ud800"},"context_window":{"used_percentage":5}}`, want: 5},
		"many small unknown fields":           {in: `{` + strings.Repeat(`"k":1,`, 50_000) + `"context_window":{"used_percentage":9}}`, want: 9},

		"percentage missing":    {in: `{"context_window":{"remaining_percentage":57.4}}`, err: report.ErrNoContext},
		"percentage null":       {in: statusline("null"), err: report.ErrNoContext},
		"context_window null":   {in: `{"context_window":null}`, err: report.ErrNoContext},
		"context_window absent": {in: `{"model":{"display_name":"Opus"}}`, err: report.ErrNoContext},
		"top-level null":        {in: `null`, err: report.ErrNoContext},

		"empty input":                {in: "", err: report.ErrMalformed},
		"whitespace only":            {in: " \n", err: report.ErrMalformed},
		"truncated JSON":             {in: `{"context_window":{"used_percentage":42`, err: report.ErrMalformed},
		"not an object":              {in: `[42]`, err: report.ErrMalformed},
		"two objects":                {in: statusline("1") + statusline("2"), err: report.ErrMalformed},
		"percentage as string":       {in: statusline(`"42.6"`), err: report.ErrMalformed},
		"percentage with % suffix":   {in: statusline(`"42%"`), err: report.ErrMalformed},
		"percentage as bool":         {in: statusline("true"), err: report.ErrMalformed},
		"context_window not object":  {in: `{"context_window":42}`, err: report.ErrMalformed},
		"negative":                   {in: statusline("-0.4"), err: report.ErrMalformed},
		"over 100":                   {in: statusline("100.4"), err: report.ErrMalformed},
		"overflows float64":          {in: statusline("1e400"), err: report.ErrMalformed},
		"invalid UTF-8 outside JSON": {in: "\xff" + statusline("1"), err: report.ErrMalformed},
		"byte-order mark":            {in: "\xef\xbb\xbf" + statusline("1"), err: report.ErrMalformed},
		"UTF-16 encoded":             {in: "\xff\xfe{\x00}\x00", err: report.ErrMalformed},
		"raw NUL byte":               {in: statusline("1") + "\x00", err: report.ErrMalformed},
		"nested past the JSON depth limit": {
			in: `{"deep":` + strings.Repeat("[", 10_001) + strings.Repeat("]", 10_001) + `,"context_window":{"used_percentage":1}}`, err: report.ErrMalformed,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, err := report.ParseContext(strings.NewReader(tc.in))
			if tc.err != nil {
				require.ErrorIs(t, err, tc.err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestParseContextSizeCap pins the cap at its boundary: exactly
// MaxStatuslineBytes is read, one byte more is refused.
func TestParseContextSizeCap(t *testing.T) {
	t.Parallel()

	got, err := report.ParseContext(strings.NewReader(padded(t, "5", report.MaxStatuslineBytes)))
	require.NoError(t, err)
	assert.Equal(t, 5, got)

	_, err = report.ParseContext(strings.NewReader(padded(t, "5", report.MaxStatuslineBytes+1)))
	require.ErrorIs(t, err, report.ErrMalformed)
}

// TestParseContextStdinConfigurations walks what stdin can be when the
// process starts: a pipe (the statusline's here-string), a regular file, a
// closed pipe, and a character device. A terminal is a character device, and
// /dev/null stands in for one: neither may be read, because a terminal would
// block until Ctrl-D.
func TestParseContextStdinConfigurations(t *testing.T) {
	t.Parallel()

	file := func(t *testing.T, content string) *os.File {
		t.Helper()
		f, err := os.CreateTemp(t.TempDir(), "in")
		require.NoError(t, err)
		_, err = f.WriteString(content)
		require.NoError(t, err)
		_, err = f.Seek(0, io.SeekStart)
		require.NoError(t, err)
		t.Cleanup(func() { _ = f.Close() })
		return f
	}
	pipe := func(t *testing.T, content string) *os.File {
		t.Helper()
		r, w, err := os.Pipe()
		require.NoError(t, err)
		go func() {
			_, _ = w.WriteString(content)
			_ = w.Close()
		}()
		t.Cleanup(func() { _ = r.Close() })
		return r
	}

	tests := map[string]struct {
		open func(t *testing.T) *os.File
		want int
		err  error
	}{
		"pipe":               {open: func(t *testing.T) *os.File { t.Helper(); return pipe(t, statusline("42.6")) }, want: 43},
		"regular file":       {open: func(t *testing.T) *os.File { t.Helper(); return file(t, statusline("42.6")) }, want: 43},
		"closed, empty pipe": {open: func(t *testing.T) *os.File { t.Helper(); return pipe(t, "") }, err: report.ErrMalformed},
		"character device": {
			open: func(t *testing.T) *os.File {
				t.Helper()
				f, err := os.Open(os.DevNull)
				require.NoError(t, err)
				t.Cleanup(func() { _ = f.Close() })
				return f
			},
			err: report.ErrMalformed,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, err := report.ParseContext(tc.open(t))
			if tc.err != nil {
				require.ErrorIs(t, err, tc.err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestParseContextReadFailure(t *testing.T) {
	t.Parallel()

	_, err := report.ParseContext(iotest.ErrReader(errors.New("EIO")))
	require.ErrorIs(t, err, report.ErrMalformed)
}

func TestPaneFromReadsTheShellEnvironment(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		env  map[string]string
		want report.Pane
	}{
		"both set":        {env: map[string]string{"HERDR_SOCKET_PATH": "/run/h.sock", "HERDR_PANE_ID": "w14:p1"}, want: report.Pane{SocketPath: "/run/h.sock", PaneID: "w14:p1"}},
		"both unset":      {env: map[string]string{}, want: report.Pane{}},
		"set but empty":   {env: map[string]string{"HERDR_SOCKET_PATH": "", "HERDR_PANE_ID": ""}, want: report.Pane{}},
		"only the socket": {env: map[string]string{"HERDR_SOCKET_PATH": "/run/h.sock"}, want: report.Pane{SocketPath: "/run/h.sock"}},
		"other variables are not read": {
			env:  map[string]string{"HERDR_PANE": "w1:p1", "herdr_pane_id": "w1:p1", "HERDR_ENV": "1"},
			want: report.Pane{},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := report.PaneFrom(func(k string) (string, bool) { v, ok := tc.env[k]; return v, ok })
			assert.Equal(t, tc.want, got)
		})
	}
}

func okReply(herdrtest.Request) herdrtest.Reply {
	return herdrtest.Reply{Result: map[string]any{"type": "ok"}}
}

// TestStatuslinePushesTheTokenContract is the token contract for ctx: key,
// bare rounded integer, source agentisan and a 180 s TTL, on the reporting
// pane only. The pane has no lineage, as on an OS without /proc, so the
// report trusts HERDR_PANE_ID and costs one call.
func TestStatuslinePushesTheTokenContract(t *testing.T) {
	t.Parallel()
	srv := herdrtest.Start(t, okReply)
	pane := report.Pane{SocketPath: srv.Path, PaneID: "w14:p1"}

	got, err := report.Statusline(t.Context(), herdr.Client{SocketPath: srv.Path}, pane, strings.NewReader(statusline("42.6")))
	require.NoError(t, err)
	assert.Equal(t, report.Result{Ctx: 43, PaneID: "w14:p1"}, got)

	reqs := srv.Requests()
	require.Len(t, reqs, 1)
	assert.Equal(t, "pane.report_metadata", reqs[0].Method)
	assert.JSONEq(t, `{"pane_id":"w14:p1","source":"agentisan","tokens":{"ctx":"43"},"ttl_ms":180000}`, string(reqs[0].Params))
}

// failReader fails the test if anything reads it: outside a pane the
// statusline JSON must not even be read.
type failReader struct{ t *testing.T }

func (r failReader) Read([]byte) (int, error) {
	r.t.Error("stdin was read although there is no pane to report to")
	return 0, io.EOF
}

func TestStatuslineDoesNothingOutsideAPane(t *testing.T) {
	t.Parallel()

	for name, pane := range map[string]func(sock string) report.Pane{
		"socket unset": func(string) report.Pane { return report.Pane{PaneID: "w1:p1"} },
		"pane unset":   func(sock string) report.Pane { return report.Pane{SocketPath: sock} },
		"both unset":   func(string) report.Pane { return report.Pane{} },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := herdrtest.Start(t, okReply)

			_, err := report.Statusline(t.Context(), herdr.Client{SocketPath: srv.Path}, pane(srv.Path), failReader{t})
			require.ErrorIs(t, err, report.ErrNotInPane)
			assert.Empty(t, srv.Requests(), "no herdr call outside a pane")
		})
	}
}

func TestStatuslineDoesNotCallHerdrForUnusableInput(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		in   string
		want error
	}{
		"missing":   {in: `{}`, want: report.ErrNoContext},
		"malformed": {in: statusline(`"42%"`), want: report.ErrMalformed},
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

func TestStatuslineSurfacesHerdrErrors(t *testing.T) {
	t.Parallel()
	srv := herdrtest.Start(t, func(herdrtest.Request) herdrtest.Reply {
		return herdrtest.Reply{Error: &herdrtest.ErrorBody{Code: "pane_not_found", Message: "pane w1:p1 not found"}}
	})

	_, err := report.Statusline(t.Context(), herdr.Client{SocketPath: srv.Path},
		report.Pane{SocketPath: srv.Path, PaneID: "w1:p1"}, strings.NewReader(statusline("5")))
	require.ErrorIs(t, err, herdr.ErrAPI)
}

// deadlineReporter records the deadline every herdr call ran under. Its one
// pane, w2:p1, runs pid 600; HERDR_PANE_ID w1:p1 names nothing, so a report
// with a lineage through 600 makes every kind of call.
type deadlineReporter struct {
	mu        sync.Mutex
	deadlines []time.Time
}

func (r *deadlineReporter) record(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	d, ok := ctx.Deadline()
	if !ok {
		d = time.Time{}
	}
	r.deadlines = append(r.deadlines, d)
}

func (r *deadlineReporter) ReportPaneMetadata(ctx context.Context, _ herdr.PaneMetadata) error {
	r.record(ctx)
	return nil
}

func (r *deadlineReporter) ListPanes(ctx context.Context) ([]herdr.PaneInfo, error) {
	r.record(ctx)
	return []herdr.PaneInfo{{PaneID: "w2:p1"}}, nil
}

func (r *deadlineReporter) PaneProcessInfo(ctx context.Context, paneID string) (herdr.ProcessInfo, error) {
	r.record(ctx)
	if paneID != "w2:p1" {
		return herdr.ProcessInfo{}, &herdr.APIError{Method: "pane.process_info", Code: "pane_not_found"}
	}
	return herdr.ProcessInfo{PaneID: "w2:p1", ShellPID: 600}, nil
}

// TestStatuslineBoundsTheCallByCallTimeout: the process context carries no
// deadline, so without one a herdr that accepts and never answers would keep
// the background reporter alive forever. One deadline bounds the whole
// report, resolution included: a stale pane makes 2+N calls, and a budget per
// call would let them add up.
func TestStatuslineBoundsTheCallByCallTimeout(t *testing.T) {
	t.Parallel()

	rep := &deadlineReporter{}
	lineage := report.ReadLineage(5000, newProcTable(map[int]report.Stat{5000: {PPID: 600, Start: 9}, 600: {PPID: 1, Start: 5}}).stat)
	start := time.Now()
	got, err := report.Statusline(context.Background(), rep,
		report.Pane{SocketPath: "/x", PaneID: "w1:p1", Lineage: lineage}, strings.NewReader(statusline("5")))
	require.NoError(t, err)
	assert.Equal(t, report.Result{Ctx: 5, PaneID: "w2:p1"}, got)

	require.Len(t, rep.deadlines, 4, "process_info, list, process_info, report")
	for i, d := range rep.deadlines {
		require.False(t, d.IsZero(), "call %d must run under a deadline", i)
		assert.Equal(t, rep.deadlines[0], d, "call %d must share the one report deadline", i)
	}
	assert.WithinDuration(t, start.Add(plugin.CallTimeout), rep.deadlines[0], time.Second)
}

// TestStatuslineConcurrentReportsAreIndependent: the statusline backgrounds
// one process per refresh, so reports overlap. Each must arrive as its own
// complete request carrying its own value; herdr applies each atomically.
func TestStatuslineConcurrentReportsAreIndependent(t *testing.T) {
	t.Parallel()
	srv := herdrtest.Start(t, okReply)
	pane := report.Pane{SocketPath: srv.Path, PaneID: "w14:p1"}

	const n = 16
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			_, err := report.Statusline(t.Context(), herdr.Client{SocketPath: srv.Path}, pane,
				strings.NewReader(statusline(strconv.Itoa(i))))
			assert.NoError(t, err)
		})
	}
	wg.Wait()

	reqs := srv.Requests()
	require.Len(t, reqs, n)
	seen := map[string]bool{}
	for _, r := range reqs {
		var p struct {
			PaneID string            `json:"pane_id"`
			Tokens map[string]string `json:"tokens"`
		}
		require.NoError(t, json.Unmarshal(r.Params, &p))
		assert.Equal(t, "w14:p1", p.PaneID)
		seen[p.Tokens["ctx"]] = true
	}
	assert.Len(t, seen, n, "every report carries its own value")
}

// fakePane is one pane of a fake herdr: its current id, the ids that alias
// it, and the processes herdr reports for it (0 is herdr's null).
type fakePane struct {
	id      string
	aliases []string
	shell   uint32
	pgid    uint32
	fg      []uint32
}

// fakeHerdr answers pane.list, pane.process_info and pane.report_metadata the
// way herdr 0.9.3 does: ids resolve directly or through an alias, an unknown
// id is pane_not_found, and process_info answers with the CURRENT id. Ids in
// gone are listed but already closed, as a pane that exits between the list
// and the query.
func fakeHerdr(panes []fakePane, gone ...string) herdrtest.Handler {
	byID := map[string]fakePane{}
	for _, p := range panes {
		byID[p.id] = p
		for _, a := range p.aliases {
			byID[a] = p
		}
	}
	notFound := herdrtest.Reply{Error: &herdrtest.ErrorBody{Code: "pane_not_found", Message: "pane not found"}}
	return func(r herdrtest.Request) herdrtest.Reply {
		var params struct {
			PaneID string `json:"pane_id"`
		}
		_ = json.Unmarshal(r.Params, &params)
		switch r.Method {
		case "pane.list":
			list := []map[string]any{}
			for _, p := range panes {
				list = append(list, map[string]any{"pane_id": p.id})
			}
			for _, id := range gone {
				list = append(list, map[string]any{"pane_id": id})
			}
			return herdrtest.Reply{Result: map[string]any{"type": "pane_list", "panes": list}}
		case "pane.process_info":
			p, ok := byID[params.PaneID]
			if !ok {
				return notFound
			}
			fg := []map[string]any{}
			for _, pid := range p.fg {
				fg = append(fg, map[string]any{"pid": pid, "name": "proc"})
			}
			info := map[string]any{"pane_id": p.id, "foreground_processes": fg, "shell_pid": nil, "foreground_process_group_id": nil}
			if p.shell != 0 {
				info["shell_pid"] = p.shell
			}
			if p.pgid != 0 {
				info["foreground_process_group_id"] = p.pgid
			}
			return herdrtest.Reply{Result: map[string]any{"type": "pane_process_info", "process_info": info}}
		case "pane.report_metadata":
			if _, ok := byID[params.PaneID]; !ok {
				return notFound
			}
			return herdrtest.Reply{Result: map[string]any{"type": "ok"}}
		}
		return herdrtest.Reply{Error: &herdrtest.ErrorBody{Code: "unknown_method", Message: r.Method}}
	}
}

// methods lists the methods a fake herdr received, in order.
func methods(reqs []herdrtest.Request) []string {
	out := make([]string, 0, len(reqs))
	for _, r := range reqs {
		out = append(out, r.Method)
	}
	return out
}

// reportedTo returns the pane every pane.report_metadata request named.
func reportedTo(t *testing.T, reqs []herdrtest.Request) []string {
	t.Helper()
	var out []string
	for _, r := range reqs {
		if r.Method != "pane.report_metadata" {
			continue
		}
		var p struct {
			PaneID string `json:"pane_id"`
		}
		require.NoError(t, json.Unmarshal(r.Params, &p))
		out = append(out, p.PaneID)
	}
	return out
}

// oursProcs is the reporting process's process table in these tests: report
// (5000) -> statusline.sh (4999) -> sh -c (4998) -> claude (900) -> pane
// shell (600) -> herdr (100) -> init. Panes that are not ours run pids 700
// and up. Each call returns a fresh table, so a test may change it.
func oursProcs() map[int]report.Stat {
	return map[int]report.Stat{
		5000: {PPID: 4999, Start: 900}, 4999: {PPID: 4998, Start: 800}, 4998: {PPID: 900, Start: 800},
		900: {PPID: 600, Start: 500}, 600: {PPID: 100, Start: 400}, 100: {PPID: 1, Start: 100},
	}
}

// ours is the lineage read from a fresh oursProcs table.
func ours() *report.Lineage {
	return report.ReadLineage(5000, newProcTable(oursProcs()).stat)
}

const (
	process = "pane.process_info"
	list    = "pane.list"
	reportM = "pane.report_metadata"
)

// TestStatuslineResolvesTheReportingPane is NERD-5268: ctx lands on the pane
// the reporting process runs in, whatever HERDR_PANE_ID says. The env id is
// checked first, the two calls a pane that never moved costs; only a pane
// that does not run the process sends the report looking through every pane,
// and a report goes nowhere rather than to a pane that is not provably ours.
func TestStatuslineResolvesTheReportingPane(t *testing.T) {
	t.Parallel()

	mine := fakePane{id: "w14:p1", shell: 600, pgid: 900, fg: []uint32{900, 901}}
	other := fakePane{id: "w2:p1", shell: 700, pgid: 701, fg: []uint32{701}}

	// claudeExited makes claude (900) exit after the lineage was read, and a
	// new process in another pane take its pid.
	claudeExited := func(p *procTable) { p.set(900, report.Stat{PPID: 7000, Start: 990}) }

	tests := map[string]struct {
		envPane string
		// lineage is read from table (oursProcs when nil) unless noLineage or
		// zeroLineage; mutate then changes the table before the report.
		table       map[int]report.Stat
		noLineage   bool
		zeroLineage bool
		mutate      func(*procTable)
		panes       []fakePane
		gone        []string
		want        string // the pane ctx lands on; "" means nothing is reported
		err         error
		calls       []string
	}{
		"valid id costs exactly two calls": {
			envPane: "w14:p1", panes: []fakePane{mine, other},
			want: "w14:p1", calls: []string{process, reportM},
		},
		"stale id finds the moved pane": {
			envPane: "wP:p1",
			panes:   []fakePane{other, {id: "wN:p2", shell: 600, pgid: 900, fg: []uint32{900}}},
			want:    "wN:p2", calls: []string{process, list, process, process, reportM},
		},
		"stale id through herdr's alias reports to the current id": {
			envPane: "wP:p1",
			panes:   []fakePane{other, {id: "wN:p2", aliases: []string{"wP:p1"}, shell: 600}},
			want:    "wN:p2", calls: []string{process, reportM},
		},
		// After a live handoff herdr can reissue a closed workspace's id, so
		// the stale id names a live pane that is not ours.
		"stale id reissued to another pane never gets the report": {
			envPane: "wZ:p1",
			panes:   []fakePane{{id: "wZ:p1", shell: 700, pgid: 701}, {id: "wN:p2", shell: 600}},
			want:    "wN:p2", calls: []string{process, list, process, process, reportM},
		},
		"matched through the pane shell alone": {
			envPane: "w14:p1", panes: []fakePane{{id: "w14:p1", shell: 600}},
			want: "w14:p1", calls: []string{process, reportM},
		},
		"matched through the foreground group alone": {
			envPane: "w14:p1", panes: []fakePane{{id: "w14:p1", pgid: 900}},
			want: "w14:p1", calls: []string{process, reportM},
		},
		"matched through a foreground process alone": {
			envPane: "w14:p1", panes: []fakePane{{id: "w14:p1", fg: []uint32{777, 900}}},
			want: "w14:p1", calls: []string{process, reportM},
		},
		"a pane that closed after the list is skipped": {
			envPane: "wP:p1", panes: []fakePane{{id: "wN:p2", shell: 600}}, gone: []string{"wX:p9"},
			want: "wN:p2", calls: []string{process, list, process, process, reportM},
		},
		"no pane runs the process": {
			envPane: "wP:p1", panes: []fakePane{other},
			err: report.ErrPaneUnresolved, calls: []string{process, list, process},
		},
		"no panes at all": {
			envPane: "wP:p1",
			err:     report.ErrPaneUnresolved, calls: []string{process, list},
		},
		// The statusline exited first and the report was re-parented to init:
		// the lineage is the report alone, and it runs in no pane.
		"re-parented before the lineage was read": {
			envPane: "w14:p1", table: map[int]report.Stat{5000: {PPID: 1, Start: 900}}, panes: []fakePane{mine, other},
			err: report.ErrPaneUnresolved, calls: []string{process, list, process, process},
		},
		"a zero Lineage verifies against nothing": {
			envPane: "w14:p1", zeroLineage: true, panes: []fakePane{mine},
			err: report.ErrPaneUnresolved, calls: []string{process, list, process},
		},
		// herdr reports an unknown pid as null (0); init is no pane's process.
		// Neither may ever match.
		"pids 0 and 1 never match": {
			envPane: "w14:p1", panes: []fakePane{{id: "w14:p1", pgid: 1}, {id: "w2:p1"}},
			err: report.ErrPaneUnresolved, calls: []string{process, list, process, process},
		},
		// Claude exited after the lineage was read and another pane's new
		// process took pid 900: that pane matches the pid, but the pid's start
		// time no longer matches, so it is not ours.
		"a pid reused since the lineage was read is not ours": {
			envPane: "w14:p1", mutate: claudeExited, panes: []fakePane{{id: "w2:p1", fg: []uint32{900}}},
			err: report.ErrPaneUnresolved, calls: []string{process, list, process},
		},
		"a pid that exited since the lineage was read is not ours": {
			envPane: "w14:p1", mutate: func(p *procTable) { p.exit(900) }, panes: []fakePane{{id: "w14:p1", fg: []uint32{900}}},
			err: report.ErrPaneUnresolved, calls: []string{process, list, process},
		},
		"one confirmed pid is enough": {
			envPane: "w14:p1", mutate: claudeExited, panes: []fakePane{{id: "w14:p1", shell: 600, fg: []uint32{900}}},
			want: "w14:p1", calls: []string{process, reportM},
		},
		"two panes claiming the process get nothing": {
			envPane: "wP:p1",
			panes:   []fakePane{{id: "wN:p2", shell: 600}, {id: "wM:p3", fg: []uint32{900}}},
			err:     report.ErrAmbiguousPane, calls: []string{process, list, process, process},
		},
		// No lineage, as on an OS without /proc: trust the env id, one call.
		"no lineage trusts HERDR_PANE_ID": {
			envPane: "w14:p1", noLineage: true, panes: []fakePane{mine, other},
			want: "w14:p1", calls: []string{reportM},
		},
		"no lineage and a stale id surfaces pane_not_found": {
			envPane: "wP:p1", noLineage: true, panes: []fakePane{mine},
			err: herdr.ErrPaneNotFound, calls: []string{reportM},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := herdrtest.Start(t, fakeHerdr(tc.panes, tc.gone...))
			var lineage *report.Lineage
			switch {
			case tc.noLineage:
			case tc.zeroLineage:
				lineage = &report.Lineage{}
			default:
				procs := tc.table
				if procs == nil {
					procs = oursProcs()
				}
				table := newProcTable(procs)
				lineage = report.ReadLineage(5000, table.stat)
				if tc.mutate != nil {
					tc.mutate(table)
				}
			}
			pane := report.Pane{SocketPath: srv.Path, PaneID: tc.envPane, Lineage: lineage}

			got, err := report.Statusline(t.Context(), herdr.Client{SocketPath: srv.Path}, pane, strings.NewReader(statusline("42.6")))
			reqs := srv.Requests()
			assert.Equal(t, tc.calls, methods(reqs))
			if tc.err != nil {
				require.ErrorIs(t, err, tc.err)
				if !errors.Is(tc.err, herdr.ErrPaneNotFound) {
					assert.Empty(t, reportedTo(t, reqs), "nothing may be reported to a pane that is not provably ours")
				}
				return
			}
			require.NoError(t, err)
			assert.Equal(t, report.Result{Ctx: 43, PaneID: tc.want}, got)
			assert.Equal(t, []string{tc.want}, reportedTo(t, reqs))
		})
	}
}

// TestStatuslineResolutionSurfacesHerdrErrors: a herdr failure part-way
// through resolution stops the report. Only pane_not_found means "keep
// looking"; anything else could hide the pane that is ours.
func TestStatuslineResolutionSurfacesHerdrErrors(t *testing.T) {
	t.Parallel()

	busy := herdrtest.Reply{Error: &herdrtest.ErrorBody{Code: "busy", Message: "try later"}}
	stale := fakeHerdr([]fakePane{{id: "wN:p2", shell: 600}, {id: "w2:p1", shell: 700}})

	tests := map[string]struct {
		fail  func(herdrtest.Request, int) bool // which request fails, by method and arrival index
		reply herdrtest.Reply
		want  error
		calls []string
	}{
		"checking HERDR_PANE_ID fails": {
			fail:  func(r herdrtest.Request, i int) bool { return i == 0 },
			reply: busy, want: herdr.ErrAPI, calls: []string{process},
		},
		"listing the panes fails": {
			fail:  func(r herdrtest.Request, _ int) bool { return r.Method == list },
			reply: busy, want: herdr.ErrAPI, calls: []string{process, list},
		},
		"checking a listed pane fails": {
			fail:  func(r herdrtest.Request, i int) bool { return i == 2 },
			reply: busy, want: herdr.ErrAPI, calls: []string{process, list, process},
		},
		"herdr drops the connection mid-scan": {
			fail:  func(r herdrtest.Request, i int) bool { return i == 3 },
			reply: herdrtest.Reply{Silent: true}, want: herdr.ErrUnavailable, calls: []string{process, list, process, process},
		},
		"the report itself fails": {
			fail:  func(r herdrtest.Request, _ int) bool { return r.Method == reportM },
			reply: busy, want: herdr.ErrAPI, calls: []string{process, list, process, process, reportM},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var mu sync.Mutex
			n := 0
			srv := herdrtest.Start(t, func(r herdrtest.Request) herdrtest.Reply {
				mu.Lock()
				i := n
				n++
				mu.Unlock()
				if tc.fail(r, i) {
					return tc.reply
				}
				return stale(r)
			})
			pane := report.Pane{SocketPath: srv.Path, PaneID: "wP:p1", Lineage: ours()}

			_, err := report.Statusline(t.Context(), herdr.Client{SocketPath: srv.Path}, pane, strings.NewReader(statusline("5")))
			require.ErrorIs(t, err, tc.want)
			assert.Equal(t, tc.calls, methods(srv.Requests()))
		})
	}
}

// TestStatuslineConcurrentResolutionsAreIndependent: overlapping reports from
// a moved pane each find the pane on their own and push their own value
// there; nothing one report learns is shared with another.
func TestStatuslineConcurrentResolutionsAreIndependent(t *testing.T) {
	t.Parallel()
	srv := herdrtest.Start(t, fakeHerdr([]fakePane{{id: "w2:p1", shell: 700}, {id: "wN:p2", shell: 600}}))

	const n = 16
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			pane := report.Pane{SocketPath: srv.Path, PaneID: "wP:p1", Lineage: ours()}
			got, err := report.Statusline(t.Context(), herdr.Client{SocketPath: srv.Path}, pane,
				strings.NewReader(statusline(strconv.Itoa(i))))
			assert.NoError(t, err)
			assert.Equal(t, report.Result{Ctx: i, PaneID: "wN:p2"}, got)
		})
	}
	wg.Wait()

	reqs := srv.Requests()
	assert.Len(t, reqs, 5*n, "each report: check, list, two panes, report")
	seen := map[string]bool{}
	for _, r := range reqs {
		if r.Method != reportM {
			continue
		}
		var p struct {
			PaneID string            `json:"pane_id"`
			Tokens map[string]string `json:"tokens"`
		}
		require.NoError(t, json.Unmarshal(r.Params, &p))
		assert.Equal(t, "wN:p2", p.PaneID)
		seen[p.Tokens["ctx"]] = true
	}
	assert.Len(t, seen, n, "every report carries its own value")
}
