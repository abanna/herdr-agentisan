package jev_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/jev"
)

// keyEq starts the key's line in a test env file. Built from the constant,
// as trail's tests do, so no line in the source reads as a key assigned.
const keyEq = jev.KeyVar + "="

// envFile writes an env file holding body and returns its path.
func envFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "typesafe.env")
	require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
	return p
}

// TestReadKey: TYPESAFE_API_KEY from the env file, and only it; a missing
// file, a missing or empty variable and a key that cannot be a Bearer token
// are ErrNoKey, and no error repeats a value of the file.
func TestReadKey(t *testing.T) {
	t.Parallel()
	good := envFile(t, "# TypeSafe\nOTHER_SECRET=s3cr3t-other\nexport "+keyEq+"'"+testKey+"'\n")
	key, err := jev.ReadKey(t.Context(), good, time.Second)
	require.NoError(t, err)
	assert.Equal(t, testKey, key)
	require.NoError(t, jev.CheckKeyFile(t.Context(), good))
	key, err = (&jev.Keys{}).Key(t.Context(), good)
	require.NoError(t, err, "the zero Keys reads the file itself")
	assert.Equal(t, testKey, key)
	assert.Greater(t, jev.NoBound, 100*365*24*time.Hour, "a read Keys starts is never abandoned mid-open")

	for name, path := range map[string]string{
		"no file":        filepath.Join(t.TempDir(), "absent.env"),
		"no variable":    envFile(t, "OTHER_SECRET=s3cr3t-other\n"),
		"empty":          envFile(t, keyEq+"\nOTHER_SECRET=s3cr3t-other\n"),
		"a space in it":  envFile(t, keyEq+"'s3cr3t key'\nOTHER_SECRET=s3cr3t-other\n"),
		"a quote unshut": envFile(t, keyEq+"'s3cr3t\nOTHER_SECRET=s3cr3t-other\n"),
		"a directory":    t.TempDir(),
	} {
		_, err := jev.ReadKey(t.Context(), path, time.Second)
		require.ErrorIs(t, err, jev.ErrNoKey, name)
		assert.NotContains(t, err.Error(), "s3cr3t", name)
		assert.ErrorIs(t, jev.CheckKeyFile(t.Context(), path), jev.ErrNoKey, name)
	}
}

// counted is a Keys reader over the real env file that counts its reads.
func counted(n *atomic.Int32) func(context.Context, string) (string, error) {
	return func(ctx context.Context, path string) (string, error) {
		n.Add(1)
		return jev.ReadKey(ctx, path, time.Minute)
	}
}

// TestKeysKeepAKeyForItsTTL: a key read is kept KeyTTL, then read again; a
// key from another file is never the one kept; Forget drops it.
func TestKeysKeepAKeyForItsTTL(t *testing.T) {
	t.Parallel()
	var reads atomic.Int32
	var mu sync.Mutex
	clock := time.Date(2026, 10, 23, 4, 0, 0, 0, time.UTC)
	advance := func(d time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		clock = clock.Add(d)
	}
	keys := &jev.Keys{Read: counted(&reads), Now: func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return clock
	}}
	a := envFile(t, keyEq+"k-one\n")
	b := envFile(t, keyEq+"k-other\n")
	ask := func(path string) string {
		t.Helper()
		key, err := keys.Key(t.Context(), path)
		require.NoError(t, err)
		return key
	}

	assert.Equal(t, "k-one", ask(a))
	require.NoError(t, os.WriteFile(a, []byte(keyEq+"k-two\n"), 0o600))
	advance(jev.KeyTTL - time.Nanosecond)
	assert.Equal(t, "k-one", ask(a), "kept until KeyTTL")
	assert.Equal(t, int32(1), reads.Load())
	advance(time.Nanosecond)
	assert.Equal(t, "k-two", ask(a), "read again at KeyTTL")
	assert.Equal(t, int32(2), reads.Load())

	assert.Equal(t, "k-other", ask(b), "another file is read, never given a's key")
	assert.Equal(t, "k-two", ask(a), "b's key replaced a's")
	assert.Equal(t, int32(4), reads.Load())

	keys.Forget(b)
	assert.Equal(t, "k-two", ask(a), "forgetting b leaves a's kept key")
	assert.Equal(t, int32(4), reads.Load())
	keys.Forget(a)
	assert.Equal(t, "k-two", ask(a))
	assert.Equal(t, int32(5), reads.Load(), "a forgotten key is read again")
}

