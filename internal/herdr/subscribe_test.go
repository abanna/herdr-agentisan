package herdr_test

import (
	"context"
	"encoding/json"
	"errors"
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

// line is one newline-terminated stream line.
func line(s string) []byte { return []byte(s + "\n") }

// streaming answers events.subscribe with herdr's ack and keeps the
// connection open, writing every line sent on lines.
func streaming(lines chan []byte) herdrtest.Handler {
	return func(herdrtest.Request) herdrtest.Reply {
		return herdrtest.Reply{Result: herdrtest.SubscriptionStarted(), Stream: lines}
	}
}

func subscribe(t *testing.T, c herdr.Client) *herdr.Subscription {
	t.Helper()
	sub, err := c.Subscribe(t.Context(), herdr.SubscribePaneFocused)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Close() })
	return sub
}

// next reads one event, failing the test if none comes within 3 s.
func next(t *testing.T, sub *herdr.Subscription) (herdr.Event, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	return sub.Next(ctx)
}

// TestSubscribeStreamsFocusEvents: one events.subscribe request in the
// schema's shape, herdr's ack, then every event line decoded in order. Lines
// herdr writes right behind the ack are not lost to the ack's read.
func TestSubscribeStreamsFocusEvents(t *testing.T) {
	t.Parallel()
	lines := make(chan []byte, 8)
	// Queued before the subscription: the server writes them straight after
	// the ack, so the client most likely reads them in the same chunk.
	lines <- herdrtest.PaneFocused("w1:p2", "w1")
	lines <- line(`{"event":"workspace_focused","data":{"type":"workspace_focused","workspace_id":"w2"}}`)
	lines <- herdrtest.PaneFocused("wé:p1;$(x) *", "wé")
	srv := herdrtest.Start(t, streaming(lines))

	sub := subscribe(t, herdr.Client{SocketPath: srv.Path})

	reqs := srv.Requests()
	require.Len(t, reqs, 1)
	assert.Equal(t, "events.subscribe", reqs[0].Method)
	assert.JSONEq(t, `{"subscriptions":[{"type":"pane.focused"}]}`, string(reqs[0].Params))

	want := []herdr.Event{
		{Kind: herdr.EventPaneFocused, PaneID: "w1:p2", WorkspaceID: "w1"},
		{Kind: "workspace_focused", WorkspaceID: "w2"},
		{Kind: herdr.EventPaneFocused, PaneID: "wé:p1;$(x) *", WorkspaceID: "wé"},
	}
	for _, w := range want {
		got, err := next(t, sub)
		require.NoError(t, err)
		var data map[string]any
		require.NoError(t, json.Unmarshal(got.Data, &data), "Data keeps the raw data object")
		assert.Equal(t, w.Kind, data["type"])
		got.Data = nil
		assert.Equal(t, w, got)
	}

	// A line sent long after the ack still arrives.
	lines <- herdrtest.PaneFocused("w3:p1", "w3")
	got, err := next(t, sub)
	require.NoError(t, err)
	assert.Equal(t, "w3:p1", got.PaneID)
}

// TestSubscribeSendsEveryType: every subscription goes in one request, as
// one JSON string each. No types is an empty list, never null: herdr
// requires the array.
func TestSubscribeSendsEveryType(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		types []string
		want  string
	}{
		"two types in one request":                      {types: []string{herdr.SubscribePaneFocused, "tab.focused"}, want: `{"subscriptions":[{"type":"pane.focused"},{"type":"tab.focused"}]}`},
		"no types is an empty list":                     {types: nil, want: `{"subscriptions":[]}`},
		"a type with JSON metacharacters is one string": {types: []string{"a\"b\n}"}, want: `{"subscriptions":[{"type":"a\"b\n}"}]}`},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := herdrtest.Start(t, streaming(make(chan []byte)))

			sub, err := herdr.Client{SocketPath: srv.Path}.Subscribe(t.Context(), tc.types...)
			require.NoError(t, err)
			require.NoError(t, sub.Close())
			require.NoError(t, sub.Close(), "closing twice is safe")

			reqs := srv.Requests()
			require.Len(t, reqs, 1)
			assert.JSONEq(t, tc.want, string(reqs[0].Params))
		})
	}
}

