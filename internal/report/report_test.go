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
// pane only.
func TestStatuslinePushesTheTokenContract(t *testing.T) {
	t.Parallel()
	srv := herdrtest.Start(t, okReply)
	pane := report.Pane{SocketPath: srv.Path, PaneID: "w14:p1"}

	got, err := report.Statusline(t.Context(), herdr.Client{SocketPath: srv.Path}, pane, strings.NewReader(statusline("42.6")))
	require.NoError(t, err)
	assert.Equal(t, 43, got)

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

// deadlineReporter records the deadline the report call ran under.
type deadlineReporter struct {
	deadline time.Time
	ok       bool
}

func (r *deadlineReporter) ReportPaneMetadata(ctx context.Context, _ herdr.PaneMetadata) error {
	r.deadline, r.ok = ctx.Deadline()
	return nil
}

// TestStatuslineBoundsTheCallByCallTimeout: the process context carries no
// deadline, so without one a herdr that accepts and never answers would keep
// the background reporter alive forever.
func TestStatuslineBoundsTheCallByCallTimeout(t *testing.T) {
	t.Parallel()

	rep := &deadlineReporter{}
	start := time.Now()
	_, err := report.Statusline(context.Background(), rep, report.Pane{SocketPath: "/x", PaneID: "w1:p1"}, strings.NewReader(statusline("5")))
	require.NoError(t, err)
	require.True(t, rep.ok, "the herdr call must run under a deadline")
	assert.WithinDuration(t, start.Add(plugin.CallTimeout), rep.deadline, time.Second)
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
