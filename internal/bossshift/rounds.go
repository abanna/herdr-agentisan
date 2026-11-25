package bossshift

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	pollEvery     = 200 * time.Millisecond
	maxWait       = 5 * time.Minute

	decisionReady  = "ready"
	decisionRevise = "revise"
)

// A wait's outcome. Everything but timeout exits zero: the caller reads the status.
const (
	StatusPublished = "published"
	StatusTimeout   = "timeout"
	StatusAborted   = "aborted"
	StatusDone      = "done"
	// StatusExhausted means the third round was revised: the review is over.
	StatusExhausted = "exhausted"
)

// ErrRecord means the requested shift directory or options cannot be used.
var ErrRecord = errors.New("invalid boss shift record")

// ErrWaitTimeout means the requested round has not been published.
var ErrWaitTimeout = errors.New("boss shift wait timed out")

// Record is one attempt's directory, named by its ID. Callers pass the folder
// they resolved once, never the `current` link, so a replaced shift is never read.
type Record struct {
	Dir     string `json:"record"`
	Attempt string `json:"attempt"`
}

var attemptID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,79}$`)

func (r Record) valid() bool {
	return filepath.IsAbs(r.Dir) && attemptID.MatchString(r.Attempt) && filepath.Base(r.Dir) == r.Attempt
}

// Briefing holds the pointers the candidate needs to start its review.
type Briefing struct {
	Record
	Role string `json:"role"`
}

// Brief checks the attempt directory and the role file and names both.
func Brief(r Record, role string) (Briefing, error) {
	if !r.valid() || !filepath.IsAbs(role) {
		return Briefing{}, fmt.Errorf("%w: pass absolute record and role paths and the attempt directory's name", ErrRecord)
	}
	if dir, err := os.Stat(r.Dir); err != nil || !dir.IsDir() {
		return Briefing{}, fmt.Errorf("%w: record directory unavailable", ErrRecord)
	}
	if file, err := os.Stat(role); err != nil || !file.Mode().IsRegular() {
		return Briefing{}, fmt.Errorf("%w: role file unavailable", ErrRecord)
	}
	return Briefing{Record: r, Role: role}, nil
}

// WaitRequest picks the account that awaits a reply (Round 0), one round's
// account, or (Reply) a round's reply. A zero Timeout probes once.
type WaitRequest struct {
	Record
	Round   int
	Reply   bool
	Timeout time.Duration
}

// RoundVerdict says what a wait found. SHA256 is the account's digest: the
// reply carries it, so an approval names the bytes it reviewed.
type RoundVerdict struct {
	Status   string `json:"status"`
	Attempt  string `json:"attempt"`
	Round    int    `json:"round"`
	Path     string `json:"path,omitempty"`
	Decision string `json:"decision,omitempty"`
	SHA256   string `json:"sha256,omitempty"`
	// Text is the account file as read, whose SHA-256 is SHA256: the reviewer reads these bytes, not a path.
	Text string `json:"text,omitempty"`
}

// Wait reads bounded round files in the one attempt directory it was given.
func Wait(ctx context.Context, req WaitRequest) (RoundVerdict, error) {
	return waitBetween(ctx, req, nil)
}

// waitBetween is Wait with a hook that runs after each round's first read, so a test can publish
// in the gap between the two reads.
func waitBetween(ctx context.Context, req WaitRequest, between func()) (RoundVerdict, error) {
	if !req.valid() || req.Round < 0 || req.Round > maxRounds || req.Reply && req.Round == 0 ||
		req.Timeout < 0 || req.Timeout > maxWait {
		return RoundVerdict{}, fmt.Errorf("%w: invalid attempt, round, mode or timeout", ErrRecord)
	}
	root, err := os.OpenRoot(req.Dir)
	if err != nil {
		return RoundVerdict{}, fmt.Errorf("%w: open record: %w", ErrRecord, err)
	}
	defer func() { _ = root.Close() }()
	timer := time.NewTimer(req.Timeout)
	defer timer.Stop()
	poll := time.NewTicker(pollEvery)
	defer poll.Stop()
	for {
		v, err := req.probe(root, between)
		if err != nil || v.Status != StatusTimeout {
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

func (req WaitRequest) probe(root *os.Root, between func()) (RoundVerdict, error) {
	v := RoundVerdict{Status: StatusTimeout, Attempt: req.Attempt, Round: req.Round}
	for _, marker := range []struct{ name, status string }{{"ABORTED", StatusAborted}, {"DONE", StatusDone}} {
		_, err := root.Lstat(marker.name)
		if err == nil {
			v.Status = marker.status
			return v, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return v, fmt.Errorf("read %s marker: %w", marker.name, err)
		}
	}
	first, last := 1, maxRounds
	if req.Round != 0 {
		first, last = req.Round, req.Round
	}
	for n := first; n <= last; n++ {
		// The reply is read before the account it must name: a replacement landing between the
		// two reads shows as a mismatch, so no verdict rests on bytes that are already gone.
		reply, err := readRound(root, req.Attempt, n, true)
		if err != nil {
			return v, err
		}
		if between != nil {
			between()
		}
		account, err := readRound(root, req.Attempt, n, false)
		v.Round = n
		if err != nil || !account.valid {
			return v, err
		}
		// A reply answers the bytes it names and no others.
		answered := reply.valid && reply.account == account.digest
		switch {
		case req.Reply && !answered:
			return v, nil
		case req.Reply:
			v.Status, v.Path, v.Decision = StatusPublished, reply.path, reply.decision
		case answered && req.Round == 0 && n < maxRounds:
			continue // the next account is awaited
		default:
			v.Status, v.Path, v.SHA256, v.Text = StatusPublished, account.path, account.digest, account.text
			if answered {
				v.Decision = reply.decision
			}
		}
		if answered && n == maxRounds && reply.decision == decisionRevise {
			v.Status = StatusExhausted
		}
		return v, nil
	}
	return v, nil
}

type publication struct {
	path, decision, account, digest, text string
	exists, valid                         bool
}

// readRound reads account-N.md or reply-N.md. A file that is not regular, too
// big, not UTF-8 or not complete is not a publication.
func readRound(root *os.Root, attempt string, n int, reply bool) (publication, error) {
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
	p := publication{exists: true}
	if !info.Mode().IsRegular() {
		return p, nil
	}
	f, err := root.Open(name)
	if err != nil {
		return p, fmt.Errorf("open %s: %w", name, err)
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, maxRoundBytes+1))
	if err != nil {
		return p, fmt.Errorf("read %s: %w", name, err)
	}
	if len(raw) > maxRoundBytes || !utf8.Valid(raw) {
		return p, nil
	}
	body, ok := strings.CutPrefix(string(raw), fmt.Sprintf("attempt: %s\nround: %d\n", attempt, n))
	if reply && ok {
		var decision, account string
		decision, body, _ = strings.Cut(body, "\n")
		account, body, _ = strings.Cut(body, "\n")
		p.decision, _ = strings.CutPrefix(decision, "decision: ")
		p.account, _ = strings.CutPrefix(account, "account: ")
		ok = (p.decision == decisionReady || p.decision == decisionRevise) && len(p.account) == 2*sha256.Size
		if _, err := hex.DecodeString(p.account); err != nil {
			ok = false
		}
	}
	body, blank := strings.CutPrefix(body, "\n")
	body, complete := strings.CutSuffix(body, roundEnd)
	if !ok || !blank || !complete || strings.TrimSpace(body) == "" {
		return publication{exists: true}, nil
	}
	sum := sha256.Sum256(raw)
	p.path, p.digest, p.text, p.valid = filepath.Join(root.Name(), name), hex.EncodeToString(sum[:]), string(raw), true
	return p, nil
}
