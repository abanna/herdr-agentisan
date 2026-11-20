package daemon_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/daemon"
	"github.com/abanna/herdr-agentisan/internal/store"
	"github.com/abanna/herdr-agentisan/internal/workforce"
)

// keyOf is the retry key the daemon's tests answer with.
var keyOf = strings.Repeat("3f9c0d5e", 4)

// exchange sends line to the daemon socket at sock and returns its answer's
// line as it was written, so a test sees every key of the error body.
func exchange(t *testing.T, sock, line string) string {
	t.Helper()
	conn, err := (&net.Dialer{}).DialContext(t.Context(), "unix", sock)
	require.NoError(t, err)
	defer conn.Close() //nolint:errcheck // one-shot connection
	require.NoError(t, conn.SetDeadline(time.Now().Add(ready)))
	_, err = conn.Write([]byte(line + "\n"))
	require.NoError(t, err)
	answer, err := bufio.NewReader(conn).ReadString('\n')
	require.NoError(t, err)
	return strings.TrimSuffix(answer, "\n")
}

// TestEveryWorkforceCodeFitsASpawnRecord (B8): every code a workforce op
// answers, and timeout, failed and spawn_unsettled, is one a spawn record
// stores, so FinishSpawn never fails on a code its grammar refuses.
func TestEveryWorkforceCodeFitsASpawnRecord(t *testing.T) {
	t.Parallel()
	codes := map[string]bool{"timeout": true, "failed": true, "spawn_unsettled": true}
	for _, row := range daemon.WorkforceCodeRows() {
		codes[row.Code] = true
	}
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	at := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	n := 0
	for code := range codes {
		n++
		key := fmt.Sprintf("%032x", n)
		require.NoError(t, st.BeginSpawn(t.Context(), store.SpawnKey{Key: key, Project: "ha", Group: "coder", Count: 1}, []string{"ha-coder-1"}, at))
		require.NoError(t, st.FinishSpawn(t.Context(), key, store.SpawnAnswer{Code: code, Message: "m"}, at), code)
		rec, err := st.SpawnRecord(t.Context(), key)
		require.NoError(t, err)
		assert.Equal(t, code, rec.Answer.Code)
	}
}

// TestSpawnRefusalIsTheServersRule (B9): refusalOf is the rule server.run
// applies: an op whose own budget ended while a call failed answers
// timeout, with the server's own message, and any other code stands with
// the error's message. server.run answers a *workforce.RefusalError's code and
// message as they are, past its budget too, since the rule already ran.
func TestSpawnRefusalIsTheServersRule(t *testing.T) {
	t.Parallel()
	expired, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	cancelled, stopNow := context.WithCancel(t.Context())
	stopNow()
	cause := fmt.Errorf("list panes: %w", context.DeadlineExceeded)
	codeOf := func(code string) func(error) string { return func(error) string { return code } }

	for name, tc := range map[string]struct {
		ctx       context.Context
		code      string
		want, msg string
	}{
		"a call failed past the budget":   {ctx: expired, code: "herdr_failed", want: "timeout", msg: "worker.spawn ran past its 1m40s budget: list panes: context deadline exceeded"},
		"a store failed past the budget":  {ctx: expired, code: "store_failed", want: "timeout", msg: "worker.spawn ran past its 1m40s budget: list panes: context deadline exceeded"},
		"no code past the budget":         {ctx: expired, code: "failed", want: "timeout", msg: "worker.spawn ran past its 1m40s budget: list panes: context deadline exceeded"},
		"a code that says more":           {ctx: expired, code: "herdr_gone", want: "herdr_gone", msg: "list panes: context deadline exceeded"},
		"a cancel, not a deadline":        {ctx: cancelled, code: "herdr_failed", want: "herdr_failed", msg: "list panes: context deadline exceeded"},
		"a call failed inside the budget": {ctx: t.Context(), code: "herdr_failed", want: "herdr_failed", msg: "list panes: context deadline exceeded"},
	} {
		code, msg := daemon.RefusalOf(tc.ctx, daemon.OpWorkerSpawn, daemon.SpawnBudget, codeOf(tc.code), cause)
		assert.Equal(t, tc.want, code, name)
		assert.Equal(t, tc.msg, msg, name)
	}

	sock := filepath.Join(shortDir(t), "d.sock")
	budget := 200 * time.Millisecond
	stop, err := daemon.ServeOp(t.Context(), sock, "op", "herdr_failed", budget, func(ctx context.Context) (any, error) {
		<-ctx.Done()
		return nil, fmt.Errorf("list panes: %w", ctx.Err())
	})
	require.NoError(t, err)
	t.Cleanup(stop)
	_, err = daemon.CallWith(t.Context(), sock, "op", []byte(`{}`), ready)
	var re *daemon.RequestError
	require.ErrorAs(t, err, &re)
	wantCode, wantMsg := daemon.RefusalOf(expired, "op", budget, codeOf("herdr_failed"), cause)
	assert.Equal(t, daemon.RequestError{Code: wantCode, Message: wantMsg}, *re, "the server answers by refusalOf")

	refusing := filepath.Join(shortDir(t), "d.sock")
	stop, err = daemon.ServeOp(t.Context(), refusing, "op", "failed", budget, func(ctx context.Context) (any, error) {
		<-ctx.Done()
		return nil, &workforce.RefusalError{Code: "store_failed", Message: "finish a spawn: as first answered", RetryKey: keyOf, KeyState: "spent", Err: store.ErrStore}
	})
	require.NoError(t, err)
	t.Cleanup(stop)
	_, err = daemon.CallWith(t.Context(), refusing, "op", []byte(`{}`), ready)
	require.ErrorAs(t, err, &re)
	assert.Equal(t, daemon.RequestError{Code: "store_failed", Message: "finish a spawn: as first answered"}, *re,
		"a refusal's code and message, never the op's code or a timeout")
}

