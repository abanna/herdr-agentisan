// Package snapshot is the versioned contract between the daemon and every
// presentation surface (ADR-001 D5/D6): one team's agents, grouped, with each
// agent's status, model, ctx, item and stage; the boss; and the project's
// pull requests, issues and test slots.
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
	"maps"
	"os"
	"regexp"
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
	// unknown or re-cased field, a value outside its domain (an unknown
	// status or PR state, a ctx outside 0..100, a negative count, a time that
	// is not RFC 3339), or an empty agent or group name.
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
	// StageStartedAt is when the agent entered Stage. Nil means not
	// reported.
	StageStartedAt *time.Time `json:"stage_started_at,omitempty"`
}

// Boss is the team's boss: an agent, plus what only the boss reports.
type Boss struct {
	Agent
	// Watchers is how many watchers (background monitors) the boss runs.
	// Nil means not reported.
	Watchers *int `json:"watchers,omitempty"`
	// StartedAt is when the boss's session started; its runtime counts from
	// it. Nil means not reported.
	StartedAt *time.Time `json:"started_at,omitempty"`
}

// Group is one group of agents, e.g. coders or qa.
type Group struct {
	Name        string  `json:"name"`
	Description string  `json:"description,omitempty"`
	Agents      []Agent `json:"agents"`
}

// Project is the state of the project the team works on: its colour, its
// open pull requests, its open issues and its test slots. Every field is
// optional, and a missing one is not reported, never zero.
type Project struct {
	// Color is the project's colour as "#rrggbb"; empty when it has none.
	Color string `json:"color,omitempty"`
	// PRs are the open pull requests. Nil means not reported; empty means
	// none are open. Not omitempty: an empty list must survive encoding.
	PRs []PR `json:"prs"`
	// Issues is the count of open issues. Nil means not reported.
	Issues *int `json:"issues,omitempty"`
	// Slots are the test slots. Nil means not reported.
	Slots *Slots `json:"slots,omitempty"`
}

// PR is one open pull request and the three states that decide whether it
// can merge.
type PR struct {
	Number int        `json:"number"`
	CI     CIState    `json:"ci"`
	Codex  CodexState `json:"codex"`
	Merge  MergeState `json:"merge"`
}

// Slots are the test slots: how many are in use, of how many. Both keys are
// required.
type Slots struct {
	Used  int `json:"used"`
	Total int `json:"total"`
}

// CIState is the combined state of a PR's checks.
type CIState string

// The CI states the contract allows.
const (
	CIPending CIState = "pending"
	CISuccess CIState = "success"
	CIFailure CIState = "failure"
	CIUnknown CIState = "unknown"
)

// Valid reports whether c is one of the contract's CI states.
func (c CIState) Valid() bool {
	switch c {
	case CIPending, CISuccess, CIFailure, CIUnknown:
		return true
	default:
		return false
	}
}

// CodexState is where Codex's review of a PR stands: not yet reviewed, a 👍,
// or findings to answer.
type CodexState string

// The Codex states the contract allows.
const (
	CodexPending  CodexState = "pending"
	CodexApproved CodexState = "approved"
	CodexFindings CodexState = "findings"
	CodexUnknown  CodexState = "unknown"
)

// Valid reports whether c is one of the contract's Codex states.
func (c CodexState) Valid() bool {
	switch c {
	case CodexPending, CodexApproved, CodexFindings, CodexUnknown:
		return true
	default:
		return false
	}
}

// MergeState is GitHub's mergeStateStatus, lower-cased.
type MergeState string

// The merge states the contract allows.
const (
	MergeClean    MergeState = "clean"
	MergeBlocked  MergeState = "blocked"
	MergeBehind   MergeState = "behind"
	MergeDirty    MergeState = "dirty"
	MergeDraft    MergeState = "draft"
	MergeUnstable MergeState = "unstable"
	MergeHasHooks MergeState = "has_hooks"
	MergeUnknown  MergeState = "unknown"
)

// Valid reports whether m is one of the contract's merge states.
func (m MergeState) Valid() bool {
	switch m {
	case MergeClean, MergeBlocked, MergeBehind, MergeDirty, MergeDraft, MergeUnstable, MergeHasHooks, MergeUnknown:
		return true
	default:
		return false
	}
}

// Snapshot is one team at one instant.
type Snapshot struct {
	V    int       `json:"v"`
	At   time.Time `json:"at"`
	Team string    `json:"team"`
	// Project is nil when the project's state is not reported.
	Project *Project `json:"project,omitempty"`
	// Boss is nil when the team has no boss running.
	Boss   *Boss   `json:"boss,omitempty"`
	Groups []Group `json:"groups"`
}

