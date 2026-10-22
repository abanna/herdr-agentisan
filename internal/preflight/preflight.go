// Package preflight holds the checks a worker spawn must pass first
// (ADR-001 D12, I8, A10, A34). Tdaddy is the one for Go repos: in a repo
// with a go.mod, tdaddy must run from the daemon's PATH, and the repo's
// Taskfile must have the tdaddy:index and tdaddy:impact tasks. The check
// reads all of that through a System, so a test never searches the real
// PATH or runs the real tdaddy.
package preflight

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

// The Taskfile tasks a Go repo must have, and the bound on one run.
const (
	// TaskIndex is the task that rebuilds tdaddy's graph.
	TaskIndex = "tdaddy:index"
	// TaskImpact is the task that reports which tests a change affects.
	TaskImpact = "tdaddy:impact"
	// DefaultTimeout bounds one `tdaddy --version`.
	DefaultTimeout = 10 * time.Second
)

// waitDelay bounds how long a run waits for tdaddy's pipes to close once it
// has exited or been killed, in case something it started holds them.
const waitDelay = 2 * time.Second

// maxOutput bounds what a run keeps of tdaddy's output, and maxReason how
// much of it an error carries.
const (
	maxOutput = 4 << 10
	maxReason = 200
)

// taskfiles are the names Task looks for in a directory, in the order it
// looks (Task 3.49): the first one there is the Taskfile.
var taskfiles = []string{
	"Taskfile.yml", "taskfile.yml", "Taskfile.yaml", "taskfile.yaml",
	"Taskfile.dist.yml", "taskfile.dist.yml", "Taskfile.dist.yaml", "taskfile.dist.yaml",
}

// Sentinel errors. Callers branch on these with errors.Is; each message says
// what to fix.
var (
	// ErrRepo means the check could not tell whether the repo is a Go repo:
	// the path is not absolute, is not a directory the daemon can stat, or its
	// go.mod could not be checked.
	ErrRepo = errors.New("cannot check the repo")
	// ErrTdaddyMissing means tdaddy is not on the daemon's PATH, or is there
	// but `tdaddy --version` does not exit 0 in the repo.
	ErrTdaddyMissing = errors.New("tdaddy is not set up")
	// ErrTdaddyTasks means the repo has no Taskfile, or its Taskfile cannot
	// be read or parsed, or lacks tdaddy:index or tdaddy:impact.
	ErrTdaddyTasks = errors.New("the repo's Taskfile is not set up for tdaddy")
	// ErrTimeout means `tdaddy --version` gave no answer within the Runner's
	// timeout and was killed. Runner.Run returns it as it is; Tdaddy wraps it
	// in ErrTdaddyMissing.
	ErrTimeout = errors.New("tdaddy timed out")
)

// System is what the check reads: the daemon's PATH, the repo's files, and a
// way to run tdaddy. Host is the real one.
type System struct {
	// LookPath finds a program on the PATH, as exec.LookPath does.
	LookPath func(file string) (string, error)
	// Stat and ReadFile read the repo's files, as os.Stat and os.ReadFile do.
	Stat     func(name string) (fs.FileInfo, error)
	ReadFile func(name string) ([]byte, error)
	// Run runs bin with args in dir. An error means it did not exit 0.
	Run func(ctx context.Context, dir, bin string, args ...string) error
}

// Host is the check against the daemon's own PATH, files and processes.
func Host() System {
	return System{LookPath: exec.LookPath, Stat: os.Stat, ReadFile: os.ReadFile, Run: Runner{}.Run}
}

// Check is a spawn precondition for the repo at repo, the absolute path of a
// project's repo. `worker spawn` and `team up` (NERD-5254 parts 4 and 5) run
// one before they open a pane.
type Check func(ctx context.Context, repo string) error

var _ Check = HostCheck

// HostCheck is Tdaddy against the daemon's own system: the Check a spawn is
// to run unless a test gives it another.
func HostCheck(ctx context.Context, repo string) error {
	return Tdaddy(ctx, repo, Host())
}

// Tdaddy is D12's check for a Go repo, as A10 and A34 define it. The repo
// must be a directory; one with no go.mod passes at once. In a Go repo,
// tdaddy must be on the PATH and `tdaddy --version` must exit 0 in the repo,
// and the repo's Taskfile must have tdaddy:index and tdaddy:impact. It
// reports every failure it finds, so one refusal says everything there is to
// fix.
func Tdaddy(ctx context.Context, repo string, sys System) error {
	if !filepath.IsAbs(repo) {
		return fmt.Errorf("%w: %q is not an absolute path; give the project an absolute repo path", ErrRepo, repo)
	}
	const fixRepo = "check the project's repo path: it must be a directory the daemon can read"
	switch fi, err := sys.Stat(repo); {
	case err != nil:
		return fmt.Errorf("%w: %w; %s", ErrRepo, err, fixRepo)
	case !fi.IsDir():
		return fmt.Errorf("%w: %s is not a directory; %s", ErrRepo, repo, fixRepo)
	}
	gomod := filepath.Join(repo, "go.mod")
	fi, err := sys.Stat(gomod)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("%w: %w; %s", ErrRepo, err, fixRepo)
	case fi.IsDir():
		return fmt.Errorf("%w: %s is a directory, not a module file; remove or rename it", ErrRepo, gomod)
	}
	runs := tdaddyRuns(ctx, repo, sys)
	if ctx.Err() != nil {
		return fmt.Errorf("tdaddy preflight: %w", ctx.Err())
	}
	return errors.Join(runs, hasTasks(repo, sys))
}