// TestKeysKeepNoFailure: a failed read is ErrNoKey for the ask that waited
// on it, and the next ask reads again.
func TestKeysKeepNoFailure(t *testing.T) {
	t.Parallel()
	var reads atomic.Int32
	path := filepath.Join(t.TempDir(), "typesafe.env")
	keys := &jev.Keys{Read: counted(&reads)}
	_, err := keys.Key(t.Context(), path)
	require.ErrorIs(t, err, jev.ErrNoKey)
	require.NoError(t, os.WriteFile(path, []byte(keyEq+testKey+"\n"), 0o600))
	key, err := keys.Key(t.Context(), path)
	require.NoError(t, err)
	assert.Equal(t, testKey, key)
	assert.Equal(t, int32(2), reads.Load())

	for name, read := range map[string]func(context.Context, string) (string, error){
		"an error of its own": func(context.Context, string) (string, error) { return "", errors.New("the pipe broke") },
		"an empty key":        func(context.Context, string) (string, error) { return "", nil },
		"not a token":         func(context.Context, string) (string, error) { return "s3cr3t key", nil },
	} {
		key, err := (&jev.Keys{Read: read}).Key(t.Context(), path)
		require.ErrorIs(t, err, jev.ErrNoKey, name)
		assert.Empty(t, key, "%s: no key beside an error", name)
		assert.NotContains(t, err.Error(), "s3cr3t", name)
	}
}

// TestKeysShareOneRead: asks for one file while it is read all wait on that
// read, and all get its key.
func TestKeysShareOneRead(t *testing.T) {
	t.Parallel()
	var reads atomic.Int32
	release := make(chan struct{})
	keys := &jev.Keys{Wait: time.Minute, Read: func(context.Context, string) (string, error) {
		reads.Add(1)
		<-release
		return testKey, nil
	}}
	const asks = 8
	got := make(chan string, asks)
	var started sync.WaitGroup
	for range asks {
		started.Go(func() {
			key, err := keys.Key(context.Background(), "/run/typesafe.env")
			assert.NoError(t, err)
			got <- key
		})
	}
	// Every ask has joined the read once none has an answer and one read
	// is counted; give the scheduler a moment to bring the rest to it.
	require.Eventually(t, func() bool { return reads.Load() == 1 }, 5*time.Second, time.Millisecond)
	time.Sleep(50 * time.Millisecond)
	close(release)
	started.Wait()
	close(got)
	for key := range got {
		assert.Equal(t, testKey, key)
	}
	assert.Equal(t, int32(1), reads.Load())
}

// TestKeyEndsWithTheAsk: an ask whose context ends first is ErrNoKey with
// the context's error, and its read, which that end does not reach, goes
// on for the next ask.
func TestKeyEndsWithTheAsk(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	var reads atomic.Int32
	keys := &jev.Keys{Wait: time.Minute, Read: func(ctx context.Context, _ string) (string, error) {
		reads.Add(1)
		select {
		case <-release:
			return testKey, nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := keys.Key(ctx, "/run/typesafe.env")
	require.ErrorIs(t, err, jev.ErrNoKey)
	require.ErrorIs(t, err, context.Canceled)
	close(release)
	key, err := keys.Key(t.Context(), "/run/typesafe.env")
	require.NoError(t, err)
	assert.Equal(t, testKey, key)
	assert.Equal(t, int32(1), reads.Load(), "the cancelled ask's read answered the next")
}

// TestAskerForgetsARefusedKey: the key from the env file goes out as the
// Bearer token; a 401 drops it, so the next ask reads the file again, and
// any other answer keeps it. No key sends nothing.
func TestAskerForgetsARefusedKey(t *testing.T) {
	t.Parallel()
	var code atomic.Int32
	code.Store(http.StatusOK)
	srv, hits := server(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer "+testKey, r.Header.Get("Authorization"))
		if c := int(code.Load()); c != http.StatusOK {
			w.WriteHeader(c)
			return
		}
		_, _ = io.WriteString(w, answer)
	})
	var reads atomic.Int32
	asker := jev.Asker{Keys: &jev.Keys{Read: counted(&reads)}, Client: jev.Client{URL: srv.URL}}
	path := envFile(t, keyEq+testKey+"\n")

	ans, err := asker.Ask(t.Context(), path, []byte(`{}`), time.Second)
	require.NoError(t, err)
	assert.Equal(t, "jev-1.13.0", ans.Model)
	for _, c := range []int{http.StatusTooManyRequests, http.StatusUnprocessableEntity, http.StatusOK} {
		code.Store(int32(c))
		_, err = asker.Ask(t.Context(), path, []byte(`{}`), time.Second)
		assert.Equal(t, c == http.StatusOK, err == nil, c)
	}
	assert.Equal(t, int32(1), reads.Load(), "a key TypeSafe did not refuse is kept")

	code.Store(http.StatusUnauthorized)
	_, err = asker.Ask(t.Context(), path, []byte(`{}`), time.Second)
	require.ErrorIs(t, err, jev.ErrUnauthorized)
	require.ErrorIs(t, err, jev.ErrStatus)
	assert.Contains(t, err.Error(), "HTTP 401")
	code.Store(http.StatusOK)
	_, err = asker.Ask(t.Context(), path, []byte(`{}`), time.Second)
	require.NoError(t, err)
	assert.Equal(t, int32(2), reads.Load(), "the refused key was read again")
	assert.Equal(t, int32(6), hits.Load())

	_, err = asker.Ask(t.Context(), filepath.Join(t.TempDir(), "absent.env"), []byte(`{}`), time.Second)
	require.ErrorIs(t, err, jev.ErrNoKey)
	assert.Equal(t, int32(6), hits.Load(), "no key, no request")
}
