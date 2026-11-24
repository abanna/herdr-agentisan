package bossshift

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	maxRounds     = 3
	maxRoundBytes = 128 << 10
	roundEnd      = "\n<!-- end shift round -->\n"
	waitTimeout   = "timeout"
	published     = "published"
)

// ErrRecord means the requested shift directory or options cannot be used.
var ErrRecord = errors.New("invalid boss shift record")

// ErrWaitTimeout means the requested round has not been published.
var ErrWaitTimeout = errors.New("boss shift wait timed out")

// Record identifies one concrete attempt directory, even if current moves.
type Record struct {
	Dir     string `json:"record"`
	Attempt string `json:"attempt"`
}

// Briefing contains the pointers the candidate needs to start its own review.
type Briefing struct {
	Record
	Role string `json:"role"`
}

var attemptID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,79}$`)

func (r Record) valid() bool {
	return filepath.IsAbs(r.Dir) && attemptID.MatchString(r.Attempt)
}

// Brief validates the pointers for the role and the attempt directory.
func Brief(r Record, role string) (Briefing, error) {
	if !r.valid() || !filepath.IsAbs(role) {
		return Briefing{}, fmt.Errorf("%w: pass absolute record and role paths and a valid attempt ID", ErrRecord)
	}
	dir, err := os.Stat(r.Dir)
	if err != nil || !dir.IsDir() {
		return Briefing{}, fmt.Errorf("%w: record directory unavailable", ErrRecord)
	}
	file, err := os.Stat(role)
	if err != nil || !file.Mode().IsRegular() {
		return Briefing{}, fmt.Errorf("%w: role file unavailable", ErrRecord)
	}
	return Briefing{Record: r, Role: role}, nil
}

// WaitRequest chooses an account, a reply, or the latest ready reply.
type WaitRequest struct {
	Record
	Round        int
	Reply, Ready bool
	Timeout      time.Duration
}

// RoundVerdict carries metadata about a publication, abort, or timeout.
type RoundVerdict struct {
	Status   string `json:"status"`
	Attempt  string `json:"attempt"`
	Round    int    `json:"round"`
	Path     string `json:"path,omitempty"`
	Decision string `json:"decision,omitempty"`
}

// Wait reads bounded round files in the captured attempt directory. A
// timeout of zero probes once.
func Wait(ctx context.Context, req WaitRequest) (RoundVerdict, error) {
	if !req.valid() || req.Round < 0 || req.Round > maxRounds || req.Reply && req.Round == 0 ||
		req.Ready && (req.Reply || req.Round != 0) || req.Timeout < 0 || req.Timeout > 5*time.Minute {
		return RoundVerdict{}, fmt.Errorf("%w: invalid attempt, round, mode or timeout", ErrRecord)
	}
	root, err := os.OpenRoot(req.Dir)
	if err != nil {
		return RoundVerdict{}, fmt.Errorf("%w: open record: %w", ErrRecord, err)
	}
	defer func() { _ = root.Close() }()
	timer := time.NewTimer(req.Timeout)
	defer timer.Stop()
	poll := time.NewTicker(200 * time.Millisecond)
	defer poll.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return RoundVerdict{}, fmt.Errorf("wait for shift round: %w", err)
		}
		v, err := req.probe(root)
		if err != nil || v.Status != waitTimeout {
			return v, err
		}
		if req.Timeout == 0 {
			return v, ErrWaitTimeout
		}
		select {
		case <-ctx.Done():
			return v, fmt.Errorf("wait for shift round: %w", ctx.Err())
		case <-timer.C:
			return v, ErrWaitTimeout
		case <-poll.C:
		}
	}
}

func (req WaitRequest) probe(root *os.Root) (RoundVerdict, error) {
	v := RoundVerdict{Status: waitTimeout, Attempt: req.Attempt, Round: req.Round}
	if _, err := root.Lstat("ABORTED"); !errors.Is(err, fs.ErrNotExist) {
		if err != nil {
			return v, fmt.Errorf("read abort marker: %w", err)
		}
		v.Status = "aborted"
		return v, nil
	}
	blocked, err := root.Lstat("MODEL-BLOCKED")
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return v, fmt.Errorf("read model refusal marker: %w", err)
	}
	first, last := 1, maxRounds
	if req.Round != 0 {
		first, last = req.Round, req.Round
	}
	for n := first; n <= last; n++ {
		account, err := req.read(root, n, false)
		if err != nil {
			return v, err
		}
		if account.path == "" && req.Ready {
			continue
		}
		v = RoundVerdict{Status: waitTimeout, Attempt: req.Attempt, Round: n}
		if account.path == "" {
			break
		}
		reply, err := req.read(root, n, true)
		if err != nil {
			return v, err
		}
		switch {
		case req.Ready:
			// Keep the verdict for the newest published account, even if
			// its reply has not arrived or its review predates a refusal.
			if reply.decision == "ready" && !reply.at.Before(account.at) && (blocked == nil ||
				account.at.After(blocked.ModTime()) && reply.at.After(blocked.ModTime())) {
				v.Status, v.Path, v.Decision = published, reply.path, reply.decision
			}
		case req.Reply:
			if reply.path != "" {
				v.Status, v.Path, v.Decision = published, reply.path, reply.decision
			}
			return v, nil
		case req.Round != 0 || reply.decision != "revise" || n == maxRounds:
			v.Status, v.Path = published, account.path
			return v, nil
		}
	}
	return v, nil
}

type publication struct {
	path, decision string
	at             time.Time
}

func (req WaitRequest) read(root *os.Root, n int, reply bool) (publication, error) {
	kind := "account"
	if reply {
		kind = "reply"
	}
	name := fmt.Sprintf("%s-%d.md", kind, n)
	info, err := root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return publication{}, nil
	}
	if err != nil {
		return publication{}, fmt.Errorf("inspect %s: %w", name, err)
	}
	if !info.Mode().IsRegular() {
		return publication{}, nil
	}
	f, err := root.Open(name)
	if errors.Is(err, fs.ErrNotExist) {
		return publication{}, nil
	}
	if err != nil {
		return publication{}, fmt.Errorf("open %s: %w", name, err)
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, maxRoundBytes+1))
	if err != nil {
		return publication{}, fmt.Errorf("read %s: %w", name, err)
	}
	if len(raw) > maxRoundBytes || !utf8.Valid(raw) {
		return publication{}, nil
	}
	body, ok := strings.CutPrefix(string(raw), fmt.Sprintf("attempt: %s\nround: %d\n", req.Attempt, n))
	if !ok {
		return publication{}, nil
	}
	var decision string
	if reply {
		line, rest, _ := strings.Cut(body, "\n")
		decision, _ = strings.CutPrefix(line, "decision: ")
		if decision != "ready" && decision != "revise" {
			return publication{}, nil
		}
		body = rest
	}
	body, ok = strings.CutPrefix(body, "\n")
	body, complete := strings.CutSuffix(body, roundEnd)
	if !ok || !complete || strings.TrimSpace(body) == "" {
		return publication{}, nil
	}
	return publication{path: filepath.Join(req.Dir, name), decision: decision, at: info.ModTime()}, nil
}