// TestServerCopiesARefusalsKeyState (B12): server.run puts a
// *workforce.RefusalError's retry key and key state in the error body, wrapped
// or not; a plain error and a refusal before any op ran carry neither.
func TestServerCopiesARefusalsKeyState(t *testing.T) {
	t.Parallel()
	sock := filepath.Join(shortDir(t), "d.sock")
	var (
		mu     sync.Mutex
		answer error
	)
	stop, err := daemon.ServeOp(t.Context(), sock, "op", "failed", time.Minute, func(context.Context) (any, error) {
		mu.Lock()
		defer mu.Unlock()
		return nil, answer
	})
	require.NoError(t, err)
	t.Cleanup(stop)
	refused := &workforce.RefusalError{Code: "slot_cap", Message: "coder: 3 of 3 slots", RetryKey: keyOf, KeyState: "free", Err: workforce.ErrSlotCap}

	for _, tc := range []struct {
		name, line string
		answer     error
		want       string
	}{
		{
			"a refusal", `{"v":1,"op":"op","args":{}}`, refused,
			`{"ok":false,"error":{"code":"slot_cap","message":"coder: 3 of 3 slots","retry_key":"` + keyOf + `","key_state":"free"}}`,
		},
		{
			"a wrapped refusal", `{"v":1,"op":"op","args":{}}`, fmt.Errorf("spawn: %w", refused),
			`{"ok":false,"error":{"code":"slot_cap","message":"coder: 3 of 3 slots","retry_key":"` + keyOf + `","key_state":"free"}}`,
		},
		{"a plain error", `{"v":1,"op":"op","args":{}}`, errors.New("plain"), `{"ok":false,"error":{"code":"failed","message":"plain"}}`},
		{"an unknown op", `{"v":1,"op":"nope","args":{}}`, refused, `{"ok":false,"error":{"code":"unknown_op","message":"unknown op \"nope\""}}`},
		{
			"another version", `{"v":2,"op":"op","args":{}}`, refused,
			`{"ok":false,"error":{"code":"unsupported_version","message":"request version 2, this daemon speaks 1"}}`,
		},
	} {
		mu.Lock()
		answer = tc.answer
		mu.Unlock()
		assert.Equal(t, tc.want, exchange(t, sock, tc.line), tc.name)
	}
}