// TestSubscribeSocketPathClasses: every way the socket path can fail is an
// error a caller can branch on, before any request is sent.
func TestSubscribeSocketPathClasses(t *testing.T) {
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
			sub, err := herdr.Client{SocketPath: tc.path}.Subscribe(t.Context(), herdr.SubscribePaneFocused)
			require.ErrorIs(t, err, tc.want)
			assert.Nil(t, sub)
		})
	}
}

// TestSubscriptionOutlivesTheCallTimeout: only the ack is bounded by the
// client's timeout. A focus stream is quiet for hours at a time.
func TestSubscriptionOutlivesTheCallTimeout(t *testing.T) {
	t.Parallel()
	lines := make(chan []byte, 1)
	srv := herdrtest.Start(t, streaming(lines))
	sub := subscribe(t, herdr.Client{SocketPath: srv.Path, Timeout: 50 * time.Millisecond})

	time.Sleep(250 * time.Millisecond)
	lines <- herdrtest.PaneFocused("w1:p1", "w1")
	got, err := next(t, sub)
	require.NoError(t, err)
	assert.Equal(t, "w1:p1", got.PaneID)
}

// TestSubscribeAckClasses walks every way the ack can fail. Each is an error a
// caller can branch on, never a subscription that silently streams nothing.
func TestSubscribeAckClasses(t *testing.T) {
	t.Parallel()

	wrongID := "someone-else"
	reply := func(r herdrtest.Reply) herdrtest.Handler {
		return func(herdrtest.Request) herdrtest.Reply { return r }
	}
	// echo answers with raw bytes, the request's id put in for "ID", and
	// keeps the connection open as a subscription would.
	const ack = `{"id":"ID","result":{"type":"subscription_started"}}`
	echo := func(raw string) herdrtest.Handler {
		return func(r herdrtest.Request) herdrtest.Reply {
			return herdrtest.Reply{Raw: []byte(strings.ReplaceAll(raw, `"ID"`, `"`+r.ID+`"`)), Stream: make(chan []byte)}
		}
	}
	tests := map[string]struct {
		handler herdrtest.Handler
		want    error
	}{
		"herdr error body": {
			handler: reply(herdrtest.Reply{Error: &herdrtest.ErrorBody{Code: "invalid_request", Message: "bad subscription"}}),
			want:    herdr.ErrAPI,
		},
		"wrong result type": {handler: reply(herdrtest.Reply{Result: map[string]any{"type": "pong"}}), want: herdr.ErrProtocol},
		"response id does not match": {
			handler: reply(herdrtest.Reply{Result: herdrtest.SubscriptionStarted(), ID: &wrongID}),
			want:    herdr.ErrProtocol,
		},
		"not JSON":                 {handler: reply(herdrtest.Reply{Raw: []byte("nope")}), want: herdr.ErrProtocol},
		"neither result nor error": {handler: reply(herdrtest.Reply{Raw: []byte(`{"id":"x"}`)}), want: herdr.ErrProtocol},
		"closed without an ack":    {handler: reply(herdrtest.Reply{Silent: true}), want: herdr.ErrUnavailable},
		"an ack cut short":         {handler: reply(herdrtest.Reply{Raw: []byte(`{"id":"x","res`), Unterminated: true}), want: herdr.ErrUnavailable},
		"oversized ack": {
			handler: reply(herdrtest.Reply{Result: map[string]any{"type": "subscription_started", "pad": strings.Repeat("x", 2<<20)}}),
			want:    herdr.ErrProtocol,
		},
		"an empty ack line":                {handler: echo(""), want: herdr.ErrProtocol},
		"a byte-order mark":                {handler: echo("\xef\xbb\xbf" + ack), want: herdr.ErrProtocol},
		"an embedded NUL":                  {handler: echo(`{"id":"ID"` + "\x00" + `,"result":{"type":"subscription_started"}}`), want: herdr.ErrProtocol},
		"invalid UTF-8 in the result type": {handler: echo(`{"id":"ID","result":{"type":"subscription_started` + "\xff" + `"}}`), want: herdr.ErrProtocol},
		"two values on the ack line":       {handler: echo(ack + " {}"), want: herdr.ErrProtocol},
		"a CRLF ack":                       {handler: echo(ack + "\r")},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := herdrtest.Start(t, tc.handler)

			sub, err := herdr.Client{SocketPath: srv.Path}.Subscribe(t.Context(), herdr.SubscribePaneFocused)
			if tc.want == nil {
				require.NoError(t, err)
				require.NoError(t, sub.Close())
				return
			}
			require.ErrorIs(t, err, tc.want)
			assert.Nil(t, sub)
		})
	}

	t.Run("no socket configured", func(t *testing.T) {
		t.Parallel()
		_, err := herdr.Client{}.Subscribe(t.Context(), herdr.SubscribePaneFocused)
		require.ErrorIs(t, err, herdr.ErrNoSocket)
	})
	t.Run("nothing at the socket path", func(t *testing.T) {
		t.Parallel()
		srv := herdrtest.Start(t, streaming(nil))
		srv.Close()
		_, err := herdr.Client{SocketPath: srv.Path}.Subscribe(t.Context(), herdr.SubscribePaneFocused)
		require.ErrorIs(t, err, herdr.ErrUnavailable)
	})
}

