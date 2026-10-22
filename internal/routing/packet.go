package routing

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"
)

// The task packet's bounds. The boss writes the packet and the daemon carries
// it in a spawn request; the bounds keep its text a brief, not a transcript.
const (
	// MaxTaskBytes bounds the packet: its file, and all its text together.
	MaxTaskBytes = 64 << 10
	// maxLongField bounds requirements, worker_assignment and
	// previous_attempts each.
	maxLongField = 16 << 10
	// maxShortField bounds task_id and title each.
	maxShortField = 512
	// maxListItems and maxItemBytes bound acceptance_criteria and
	// required_tools: how many items, and each item.
	maxListItems = 64
	maxItemBytes = 2 << 10
)

// DecodeTask reads a task packet: one JSON object of the rules' task_packet
// keys and no other, in UTF-8, at most MaxTaskBytes, each field within its
// bound. Bytes that are not UTF-8 are refused, not decoded: JSON would fold
// them into U+FFFD, and the assignment's digest is of its bytes. Keys match
// as encoding/json matches them, in any case, and an escaped lone surrogate
// is JSON's own U+FFFD.
func DecodeTask(r io.Reader) (*Task, error) {
	raw, err := io.ReadAll(io.LimitReader(r, MaxTaskBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: read the task packet: %w", ErrInvalid, err)
	}
	if len(raw) > MaxTaskBytes {
		return nil, fmt.Errorf("%w: the task packet is over %d bytes", ErrInvalid, MaxTaskBytes)
	}
	if !utf8.Valid(raw) {
		return nil, fmt.Errorf("%w: the task packet is not UTF-8", ErrInvalid)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var t Task
	if err := dec.Decode(&t); err != nil {
		return nil, fmt.Errorf("%w: the task packet: %w", ErrInvalid, err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: the task packet is more than one JSON object", ErrInvalid)
	}
	if err := t.CheckBounds(); err != nil {
		return nil, err
	}
	return &t, nil
}

// CheckBounds checks each field of a packet against its bound, and all its
// text together against MaxTaskBytes.
func (t *Task) CheckBounds() error {
	total := 0
	field := func(name, s string, limit int) error {
		total += len(s)
		if len(s) > limit {
			return fmt.Errorf("%w: the task packet's %s is over %d bytes", ErrInvalid, name, limit)
		}
		return nil
	}
	list := func(name string, items []string) error {
		if len(items) > maxListItems {
			return fmt.Errorf("%w: the task packet's %s has over %d items", ErrInvalid, name, maxListItems)
		}
		for i, s := range items {
			if err := field(fmt.Sprintf("%s[%d]", name, i), s, maxItemBytes); err != nil {
				return err
			}
		}
		return nil
	}
	var id string
	if t.TaskID != nil {
		id = *t.TaskID
	}
	if err := errors.Join(
		field("task_id", id, maxShortField),
		field("title", t.Title, maxShortField),
		field("requirements", t.Requirements, maxLongField),
		field("worker_assignment", t.WorkerAssignment, maxLongField),
		field("previous_attempts", t.PreviousAttempts, maxLongField),
		list("acceptance_criteria", t.AcceptanceCriteria),
		list("required_tools", t.RequiredTools),
	); err != nil {
		return err
	}
	total += len(t.LookupStatus) + len(t.RequestedRole) + len(t.ProviderPreference)
	if total > MaxTaskBytes {
		return fmt.Errorf("%w: the task packet's text is over %d bytes", ErrInvalid, MaxTaskBytes)
	}
	return nil
}