// tdaddyRuns finds tdaddy on the PATH and runs `tdaddy --version` in repo.
func tdaddyRuns(ctx context.Context, repo string, sys System) error {
	bin, err := sys.LookPath("tdaddy")
	if err != nil {
		return fmt.Errorf("%w: no tdaddy on the daemon's PATH (%w); put tdaddy on the PATH herdr starts the daemon with, then restart the daemon", ErrTdaddyMissing, err)
	}
	if err := sys.Run(ctx, repo, bin, "--version"); err != nil {
		return fmt.Errorf("%w: %s --version failed in %s: %w; fix it so it runs there (restart the daemon if the fix changes its PATH)", ErrTdaddyMissing, bin, repo, err)
	}
	return nil
}

// hasTasks reads the Taskfile Task would use in repo and checks its
// top-level tasks for tdaddy:index and tdaddy:impact. Includes are not
// followed.
func hasTasks(repo string, sys System) error {
	fix := fmt.Sprintf("add %s and %s tasks that run tdaddy, as herdr-agentisan's Taskfile does", TaskIndex, TaskImpact)
	for _, name := range taskfiles {
		path := filepath.Join(repo, name)
		raw, err := sys.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("%w: %w; make it a regular file the daemon can read", ErrTdaddyTasks, err)
		}
		var tf struct {
			Tasks map[string]yaml.Node `yaml:"tasks"`
		}
		if err := yaml.Unmarshal(raw, &tf); err != nil {
			return fmt.Errorf("%w: %s does not parse: %w; fix it so `task --list-all` reads it", ErrTdaddyTasks, path, err)
		}
		var missing []string
		for _, task := range []string{TaskIndex, TaskImpact} {
			if _, ok := tf.Tasks[task]; !ok {
				missing = append(missing, task)
			}
		}
		if len(missing) > 0 {
			return fmt.Errorf("%w: %s has no %s task; %s", ErrTdaddyTasks, path, strings.Join(missing, " or "), fix)
		}
		return nil
	}
	return fmt.Errorf("%w: %s has no Taskfile; %s", ErrTdaddyTasks, repo, fix)
}

// Runner runs tdaddy for the check: in the repo, under a timeout, as the
// leader of a process group of its own, so a timeout or a cancel kills
// everything it started (a mise shim runs tdaddy as its child).
type Runner struct {
	// Env is tdaddy's environment; nil means the daemon's own.
	Env []string
	// Timeout bounds one run; zero is DefaultTimeout.
	Timeout time.Duration
}

// Run runs bin, an absolute path, with args in dir. An error carries the
// first line of what bin printed.
func (r Runner) Run(ctx context.Context, dir, bin string, args ...string) error {
	if !filepath.IsAbs(bin) {
		return fmt.Errorf("no absolute path to run (%q)", bin)
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// #nosec G204 -- bin is the tdaddy LookPath found on the daemon's PATH and the caller fixes args (--version); each is one argv element with no shell.
	cmd := exec.CommandContext(callCtx, bin, args...)
	cmd.Dir = dir
	if r.Env != nil {
		cmd.Env = slices.Clip(r.Env)
	}
	out := &capped{max: maxOutput}
	cmd.Stdout, cmd.Stderr = out, out
	cmd.WaitDelay = waitDelay
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		if err != nil {
			return fmt.Errorf("kill %s's process group: %w", bin, err)
		}
		return nil
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", bin, err)
	}
	err := cmd.Wait()
	switch {
	case ctx.Err() != nil:
		return fmt.Errorf("%s: %w", bin, ctx.Err())
	case errors.Is(callCtx.Err(), context.DeadlineExceeded):
		return fmt.Errorf("%w: no answer within %v", ErrTimeout, timeout)
	case err != nil:
		if reason := firstLine(out.String()); reason != "" {
			return fmt.Errorf("%w: %s", err, reason)
		}
		return fmt.Errorf("%s: %w", bin, err)
	}
	return nil
}

// capped keeps the first max bytes written to it and drops the rest. The
// write itself never fails, so tdaddy is never left blocked on a pipe
// nobody reads.
type capped struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (c *capped) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if room := c.max - len(c.buf); room > 0 {
		c.buf = append(c.buf, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

func (c *capped) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return string(c.buf)
}

// firstLine is the first line of s, cut to maxReason bytes and kept valid
// UTF-8.
func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	if len(line) > maxReason {
		line = line[:maxReason]
	}
	return strings.TrimSpace(strings.ToValidUTF8(line, ""))
}