// TestSubscribeAckIsBounded: a herdr that accepts and never acknowledges
// does not hold the subscriber past the client's timeout, or an earlier
// caller deadline.
func TestSubscribeAckIsBounded(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		timeout time.Duration
		ctx     func() (context.Context, context.CancelFunc)
	}{
		"the client's timeout": {
			timeout: 150 * time.Millisecond,
			ctx:     func() (context.Context, context.CancelFunc) { return context.WithCancel(context.Background()) },
		},
		"an earlier caller deadline": {
			timeout: time.Hour,
			ctx: func() (context.Context, context.CancelFunc) {
				return context.WithTimeout(context.Background(), 150*time.Millisecond)
			},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := tc.ctx()
			defer cancel()

			start := time.Now()
			_, err := herdr.Client{SocketPath: silentSocket(t), Timeout: tc.timeout}.Subscribe(ctx, herdr.SubscribePaneFocused)
			require.Error(t, err)
			assert.True(t, errors.Is(err, context.DeadlineExceeded) || errors.Is(err, herdr.ErrUnavailable), "got %v", err)
			assert.Less(t, time.Since(start), 2*time.Second)
		})
	}
}

// TestNextFailureClasses walks what can arrive on an open subscription. Every
// failure ends it: the caller resubscribes and resyncs (ADR-001 A3).
func TestNextFailureClasses(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		lines func(id string) []string // written verbatim, then the stream ends
		want  error
		not   error // must not match
	}{
		"events lost": {
			lines: func(id string) []string { return []string{string(herdrtest.EventsLost(id))} },
			want:  herdr.ErrEventsLost,
		},
		"events lost is an API error too": {
			lines: func(id string) []string { return []string{string(herdrtest.EventsLost(id))} },
			want:  herdr.ErrAPI,
		},
		"another error code": {
			lines: func(id string) []string {
				return []string{`{"id":"` + id + `","error":{"code":"server_unavailable","message":"event history is unavailable"}}` + "\n"}
			},
			want: herdr.ErrAPI, not: herdr.ErrEventsLost,
		},
		"an error for another request": {
			lines: func(string) []string { return []string{string(herdrtest.EventsLost("someone-else"))} },
			want:  herdr.ErrProtocol,
		},
		"the server ends the stream": {lines: func(string) []string { return nil }, want: herdr.ErrUnavailable},
		"a line cut short by the end": {
			lines: func(string) []string { return []string{`{"event":"pane_focused","data":{"ty`} },
			want:  herdr.ErrUnavailable,
		},
		"not JSON":                      {lines: func(string) []string { return []string{"nope\n"} }, want: herdr.ErrProtocol},
		"two values on a line":          {lines: func(string) []string { return []string{`{"event":"x","data":{"type":"x"}} {}` + "\n"} }, want: herdr.ErrProtocol},
		"neither an event nor an error": {lines: func(string) []string { return []string{`{"result":{}}` + "\n"} }, want: herdr.ErrProtocol},
		"an event without data":         {lines: func(string) []string { return []string{`{"event":"pane_focused"}` + "\n"} }, want: herdr.ErrProtocol},
		"data that is not an object":    {lines: func(string) []string { return []string{`{"event":"pane_focused","data":"w1:p1"}` + "\n"} }, want: herdr.ErrProtocol},
		"data of another kind": {
			lines: func(string) []string {
				return []string{`{"event":"pane_focused","data":{"type":"tab_focused","pane_id":"w1:p1"}}` + "\n"}
			},
			want: herdr.ErrProtocol,
		},
		"a focus without a pane id": {
			lines: func(string) []string {
				return []string{`{"event":"pane_focused","data":{"type":"pane_focused","workspace_id":"w1"}}` + "\n"}
			},
			want: herdr.ErrProtocol,
		},
		"an oversized line": {
			lines: func(string) []string {
				return []string{`{"event":"pane_focused","data":{"type":"pane_focused","pane_id":"` + strings.Repeat("p", 2<<20) + `"}}` + "\n"}
			},
			want: herdr.ErrProtocol,
		},
		"an empty line":     {lines: func(string) []string { return []string{"\n"} }, want: herdr.ErrProtocol},
		"a byte-order mark": {lines: func(string) []string { return []string{"\xef\xbb\xbf" + string(herdrtest.PaneFocused("w1:p1", "w1"))} }, want: herdr.ErrProtocol},
		"a raw NUL byte": {
			lines: func(string) []string {
				return []string{`{"event":"pane_focused","data":{"type":"pane_focused","pane_id":"w1` + "\x00" + `"}}` + "\n"}
			},
			want: herdr.ErrProtocol,
		},
		"data nested past the decoder's depth limit": {
			lines: func(string) []string {
				return []string{`{"event":"pane_focused","data":{"type":"pane_focused","pane_id":"w1:p1","x":` +
					strings.Repeat("[", 20000) + strings.Repeat("]", 20000) + `}}` + "\n"}
			},
			want: herdr.ErrProtocol,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			lines := make(chan []byte, 4)
			srv := herdrtest.Start(t, func(r herdrtest.Request) herdrtest.Reply {
				for _, l := range tc.lines(r.ID) {
					lines <- []byte(l)
				}
				close(lines)
				return herdrtest.Reply{Result: herdrtest.SubscriptionStarted(), Stream: lines}
			})
			sub := subscribe(t, herdr.Client{SocketPath: srv.Path})

			_, err := next(t, sub)
			require.ErrorIs(t, err, tc.want)
			if tc.not != nil {
				require.NotErrorIs(t, err, tc.not)
			}
			_, again := next(t, sub)
			require.Error(t, again, "a failed subscription stays failed")
		})
	}
}

