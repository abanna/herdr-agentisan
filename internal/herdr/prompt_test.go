package herdr_test

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/herdr/herdrtest"
)

func prompted(paneID, name string) map[string]any {
	return map[string]any{"type": "agent_prompted", "agent": map[string]any{"pane_id": paneID, "name": name}}
}

// TestPromptAgentSendsTargetAndText: agent.prompt carries exactly the target
// and the text, never wait (herdr would answer only once the agent reached a
// status, past the call's timeout), and the text goes as it is given: the
// caller cleans it.
func TestPromptAgentSendsTargetAndText(t *testing.T) {
	t.Parallel()

	tests := map[string]struct{ target, text string }{
		"a worker by name":            {target: "ha-coder-2", text: "[msg 01J from ha-boss] go"},
		"a boss by name":              {target: "boss", text: "[agentisan] message 01J to x may not have been delivered"},
		"lines and tabs":              {target: "w", text: "one\ntwo\tthree"},
		"bytes the caller left alone": {target: "w", text: "\x1b[201~ \r"},
		"non-ASCII":                   {target: "エージェント", text: "👍 ok"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := herdrtest.Start(t, func(herdrtest.Request) herdrtest.Reply {
				return herdrtest.Reply{Result: prompted("w2:p4", tc.target)}
			})

			got, err := herdr.Client{SocketPath: srv.Path}.PromptAgent(t.Context(), tc.target, tc.text)
			require.NoError(t, err)
			assert.Equal(t, herdr.AgentInfo{PaneID: "w2:p4", Name: tc.target}, got)

			reqs := srv.Requests()
			require.Len(t, reqs, 1)
			assert.Equal(t, "agent.prompt", reqs[0].Method)
			want, mErr := json.Marshal(map[string]string{"target": tc.target, "text": tc.text})
			require.NoError(t, mErr)
			assert.JSONEq(t, string(want), string(reqs[0].Params))
		})
	}
}

// TestPromptAgentRefusals: each of herdr's errors reaches the caller with
// its code, which is what tells a refusal before any byte was typed from a
// prompt that may have landed. None of them is ErrNotSent: the request was
// written, so only the code can say.
func TestPromptAgentRefusals(t *testing.T) {
	t.Parallel()

	for _, code := range []string{"empty_agent_prompt", "agent_not_found", "agent_target_ambiguous", "agent_blocked", "agent_not_ready", "timeout", "agent_prompt_failed"} {
		t.Run(code, func(t *testing.T) {
			t.Parallel()
			srv := herdrtest.Start(t, func(herdrtest.Request) herdrtest.Reply {
				return herdrtest.Reply{Error: &herdrtest.ErrorBody{Code: code, Message: "m"}}
			})

			_, err := herdr.Client{SocketPath: srv.Path}.PromptAgent(t.Context(), "w", "x")
			var apiErr *herdr.APIError
			require.ErrorAs(t, err, &apiErr)
			assert.Equal(t, code, apiErr.Code)
			assert.NotErrorIs(t, err, herdr.ErrNotSent)
		})
	}
}

// TestPromptAgentBadResults: an answer that is not agent_prompted with an
// agent is a protocol error, not a delivery.
func TestPromptAgentBadResults(t *testing.T) {
	t.Parallel()

	tests := map[string]map[string]any{
		"another type": {"type": "agent_info", "agent": map[string]any{"pane_id": "w2:p4"}},
		"no agent":     {"type": "agent_prompted"},
		"no pane id":   {"type": "agent_prompted", "agent": map[string]any{"name": "w"}},
	}
	for name, reply := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := herdrtest.Start(t, func(herdrtest.Request) herdrtest.Reply { return herdrtest.Reply{Result: reply} })

			_, err := herdr.Client{SocketPath: srv.Path}.PromptAgent(t.Context(), "w", "x")
			require.ErrorIs(t, err, herdr.ErrProtocol)
			assert.NotErrorIs(t, err, herdr.ErrNotSent)
		})
	}
}

// TestPromptAgentNotSent: a call that wrote nothing says so (ErrNotSent), so
// a message can be tried again; one that wrote its request never does, even
// when no answer came.
func TestPromptAgentNotSent(t *testing.T) {
	t.Parallel()

	refused := errors.New("not our server")
	t.Run("no socket to dial", func(t *testing.T) {
		t.Parallel()
		_, err := herdr.Client{SocketPath: filepath.Join(t.TempDir(), "none.sock")}.PromptAgent(t.Context(), "w", "x")
		require.ErrorIs(t, err, herdr.ErrNotSent)
		assert.ErrorIs(t, err, herdr.ErrUnavailable, "a failed dial is still herdr unavailable")
	})
	t.Run("a file where the socket should be", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "not.sock")
		require.NoError(t, os.WriteFile(path, nil, 0o600))
		_, err := herdr.Client{SocketPath: path}.PromptAgent(t.Context(), "w", "x")
		require.ErrorIs(t, err, herdr.ErrNotSent)
	})
	t.Run("a FIFO where the socket should be", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "fifo.sock")
		require.NoError(t, syscall.Mkfifo(path, 0o600))
		_, err := herdr.Client{SocketPath: path}.PromptAgent(t.Context(), "w", "x")
		require.ErrorIs(t, err, herdr.ErrNotSent)
	})
	for name, deny := range map[string]func(sock string) string{
		"a socket it may not write": func(sock string) string { return sock },
		"a parent it cannot search": filepath.Dir,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := herdrtest.Start(t, func(herdrtest.Request) herdrtest.Reply { return herdrtest.Reply{Result: prompted("w2:p4", "w")} })
			denied := deny(srv.Path)
			info, err := os.Stat(denied)
			require.NoError(t, err)
			require.NoError(t, os.Chmod(denied, 0))
			t.Cleanup(func() { _ = os.Chmod(denied, info.Mode().Perm()) })

			_, err = herdr.Client{SocketPath: srv.Path}.PromptAgent(t.Context(), "w", "x")
			require.ErrorIs(t, err, herdr.ErrNotSent)
			assert.ErrorIs(t, err, fs.ErrPermission)
			assert.Empty(t, srv.Requests(), "nothing reached herdr")
		})
	}
	t.Run("the Dialed check refused the server", func(t *testing.T) {
		t.Parallel()
		srv := herdrtest.Start(t, func(herdrtest.Request) herdrtest.Reply { return herdrtest.Reply{Result: prompted("w2:p4", "w")} })

		_, err := herdr.Client{SocketPath: srv.Path, Dialed: func() error { return refused }}.PromptAgent(t.Context(), "w", "x")
		require.ErrorIs(t, err, herdr.ErrNotSent)
		assert.ErrorIs(t, err, refused)
		assert.Empty(t, srv.Requests(), "nothing reached herdr")
	})
	t.Run("no answer before the timeout", func(t *testing.T) {
		t.Parallel()
		release := make(chan struct{})
		srv := herdrtest.Start(t, func(herdrtest.Request) herdrtest.Reply {
			<-release
			return herdrtest.Reply{Result: prompted("w2:p4", "w")}
		})
		t.Cleanup(func() { close(release) })

		_, err := herdr.Client{SocketPath: srv.Path, Timeout: 100 * time.Millisecond}.PromptAgent(t.Context(), "w", "x")
		require.Error(t, err)
		assert.NotErrorIs(t, err, herdr.ErrNotSent, "the request was written: it may have been typed")
	})
	t.Run("a context already done", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := herdr.Client{SocketPath: filepath.Join(t.TempDir(), "none.sock")}.PromptAgent(ctx, "w", "x")
		require.ErrorIs(t, err, herdr.ErrNotSent)
	})
}
