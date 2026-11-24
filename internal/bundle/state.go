package bundle

import (
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

// ReadState reads the workflow state of one worktree through the bundle's
// convergence.read_state. It requires an absolute directory that resolves
// at or under the repo's real path. An empty work dir would make the server
// search upward from its launch directory. The reply's updated_at must be
// RFC 3339. Stage and ticket are optional strings.
func ReadState(s *Session, workDir string) (State, error) {
	dir, err := s.contain(workDir)
	if err != nil {
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

// contain returns workDir's real path when it is a directory at or under
// the session's repo, compared component by component.
func (s *Session) contain(workDir string) (string, error) {
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
	root, err := filepath.EvalSymlinks(s.cmd.Dir)
	if err != nil {
		return "", fmt.Errorf("%w: the repo: %w", ErrRefused, err)
	}
	if rel, err := filepath.Rel(root, real); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: the work dir %s is outside the repo %s", ErrRefused, real, root)
	}
	return real, nil
}