// TestNextDecodesEveryEncoding: an event line that is valid JSON decodes,
// whatever its framing or the bytes in its strings. encoding/json replaces
// bytes that are not UTF-8, and a lone surrogate escape, with U+FFFD, so a
// pane id is always valid UTF-8 by the time a caller sees it.
func TestNextDecodesEveryEncoding(t *testing.T) {
	t.Parallel()

	focus := func(paneJSON string) string {
		return `{"event":"pane_focused","data":{"type":"pane_focused","pane_id":"` + paneJSON + `","workspace_id":"w1"}}`
	}
	tests := map[string]struct {
		line string // written verbatim
		pane string
	}{
		"a CRLF line ending":                   {line: focus("w1:p1") + "\r\n", pane: "w1:p1"},
		"an escaped NUL in the pane id":        {line: focus(`w1\u0000p1`) + "\n", pane: "w1\x00p1"},
		"invalid UTF-8 in the pane id":         {line: focus("w1\xff") + "\n", pane: "w1\ufffd"},
		"a truncated multibyte sequence":       {line: focus("w1\xe2\x82") + "\n", pane: "w1\ufffd\ufffd"},
		"a Latin-1 byte":                       {line: focus("w\xe9") + "\n", pane: "w\ufffd"},
		"mixed encodings in one pane id":       {line: focus("wé\xe9") + "\n", pane: "wé\ufffd"},
		"a lone surrogate escape":              {line: focus(`w1\ud800`) + "\n", pane: "w1\ufffd"},
		"a byte-order mark inside the pane id": {line: focus("\ufeffw1") + "\n", pane: "\ufeffw1"},
		"an escaped newline in the pane id":    {line: focus(`w1\np1`) + "\n", pane: "w1\np1"},
		"deeply nested data the client skips": {
			line: `{"event":"pane_focused","data":{"type":"pane_focused","pane_id":"w1:p1","x":` +
				strings.Repeat("[", 1000) + strings.Repeat("]", 1000) + `}}` + "\n",
			pane: "w1:p1",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			lines := make(chan []byte, 1)
			lines <- []byte(tc.line)
			srv := herdrtest.Start(t, streaming(lines))
			sub := subscribe(t, herdr.Client{SocketPath: srv.Path})

			got, err := next(t, sub)
			require.NoError(t, err)
			assert.Equal(t, herdr.EventPaneFocused, got.Kind)
			assert.Equal(t, tc.pane, got.PaneID)
		})
	}
}

