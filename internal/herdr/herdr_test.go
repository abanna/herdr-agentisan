package herdr_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/herdr/herdrtest"
)

func pong(herdrtest.Request) herdrtest.Reply {
	return herdrtest.Reply{Result: map[string]any{"type": "pong", "version": "0.9.3", "protocol": 22}}
}

func TestPingRoundTrip(t *testing.T) {
	t.Parallel()
	srv := herdrtest.Start(t, pong)

	got, err := herdr.Client{SocketPath: srv.Path}.Ping(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "0.9.3", got.Version)
	assert.EqualValues(t, 22, got.Protocol)

	reqs := srv.Requests()
	require.Len(t, reqs, 1)
	assert.Equal(t, "ping", reqs[0].Method)
	assert.NotEmpty(t, reqs[0].ID)
	// The schema REQUIRES params on every request, even one that takes none.
	assert.JSONEq(t, `{}`, string(reqs[0].Params))
}

func TestShowNotificationSendsTheSchemaShape(t *testing.T) {
	t.Parallel()
	srv := herdrtest.Start(t, func(herdrtest.Request) herdrtest.Reply {
		return herdrtest.Reply{Result: map[string]any{"type": "notification_show", "shown": true, "reason": "shown"}}
	})

	got, err := herdr.Client{SocketPath: srv.Path}.ShowNotification(t.Context(),
		herdr.Notification{Title: "hello", Body: "world"})
	require.NoError(t, err)
	assert.True(t, got.Shown)
	assert.Equal(t, "shown", got.Reason)

	reqs := srv.Requests()
	require.Len(t, reqs, 1)
	assert.Equal(t, "notification.show", reqs[0].Method)
	assert.JSONEq(t, `{"title":"hello","body":"world"}`, string(reqs[0].Params),
		"unset optional fields must be omitted, not sent as empty strings herdr would reject")
}

func okReply(herdrtest.Request) herdrtest.Reply {
	return herdrtest.Reply{Result: map[string]any{"type": "ok"}}
}

func TestReportPaneMetadataSendsTheSchemaShape(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		meta herdr.PaneMetadata
		want string
	}{
		"tokens with a TTL": {
			meta: herdr.PaneMetadata{PaneID: "w1:p2", Source: "agentisan", Tokens: map[string]string{"ctx": "43"}, TTLMillis: 180_000},
			want: `{"pane_id":"w1:p2","source":"agentisan","tokens":{"ctx":"43"},"ttl_ms":180000}`,
		},
		// herdr reads an absent ttl_ms as "never expires" but rejects 0, so a
		// zero TTL must be omitted rather than sent.
		"no TTL omits ttl_ms": {
			meta: herdr.PaneMetadata{PaneID: "w1:p2", Source: "agentisan", Tokens: map[string]string{"ctx": "0"}},
			want: `{"pane_id":"w1:p2","source":"agentisan","tokens":{"ctx":"0"}}`,
		},
		// Pane ids come from the environment. Whatever bytes they hold, the
		// request must stay one valid JSON line.
		"pane id with a newline stays one request line": {
			meta: herdr.PaneMetadata{PaneID: "w1\np2", Source: "agentisan", Tokens: map[string]string{"ctx": "1"}},
			want: `{"pane_id":"w1\np2","source":"agentisan","tokens":{"ctx":"1"}}`,
		},
		"pane id with invalid UTF-8 is sent as valid JSON": {
			meta: herdr.PaneMetadata{PaneID: "w1\xff", Source: "agentisan", Tokens: map[string]string{"ctx": "1"}},
			want: `{"pane_id":"w1\ufffd","source":"agentisan","tokens":{"ctx":"1"}}`,
		},
		"pane id with a truncated multibyte sequence": {
			meta: herdr.PaneMetadata{PaneID: "w1\xe2\x82", Source: "agentisan", Tokens: map[string]string{"ctx": "1"}},
			want: `{"pane_id":"w1\ufffd\ufffd","source":"agentisan","tokens":{"ctx":"1"}}`,
		},
		"pane id with an embedded NUL is escaped": {
			meta: herdr.PaneMetadata{PaneID: "w1\x00p2", Source: "agentisan", Tokens: map[string]string{"ctx": "1"}},
			want: `{"pane_id":"w1\u0000p2","source":"agentisan","tokens":{"ctx":"1"}}`,
		},
		"pane id with non-ASCII and shell metacharacters": {
			meta: herdr.PaneMetadata{PaneID: "wé:p1;$(x) *", Source: "agentisan", Tokens: map[string]string{"ctx": "1"}},
			want: `{"pane_id":"wé:p1;$(x) *","source":"agentisan","tokens":{"ctx":"1"}}`,
		},
		// An empty value is how herdr clears a token, so it must be sent.
		"empty value is sent to clear the token": {
			meta: herdr.PaneMetadata{PaneID: "w1:p2", Source: "agentisan", Tokens: map[string]string{"ctx": ""}},
			want: `{"pane_id":"w1:p2","source":"agentisan","tokens":{"ctx":""}}`,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := herdrtest.Start(t, okReply)

			require.NoError(t, herdr.Client{SocketPath: srv.Path}.ReportPaneMetadata(t.Context(), tc.meta))

			reqs := srv.Requests()
			require.Len(t, reqs, 1)
			assert.Equal(t, "pane.report_metadata", reqs[0].Method)
			assert.JSONEq(t, tc.want, string(reqs[0].Params))
		})
	}
}

