// Package github reads what the dashboard header shows of a project's
// GitHub repo (ADR-001 D5, A29): its open pull requests, each with its CI,
// Codex and merge state, and its open-issue count. It runs the gh CLI, so
// gh's own login (the keyring, GH_TOKEN or GITHUB_TOKEN) is the only
// credential, and no token passes through this package.
package github

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/abanna/herdr-agentisan/internal/snapshot"
)

const (
	// DefaultTimeout bounds one gh call.
	DefaultTimeout = 15 * time.Second
	// DefaultMaxOutput bounds what one gh call may print.
	DefaultMaxOutput = 4 << 20
	// PRLimit is how many open PRs one call lists, newest first.
	PRLimit = 100
)

// waitDelay bounds how long a call waits for gh's pipes to close once gh
// has exited or been killed, in case something it started holds them.
const waitDelay = 2 * time.Second

// maxReason bounds how much of gh's stderr an error carries.
const maxReason = 200

// The fields `gh pr list` is asked for: what herdr-status.sh asked for, and
// the head commit the boss's notices are keyed by (A45).
const prFields = "number,headRefName,headRefOid,mergeStateStatus,reactionGroups,statusCheckRollup"

// issuesQuery counts a repo's open issues exactly; `gh issue list` caps
// what it lists.
const issuesQuery = `query($owner: String!, $name: String!) { repository(owner: $owner, name: $name) { issues(states: OPEN) { totalCount } } }`

