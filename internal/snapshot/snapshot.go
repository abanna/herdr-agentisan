// Package snapshot is the versioned contract between the daemon and every
// presentation surface (ADR-001 D5/D6): one team's agents, grouped, with each
// agent's status, model, ctx, item and stage.
//
// The daemon will serve this as the `snapshot` op of its socket protocol: the
// request {"v":1,"op":"snapshot"} is answered with {"ok":true,"data":<Snapshot>}.
// Until the daemon is on main, FileSource reads the same JSON from a file, and
// the socket source arrives beside it as a second Source implementation.
//
// Decoding is strict. A snapshot from a newer contract version is refused as
// ErrUnsupportedVersion before its fields are looked at, so a reader one
// version behind says so instead of misreporting the new fields as invalid.
// Everything else that breaks the contract is ErrInvalid.
package snapshot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
	"time"
)

// Version is the contract version this build reads and writes.
const Version = 1

// MaxBytes bounds one encoded snapshot. A full team is a few kilobytes; the
// cap stops a runaway writer from growing the reader's buffer without limit.
const MaxBytes = 4 << 20

// MaxCtx is the largest ctx value: ctx is a percentage of the context window.
const MaxCtx = 100

// Sentinel errors. Callers branch on these with errors.Is.
var (
	// ErrUnsupportedVersion means the snapshot's v is not Version.
	ErrUnsupportedVersion = errors.New("unsupported snapshot version")
	// ErrInvalid means the snapshot breaks the contract: malformed JSON, an
	// unknown field, an unknown status, a ctx outside 0..100, or an empty
	// agent or group name.
	ErrInvalid = errors.New("invalid snapshot")
	// ErrUnavailable means the source could not be read at all.
	ErrUnavailable = errors.New("snapshot source is unavailable")
)

// Status is an agent's state, as the daemon derives it.
type Status string

// The statuses the contract allows.
const (
	StatusWorking Status = "working"
	StatusIdle    Status = "idle"
	StatusBlocked Status = "blocked"
	StatusDone    Status = "done"
	StatusReady   Status = "ready"
	StatusUnknown Status = "unknown"
)

// Valid reports whether s is one of the contract's statuses. The empty
// status is not: a writer that does not know says "unknown".
func (s Status) Valid() bool {
	switch s {
	case StatusWorking, StatusIdle, StatusBlocked, StatusDone, StatusReady, StatusUnknown:
		return true
	default:
		return false
	}
}

// Agent is one agent: a herdr agent name and the pane it runs in.
type Agent struct {
	// Name is the herdr agent name, the target agent.focus takes.
	Name   string `json:"name"`
	PaneID string `json:"pane_id"`
	Status Status `json:"status"`
	// Model is the model as the agent reports it, e.g. "claude-opus-4-1".
	Model string `json:"model"`
	// Ctx is the context window used, 0..100. Nil means not reported.
	Ctx   *int   `json:"ctx,omitempty"`
	Item  string `json:"item,omitempty"`
	Stage string `json:"stage,omitempty"`
}

// Group is one group of agents, e.g. coders or qa.
type Group struct {
	Name        string  `json:"name"`
	Description string  `json:"description,omitempty"`
	Agents      []Agent `json:"agents"`
}

// Snapshot is one team at one instant.
type Snapshot struct {
	V    int       `json:"v"`
	At   time.Time `json:"at"`
	Team string    `json:"team"`
	// Boss is nil when the team has no boss running.
	Boss   *Agent  `json:"boss,omitempty"`
	Groups []Group `json:"groups"`
}

// Validate checks the rules JSON decoding cannot: every status is known,
// every ctx is within 0..100, and every agent and group has a name.
func (s Snapshot) Validate() error {
	if s.V != Version {
		return fmt.Errorf("%w: v %d, this build reads v%d", ErrUnsupportedVersion, s.V, Version)
	}
	if s.Boss != nil {
		if err := s.Boss.validate(); err != nil {
			return fmt.Errorf("%w: boss: %w", ErrInvalid, err)
		}
	}
	for gi, g := range s.Groups {
		if g.Name == "" {
			return fmt.Errorf("%w: group %d has no name", ErrInvalid, gi)
		}
		for ai, a := range g.Agents {
			if err := a.validate(); err != nil {
				return fmt.Errorf("%w: group %q agent %d: %w", ErrInvalid, g.Name, ai, err)
			}
		}
	}
	return nil
}

// errAgent is the cause Validate wraps under ErrInvalid; it is not exported
// because callers branch on ErrInvalid alone.
var errAgent = errors.New("agent breaks the contract")

func (a Agent) validate() error {
	switch {
	case a.Name == "":
		return fmt.Errorf("%w: no name", errAgent)
	case !a.Status.Valid():
		return fmt.Errorf("%w: %q has unknown status %q", errAgent, a.Name, a.Status)
	case a.Ctx != nil && (*a.Ctx < 0 || *a.Ctx > MaxCtx):
		return fmt.Errorf("%w: %q has ctx %d, outside 0..%d", errAgent, a.Name, *a.Ctx, MaxCtx)
	default:
		return nil
	}
}

