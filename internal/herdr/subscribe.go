package herdr

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"
)

// Subscription types and the event kinds they stream. herdr names a
// subscription with a dot and the event it streams with an underscore.
const (
	// SubscribePaneFocused subscribes to focus moving to another pane.
	SubscribePaneFocused = "pane.focused"
	// EventPaneFocused is the kind of event a pane.focused subscription
	// streams; its PaneID is the pane that took focus.
	EventPaneFocused = "pane_focused"
)

// methodSubscribe is the request that opens a subscription.
const methodSubscribe = "events.subscribe"

// errLineTooLong means a stream line exceeded maxResponseBytes.
var errLineTooLong = fmt.Errorf("%w: line exceeds %d bytes", ErrProtocol, maxResponseBytes)

// Event is one event a subscription streams: herdr's
// {"event":<kind>,"data":{"type":<kind>,...}} envelope.
type Event struct {
	// Kind is the event kind, such as EventPaneFocused.
	Kind string
	// PaneID and WorkspaceID are the data's pane_id and workspace_id, empty
	// for a kind that carries none.
	PaneID      string
	WorkspaceID string
	// Data is the raw data object, for fields this client does not model.
	Data json.RawMessage
}

// Subscription is an open events.subscribe stream. herdr writes each event
// as one line on the connection the request was made on, and ends the stream
// by closing it, after an error line when it ends it for a reason. Next is
// not safe for concurrent use; Close may be called from any goroutine.
type Subscription struct {
	conn net.Conn
	r    *bufio.Reader
	id   string
	// ctx bounds the subscription's life; stop releases its watch.
	ctx  context.Context //nolint:containedctx // the context Subscribe was given governs the stream, as a gRPC stream's does
	stop func() bool
	// err ends the subscription: once set, Next returns it.
	err error
}

type subscriptionParam struct {
	Type string `json:"type"`
}

// Subscribe opens an events.subscribe stream for the given subscription
// types, such as SubscribePaneFocused. Only the dial and herdr's
// acknowledgement are bounded by the client's timeout: the stream itself
// lasts until ctx ends, Close is called, or herdr ends it.
func (c Client) Subscribe(ctx context.Context, types ...string) (*Subscription, error) {
	if c.SocketPath == "" {
		return nil, ErrNoSocket
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", methodSubscribe, err)
	}
	ackCtx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(ackCtx, "unix", c.SocketPath)
	if err != nil {
		return nil, fmt.Errorf("%w: dial %s: %w", ErrUnavailable, c.SocketPath, err)
	}
	r, id, err := c.handshake(ackCtx, conn, types)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	s := &Subscription{conn: conn, r: r, id: id, ctx: ctx}
	s.stop = context.AfterFunc(ctx, func() { _ = conn.Close() })
	return s, nil
}

// handshake sends the subscribe request on conn and reads herdr's
// acknowledgement, bounded by ctx. It returns the reader that holds whatever
// herdr wrote after the acknowledgement.
func (c Client) handshake(ctx context.Context, conn net.Conn, types []string) (*bufio.Reader, string, error) {
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })

	subs := make([]subscriptionParam, 0, len(types))
	for _, t := range types {
		subs = append(subs, subscriptionParam{Type: t})
	}
	id := nextID()
	req, err := json.Marshal(request{ID: id, Method: methodSubscribe, Params: struct {
		Subscriptions []subscriptionParam `json:"subscriptions"`
	}{subs}})
	if err != nil {
		stop()
		return nil, "", fmt.Errorf("encode %s request: %w", methodSubscribe, err)
	}
	// The write side stays open for the stream's life: herdr checks for a
	// closed peer between events and ends the subscription the moment it
	// sees one, so the request must never be followed by a half-close.
	if _, err := conn.Write(append(req, '\n')); err != nil {
		stop()
		return nil, "", c.ioError(ctx, methodSubscribe, err)
	}
	r := bufio.NewReader(conn)
	raw, err := readStreamLine(r)
	if !stop() {
		// The acknowledgement's time ran out, whether or not it arrived: the
		// deadline is in the past and the stream would fail at once.
		return nil, "", c.ioError(ctx, methodSubscribe, errors.Join(err, ctx.Err()))
	}
	if err != nil {
		if errors.Is(err, ErrProtocol) {
			return nil, "", fmt.Errorf("%s: %w", methodSubscribe, err)
		}
		return nil, "", c.ioError(ctx, methodSubscribe, err)
	}
	var out struct {
		Type string `json:"type"`
	}
	if err := decodeResponse(methodSubscribe, id, raw, &out); err != nil {
		return nil, "", err
	}
	if out.Type != "subscription_started" {
		return nil, "", fmt.Errorf("%w: %s: result type %q, want \"subscription_started\"", ErrProtocol, methodSubscribe, out.Type)
	}
	_ = conn.SetDeadline(time.Time{})
	return r, id, nil
}