func TestReportPaneMetadataFailureClasses(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		handler herdrtest.Handler
		want    error
	}{
		"pane not found": {
			handler: func(herdrtest.Request) herdrtest.Reply {
				return herdrtest.Reply{Error: &herdrtest.ErrorBody{Code: "pane_not_found", Message: "pane w9:p9 not found"}}
			},
			want: herdr.ErrAPI,
		},
		"wrong result type": {
			handler: func(herdrtest.Request) herdrtest.Reply {
				return herdrtest.Reply{Result: map[string]any{"type": "pong"}}
			},
			want: herdr.ErrProtocol,
		},
		"closed without a reply": {
			handler: func(herdrtest.Request) herdrtest.Reply { return herdrtest.Reply{Silent: true} },
			want:    herdr.ErrUnavailable,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := herdrtest.Start(t, tc.handler)

			err := herdr.Client{SocketPath: srv.Path}.ReportPaneMetadata(t.Context(),
				herdr.PaneMetadata{PaneID: "w9:p9", Source: "agentisan", Tokens: map[string]string{"ctx": "1"}})
			require.ErrorIs(t, err, tc.want)
		})
	}
}

// TestCallFailureClasses walks every way a call can fail. Each must surface as
// an error a caller can branch on with errors.Is, never as a zero result.
func TestCallFailureClasses(t *testing.T) {
	t.Parallel()

	wrongID := "someone-else"
	tests := map[string]struct {
		handler herdrtest.Handler
		want    error
		check   func(t *testing.T, err error)
	}{
		"herdr error body": {
			handler: func(herdrtest.Request) herdrtest.Reply {
				return herdrtest.Reply{Error: &herdrtest.ErrorBody{Code: "not_found", Message: "pane not found"}}
			},
			want: herdr.ErrAPI,
			check: func(t *testing.T, err error) {
				t.Helper()
				var apiErr *herdr.APIError
				require.ErrorAs(t, err, &apiErr)
				assert.Equal(t, "not_found", apiErr.Code)
				assert.Equal(t, "pane not found", apiErr.Message)
			},
		},
		"response id does not match": {
			handler: func(r herdrtest.Request) herdrtest.Reply {
				rep := pong(r)
				rep.ID = &wrongID
				return rep
			},
			want: herdr.ErrProtocol,
		},
		"response is not JSON":     {handler: func(herdrtest.Request) herdrtest.Reply { return herdrtest.Reply{Raw: []byte("not json")} }, want: herdr.ErrProtocol},
		"neither result nor error": {handler: func(herdrtest.Request) herdrtest.Reply { return herdrtest.Reply{Raw: []byte(`{"id":"x"}`)} }, want: herdr.ErrProtocol},
		"wrong result type": {
			handler: func(herdrtest.Request) herdrtest.Reply {
				return herdrtest.Reply{Result: map[string]any{"type": "notification_show", "shown": true, "reason": "shown"}}
			},
			want: herdr.ErrProtocol,
		},
		"oversized response": {
			handler: func(herdrtest.Request) herdrtest.Reply {
				return herdrtest.Reply{Result: map[string]any{"type": "pong", "version": strings.Repeat("v", 2<<20), "protocol": 22}}
			},
			want: herdr.ErrProtocol,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := herdrtest.Start(t, tc.handler)

			_, err := herdr.Client{SocketPath: srv.Path}.Ping(t.Context())
			require.Error(t, err)
			require.ErrorIs(t, err, tc.want)
			if tc.check != nil {
				tc.check(t, err)
			}
		})
	}
}

