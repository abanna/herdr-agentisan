package bundle

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// State is what a worktree's .agentisan/state.json says of its workflow,
// as convergence.read_state reports it.
type State struct {
	// Stage is the step logged last; empty before any step.
	Stage string
	// Ticket is the Linear ticket the worktree was set up for.
	Ticket    string
	UpdatedAt time.Time
	// StateFile is the file the server read.
	StateFile string
}

// ReadState reads a canonical worktree at or under the repo through convergence.read_state.
func ReadState(s *Session, workDir string) (State, error) {
	if s.failed != nil {
		return State{}, s.failed
	}
	root := s.cmd.Dir
	dir, err := ResolveWithin(s.ctx, func() (string, error) { return contain(root, workDir) })
	if err != nil {
		if s.ctx.Err() != nil {
			return State{}, s.broken(err)
		}
		return State{}, err
	}
	raw, err := s.Execute("convergence.read_state", map[string]any{"work_dir": dir})
	if err != nil {
		return State{}, err
	}
	var r struct {
		Status    string `json:"status"`
		StateFile string `json:"state_file"`
		State     struct {
			Stage     *string `json:"stage"`
			UpdatedAt string  `json:"updated_at"`
			Worktree  *struct {
				Ticket *string `json:"ticket"`
			} `json:"worktree"`
		} `json:"state"`
	}
	if err := json.Unmarshal(raw, &r); err != nil || r.Status != "ok" || r.StateFile == "" {
		return State{}, fmt.Errorf("%w: the read_state result is not {status ok, state_file, state}", ErrContract)
	}
	at, err := time.Parse(time.RFC3339, r.State.UpdatedAt)
	if err != nil {
		return State{}, fmt.Errorf("%w: state.updated_at: %w", ErrContract, err)
	}
	st := State{StateFile: r.StateFile, UpdatedAt: at}
	if r.State.Stage != nil {
		st.Stage = *r.State.Stage
	}
	if r.State.Worktree != nil && r.State.Worktree.Ticket != nil {
		st.Ticket = *r.State.Worktree.Ticket
	}
	return st, nil
}

// contain resolves workDir and compares path components against the repo.
func contain(repo, workDir string) (string, error) {
	if !filepath.IsAbs(workDir) {
		return "", fmt.Errorf("%w: the work dir %q is not an absolute path", ErrRefused, workDir)
	}
	real, err := filepath.EvalSymlinks(workDir)
	if err != nil {
		return "", fmt.Errorf("%w: the work dir: %w", ErrRefused, err)
	}
	if info, err := os.Stat(real); err != nil || !info.IsDir() {
		return "", fmt.Errorf("%w: the work dir %s is not a directory", ErrRefused, real)
	}
	root, err := filepath.EvalSymlinks(repo)
	if err != nil {
		return "", fmt.Errorf("%w: the repo: %w", ErrRefused, err)
	}
	if rel, err := filepath.Rel(root, real); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: the work dir %s is outside the repo %s", ErrRefused, real, root)
	}
	return real, nil
}

// ResolveWithin bounds the wait for a read-only lookup, which may finish after cancellation.
func ResolveWithin[T any](ctx context.Context, resolve func() (T, error)) (T, error) {
	var value T
	var err error
	done := make(chan struct{})
	go func() {
		value, err = resolve()
		close(done)
	}()
	select {
	case <-done:
		if ctx.Err() == nil {
			return value, err
		}
	case <-ctx.Done():
	}
	var zero T
	return zero, fmt.Errorf("%w: %w", ErrTimeout, ctx.Err())
}