// readStreamLine reads one newline-terminated line of at most
// maxResponseBytes. A stream that ends, even part way through a line, is
// the connection failing, not a line.
func readStreamLine(r *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		chunk, err := r.ReadSlice('\n')
		if len(line)+len(chunk) > maxResponseBytes {
			return nil, errLineTooLong
		}
		line = append(line, chunk...)
		if err == nil {
			return line, nil
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return nil, err //nolint:wrapcheck // the caller wraps with the method and sentinel
		}
	}
}

// streamLine is any line herdr writes on a subscription after the ack: an
// event envelope, or an error that ends the stream.
type streamLine struct {
	ID    *string         `json:"id"`
	Event string          `json:"event"`
	Data  json.RawMessage `json:"data"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// Next waits for the next event. ctx bounds this wait; the context Subscribe
// was given bounds the subscription. Every error ends the subscription, and
// every later call returns it: ErrEventsLost when herdr dropped events,
// ErrUnavailable when the connection ended, ErrProtocol for a line the schema
// does not allow, and the context's error when a context ended.
func (s *Subscription) Next(ctx context.Context) (Event, error) {
	if s.err != nil {
		return Event{}, s.err
	}
	if err := ctx.Err(); err != nil {
		return Event{}, s.fail(fmt.Errorf("%s: %w", methodSubscribe, err))
	}
	stop := context.AfterFunc(ctx, func() { _ = s.conn.SetReadDeadline(time.Unix(1, 0)) })
	raw, err := readStreamLine(s.r)
	stop()
	if err != nil {
		return Event{}, s.fail(s.readError(ctx, err))
	}
	ev, err := s.decode(raw)
	if err != nil {
		return Event{}, s.fail(err)
	}
	return ev, nil
}

// readError names why reading the stream failed, preferring a context that
// ended over the connection error that ending it caused.
func (s *Subscription) readError(ctx context.Context, err error) error {
	switch {
	case ctx.Err() != nil:
		return fmt.Errorf("%s: %w", methodSubscribe, ctx.Err())
	case s.ctx.Err() != nil:
		return fmt.Errorf("%s: %w", methodSubscribe, s.ctx.Err())
	case errors.Is(err, ErrProtocol):
		return fmt.Errorf("%s: %w", methodSubscribe, err)
	}
	return fmt.Errorf("%w: %s: %w", ErrUnavailable, methodSubscribe, err)
}

// decode turns one stream line into an event, or the error that ended the
// stream.
func (s *Subscription) decode(raw []byte) (Event, error) {
	var l streamLine
	if err := json.Unmarshal(raw, &l); err != nil {
		return Event{}, fmt.Errorf("%w: %s: decode line: %w", ErrProtocol, methodSubscribe, err)
	}
	if l.Error != nil {
		if l.ID == nil || *l.ID != s.id {
			return Event{}, fmt.Errorf("%w: %s: an error line for another request", ErrProtocol, methodSubscribe)
		}
		return Event{}, &APIError{Method: methodSubscribe, Code: l.Error.Code, Message: l.Error.Message}
	}
	if l.Event == "" {
		return Event{}, fmt.Errorf("%w: %s: a line that is neither an event nor an error", ErrProtocol, methodSubscribe)
	}
	var data struct {
		Type        string `json:"type"`
		PaneID      string `json:"pane_id"`
		WorkspaceID string `json:"workspace_id"`
	}
	if len(l.Data) == 0 || l.Data[0] != '{' {
		return Event{}, fmt.Errorf("%w: %s: %s event has no data object", ErrProtocol, methodSubscribe, l.Event)
	}
	if err := json.Unmarshal(l.Data, &data); err != nil {
		return Event{}, fmt.Errorf("%w: %s: decode %s data: %w", ErrProtocol, methodSubscribe, l.Event, err)
	}
	if data.Type != l.Event {
		return Event{}, fmt.Errorf("%w: %s: %s event carries %q data", ErrProtocol, methodSubscribe, l.Event, data.Type)
	}
	if l.Event == EventPaneFocused && data.PaneID == "" {
		return Event{}, fmt.Errorf("%w: %s: %s event has no pane_id", ErrProtocol, methodSubscribe, l.Event)
	}
	return Event{Kind: l.Event, PaneID: data.PaneID, WorkspaceID: data.WorkspaceID, Data: l.Data}, nil
}

// fail ends the subscription with err.
func (s *Subscription) fail(err error) error {
	s.err = err
	_ = s.Close()
	return err
}

// Close ends the subscription; herdr notices the closed connection and stops
// streaming. It is safe to call more than once, and from another goroutine
// than Next's.
func (s *Subscription) Close() error {
	s.stop()
	if err := s.conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		return fmt.Errorf("%w: close %s: %w", ErrUnavailable, methodSubscribe, err)
	}
	return nil
}
