//go:build integration

// Package api_test's integration suite exercises the REST API as a client
// would: a real http.Server on a real port, over the loopback network, with
// the real FileStore writing to disk.
//
// The unit suite in api_test.go drives the gin engine in-process with
// httptest and an in-memory store. That is fast and covers handler logic, but
// it never proves the pieces fit: process wiring, JSON over the wire, the
// Location header a client follows, persistence across requests, and graceful
// shutdown are all invisible to it.
//
// Run with:  task test:integration     (go test -tags=integration ./...)
package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nerds-run/go-agents/internal/api"
	"github.com/nerds-run/go-agents/internal/notes"
)

const testToken = "integration-token"

// liveServer starts the API on an ephemeral port backed by a real file store.
// It returns the base URL, the store path, and a client BOUND TO THIS SERVER.
//
// The client is per-server on purpose. Sharing http.DefaultClient across tests
// made shutdown flaky in CI: its transport pools connections keyed by
// host:port, these listeners take ephemeral ports the OS reuses, and a pooled
// connection to a previous test's port can be handed to a new server on that
// same port. The server then counts a half-used connection as active and
// Shutdown — which waits for connections to return to IDLE — blocks until its
// deadline. Keep-alives are disabled so there is no pool to go stale, and idle
// connections are closed before shutdown runs.
func liveServer(t *testing.T) (string, string, *http.Client) {
	t.Helper()

	storePath := filepath.Join(t.TempDir(), "notes.json")
	srv := api.NewServer(notes.NewFileStore(storePath), zerolog.Nop(), testToken)

	// Port 0: let the kernel pick, so parallel runs cannot collide.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	httpSrv := &http.Server{Handler: srv.Router(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("serve: %v", err)
		}
	}()

	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			// No pooling: every request gets a fresh connection, so no
			// connection can outlive this server or be reused against the
			// next one to land on this port.
			DisableKeepAlives: true,
		},
	}

	// Registered FIRST, so LIFO cleanup runs it LAST — after the client below
	// has dropped every connection.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		assert.NoError(t, httpSrv.Shutdown(ctx),
			"server did not drain: a connection never returned to idle")
	})
	t.Cleanup(client.CloseIdleConnections)

	base := "http://" + ln.Addr().String()
	waitReady(t, base, client)
	return base, storePath, client
}

// waitReady polls /readyz until the listener answers, so a slow start does
// not look like a test failure.
func waitReady(t *testing.T, base string, client *http.Client) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get(base + "/readyz") //nolint:noctx // short-lived readiness poll
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("server at %s never became ready", base)
}

func doHTTP(t *testing.T, client *http.Client, method, url, body, token string) (*http.Response, []byte) {
	t.Helper()

	var rdr io.Reader
	if body != "" {
		rdr = bytes.NewBufferString(body)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, url, rdr)
	require.NoError(t, err)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := client.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, raw
}

// TestFullNoteLifecycleOverTheWire is the journey a real client makes:
// create, follow the Location header, list, delete, confirm gone.
func TestFullNoteLifecycleOverTheWire(t *testing.T) {
	base, _, client := liveServer(t)

	resp, raw := doHTTP(t, client, http.MethodPost, base+"/v1/notes",
		`{"title":"integration","body":"over the wire"}`, testToken)
	require.Equal(t, http.StatusCreated, resp.StatusCode, "body: %s", raw)

	var created notes.Note
	require.NoError(t, json.Unmarshal(raw, &created))

	// A client follows Location rather than rebuilding the URL itself.
	loc := resp.Header.Get("Location")
	require.NotEmpty(t, loc, "201 must carry a Location header")

	resp, raw = doHTTP(t, client, http.MethodGet, base+loc, "", "")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var fetched notes.Note
	require.NoError(t, json.Unmarshal(raw, &fetched))
	assert.Equal(t, created.ID, fetched.ID)
	assert.Equal(t, "integration", fetched.Title)

	resp, raw = doHTTP(t, client, http.MethodGet, base+"/v1/notes", "", "")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var list []notes.Note
	require.NoError(t, json.Unmarshal(raw, &list))
	require.Len(t, list, 1)

	resp, _ = doHTTP(t, client, http.MethodDelete, base+"/v1/notes/"+created.ID, "", testToken)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)

	resp, _ = doHTTP(t, client, http.MethodGet, base+"/v1/notes/"+created.ID, "", "")
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// TestPersistenceSurvivesAcrossRequests proves the FileStore is actually
// wired in — an in-memory store would pass every handler test and still lose
// this, because the assertion is about durability, not response shape.
func TestPersistenceSurvivesAcrossRequests(t *testing.T) {
	base, storePath, client := liveServer(t)

	for _, title := range []string{"first", "second"} {
		resp, raw := doHTTP(t, client, http.MethodPost, base+"/v1/notes",
			fmt.Sprintf(`{"title":%q}`, title), testToken)
		require.Equal(t, http.StatusCreated, resp.StatusCode, "body: %s", raw)
	}

	// Read the file directly: the API said it stored them, so they must be
	// on disk, not merely in a response body.
	direct, err := notes.NewFileStore(storePath).List(t.Context())
	require.NoError(t, err)
	assert.Len(t, direct, 2)
}