// Decode reads exactly one snapshot from r and validates it.
func Decode(r io.Reader) (Snapshot, error) {
	raw, err := io.ReadAll(io.LimitReader(r, MaxBytes+1))
	if err != nil {
		return Snapshot{}, fmt.Errorf("%w: read: %w", ErrInvalid, err)
	}
	if len(raw) > MaxBytes {
		return Snapshot{}, fmt.Errorf("%w: larger than %d bytes", ErrInvalid, MaxBytes)
	}

	// The version first, leniently: a newer snapshot is allowed fields this
	// build has never heard of, and must be refused for its version, not
	// for those fields.
	var head struct {
		V *int `json:"v"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return Snapshot{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if head.V == nil {
		return Snapshot{}, fmt.Errorf("%w: no v", ErrUnsupportedVersion)
	}
	if *head.V != Version {
		return Snapshot{}, fmt.Errorf("%w: v %d, this build reads v%d", ErrUnsupportedVersion, *head.V, Version)
	}

	if err := exactKeys(raw); err != nil {
		return Snapshot{}, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var s Snapshot
	if err := dec.Decode(&s); err != nil {
		return Snapshot{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	// One snapshot per document: anything after it is a writer bug.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Snapshot{}, fmt.Errorf("%w: data after the snapshot", ErrInvalid)
	}
	if err := s.Validate(); err != nil {
		return Snapshot{}, err
	}
	return s, nil
}

// shape is the set of keys an object may hold and the shape of each key's
// value; elem is the shape of an array's elements. A nil shape accepts any
// value: its type is checked by the strict decode that follows.
type shape struct {
	keys map[string]*shape
	elem *shape
}

var (
	agentShape = &shape{keys: map[string]*shape{
		"name": nil, "pane_id": nil, "status": nil, "model": nil, "ctx": nil, "item": nil, "stage": nil,
	}}
	groupShape = &shape{keys: map[string]*shape{
		"name": nil, "description": nil, "agents": {elem: agentShape},
	}}
	snapshotShape = &shape{keys: map[string]*shape{
		"v": nil, "at": nil, "team": nil, "boss": agentShape, "groups": {elem: groupShape},
	}}
)

// exactKeys refuses an object key that is not spelled exactly as the
// contract spells it, or that appears twice in one object. encoding/json
// matches keys case-insensitively and keeps the last of a duplicate, so
// DisallowUnknownFields alone would accept {"TEAM":…} and {"v":1,"v":2}.
// raw is already known to be valid JSON within encoding/json's depth limit.
func exactKeys(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	return walk(dec, snapshotShape)
}

// walk consumes one value from dec, checking its keys against s.
func walk(dec *json.Decoder, s *shape) error {
	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	switch tok {
	case json.Delim('{'):
		seen := map[string]bool{}
		for dec.More() {
			keyTok, err := dec.Token()
			if err != nil {
				return fmt.Errorf("%w: %w", ErrInvalid, err)
			}
			key, _ := keyTok.(string) // an object key is always a string
			var child *shape
			if s != nil && s.keys != nil {
				known := false
				child, known = s.keys[key]
				if !known {
					return fmt.Errorf("%w: unknown key %q", ErrInvalid, key)
				}
				if seen[key] {
					return fmt.Errorf("%w: key %q appears twice", ErrInvalid, key)
				}
				seen[key] = true
			}
			if err := walk(dec, child); err != nil {
				return err
			}
		}
		_, err = dec.Token() // '}'
	case json.Delim('['):
		var elem *shape
		if s != nil {
			elem = s.elem
		}
		for dec.More() {
			if err := walk(dec, elem); err != nil {
				return err
			}
		}
		_, err = dec.Token() // ']'
	}
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	return nil
}

// Source yields the current snapshot. The dashboard polls it.
type Source interface {
	Snapshot(ctx context.Context) (Snapshot, error)
}

// FileSource reads a snapshot from a JSON file, again on every call, so an
// edited file shows up on the next poll. It stands in for the daemon's
// socket while that is not served.
type FileSource struct {
	Path string
}

// Snapshot reads and decodes the file. Anything but a regular file is
// ErrUnavailable and is not read: a directory or device holds no snapshot,
// and a FIFO with no writer would block the poll forever.
func (f FileSource) Snapshot(ctx context.Context) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, fmt.Errorf("read snapshot %s: %w", f.Path, err)
	}
	// O_NONBLOCK makes opening a FIFO return at once instead of waiting for
	// a writer; it changes nothing for a regular file. The type is then
	// checked on the descriptor that was opened, so a node swapped in after
	// the open cannot be what is read.
	file, err := os.OpenFile(f.Path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return Snapshot{}, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	defer file.Close() //nolint:errcheck // read-only; Decode has already consumed it
	info, err := file.Stat()
	if err != nil {
		return Snapshot{}, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	if !info.Mode().IsRegular() {
		return Snapshot{}, fmt.Errorf("%w: %s is not a regular file (%s)", ErrUnavailable, f.Path, info.Mode().Type())
	}

	s, err := Decode(file)
	if err != nil {
		return Snapshot{}, fmt.Errorf("%s: %w", f.Path, err)
	}
	return s, nil
}
