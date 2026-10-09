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
