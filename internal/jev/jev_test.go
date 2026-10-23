package jev_test

import (
	"context"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/jev"
)

// testKey stands in for the user's key; no test reaches TypeSafe.
const testKey = "k-for-tests"

const answer = `{"model":"jev-1.13.0","answers":{"complexity":{"type":"choice","choice":"bounded","confidence":0.9,"probabilities":{"bounded":0.93,"demanding":0.05,"unknown":0.02}},"information_sufficient":{"type":"noul","noul":0.97}},"usage":{"input_tokens":120,"output_tokens":12}}`

// server answers every request with h and counts them.
func server(t *testing.T, h http.HandlerFunc) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// TestAskPostsTheBodyWithTheKey: the body goes out as given, once, with the
// key as the Bearer token, and the answer comes back by question.
func TestAskPostsTheBodyWithTheKey(t *testing.T) {
	t.Parallel()
	body := []byte(`{"model":"jev-1.13.0","state":{},"questions":{}}`)
	var got *http.Request
	var sent []byte
	srv, hits := server(t, func(w http.ResponseWriter, r *http.Request) {
		got = r
		sent, _ = io.ReadAll(r.Body)
		_, _ = io.WriteString(w, answer)
	})

	a, err := jev.Client{URL: srv.URL + "/v1/systemone", Key: testKey}.Ask(t.Context(), body, time.Second)
	require.NoError(t, err)
	// Close waits for the handler, so what it recorded is read after it.
	srv.Close()
	assert.Equal(t, int32(1), hits.Load())
	assert.Equal(t, http.MethodPost, got.Method)
	assert.Equal(t, "/v1/systemone", got.URL.Path)
	assert.Equal(t, "Bearer "+testKey, got.Header.Get("Authorization"))
	assert.Equal(t, "application/json", got.Header.Get("Content-Type"))
	assert.Equal(t, body, sent)
	assert.Equal(t, "jev-1.13.0", a.Model)
	assert.ElementsMatch(t, []string{"complexity", "information_sufficient"}, slices.Collect(maps.Keys(a.Answers)))
	assert.JSONEq(t, `{"type":"noul","noul":0.97}`, string(a.Answers["information_sufficient"]))
}

// roundTrip is a transport that answers without a network.
type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestAskDefaultsToSystemOne: with no URL the request goes to System One's
// endpoint (here caught before any network).
func TestAskDefaultsToSystemOne(t *testing.T) {
	t.Parallel()
	var url string
	h := &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		url = r.URL.String()
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(answer)), Header: http.Header{}}, nil
	})}
	_, err := jev.Client{Key: testKey, HTTP: h}.Ask(t.Context(), []byte(`{}`), time.Second)
	require.NoError(t, err)
	assert.Equal(t, "https://api.typesafe.ai/v1/systemone", url)
	assert.Equal(t, jev.Endpoint, url)
}

// TestAskRefusesABadKey: no key, or one that cannot be a Bearer token, is
// refused before anything is sent.
func TestAskRefusesABadKey(t *testing.T) {
	t.Parallel()
	srv, hits := server(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, answer) })
	for name, key := range map[string]string{"empty": "", "a space": "k k", "a newline": "k\nX-Other: 1", "non-ASCII": "kéy", "a DEL": "k\x7f"} {
		_, err := jev.Client{URL: srv.URL, Key: key}.Ask(t.Context(), []byte(`{}`), time.Second)
		require.ErrorIs(t, err, jev.ErrNoKey, name)
		if key != "" {
			assert.NotContains(t, err.Error(), key, name)
		}
	}
	assert.Zero(t, hits.Load(), "nothing is sent")
}

// TestAskRefusals: a status other than 200 is ErrStatus with its code, asked
// once, never retried; the error holds neither the key nor TypeSafe's body,
// which can echo the request. A redirect is a status, never followed.
func TestAskRefusals(t *testing.T) {
	t.Parallel()
	elsewhere, elsewhereHits := server(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, answer) })
	for _, code := range []int{http.StatusUnauthorized, http.StatusUnprocessableEntity, http.StatusTooManyRequests, http.StatusInternalServerError, 529, http.StatusTemporaryRedirect} {
		srv, hits := server(t, func(w http.ResponseWriter, r *http.Request) {
			if code == http.StatusTemporaryRedirect {
				http.Redirect(w, r, elsewhere.URL, code)
				return
			}
			w.WriteHeader(code)
			_, _ = io.WriteString(w, `{"detail":[{"loc":["body","state"],"msg":"bad","type":"x","input":"the secret plan"}]}`)
		})
		_, err := jev.Client{URL: srv.URL, Key: testKey}.Ask(t.Context(), []byte(`{"state":"the secret plan"}`), time.Second)
		require.ErrorIs(t, err, jev.ErrStatus, "%d", code)
		assert.Contains(t, err.Error(), "HTTP "+strconv.Itoa(code))
		assert.NotContains(t, err.Error(), testKey)
		assert.NotContains(t, err.Error(), "secret plan")
		assert.Equal(t, int32(1), hits.Load(), "asked once: %d", code)
	}
	assert.Zero(t, elsewhereHits.Load(), "the redirect is never followed, so the key stays where it was sent")
}

