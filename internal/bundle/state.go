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

// ReadInstalledState discovers, starts, reads and closes the bundle session.
func ReadInstalledState(ctx context.Context, locate func(string) (Launch, error), repo, workDir string) (State, error) {
	return resolveWithin(ctx, 250*time.Millisecond, func() (st State, err error) {
		paths, err := resolveWorktree(repo, workDir)
		if err != nil {
			return State{}, err
		}
		launch, err := locate(paths.repo)
		if err != nil {
			return State{}, err
		}
		launch.Dir = paths.repo
		s, err := Open(ctx, launch)
		if err != nil {
			return State{}, err
		}
		defer func() {
			if closeErr := s.Close(); err == nil {
				err = closeErr
			}
		}()
		return readResolvedState(s, paths.dir)
	})
}

// ReadState reads a canonical worktree at or under the repo through convergence.read_state.
func ReadState(s *Session, workDir string) (State, error) {
	if s.failed != nil {
		return State{}, s.failed
	}
	root := s.cmd.Dir
	paths, err := resolveWithin(s.ctx, 0, func() (worktree, error) { return resolveWorktree(root, workDir) })
	if err != nil {
		if s.ctx.Err() != nil {
			return State{}, s.broken(err)
		}
		return State{}, err
	}
	return readResolvedState(s, paths.dir)
}

func readResolvedState(s *Session, dir string) (State, error) {
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

type worktree struct{ repo, dir string }

func resolveWorktree(repo, workDir string) (worktree, error) {
	root, err := canonicalDir(repo, ErrUnavailable)
	if err != nil {
		return worktree{}, err
	}
	dir, err := canonicalDir(workDir, ErrRefused)
	if err != nil {
		return worktree{}, err
	}
	if rel, err := filepath.Rel(root, dir); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return worktree{}, fmt.Errorf("%w: the work dir %s is outside the repo %s", ErrRefused, dir, root)
	}
	return worktree{repo: root, dir: dir}, nil
}

func canonicalDir(path string, unavailable error) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("%w: %q is not an absolute path", ErrRefused, path)
	}
	dir, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("%w: %q: %w", unavailable, path, err)
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("%w: %q: %w", unavailable, path, err)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return "", fmt.Errorf("%w: %s is not a directory", unavailable, dir)
	}
	return dir, nil
}

// resolveWithin bounds the caller's wait and allows cleanup grace after cancellation.
func resolveWithin[T any](ctx context.Context, grace time.Duration, resolve func() (T, error)) (T, error) {
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
		if grace > 0 {
			select {
			case <-done:
			case <-time.After(grace):
			}
		}
	}
	var zero T
	return zero, fmt.Errorf("%w: %w", ErrTimeout, ctx.Err())
}