// TestStreamLineSizeBoundary: a line, newline included, of exactly 1 MiB is
// read; one byte more is ErrProtocol. The ack and the events share the cap.
func TestStreamLineSizeBoundary(t *testing.T) {
	t.Parallel()
	const limit = 1 << 20
	// sized pads prefix + p...p + suffix + "\n" to total bytes.
	sized := func(prefix, suffix string, total int) string {
		return prefix + strings.Repeat("p", total-len(prefix)-len(suffix)-1) + suffix
	}
	const eventPrefix, eventSuffix = `{"event":"pane_focused","data":{"type":"pane_focused","workspace_id":"w1","pane_id":"`, `"}}`
	event := func(total int) string { return sized(eventPrefix, eventSuffix, total) + "\n" }

	tests := map[string]struct {
		ack   func(id string) string // "" is the plain ack
		event string
		want  error
	}{
		"an ack of exactly 1 MiB": {
			ack: func(id string) string {
				return sized(`{"id":"`+id+`","result":{"type":"subscription_started","pad":"`, `"}}`, limit)
			},
		},
		"an ack one byte past 1 MiB": {
			ack: func(id string) string {
				return sized(`{"id":"`+id+`","result":{"type":"subscription_started","pad":"`, `"}}`, limit+1)
			},
			want: herdr.ErrProtocol,
		},
		"an event line of exactly 1 MiB":    {event: event(limit)},
		"an event line one byte past 1 MiB": {event: event(limit + 1), want: herdr.ErrProtocol},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			lines := make(chan []byte, 1)
			if tc.event != "" {
				lines <- []byte(tc.event)
			}
			srv := herdrtest.Start(t, func(r herdrtest.Request) herdrtest.Reply {
				if tc.ack != nil {
					return herdrtest.Reply{Raw: []byte(tc.ack(r.ID)), Stream: lines}
				}
				return herdrtest.Reply{Result: herdrtest.SubscriptionStarted(), Stream: lines}
			})

			sub, err := herdr.Client{SocketPath: srv.Path}.Subscribe(t.Context(), herdr.SubscribePaneFocused)
			if tc.ack != nil {
				if tc.want != nil {
					require.ErrorIs(t, err, tc.want)
					return
				}
				require.NoError(t, err)
				require.NoError(t, sub.Close())
				return
			}
			require.NoError(t, err)
			t.Cleanup(func() { _ = sub.Close() })
			got, err := next(t, sub)
			if tc.want != nil {
				require.ErrorIs(t, err, tc.want)
				return
			}
			require.NoError(t, err)
			assert.Len(t, got.PaneID, limit-len(eventPrefix)-len(eventSuffix)-1, "the whole pane id arrives")
		})
	}
}