// Validate checks the rules JSON decoding cannot: every status and PR state
// is known, every ctx is within 0..100, no count is negative, slots never
// exceed their total, the colour is #rrggbb, and every agent and group has a
// name. The RFC 3339 form of times is checked by Decode, on the text.
func (s Snapshot) Validate() error {
	if s.V != Version {
		return fmt.Errorf("%w: v %d, this build reads v%d", ErrUnsupportedVersion, s.V, Version)
	}
	if s.Project != nil {
		if err := s.Project.validate(); err != nil {
			return fmt.Errorf("%w: project: %w", ErrInvalid, err)
		}
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

// errAgent and errProject are the causes Validate wraps under ErrInvalid;
// they are not exported because callers branch on ErrInvalid alone.
var (
	errAgent   = errors.New("agent breaks the contract")
	errProject = errors.New("project breaks the contract")
)

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

func (b Boss) validate() error {
	if err := b.Agent.validate(); err != nil {
		return err
	}
	if b.Watchers != nil && *b.Watchers < 0 {
		return fmt.Errorf("%w: %q has watchers %d", errAgent, b.Name, *b.Watchers)
	}
	return nil
}

// colorPattern is the one spelling of a colour the contract allows.
var colorPattern = regexp.MustCompile(`^#[0-9A-Fa-f]{6}$`)

// ValidColor reports whether s is a colour the contract allows: "#rrggbb",
// in either case.
func ValidColor(s string) bool {
	return colorPattern.MatchString(s)
}

func (p Project) validate() error {
	if p.Color != "" && !ValidColor(p.Color) {
		return fmt.Errorf("%w: color %q is not #rrggbb", errProject, p.Color)
	}
	if p.Issues != nil && *p.Issues < 0 {
		return fmt.Errorf("%w: issues %d", errProject, *p.Issues)
	}
	if sl := p.Slots; sl != nil && (sl.Used < 0 || sl.Total < 0 || sl.Used > sl.Total) {
		return fmt.Errorf("%w: slots %d/%d: both are counts, and used is at most total", errProject, sl.Used, sl.Total)
	}
	seen := map[int]bool{}
	for _, pr := range p.PRs {
		switch {
		case pr.Number < 1:
			return fmt.Errorf("%w: PR number %d", errProject, pr.Number)
		case seen[pr.Number]:
			return fmt.Errorf("%w: PR #%d is listed twice", errProject, pr.Number)
		case !pr.CI.Valid():
			return fmt.Errorf("%w: PR #%d has unknown ci %q", errProject, pr.Number, pr.CI)
		case !pr.Codex.Valid():
			return fmt.Errorf("%w: PR #%d has unknown codex %q", errProject, pr.Number, pr.Codex)
		case !pr.Merge.Valid():
			return fmt.Errorf("%w: PR #%d has unknown merge %q", errProject, pr.Number, pr.Merge)
		}
		seen[pr.Number] = true
	}
	return nil
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
// value: its type is checked by the strict decode that follows. required are
// keys an object must hold even though their zero value is valid. A time
// shape accepts null or a string in RFC 3339 form.
type shape struct {
	keys     map[string]*shape
	elem     *shape
	required []string
	time     bool
}

var (
	timeShape  = &shape{time: true}
	agentKeys  = map[string]*shape{"name": nil, "pane_id": nil, "status": nil, "model": nil, "ctx": nil, "item": nil, "stage": nil, "stage_started_at": timeShape}
	agentShape = &shape{keys: agentKeys}
	bossShape  = &shape{keys: union(agentKeys, map[string]*shape{"watchers": nil, "started_at": timeShape})}
	groupShape = &shape{keys: map[string]*shape{
		"name": nil, "description": nil, "agents": {elem: agentShape},
	}}
	prShape      = &shape{keys: map[string]*shape{"number": nil, "ci": nil, "codex": nil, "merge": nil}}
	slotsShape   = &shape{keys: map[string]*shape{"used": nil, "total": nil}, required: []string{"used", "total"}}
	projectShape = &shape{keys: map[string]*shape{
		"color": nil, "prs": {elem: prShape}, "issues": nil, "slots": slotsShape,
	}}
	snapshotShape = &shape{keys: map[string]*shape{
		"v": nil, "at": timeShape, "team": nil, "project": projectShape, "boss": bossShape, "groups": {elem: groupShape},
	}}
)

// union is a new map holding a's keys and b's.
func union(a, b map[string]*shape) map[string]*shape {
	out := maps.Clone(a)
	maps.Copy(out, b)
	return out
}

// rfc3339 is RFC 3339's date-time grammar: two-digit fields, an upper-case T
// and Z, a '.' before any fraction, and an offset within ±23:59. Go's time
// package then checks the ranges (month, day, hour, minute, second); it reads
// some spellings this grammar does not allow, which is why both are applied.
var rfc3339 = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]([01]\d|2[0-3]):[0-5]\d)$`)

// exactKeys refuses an object key that is not spelled exactly as the
// contract spells it, that appears twice in one object, or that a required
// key's object lacks, and a time that is not RFC 3339. encoding/json matches
// keys case-insensitively and keeps the last of a duplicate, so
// DisallowUnknownFields alone would accept {"TEAM":…} and {"v":1,"v":2}.
// raw is already known to be valid JSON within encoding/json's depth limit.
func exactKeys(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	return walk(dec, snapshotShape)
}

// walk consumes one value from dec, checking it against s.
func walk(dec *json.Decoder, s *shape) error {
	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if s != nil && s.time {
		if str, ok := tok.(string); (ok && !rfc3339.MatchString(str)) || (!ok && tok != nil) {
			return fmt.Errorf("%w: %v is not an RFC 3339 time", ErrInvalid, tok)
		}
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
		if s != nil {
			for _, key := range s.required {
				if !seen[key] {
					return fmt.Errorf("%w: key %q is required", ErrInvalid, key)
				}
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
