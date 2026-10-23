// Package jev is the client of TypeSafe's System One, which routing asks how
// demanding a coder's assignment is (ADR-001 A57, NERD-5289): one POST of a
// request routing built, answered within a timeout, never retried. It knows
// nothing of the questions or the answers' meaning; routing does.
//
// The key goes out as the Bearer token and nowhere else: no error carries it,
// nor the request, nor what TypeSafe answered beyond its status.
package jev

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Endpoint is System One's.
const Endpoint = "https://api.typesafe.ai/v1/systemone"

// maxAnswer is the most of an answer read: far more than two answers hold,
// small enough that a wrong server cannot fill memory.
const maxAnswer = 64 << 10

// Sentinel errors. Callers branch on them with errors.Is.
var (
	// ErrNoKey means no key, or one that cannot be a Bearer token.
	ErrNoKey = errors.New("no TypeSafe key")
	// ErrCall means the request never got an answer: it could not be
	// sent, or the timeout or the caller's context ended it.
	ErrCall = errors.New("the TypeSafe call failed")
	// ErrStatus means TypeSafe answered with a status other than 200.
	ErrStatus = errors.New("TypeSafe refused the request")
	// ErrAnswer means a 200 whose body is not an answer.
	ErrAnswer = errors.New("TypeSafe's answer is malformed")
)

// Client asks System One.
type Client struct {
	// URL is System One's; "" is Endpoint.
	URL string
	// Key is the API key.
	Key string
	// HTTP sends the request; nil is Go's default transport. Whichever it
	// is, Ask follows no redirect, and the timeout it is given bounds it.
	HTTP *http.Client
}

// Answer is System One's answer: the model that answered, and each answer
// by the name of the question it answers, as TypeSafe sent it.
type Answer struct {
	Model   string
	Answers map[string]json.RawMessage
}

// noRedirect keeps the request, and its key, at the URL it was sent to: a
// redirect is answered as the status it is.
func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// Ask posts body to System One once, within timeout. A status other than
// 200 is ErrStatus with the status; an answer cut off, too large or without
// a model and answers is ErrAnswer; anything that leaves no answer is
// ErrCall.
func (c Client) Ask(ctx context.Context, body []byte, timeout time.Duration) (Answer, error) {
	if c.Key == "" || strings.IndexFunc(c.Key, func(r rune) bool { return r <= ' ' || r > '~' }) >= 0 {
		return Answer{}, fmt.Errorf("%w: the key is empty or not a token", ErrNoKey)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cmp.Or(c.URL, Endpoint), bytes.NewReader(body))
	if err != nil {
		return Answer{}, fmt.Errorf("%w: %w", ErrCall, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.Key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	var h http.Client
	if c.HTTP != nil {
		h = *c.HTTP
	}
	h.CheckRedirect = noRedirect
	resp, err := h.Do(req)
	if err != nil {
		return Answer{}, fmt.Errorf("%w: %w", ErrCall, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer+1))
	switch {
	case resp.StatusCode != http.StatusOK:
		// The body can echo the request (a 422's detail does), so it is
		// never kept.
		return Answer{}, fmt.Errorf("%w: HTTP %d", ErrStatus, resp.StatusCode)
	case err != nil:
		return Answer{}, fmt.Errorf("%w: reading it: %w", ErrCall, err)
	case len(raw) > maxAnswer:
		return Answer{}, fmt.Errorf("%w: over %d KiB", ErrAnswer, maxAnswer>>10)
	}
	var doc struct {
		Model   string                     `json:"model"`
		Answers map[string]json.RawMessage `json:"answers"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return Answer{}, fmt.Errorf("%w: it is not a JSON object of model and answers", ErrAnswer)
	}
	if doc.Model == "" || len(doc.Answers) == 0 {
		return Answer{}, fmt.Errorf("%w: it names no model or holds no answers", ErrAnswer)
	}
	return Answer{Model: doc.Model, Answers: doc.Answers}, nil
}