// TestNextHonoursTheContext: a cancelled wait returns at once with the
// context's error, never ErrUnavailable, so a stopping daemon exits rather
// than resubscribes. The context Subscribe was given bounds the whole
// subscription.
func TestNextHonoursTheContext(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		run  func(t *testing.T, c herdr.Client) error
		want error
	}{
		"Next's context is cancelled while it waits": {
			run: func(t *testing.T, c herdr.Client) error {
				sub := subscribe(t, c)
				ctx, cancel := context.WithCancel(t.Context())
				time.AfterFunc(100*time.Millisecond, cancel)
				_, err := sub.Next(ctx)
				return err
			},
			want: context.Canceled,
		},
		"Next's deadline passes": {
			run: func(t *testing.T, c herdr.Client) error {
				sub := subscribe(t, c)
				ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
				defer cancel()
				_, err := sub.Next(ctx)
				return err
			},
			want: context.DeadlineExceeded,
		},
		"Next's context is already done": {
			run: func(t *testing.T, c herdr.Client) error {
				sub := subscribe(t, c)
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				_, err := sub.Next(ctx)
				return err
			},
			want: context.Canceled,
		},
		"the subscription's context is cancelled": {
			run: func(t *testing.T, c herdr.Client) error {
				ctx, cancel := context.WithCancel(t.Context())
				sub, err := c.Subscribe(ctx, herdr.SubscribePaneFocused)
				require.NoError(t, err)
				t.Cleanup(func() { _ = sub.Close() })
				time.AfterFunc(100*time.Millisecond, cancel)
				_, err = sub.Next(context.Background())
				return err
			},
			want: context.Canceled,
		},
		"Subscribe's context is already done": {
			run: func(t *testing.T, c herdr.Client) error {
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				_, err := c.Subscribe(ctx, herdr.SubscribePaneFocused)
				return err
			},
			want: context.Canceled,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := herdrtest.Start(t, streaming(make(chan []byte)))

			start := time.Now()
			err := tc.run(t, herdr.Client{SocketPath: srv.Path})
			require.ErrorIs(t, err, tc.want)
			require.NotErrorIs(t, err, herdr.ErrUnavailable)
			assert.Less(t, time.Since(start), 2*time.Second, "the wait ends promptly")
		})
	}
}

// TestCloseEndsAWaitingNext: closing the subscription from another goroutine
// ends a Next that is waiting on it.
func TestCloseEndsAWaitingNext(t *testing.T) {
	t.Parallel()
	srv := herdrtest.Start(t, streaming(make(chan []byte)))
	sub := subscribe(t, herdr.Client{SocketPath: srv.Path})

	time.AfterFunc(100*time.Millisecond, func() { _ = sub.Close() })
	_, err := next(t, sub)
	require.ErrorIs(t, err, herdr.ErrUnavailable)
}

// TestServerCloseEndsTheStream: a herdr that exits or crashes mid-stream
// ends the subscription with ErrUnavailable.
func TestServerCloseEndsTheStream(t *testing.T) {
	t.Parallel()

	for name, end := range map[string]func(*herdrtest.Server){
		"server closed":  (*herdrtest.Server).Close,
		"server crashed": (*herdrtest.Server).Crash,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := herdrtest.Start(t, streaming(make(chan []byte)))
			sub := subscribe(t, herdr.Client{SocketPath: srv.Path})

			time.AfterFunc(100*time.Millisecond, func() { end(srv) })
			_, err := next(t, sub)
			require.ErrorIs(t, err, herdr.ErrUnavailable)
		})
	}
}

// TestAPIErrorSentinelsMatchOnlyTheirCode: callers tell herdr's error codes
// apart with errors.Is, never by reading the code string.
func TestAPIErrorSentinelsMatchOnlyTheirCode(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		code         string
		paneNotFound bool
		eventsLost   bool
	}{
		"pane_not_found":            {code: "pane_not_found", paneNotFound: true},
		"events_lost":               {code: "events_lost", eventsLost: true},
		"events_lost in caps":       {code: "EVENTS_LOST"},
		"a longer events_lost code": {code: "events_lost_x"},
		"server_unavailable":        {code: "server_unavailable"},
		"empty code":                {code: ""},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var err error = &herdr.APIError{Method: "events.subscribe", Code: tc.code, Message: "m"}
			assert.ErrorIs(t, err, herdr.ErrAPI)
			assert.Equal(t, tc.paneNotFound, errors.Is(err, herdr.ErrPaneNotFound))
			assert.Equal(t, tc.eventsLost, errors.Is(err, herdr.ErrEventsLost))
		})
	}
}