func TestSocketPathClasses(t *testing.T) {
	t.Parallel()

	dir, err := os.MkdirTemp("", "hd")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	regular := filepath.Join(dir, "file")
	require.NoError(t, os.WriteFile(regular, nil, 0o600))

	tests := map[string]struct {
		path string
		want error
	}{
		"unset":             {path: "", want: herdr.ErrNoSocket},
		"missing":           {path: filepath.Join(dir, "absent.sock"), want: herdr.ErrUnavailable},
		"not a socket":      {path: regular, want: herdr.ErrUnavailable},
		"directory":         {path: dir, want: herdr.ErrUnavailable},
		"embedded NUL":      {path: "a\x00b", want: herdr.ErrUnavailable},
		"over the sun_path": {path: "/" + strings.Repeat("x", 200), want: herdr.ErrUnavailable},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := herdr.Client{SocketPath: tc.path}.Ping(t.Context())
			require.ErrorIs(t, err, tc.want)
		})
	}
}

// TestCallHonoursTheContext: a herdr that accepts but never answers must not
// hang a plugin action, which holds one of herdr's 32 in-flight slots.
func TestCallHonoursTheContext(t *testing.T) {
	t.Parallel()

	dir, err := os.MkdirTemp("", "hd")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "s.sock")
	ln, err := net.Listen("unix", path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		<-t.Context().Done() // hold the connection open, never reply
		_ = conn.Close()
	}()

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = herdr.Client{SocketPath: path}.Ping(ctx)
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.DeadlineExceeded) || errors.Is(err, herdr.ErrUnavailable), "got %v", err)
	assert.Less(t, time.Since(start), 3*time.Second, "the call must stop at the deadline")
}

func TestAPIErrorMessageNamesTheMethodAndCode(t *testing.T) {
	t.Parallel()

	err := &herdr.APIError{Method: "ping", Code: "busy", Message: "try later"}
	assert.Contains(t, err.Error(), "ping")
	assert.Contains(t, err.Error(), "busy")
	assert.Contains(t, err.Error(), "try later")

	raw, mErr := json.Marshal(herdr.Notification{Title: "t"})
	require.NoError(t, mErr)
	assert.JSONEq(t, `{"title":"t"}`, string(raw))
}