// TestAskNeverFollowsARedirect: whatever client the caller gives, even one
// that follows redirects, a redirect is answered as its status, so the key
// never goes to another URL.
func TestAskNeverFollowsARedirect(t *testing.T) {
	t.Parallel()
	elsewhere, elsewhereHits := server(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, answer) })
	srv, _ := server(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, elsewhere.URL, http.StatusFound) })
	follows := &http.Client{Timeout: time.Minute}
	_, err := jev.Client{URL: srv.URL, Key: testKey, HTTP: follows}.Ask(t.Context(), []byte(`{}`), time.Second)
	require.ErrorIs(t, err, jev.ErrStatus)
	assert.Contains(t, err.Error(), "HTTP 302")
	assert.Zero(t, elsewhereHits.Load())
	assert.Nil(t, follows.CheckRedirect, "the caller's client is left as it was")
}

// TestAskMalformedAnswers: a 200 that is not an answer is ErrAnswer.
func TestAskMalformedAnswers(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]string{
		"not JSON":       "<html>busy</html>",
		"no model":       `{"answers":{"complexity":{}}}`,
		"no answers":     `{"model":"jev-1.13.0"}`,
		"empty answers":  `{"model":"jev-1.13.0","answers":{}}`,
		"answers a list": `{"model":"jev-1.13.0","answers":[]}`,
		"over 64 KiB":    `{"model":"jev-1.13.0","answers":{"complexity":"` + strings.Repeat("x", 64<<10) + `"}}`,
		// Cut at 64 KiB this would still parse: the bound, not the parse,
		// refuses it.
		"an answer padded past 64 KiB": answer + strings.Repeat(" ", 64<<10),
	} {
		srv, _ := server(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) })
		_, err := jev.Client{URL: srv.URL, Key: testKey}.Ask(t.Context(), []byte(`{}`), time.Second)
		require.ErrorIs(t, err, jev.ErrAnswer, name)
	}
}

// TestAskEndsAtItsTimeout: a TypeSafe that holds the call past the timeout
// leaves no answer (ErrCall, deadline exceeded), asked once; so does the
// caller's context ending, and a server that cannot be reached.
func TestAskEndsAtItsTimeout(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	srv, hits := server(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
		_, _ = io.WriteString(w, answer)
	})
	defer close(release)
	start := time.Now()
	_, err := jev.Client{URL: srv.URL, Key: testKey}.Ask(t.Context(), []byte(`{}`), 100*time.Millisecond)
	require.ErrorIs(t, err, jev.ErrCall)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(start), 5*time.Second)
	// At most once: on a loaded machine the timeout can end the call before
	// the server's handler has run at all.
	assert.LessOrEqual(t, hits.Load(), int32(1), "never retried")

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = jev.Client{URL: srv.URL, Key: testKey}.Ask(ctx, []byte(`{}`), time.Second)
	require.ErrorIs(t, err, jev.ErrCall)
	require.ErrorIs(t, err, context.Canceled)

	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	_, err = jev.Client{URL: gone.URL, Key: testKey}.Ask(t.Context(), []byte(`{}`), time.Second)
	require.ErrorIs(t, err, jev.ErrCall)
	assert.NotContains(t, err.Error(), testKey)
}

// TestAskNeverRetriesADroppedConnection: the rules' jev_retries is 0, and a
// request that gets no answer at all is not sent again. The server hangs up
// on every request and counts them; TestAskRefusals' counts cover only
// answered statuses, and a timeout cannot show a second send (the retry would
// share the ended context).
func TestAskNeverRetriesADroppedConnection(t *testing.T) {
	t.Parallel()
	srv, hits := server(t, func(w http.ResponseWriter, _ *http.Request) {
		c, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = c.Close()
		}
	})
	_, err := jev.Client{URL: srv.URL, Key: testKey}.Ask(t.Context(), []byte(`{}`), time.Second)
	require.ErrorIs(t, err, jev.ErrCall)
	srv.Close()
	assert.Equal(t, int32(1), hits.Load(), "one POST, whatever the failure")
}

// TestAskReadsAnAnswerOfExactlyTheLimit: an answer of 64 KiB is read, one
// byte more is not.
func TestAskReadsAnAnswerOfExactlyTheLimit(t *testing.T) {
	t.Parallel()
	pad := func(n int) string {
		head, tail := `{"model":"jev-1.13.0","answers":{"a":1},"pad":"`, `"}`
		return head + strings.Repeat("x", n-len(head)-len(tail)) + tail
	}
	for n, ok := range map[int]bool{64 << 10: true, 64<<10 + 1: false} {
		srv, _ := server(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, pad(n)) })
		_, err := jev.Client{URL: srv.URL, Key: testKey}.Ask(t.Context(), []byte(`{}`), time.Second)
		if ok {
			require.NoError(t, err, "%d bytes", n)
		} else {
			require.ErrorIs(t, err, jev.ErrAnswer, "%d bytes", n)
		}
	}
}
