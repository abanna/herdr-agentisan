package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"

	"github.com/nerds-run/go-agents/internal/api"
	"github.com/nerds-run/go-agents/internal/notes"
)

func TestMain(m *testing.M) {
	gin.SetMode(gin.TestMode)
	m.Run()
}

// newTestServer returns a router over an empty store. token "" disables auth.
func newTestServer(t *testing.T, token string) (*gin.Engine, notes.Store) {
	t.Helper()
	store := notes.NewMemStore()
	return api.NewServer(store, zerolog.Nop(), token).Router(), store
}

func do(t *testing.T, r http.Handler, method, path, body, token string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *strings.Reader
	if body == "" {
		rdr = strings.NewReader("")
	} else {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestHealthAndReady(t *testing.T) {
	t.Parallel()
	r, _ := newTestServer(t, "")

	for _, path := range []string{"/healthz", "/readyz"} {
		w := do(t, r, http.MethodGet, path, "", "")
		assert.Equalf(t, http.StatusOK, w.Code, "path %s", path)
	}
}

func TestVersionRoute(t *testing.T) {
	t.Parallel()
	r, _ := newTestServer(t, "")

	w := do(t, r, http.MethodGet, "/version", "", "")
	require.Equal(t, http.StatusOK, w.Code)

	var got api.VersionBody
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.NotEmpty(t, got.Version)
}

func TestCreateThenGetNote(t *testing.T) {
	t.Parallel()
	r, _ := newTestServer(t, "")

	w := do(t, r, http.MethodPost, "/v1/notes", `{"title":"hello","body":"world"}`, "")
	require.Equal(t, http.StatusCreated, w.Code)

	var created notes.Note
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &created))
	assert.Equal(t, "hello", created.Title)
	assert.Equal(t, "/v1/notes/"+created.ID, w.Header().Get("Location"))

	got := do(t, r, http.MethodGet, "/v1/notes/"+created.ID, "", "")
	require.Equal(t, http.StatusOK, got.Code)

	var fetched notes.Note
	require.NoError(t, json.Unmarshal(got.Body.Bytes(), &fetched))
	assert.Equal(t, created.ID, fetched.ID)
}

func TestListEmptyReturnsJSONArrayNotNull(t *testing.T) {
	t.Parallel()
	r, _ := newTestServer(t, "")

	w := do(t, r, http.MethodGet, "/v1/notes", "", "")
	require.Equal(t, http.StatusOK, w.Code)
	// A `null` body here silently breaks clients that iterate the result.
	assert.JSONEq(t, `[]`, w.Body.String())
}

func TestGetMissingNoteIs404(t *testing.T) {
	t.Parallel()
	r, _ := newTestServer(t, "")

	w := do(t, r, http.MethodGet, "/v1/notes/missing", "", "")
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestCreateValidationIs400(t *testing.T) {
	t.Parallel()
	r, _ := newTestServer(t, "")

	tests := map[string]string{
		"empty title":      `{"title":"  ","body":"b"}`,
		"malformed json":   `{"title":`,
		"body over limit":  `{"title":"ok","body":"` + strings.Repeat("x", notes.MaxBodyLen+1) + `"}`,
		"title over limit": `{"title":"` + strings.Repeat("x", notes.MaxTitleLen+1) + `"}`,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			w := do(t, r, http.MethodPost, "/v1/notes", body, "")
			assert.Equal(t, http.StatusBadRequest, w.Code)
		})
	}
}

func TestDeleteNote(t *testing.T) {
	t.Parallel()
	r, _ := newTestServer(t, "")

	w := do(t, r, http.MethodPost, "/v1/notes", `{"title":"doomed"}`, "")
	require.Equal(t, http.StatusCreated, w.Code)
	var created notes.Note
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &created))

	del := do(t, r, http.MethodDelete, "/v1/notes/"+created.ID, "", "")
	assert.Equal(t, http.StatusNoContent, del.Code)

	again := do(t, r, http.MethodDelete, "/v1/notes/"+created.ID, "", "")
	assert.Equal(t, http.StatusNotFound, again.Code)
}

func TestTokenGuardsMutatingRoutesOnly(t *testing.T) {
	t.Parallel()
	const token = "s3cret"
	r, _ := newTestServer(t, token)

	// Reads stay open.
	assert.Equal(t, http.StatusOK, do(t, r, http.MethodGet, "/v1/notes", "", "").Code)

	// Writes without a token are rejected.
	assert.Equal(t, http.StatusUnauthorized,
		do(t, r, http.MethodPost, "/v1/notes", `{"title":"x"}`, "").Code)

	// A wrong token is rejected.
	assert.Equal(t, http.StatusUnauthorized,
		do(t, r, http.MethodPost, "/v1/notes", `{"title":"x"}`, "wrong").Code)

	// The right token is accepted.
	assert.Equal(t, http.StatusCreated,
		do(t, r, http.MethodPost, "/v1/notes", `{"title":"x"}`, token).Code)
}