// TestResponseFramingClasses walks the byte-level shapes a response line can
// take. herdr closes the connection after answering, so a complete line
// without a newline is fine; anything that is not one JSON object is not.
func TestResponseFramingClasses(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		reply herdrtest.Reply
		want  error // nil means the ping must succeed
	}{
		"no trailing newline":      {reply: herdrtest.Reply{Raw: []byte(`{"id":"ID","result":{"type":"pong","version":"v","protocol":22}}`), Unterminated: true}},
		"CRLF line ending":         {reply: herdrtest.Reply{Raw: []byte("{\"id\":\"ID\",\"result\":{\"type\":\"pong\",\"version\":\"v\",\"protocol\":22}}\r")}},
		"invalid UTF-8 in a value": {reply: herdrtest.Reply{Raw: []byte("{\"id\":\"ID\",\"result\":{\"type\":\"pong\",\"version\":\"v\xff\",\"protocol\":22}}")}},
		"byte-order mark":          {reply: herdrtest.Reply{Raw: []byte("\xef\xbb\xbf{\"id\":\"ID\",\"result\":{}}")}, want: herdr.ErrProtocol},
		"embedded NUL":             {reply: herdrtest.Reply{Raw: []byte("{\"id\":\"ID\"\x00}")}, want: herdr.ErrProtocol},
		"truncated object":         {reply: herdrtest.Reply{Raw: []byte(`{"id":"ID","result":{"type":"po`), Unterminated: true}, want: herdr.ErrProtocol},
		"two objects on one line":  {reply: herdrtest.Reply{Raw: []byte(`{"id":"ID","result":{}} {"id":"ID"}`)}, want: herdr.ErrProtocol},
		"closed without a reply":   {reply: herdrtest.Reply{Silent: true}, want: herdr.ErrUnavailable},
		"empty line":               {reply: herdrtest.Reply{Raw: []byte{}}, want: herdr.ErrProtocol},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := herdrtest.Start(t, func(r herdrtest.Request) herdrtest.Reply {
				rep := tc.reply
				// Echo the real request id into the raw fixtures.
				rep.Raw = []byte(strings.ReplaceAll(string(rep.Raw), `"ID"`, `"`+r.ID+`"`))
				return rep
			})

			_, err := herdr.Client{SocketPath: srv.Path}.Ping(t.Context())
			if tc.want == nil {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, tc.want)
		})
	}
}

// TestErrPaneNotFoundMatchesOnlyThatCode: a caller tells "this pane id names
// nothing" apart from every other herdr error with errors.Is, never by
// reading the code string. The match is exact: herdr's codes are snake_case
// constants, so anything else is a different error.
func TestErrPaneNotFoundMatchesOnlyThatCode(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		code string
		want bool
	}{
		"pane_not_found":      {code: "pane_not_found", want: true},
		"another code":        {code: "pane_move_failed"},
		"a different case":    {code: "PANE_NOT_FOUND"},
		"a longer code":       {code: "pane_not_found_x"},
		"workspace_not_found": {code: "workspace_not_found"},
		"empty code":          {code: ""},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := herdrtest.Start(t, func(herdrtest.Request) herdrtest.Reply {
				return herdrtest.Reply{Error: &herdrtest.ErrorBody{Code: tc.code, Message: "m"}}
			})

			err := herdr.Client{SocketPath: srv.Path}.ReportPaneMetadata(t.Context(),
				herdr.PaneMetadata{PaneID: "w9:p9", Source: "agentisan", Tokens: map[string]string{"ctx": "1"}})
			require.ErrorIs(t, err, herdr.ErrAPI, "every error body stays an ErrAPI")
			assert.Equal(t, tc.want, errors.Is(err, herdr.ErrPaneNotFound))
		})
	}
}