// repoPattern is owner/name as GitHub allows them: an owner of at most 39
// characters, a name of at most 100. Settings checks the same shape,
// unbounded. A repo starting with '-' cannot match.
var repoPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}/[A-Za-z0-9._-]{1,100}$`)

var (
	// ErrNoGh means gh cannot be run: no absolute path to it, or it would
	// not start.
	ErrNoGh = errors.New("gh cannot be run")
	// ErrRepo means a repo is not owner/name.
	ErrRepo = errors.New("not a GitHub repo as owner/name")
	// ErrNumber means a PR number is not one GitHub gives: below 1.
	ErrNumber = errors.New("not a PR number")
	// ErrGh means gh ran and failed: not logged in, no such repo, rate
	// limited, killed. The error carries the first line of its stderr.
	ErrGh = errors.New("gh failed")
	// ErrTimeout means gh did not finish within the timeout and was killed.
	ErrTimeout = errors.New("gh timed out")
	// ErrOutput means gh printed something other than what it was asked
	// for, or more than the cap.
	ErrOutput = errors.New("gh's output is not what was asked for")
)

// Runner runs gh.
type Runner struct {
	// Bin is gh's absolute path. A relative one, even "gh", is never looked
	// up: Find does that, against the PATH it is given.
	Bin string
	// Env is gh's whole environment; nil is this process's own. Either way
	// gh also gets GH_PROMPT_DISABLED, NO_COLOR and GH_NO_UPDATE_NOTIFIER.
	Env []string
	// Timeout bounds one call; zero is DefaultTimeout.
	Timeout time.Duration
	// MaxOutput bounds what one call may print; zero is DefaultMaxOutput.
	MaxOutput int
}

// Find returns gh's path in path, a PATH value: the first absolute
// directory in it holding an executable file named gh, or "" when none
// does. Relative entries are skipped, since they name wherever the daemon
// happened to start.
func Find(path string) string {
	for _, dir := range filepath.SplitList(path) {
		if !filepath.IsAbs(dir) {
			continue
		}
		p := filepath.Join(dir, "gh")
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0 {
			return p
		}
	}
	return ""
}

// PRs lists repo's open pull requests, release-please's skipped, by number.
func (r Runner) PRs(ctx context.Context, repo string) ([]snapshot.PR, error) {
	o, err := r.OpenPRs(ctx, repo)
	if err != nil {
		return nil, err
	}
	return o.Headers(), nil
}

// OpenPRs lists repo's open pull requests as PRs does, each with its head
// and check counts.
func (r Runner) OpenPRs(ctx context.Context, repo string) (Open, error) {
	if err := checkRepo(repo); err != nil {
		return Open{}, err
	}
	out, err := r.run(ctx, "pr", "list", "--repo="+repo, "--state=open", fmt.Sprintf("--limit=%d", PRLimit), "--json="+prFields)
	if err != nil {
		return Open{}, err
	}
	return ParseOpenPRs(out)
}

// PRView reads the state of repo's PR n, which has left the open list.
func (r Runner) PRView(ctx context.Context, repo string, n int) (PRState, error) {
	if err := checkRepo(repo); err != nil {
		return PRState{}, err
	}
	if n < 1 {
		return PRState{}, fmt.Errorf("%w: %d", ErrNumber, n)
	}
	out, err := r.run(ctx, "pr", "view", strconv.Itoa(n), "--repo="+repo, "--json=state,mergeCommit")
	if err != nil {
		return PRState{}, err
	}
	return ParsePRView(out)
}

// OpenIssues counts repo's open issues, exactly.
func (r Runner) OpenIssues(ctx context.Context, repo string) (int, error) {
	if err := checkRepo(repo); err != nil {
		return 0, err
	}
	owner, name, _ := strings.Cut(repo, "/")
	out, err := r.run(ctx, "api", "graphql", "-f", "query="+issuesQuery, "-f", "owner="+owner, "-f", "name="+name)
	if err != nil {
		return 0, err
	}
	return ParseOpenIssues(out)
}

func checkRepo(repo string) error {
	if !repoPattern.MatchString(repo) || strings.HasSuffix(repo, "/.") || strings.HasSuffix(repo, "/..") {
		return fmt.Errorf("%w: %q", ErrRepo, repo)
	}
	return nil
}

// run runs gh with args under the timeout, as the leader of a process group
// of its own: a timeout, a cancel or too much output kills the whole group,
// so nothing gh started outlives the call. It returns what gh printed.
func (r Runner) run(ctx context.Context, args ...string) ([]byte, error) {
	if !filepath.IsAbs(r.Bin) {
		return nil, fmt.Errorf("%w: no absolute path to gh (%q)", ErrNoGh, r.Bin)
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	maxOut := r.MaxOutput
	if maxOut <= 0 {
		maxOut = DefaultMaxOutput
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// #nosec G204 -- Bin is the gh the daemon found; args are fixed verbs and flags, the repo checked against repoPattern, each one argv element with no shell.
	cmd := exec.CommandContext(callCtx, r.Bin, args...)
	env := r.Env
	if env == nil {
		env = os.Environ()
	}
	cmd.Env = append(slices.Clip(env), "GH_PROMPT_DISABLED=1", "NO_COLOR=1", "GH_NO_UPDATE_NOTIFIER=1")
	stdout := &capped{max: maxOut, full: cancel}
	stderr := &capped{max: maxReason * 4}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.WaitDelay = waitDelay
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return killGroup(syscall.Kill, cmd.Process.Pid, "gh") }
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrNoGh, r.Bin, err)
	}
	err := cmd.Wait()
	verb := strings.Join(args[:2], " ")
	switch {
	case stdout.over():
		return nil, fmt.Errorf("gh %s: %w: more than %d bytes", verb, ErrOutput, maxOut)
	case ctx.Err() != nil:
		return nil, fmt.Errorf("gh %s: %w", verb, ctx.Err())
	case errors.Is(callCtx.Err(), context.DeadlineExceeded):
		return nil, fmt.Errorf("gh %s: %w after %v", verb, ErrTimeout, timeout)
	case err != nil:
		if reason := firstLine(stderr.String()); reason != "" {
			return nil, fmt.Errorf("gh %s: %w: %w: %s", verb, ErrGh, err, reason)
		}
		return nil, fmt.Errorf("gh %s: %w: %w", verb, ErrGh, err)
	}
	return stdout.Bytes(), nil
}

// killGroup sends SIGKILL, through kill (syscall.Kill), to the process group
// that pid leads, and is what run's Cancel does. A group already gone is
// os.ErrProcessDone, which exec takes as done. Any other failure goes back
// to exec, which kills the leader alone once WaitDelay passes, and run still
// reports the cancel, the timeout or the output cap. kill passes over a
// member the daemon may not signal, so an EPERM means it could signal none
// of them, the leader included: exec's kill fails too, and run waits for the
// leader to exit.
func killGroup(kill func(pid int, sig syscall.Signal) error, pid int, what string) error {
	err := kill(-pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	if err != nil {
		return fmt.Errorf("kill %s's process group: %w", what, err)
	}
	return nil
}

// capped keeps the first max bytes written to it and drops the rest. Going
// past max calls full, once, so a gh that floods is stopped then rather than
// at the timeout; the write itself never fails, so gh is never left blocked
// on a pipe nobody reads.
type capped struct {
	mu   sync.Mutex
	buf  []byte
	max  int
	past bool
	full func()
}

func (c *capped) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if room := c.max - len(c.buf); len(p) > room {
		c.buf = append(c.buf, p[:max(room, 0)]...)
		if !c.past && c.full != nil {
			c.full()
		}
		c.past = true
		return len(p), nil
	}
	c.buf = append(c.buf, p...)
	return len(p), nil
}

func (c *capped) over() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.past
}

func (c *capped) Bytes() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.buf)
}

func (c *capped) String() string { return string(c.Bytes()) }

// firstLine is the first line of gh's stderr, cut to maxReason bytes and
// kept valid UTF-8.
func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	if len(line) > maxReason {
		line = line[:maxReason]
	}
	return strings.TrimSpace(strings.ToValidUTF8(line, ""))
}
