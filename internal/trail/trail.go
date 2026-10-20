// Package trail reads the workflow events back out of Elasticsearch
// (ADR-001 D10, item 12): an ES|QL client over the trail's data stream, the
// recipes the debugger agent runs, and the env file its URL and read key
// come from.
//
// The key never leaves this package. It is read from the env file each time
// a query runs, sent only in the Authorization header to the URL read from
// the same file, and never formatted into an error, a String or an output.
// Nothing here lets a caller choose the URL, the variables or the file: the
// file is the shared config's debugger.env_file, so an agent allowed to run
// the events commands cannot send the key anywhere else.
package trail

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
)

// Sentinel errors. Callers branch on these with errors.Is.
var (
	// ErrNotConfigured means the trail's config dir, env file or one of its
	// two names is missing.
	ErrNotConfigured = errors.New("the trail is not configured")
	// ErrEnvFile means the env file could not be read or parsed.
	ErrEnvFile = errors.New("env file")
	// ErrInvalid means an argument or the env file's URL is not usable.
	ErrInvalid = errors.New("invalid")
	// ErrUnauthorized means Elasticsearch refused the read key (401 or 403).
	ErrUnauthorized = errors.New("elasticsearch refused the read key")
	// ErrBadQuery means Elasticsearch refused the query (400), with its
	// reason, which says what to fix.
	ErrBadQuery = errors.New("elasticsearch refused the query")
	// ErrUnavailable means Elasticsearch could not be reached or answered
	// with something other than an ES|QL result.
	ErrUnavailable = errors.New("elasticsearch is unavailable")
)

// The names the env file holds the trail's Elasticsearch under.
const (
	URLVar = "ELASTIC_URL"
	KeyVar = "ELASTIC_API_KEY"
)

// QueryTimeout bounds one query, from connecting to the last byte.
const QueryTimeout = 30 * time.Second

// maxAnswer is the most of an answer read. An ES|QL answer that large is a
// query missing a LIMIT, and is refused rather than half rendered.
const maxAnswer = 16 << 20

// maxReason bounds the Elasticsearch reason an error repeats, in runes.
const maxReason = 512

// Column is one column of an ES|QL answer.
type Column struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// Table is an ES|QL answer: its columns, then one row of values per
// document, each value a string, a bool, a json.Number or nil. Partial
// means Elasticsearch did not search everything it was asked to.
type Table struct {
	Columns []Column `json:"columns"`
	Values  [][]any  `json:"values"`
	Partial bool     `json:"partial,omitempty"`
}

// Client queries the trail's Elasticsearch. Build it with NewClient or
// FromConfig.
type Client struct {
	url  string
	key  string
	http *http.Client
}

// String names the client by its URL alone, so a Client printed by mistake
// never shows the key.
func (c *Client) String() string { return "trail.Client(" + c.url + ")" }

// GoString is String, for %#v.
func (c *Client) GoString() string { return c.String() }

// NewClient checks rawURL is http(s)://host[:port], with no user, path,
// query or fragment, and key is not empty. hc nil uses a client with
// QueryTimeout. Neither value is repeated in an error.
func NewClient(rawURL, key string, hc *http.Client) (*Client, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return nil, fmt.Errorf("%w: %s is not http(s)://host[:port]", ErrInvalid, URLVar)
	}
	if key == "" {
		return nil, fmt.Errorf("%w: %s is empty", ErrNotConfigured, KeyVar)
	}
	if hc == nil {
		hc = &http.Client{Timeout: QueryTimeout}
	}
	return &Client{url: u.Scheme + "://" + u.Host, key: key, http: hc}, nil
}

// Query runs esql through Elasticsearch's _query endpoint and returns its
// answer.
func (c *Client) Query(ctx context.Context, esql string) (Table, error) {
	body, err := json.Marshal(map[string]string{"query": esql})
	if err != nil {
		return Table{}, fmt.Errorf("encode query: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url+"/_query", bytes.NewReader(body))
	if err != nil {
		return Table{}, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "ApiKey "+c.key)
	resp, err := c.http.Do(req)
	if err != nil {
		return Table{}, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer+1))
	if err != nil {
		return Table{}, fmt.Errorf("%w: reading the answer: %w", ErrUnavailable, err)
	}
	if len(data) > maxAnswer {
		return Table{}, fmt.Errorf("%w: the answer is over %d MiB; add a LIMIT", ErrBadQuery, maxAnswer>>20)
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return Table{}, fmt.Errorf("%w (%d)", ErrUnauthorized, resp.StatusCode)
	case resp.StatusCode == http.StatusBadRequest:
		return Table{}, fmt.Errorf("%w: %s", ErrBadQuery, reason(data))
	case resp.StatusCode/100 != 2:
		return Table{}, fmt.Errorf("%w: HTTP %d", ErrUnavailable, resp.StatusCode)
	}
	return decode(data)
}

// decode reads an ES|QL answer, numbers kept exact.
func decode(data []byte) (Table, error) {
	var a struct {
		Columns   []Column `json:"columns"`
		Values    [][]any  `json:"values"`
		IsPartial bool     `json:"is_partial"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&a); err != nil || a.Columns == nil {
		return Table{}, fmt.Errorf("%w: the answer is not an ES|QL result", ErrUnavailable)
	}
	for i, row := range a.Values {
		if len(row) != len(a.Columns) {
			return Table{}, fmt.Errorf("%w: row %d has %d values for %d columns", ErrUnavailable, i, len(row), len(a.Columns))
		}
	}
	return Table{Columns: a.Columns, Values: a.Values, Partial: a.IsPartial}, nil
}

// reason is Elasticsearch's error.reason, bounded and stripped of control
// characters, or a placeholder when the body has none.
func reason(data []byte) string {
	var e struct {
		Error struct {
			Reason string `json:"reason"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &e) != nil || e.Error.Reason == "" {
		return "no reason given"
	}
	r := strings.Map(func(c rune) rune {
		if unicode.IsControl(c) {
			return ' '
		}
		return c
	}, e.Error.Reason)
	if runes := []rune(r); len(runes) > maxReason {
		r = string(runes[:maxReason]) + "…"
	}
	return r
}