func TestListPanesSendsTheSchemaShape(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		panes []map[string]any
		want  []herdr.PaneInfo
	}{
		"several panes across workspaces": {
			panes: []map[string]any{
				{"pane_id": "w1:p1", "terminal_id": "term_a", "workspace_id": "w1", "tab_id": "w1:t1", "focused": true, "agent_status": "idle", "revision": 3},
				{"pane_id": "wN:p2", "terminal_id": "term_b", "workspace_id": "wN", "tab_id": "wN:t1", "focused": false, "agent_status": "working", "revision": 9, "agent": "claude"},
			},
			want: []herdr.PaneInfo{{PaneID: "w1:p1", WorkspaceID: "w1", Focused: true}, {PaneID: "wN:p2", WorkspaceID: "wN"}},
		},
		"no panes": {panes: []map[string]any{}, want: []herdr.PaneInfo{}},
		// The schema requires both; a pane without them reads as not focused.
		"a pane without focused or workspace_id": {panes: []map[string]any{{"pane_id": "w1:p1"}}, want: []herdr.PaneInfo{{PaneID: "w1:p1"}}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := herdrtest.Start(t, func(herdrtest.Request) herdrtest.Reply {
				return herdrtest.Reply{Result: map[string]any{"type": "pane_list", "panes": tc.panes}}
			})

			got, err := herdr.Client{SocketPath: srv.Path}.ListPanes(t.Context())
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)

			reqs := srv.Requests()
			require.Len(t, reqs, 1)
			assert.Equal(t, "pane.list", reqs[0].Method)
			// No workspace_id: herdr lists every workspace's panes.
			assert.JSONEq(t, `{}`, string(reqs[0].Params))
		})
	}
}

func TestPaneProcessInfoSendsTheSchemaShape(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		paneID string
		info   map[string]any
		want   herdr.ProcessInfo
	}{
		"a shell running a foreground job": {
			paneID: "w14:p1",
			info: map[string]any{
				"pane_id": "w14:p1", "shell_pid": 599895, "foreground_process_group_id": 894377, "tty": nil,
				"foreground_processes": []map[string]any{
					{"pid": 894377, "name": "claude", "argv": []string{"claude"}, "cmdline": "claude", "cwd": "/src"},
					{"pid": 919306, "name": "uv"},
				},
			},
			want: herdr.ProcessInfo{
				PaneID: "w14:p1", ShellPID: 599895, ForegroundProcessGroupID: 894377,
				ForegroundProcesses: []herdr.Process{{PID: 894377, Name: "claude"}, {PID: 919306, Name: "uv"}},
			},
		},
		// herdr answers an alias with the pane's current id.
		"an alias answers with the current id": {
			paneID: "wP:p1",
			info:   map[string]any{"pane_id": "wN:p2", "shell_pid": 10, "foreground_process_group_id": 11, "foreground_processes": []map[string]any{}},
			want:   herdr.ProcessInfo{PaneID: "wN:p2", ShellPID: 10, ForegroundProcessGroupID: 11, ForegroundProcesses: []herdr.Process{}},
		},
		// A pane whose shell has exited reports nulls: they decode to 0, which
		// names no process.
		"null pids decode to zero": {
			paneID: "w1:p1",
			info:   map[string]any{"pane_id": "w1:p1", "shell_pid": nil, "foreground_process_group_id": nil},
			want:   herdr.ProcessInfo{PaneID: "w1:p1"},
		},
		// Pane ids come from the environment: whatever bytes they hold, the
		// request stays one valid JSON line.
		"pane id with a newline":                {paneID: "w1\np1", info: map[string]any{"pane_id": "w1:p1"}, want: herdr.ProcessInfo{PaneID: "w1:p1"}},
		"pane id with invalid UTF-8":            {paneID: "w1\xff", info: map[string]any{"pane_id": "w1:p1"}, want: herdr.ProcessInfo{PaneID: "w1:p1"}},
		"pane id with a truncated multibyte":    {paneID: "w1\xe2\x82", info: map[string]any{"pane_id": "w1:p1"}, want: herdr.ProcessInfo{PaneID: "w1:p1"}},
		"pane id with an embedded NUL":          {paneID: "w1\x00p1", info: map[string]any{"pane_id": "w1:p1"}, want: herdr.ProcessInfo{PaneID: "w1:p1"}},
		"pane id with non-ASCII and shell meta": {paneID: "wé:p1;$(x) *", info: map[string]any{"pane_id": "w1:p1"}, want: herdr.ProcessInfo{PaneID: "w1:p1"}},
		"pids at the uint32 maximum": {
			paneID: "w1:p1",
			info:   map[string]any{"pane_id": "w1:p1", "shell_pid": uint32(1<<32 - 1), "foreground_processes": []map[string]any{{"pid": uint32(1<<32 - 1), "name": "x"}}},
			want:   herdr.ProcessInfo{PaneID: "w1:p1", ShellPID: 1<<32 - 1, ForegroundProcesses: []herdr.Process{{PID: 1<<32 - 1, Name: "x"}}},
		},
		// A null pane_id would make herdr answer for the focused pane, so an
		// empty id must still be sent as a string.
		"empty pane id is sent as a string, never null": {
			paneID: "",
			info:   map[string]any{"pane_id": "w1:p1"},
			want:   herdr.ProcessInfo{PaneID: "w1:p1"},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := herdrtest.Start(t, func(herdrtest.Request) herdrtest.Reply {
				return herdrtest.Reply{Result: map[string]any{"type": "pane_process_info", "process_info": tc.info}}
			})

			got, err := herdr.Client{SocketPath: srv.Path}.PaneProcessInfo(t.Context(), tc.paneID)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)

			reqs := srv.Requests()
			require.Len(t, reqs, 1)
			assert.Equal(t, "pane.process_info", reqs[0].Method)
			want, mErr := json.Marshal(map[string]string{"pane_id": tc.paneID})
			require.NoError(t, mErr)
			assert.JSONEq(t, string(want), string(reqs[0].Params))
		})
	}
}

