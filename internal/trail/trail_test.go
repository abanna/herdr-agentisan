package trail_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/trail"
)

// probeKey stands in for the read key: distinctive, so a test can assert it
// never appears where it must not.
const probeKey = "probe-read-key-7f3a"

// recorded is a real _query answer, the live check's worker trail for
// w14:p8 (2026-10-20), saved as Elasticsearch returned it, oldest first.
func recorded(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "worker-trail.json"))
	require.NoError(t, err)
	return data
}

// newestFirst is answer with its rows reversed: what Elasticsearch returns
// for WorkerQuery, which sorts newest first.
func newestFirst(t *testing.T, answer []byte) []byte {
	t.Helper()
	var doc map[string]any
	require.NoError(t, json.Unmarshal(answer, &doc))
	values, _ := doc["values"].([]any)
	slices.Reverse(values)
	out, err := json.Marshal(doc)
	require.NoError(t, err)
	return out
}

// esStandIn answers every request with status and body, and records the
// last request it saw.
func esStandIn(t *testing.T, status int, body []byte) (*httptest.Server, *http.Request, *[]byte) {
	t.Helper()
	var got http.Request
	var sent []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = *r.Clone(r.Context())
		sent, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &got, &sent
}

// TestQueryReadsTheRecordedTrail: a query is one POST to _query with the
// ES|QL in the body and the key in an ApiKey header, and the live check's
// recorded answer decodes into its columns and rows, then into the worker's
// six changes, in order, exactly as the trail holds them.
func TestQueryReadsTheRecordedTrail(t *testing.T) {
	t.Parallel()
	srv, req, sent := esStandIn(t, http.StatusOK, newestFirst(t, recorded(t)))
	c, err := trail.NewClient(srv.URL, probeKey, nil)
	require.NoError(t, err)
	q, err := trail.WorkerQuery("w14:p8", trail.MinSince*60)
	require.NoError(t, err)

	table, err := c.Query(t.Context(), q)
	require.NoError(t, err)

	assert.Equal(t, http.MethodPost, req.Method)
	assert.Equal(t, "/_query", req.URL.Path)
	assert.Equal(t, "application/json", req.Header.Get("Content-Type"))
	assert.Equal(t, "ApiKey "+probeKey, req.Header.Get("Authorization"))
	var body map[string]string
	require.NoError(t, json.Unmarshal(*sent, &body))
	assert.Equal(t, map[string]string{"query": q}, body)

	require.Len(t, table.Columns, 8)
	assert.Equal(t, trail.Column{Name: "attributes.agentisan.worker.bound", Type: "boolean"}, table.Columns[7])
	assert.False(t, table.Partial)
	changes, cut, err := trail.WorkerChanges(table)
	require.NoError(t, err)
	assert.False(t, cut)
	want := [][2]string{
		{"2026-10-20T11:29:10.415Z", "done→working"},
		{"2026-10-20T11:31:31.416Z", "working→done"},
		{"2026-10-20T11:32:07.416Z", "done→working"},
		{"2026-10-20T11:32:49.417Z", "working→done"},
		{"2026-10-20T11:38:37.597Z", "done→working"},
		{"2026-10-20T12:07:26.969Z", "working→done"},
	}
	require.Len(t, changes, len(want))
	for i, ch := range changes {
		assert.Equal(t, want[i], [2]string{ch.At, ch.From + "→" + ch.To}, "row %d", i)
		assert.Equal(t, trail.Change{At: ch.At, WorkerID: "w14:p8", PaneID: "w14:p8", From: ch.From, To: ch.To, Group: "coder"}, ch,
			"unbound, so the worker id is the pane id")
	}
}