func TestInternalErrorIsNotEchoedToClient(t *testing.T) {
	t.Parallel()

	// A store that fails with an error the handler does not recognise must
	// produce a generic 500 — never the underlying message.
	srv := api.NewServer(explodingStore{}, zerolog.Nop(), "")
	w := do(t, srv.Router(), http.MethodGet, "/v1/notes", "", "")

	require.Equal(t, http.StatusInternalServerError, w.Code)
	assert.NotContains(t, w.Body.String(), "database credentials")
	assert.JSONEq(t, `{"error":"internal error"}`, w.Body.String())
}

func TestReadinessFailsWhenStoreIsBroken(t *testing.T) {
	t.Parallel()

	srv := api.NewServer(explodingStore{}, zerolog.Nop(), "")
	w := do(t, srv.Router(), http.MethodGet, "/readyz", "", "")
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
}

func TestCreateRoundTripsThroughStore(t *testing.T) {
	t.Parallel()
	r, store := newTestServer(t, "")

	var buf bytes.Buffer
	require.NoError(t, json.NewEncoder(&buf).Encode(notes.Draft{Title: "via api"}))
	w := do(t, r, http.MethodPost, "/v1/notes", buf.String(), "")
	require.Equal(t, http.StatusCreated, w.Code)

	stored, err := store.List(t.Context())
	require.NoError(t, err)
	require.Len(t, stored, 1)
	assert.Equal(t, "via api", stored[0].Title)
}

// TestObserversRunInOrderAheadOfHandlers proves WithObservers is a real
// middleware slot and not a field the router ignores.
func TestObserversRunInOrderAheadOfHandlers(t *testing.T) {
	t.Parallel()

	var order []string
	mark := func(name string) gin.HandlerFunc {
		return func(c *gin.Context) { order = append(order, name); c.Next() }
	}

	r := api.NewServer(notes.NewMemStore(), zerolog.Nop(), "",
		api.WithObservers(mark("first"), mark("second")),
	).Router()

	require.Equal(t, http.StatusOK, do(t, r, http.MethodGet, "/healthz", "", "").Code)
	assert.Equal(t, []string{"first", "second"}, order)
}

// TestRequestLoggerCorrelatesWithTheActiveSpan is the half of "tracing is
// configured" that makes an incident survivable: a log line names the trace it
// belongs to, so a 500 in the logs leads straight to the span that caused it.
func TestRequestLoggerCorrelatesWithTheActiveSpan(t *testing.T) {
	t.Parallel()

	traceID, err := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	require.NoError(t, err)
	spanID, err := trace.SpanIDFromHex("00f067aa0ba902b7")
	require.NoError(t, err)

	// A synthetic span context rather than the SDK: this asserts what the
	// logger does with a span, not that the exporter works.
	withSpan := func(c *gin.Context) {
		sc := trace.NewSpanContext(trace.SpanContextConfig{
			TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled,
		})
		c.Request = c.Request.WithContext(trace.ContextWithSpanContext(c.Request.Context(), sc))
		c.Next()
	}

	var buf bytes.Buffer
	log := zerolog.New(&buf)
	r := api.NewServer(notes.NewMemStore(), log, "", api.WithObservers(withSpan)).Router()

	require.Equal(t, http.StatusOK, do(t, r, http.MethodGet, "/healthz", "", "").Code)

	var line map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &line))
	assert.Equal(t, "4bf92f3577b34da6a3ce929d0e0e4736", line["trace_id"])
	assert.Equal(t, "00f067aa0ba902b7", line["span_id"])
}

// TestRequestLoggerOmitsTraceFieldsWithoutASpan: an unwired binary must not
// emit an all-zero trace ID, which would look like a real trace and lead an
// on-call engineer to a trace that does not exist.
func TestRequestLoggerOmitsTraceFieldsWithoutASpan(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	r := api.NewServer(notes.NewMemStore(), zerolog.New(&buf), "").Router()

	require.Equal(t, http.StatusOK, do(t, r, http.MethodGet, "/healthz", "", "").Code)

	var line map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &line))
	assert.NotContains(t, line, "trace_id")
	assert.NotContains(t, line, "span_id")
}