// TestPaneQueryFailureClasses: both queries fail the way every call does, and
// a pid that is not a non-negative integer is a protocol violation rather
// than a silently wrong process.
func TestPaneQueryFailureClasses(t *testing.T) {
	t.Parallel()

	reply := func(result map[string]any) herdrtest.Handler {
		return func(herdrtest.Request) herdrtest.Reply { return herdrtest.Reply{Result: result} }
	}
	notFound := func(herdrtest.Request) herdrtest.Reply {
		return herdrtest.Reply{Error: &herdrtest.ErrorBody{Code: "pane_not_found", Message: "pane not found"}}
	}
	list := func(c herdr.Client) error { _, err := c.ListPanes(t.Context()); return err }
	info := func(c herdr.Client) error { _, err := c.PaneProcessInfo(t.Context(), "w1:p1"); return err }

	tests := map[string]struct {
		handler herdrtest.Handler
		call    func(herdr.Client) error
		want    error
	}{
		"list: wrong result type":            {handler: reply(map[string]any{"type": "pane_info"}), call: list, want: herdr.ErrProtocol},
		"list: panes missing":                {handler: reply(map[string]any{"type": "pane_list"}), call: list, want: herdr.ErrProtocol},
		"list: a pane without an id":         {handler: reply(map[string]any{"type": "pane_list", "panes": []map[string]any{{"pane_id": ""}}}), call: list, want: herdr.ErrProtocol},
		"list: panes is not an array":        {handler: reply(map[string]any{"type": "pane_list", "panes": "w1:p1"}), call: list, want: herdr.ErrProtocol},
		"list: closed without a reply":       {handler: func(herdrtest.Request) herdrtest.Reply { return herdrtest.Reply{Silent: true} }, call: list, want: herdr.ErrUnavailable},
		"list: focused is not a bool":        {handler: reply(map[string]any{"type": "pane_list", "panes": []map[string]any{{"pane_id": "w1:p1", "focused": "yes"}}}), call: list, want: herdr.ErrProtocol},
		"list: workspace_id is not a string": {handler: reply(map[string]any{"type": "pane_list", "panes": []map[string]any{{"pane_id": "w1:p1", "workspace_id": 7}}}), call: list, want: herdr.ErrProtocol},
		"info: pane not found":               {handler: notFound, call: info, want: herdr.ErrPaneNotFound},
		"info: wrong result type":            {handler: reply(map[string]any{"type": "pane_list", "panes": []any{}}), call: info, want: herdr.ErrProtocol},
		"info: negative pid":                 {handler: reply(map[string]any{"type": "pane_process_info", "process_info": map[string]any{"pane_id": "w1:p1", "shell_pid": -1}}), call: info, want: herdr.ErrProtocol},
		"info: fractional pid":               {handler: reply(map[string]any{"type": "pane_process_info", "process_info": map[string]any{"pane_id": "w1:p1", "shell_pid": 1.5}}), call: info, want: herdr.ErrProtocol},
		"info: pid as a string":              {handler: reply(map[string]any{"type": "pane_process_info", "process_info": map[string]any{"pane_id": "w1:p1", "foreground_process_group_id": "7"}}), call: info, want: herdr.ErrProtocol},
		"info: pid one past uint32":          {handler: reply(map[string]any{"type": "pane_process_info", "process_info": map[string]any{"pane_id": "w1:p1", "foreground_processes": []map[string]any{{"pid": 1 << 32, "name": "x"}}}}), call: info, want: herdr.ErrProtocol},
		"info: pane_id empty":                {handler: reply(map[string]any{"type": "pane_process_info", "process_info": map[string]any{"pane_id": ""}}), call: info, want: herdr.ErrProtocol},
		"info: process_info missing":         {handler: reply(map[string]any{"type": "pane_process_info"}), call: info, want: herdr.ErrProtocol},
		"info: closed without a reply":       {handler: func(herdrtest.Request) herdrtest.Reply { return herdrtest.Reply{Silent: true} }, call: info, want: herdr.ErrUnavailable},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := herdrtest.Start(t, tc.handler)

			err := tc.call(herdr.Client{SocketPath: srv.Path})
			require.ErrorIs(t, err, tc.want)
		})
	}
}