// TestQueryErrors: each way Elasticsearch can fail maps to its sentinel,
// with what the caller needs to act on it and never the key.
func TestQueryErrors(t *testing.T) {
	t.Parallel()
	ragged := `{"columns":[{"name":"a","type":"keyword"},{"name":"b","type":"keyword"}],"values":[["x"]]}`
	tests := map[string]struct {
		status  int
		body    string
		wantErr error
		wantIn  string
	}{
		"a refused query, with its reason": {status: 400, body: `{"error":{"type":"verification_exception","reason":"Unknown column [agentisan.bound]"},"status":400}`, wantErr: trail.ErrBadQuery, wantIn: "Unknown column [agentisan.bound]"},
		"a refused query, no reason":       {status: 400, body: `oops`, wantErr: trail.ErrBadQuery, wantIn: "no reason given"},
		"a reason with control characters": {status: 400, body: `{"error":{"reason":"line 1\nline 2\u001b[31m"}}`, wantErr: trail.ErrBadQuery, wantIn: "line 1 line 2 [31m"},
		"a reason past its bound":          {status: 400, body: `{"error":{"reason":"` + strings.Repeat("é", 600) + `"}}`, wantErr: trail.ErrBadQuery, wantIn: strings.Repeat("é", 512) + "…"},
		"the key refused":                  {status: 401, body: `{}`, wantErr: trail.ErrUnauthorized, wantIn: "401"},
		"the key not allowed":              {status: 403, body: `{}`, wantErr: trail.ErrUnauthorized, wantIn: "403"},
		"a server error":                   {status: 503, body: `{}`, wantErr: trail.ErrUnavailable, wantIn: "HTTP 503"},
		"not JSON":                         {status: 200, body: `<html>`, wantErr: trail.ErrUnavailable, wantIn: "not an ES|QL result"},
		"JSON with no columns":             {status: 200, body: `{"took":1}`, wantErr: trail.ErrUnavailable, wantIn: "not an ES|QL result"},
		"a row short of its columns":       {status: 200, body: ragged, wantErr: trail.ErrUnavailable, wantIn: "row 0 has 1 values for 2 columns"},
		"an answer past 16 MiB":            {status: 200, body: `{"columns":[],"values":[["` + strings.Repeat("x", 16<<20) + `"]]}`, wantErr: trail.ErrBadQuery, wantIn: "add a LIMIT"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv, _, _ := esStandIn(t, tc.status, []byte(tc.body))
			c, err := trail.NewClient(srv.URL, probeKey, nil)
			require.NoError(t, err)
			_, err = c.Query(t.Context(), "FROM logs-agentisan.otel-* | LIMIT 1")
			require.ErrorIs(t, err, tc.wantErr)
			assert.Contains(t, err.Error(), tc.wantIn)
			assert.NotContains(t, err.Error(), probeKey)
		})
	}

	t.Run("a key with a line break", func(t *testing.T) {
		t.Parallel()
		srv, _, _ := esStandIn(t, http.StatusOK, recorded(t))
		c, err := trail.NewClient(srv.URL, probeKey+"\nX-Injected: 1", nil)
		require.NoError(t, err)
		_, err = c.Query(t.Context(), "FROM logs-agentisan.otel-*")
		require.ErrorIs(t, err, trail.ErrUnavailable, "the HTTP client refuses the header; nothing is sent")
		assert.NotContains(t, err.Error(), probeKey)
	})

	t.Run("nothing listening", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.NotFoundHandler())
		srv.Close()
		c, err := trail.NewClient(srv.URL, probeKey, nil)
		require.NoError(t, err)
		_, err = c.Query(t.Context(), "FROM logs-agentisan.otel-*")
		require.ErrorIs(t, err, trail.ErrUnavailable)
		assert.NotContains(t, err.Error(), probeKey)
	})
}

// TestNewClient: the URL is http(s)://host[:port] and nothing more, since
// a path or a user would bend where the key goes; neither the URL nor the
// key is repeated in the error.
func TestNewClient(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{"http://172.25.0.2:32698", "https://es.example:9200/", "http://[::1]:9200", "https://es.example"} {
		c, err := trail.NewClient(ok, probeKey, nil)
		require.NoError(t, err, ok)
		assert.Equal(t, "trail.Client("+strings.TrimSuffix(ok, "/")+")", c.String())
	}
	for name, bad := range map[string]string{
		"no scheme": "es.example:9200", "ftp": "ftp://es.example", "a user": "http://elastic:s3cret-pass@es:9200",
		"a path": "http://es:9200/proxy", "a query": "http://es:9200?x=1", "a fragment": "http://es:9200#f",
		"no host": "http://", "empty": "", "opaque": "http:es:9200", "white space": "http://es example:9200",
	} {
		_, err := trail.NewClient(bad, probeKey, nil)
		require.ErrorIs(t, err, trail.ErrInvalid, name)
		assert.Contains(t, err.Error(), trail.URLVar, name)
		if bad != "" {
			assert.NotContains(t, err.Error(), bad, name)
		}
		assert.NotContains(t, err.Error(), "s3cret", name)
	}
	_, err := trail.NewClient("http://es:9200", "", nil)
	require.ErrorIs(t, err, trail.ErrNotConfigured)
}

// TestNewClientNeverUsesAProxy: the key goes to the env file's URL and
// nowhere else, so the client's transport has no proxy: HTTP_PROXY in the
// environment would otherwise read it in clear over http.
func TestNewClientNeverUsesAProxy(t *testing.T) {
	t.Parallel()
	c, err := trail.NewClient("http://172.25.0.2:32698", probeKey, nil)
	require.NoError(t, err)
	tr, ok := trail.TransportOf(c).(*http.Transport)
	require.True(t, ok, "its own transport, not http.DefaultTransport")
	assert.Nil(t, tr.Proxy)
	assert.NotSame(t, http.DefaultTransport, tr)
}

// TestQueryFollowsNoRedirect: Go keeps Authorization on a redirect to the
// same host name, whatever the port or scheme, so an answer that redirects
// is an error, and its target is never asked.
func TestQueryFollowsNoRedirect(t *testing.T) {
	t.Parallel()
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the redirect was followed")
	}))
	t.Cleanup(target.Close)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/_query", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(srv.Close)
	c, err := trail.NewClient(srv.URL, probeKey, nil)
	require.NoError(t, err)
	_, err = c.Query(t.Context(), "FROM logs-agentisan.otel-*")
	require.ErrorIs(t, err, trail.ErrUnavailable)
	assert.Contains(t, err.Error(), "HTTP 307, a redirect")
}

// TestClientNeverPrintsTheKey: a client formatted by mistake, in any verb,
// shows its URL and never its key.
func TestClientNeverPrintsTheKey(t *testing.T) {
	t.Parallel()
	c, err := trail.NewClient("http://es:9200", probeKey, nil)
	require.NoError(t, err)
	for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
		out := fmt.Sprintf(verb, c)
		assert.NotContains(t, out, probeKey, verb)
		assert.Contains(t, out, "http://es:9200", verb)
	}
}
