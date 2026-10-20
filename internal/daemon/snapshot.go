package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/abanna/herdr-agentisan/internal/snapshot"
)

// codeNotReady is the snapshot op's answer before the team poll has built a
// snapshot.
const codeNotReady = "not_ready"

// maxSnapshot bounds an encoded snapshot: it must fit one answer line, with
// room for the {"ok":true,"data":…} around it.
const maxSnapshot = maxMessage - 1024

var (
	// ErrNotReady means the daemon has no snapshot yet: its team poll has
	// not finished a poll with agent names, or is off. A daemon refusal
	// with code not_ready matches it.
	ErrNotReady = errors.New("the daemon has no snapshot yet")
	// ErrSnapshotTooLarge means a snapshot would not fit one answer line, so
	// the team poll keeps serving the last one that did.
	ErrSnapshotTooLarge = errors.New("snapshot too large to serve")
)

// snapshotCache is the snapshot op's state: the last snapshot the team poll
// built, encoded once, so the op answers without calling herdr (D3).
type snapshotCache struct {
	mu  sync.Mutex
	raw json.RawMessage
}

// store encodes s and serves it from now on. A snapshot that would not fit
// one answer line is ErrSnapshotTooLarge, and the last one stays.
func (c *snapshotCache) store(s snapshot.Snapshot) error {
	raw, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("encode the snapshot: %w", err)
	}
	if len(raw) > maxSnapshot {
		return fmt.Errorf("%w: %d bytes, at most %d", ErrSnapshotTooLarge, len(raw), maxSnapshot)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.raw = raw
	return nil
}

// load is the snapshot the op serves, or ErrNotReady before the first.
func (c *snapshotCache) load() (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.raw == nil {
		return nil, fmt.Errorf("%w: the team poll has not built one", ErrNotReady)
	}
	return c.raw, nil
}

// Snapshot asks the daemon at socket for its snapshot, decoded as strictly
// as snapshot.Decode decodes a file. Data that breaks the contract is
// ErrProtocol, wrapping snapshot's own sentinel.
func Snapshot(ctx context.Context, socket string) (snapshot.Snapshot, error) {
	data, err := call(ctx, socket, "snapshot")
	if err != nil {
		return snapshot.Snapshot{}, err
	}
	s, err := snapshot.Decode(bytes.NewReader(data))
	if err != nil {
		return snapshot.Snapshot{}, fmt.Errorf("%w: snapshot: %w", ErrProtocol, err)
	}
	return s, nil
}

// SnapshotSource is the dashboard's snapshot.Source on the daemon socket:
// each poll is one snapshot op, bounded by the caller's context and the
// socket's own timeout, so a missing or wedged daemon costs one poll and
// never the dashboard. Its errors are one line naming what failed:
// "daemon not running: …", "daemon starting: …", "daemon not answering: …".
type SnapshotSource struct {
	// Socket is the daemon's socket, Paths.Socket.
	Socket string
}

// Snapshot asks the daemon for its snapshot.
func (s SnapshotSource) Snapshot(ctx context.Context) (snapshot.Snapshot, error) {
	snap, err := Snapshot(ctx, s.Socket)
	switch {
	case err == nil:
		return snap, nil
	case errors.Is(err, ErrNotReady):
		return snapshot.Snapshot{}, fmt.Errorf("daemon starting: %w", err)
	case errors.Is(err, context.DeadlineExceeded):
		return snapshot.Snapshot{}, fmt.Errorf("daemon not answering: %w", err)
	case errors.Is(err, context.Canceled):
		return snapshot.Snapshot{}, fmt.Errorf("daemon snapshot cancelled: %w", err)
	case errors.Is(err, ErrUnavailable):
		return snapshot.Snapshot{}, fmt.Errorf("daemon not running: %w", err)
	default:
		return snapshot.Snapshot{}, fmt.Errorf("daemon snapshot: %w", err)
	}
}