func TestAuthEnforcedOverTheWire(t *testing.T) {
	base, _, client := liveServer(t)

	resp, _ := doHTTP(t, client, http.MethodPost, base+"/v1/notes", `{"title":"nope"}`, "")
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, "write without a token")

	resp, _ = doHTTP(t, client, http.MethodPost, base+"/v1/notes", `{"title":"nope"}`, "wrong-token")
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, "write with a wrong token")

	// Reads stay open.
	resp, _ = doHTTP(t, client, http.MethodGet, base+"/v1/notes", "", "")
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestProbesAndVersionOverTheWire(t *testing.T) {
	base, _, client := liveServer(t)

	for _, path := range []string{"/healthz", "/readyz", "/version"} {
		resp, raw := doHTTP(t, client, http.MethodGet, base+path, "", "")
		assert.Equalf(t, http.StatusOK, resp.StatusCode, "%s -> %s", path, raw)
		assert.Containsf(t, resp.Header.Get("Content-Type"), "application/json",
			"%s should answer JSON", path)
	}
}

func TestValidationRejectedOverTheWire(t *testing.T) {
	base, _, client := liveServer(t)

	resp, raw := doHTTP(t, client, http.MethodPost, base+"/v1/notes", `{"title":"   "}`, testToken)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)

	var body api.ErrorBody
	require.NoError(t, json.Unmarshal(raw, &body))
	assert.NotEmpty(t, body.Error)
	// The client must never receive an internal detail.
	assert.NotContains(t, body.Error, "goroutine")
}

// TestConcurrentClientsGetDistinctNotes drives the store through the real
// server from several connections at once — the race the unit suite's
// single-goroutine httptest calls cannot produce.
//
// The workers deliberately do NOT use doHTTP or any require.* helper. Those
// call t.FailNow, which is runtime.Goexit: a worker hitting a transport error
// would die before sending its result, and a drain loop reading a fixed count
// would then block forever, turning a real failure into a suite-wide hang.
// Workers return values; every assertion happens on the test goroutine.
func TestConcurrentClientsGetDistinctNotes(t *testing.T) {
	base, _, client := liveServer(t)

	const clients = 12
	results := make(chan createResult, clients)

	var wg sync.WaitGroup
	wg.Add(clients)
	for i := range clients {
		go func() {
			defer wg.Done()
			results <- createNote(t.Context(), client, base, fmt.Sprintf("client-%d", i))
		}()
	}
	// Close after the last send so the range below terminates on its own
	// rather than on a count a dead worker could make unreachable.
	go func() { wg.Wait(); close(results) }()

	seen := map[string]bool{}
	for r := range results {
		require.NoError(t, r.err)
		require.NotEmpty(t, r.id, "every concurrent create should succeed")
		require.False(t, seen[r.id], "IDs must be unique across concurrent clients")
		seen[r.id] = true
	}
	assert.Len(t, seen, clients)
}

// createResult carries a worker's outcome back to the test goroutine.
type createResult struct {
	id  string
	err error
}

// createNote posts one note and returns its ID. It reports errors rather than
// failing the test, so it is safe to call from a goroutine.
func createNote(ctx context.Context, client *http.Client, base, title string) (res createResult) {
	body := fmt.Sprintf(`{"title":%q}`, title)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/notes",
		strings.NewReader(body))
	if err != nil {
		res.err = fmt.Errorf("build request: %w", err)
		return res
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testToken)

	resp, err := client.Do(req)
	if err != nil {
		res.err = fmt.Errorf("post note: %w", err)
		return res
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		res.err = fmt.Errorf("read body: %w", err)
		return res
	}
	if resp.StatusCode != http.StatusCreated {
		res.err = fmt.Errorf("status %d: %s", resp.StatusCode, raw)
		return res
	}

	var n notes.Note
	if err := json.Unmarshal(raw, &n); err != nil {
		res.err = fmt.Errorf("decode note: %w", err)
		return res
	}
	res.id = n.ID
	return res
}