// silentSocket accepts connections and never answers, like a wedged herdr.
func silentSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "hd")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "s.sock")
	ln, err := net.Listen("unix", path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				<-t.Context().Done()
				_ = conn.Close()
			}()
		}
	}()
	return path
}

// TestEveryCallIsBounded: the client bounds each call by its own timeout, so
// a long-running caller such as the daemon cannot forget to and hang on a
// wedged herdr. A caller's earlier deadline still wins.
func TestEveryCallIsBounded(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		timeout time.Duration
		ctx     func() (context.Context, context.CancelFunc)
		within  time.Duration
	}{
		"no caller deadline: the client's timeout": {
			timeout: 150 * time.Millisecond,
			ctx:     func() (context.Context, context.CancelFunc) { return context.WithCancel(context.Background()) },
			within:  2 * time.Second,
		},
		"an earlier caller deadline wins": {
			timeout: time.Hour,
			ctx: func() (context.Context, context.CancelFunc) {
				return context.WithTimeout(context.Background(), 150*time.Millisecond)
			},
			within: 2 * time.Second,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := tc.ctx()
			defer cancel()

			start := time.Now()
			_, err := herdr.Client{SocketPath: silentSocket(t), Timeout: tc.timeout}.Ping(ctx)
			require.Error(t, err)
			// The socket deadline and the context deadline are the same
			// instant; whichever the read notices first names the error.
			assert.True(t, errors.Is(err, context.DeadlineExceeded) || errors.Is(err, herdr.ErrUnavailable), "got %v", err)
			assert.Less(t, time.Since(start), tc.within)
		})
	}
}

func TestCallTimeoutIsFiveSeconds(t *testing.T) {
	t.Parallel()
	assert.Equal(t, 5*time.Second, herdr.CallTimeout, "the zero Client bounds each call by CallTimeout")
}
