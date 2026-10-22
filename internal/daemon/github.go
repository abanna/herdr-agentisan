package daemon

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/abanna/herdr-agentisan/internal/github"
	"github.com/abanna/herdr-agentisan/internal/settings"
	"github.com/abanna/herdr-agentisan/internal/snapshot"
)

// GitHubInterval is how often the daemon collects each project's header
// from GitHub (ADR-001 A29): herdr-status.sh's 60 s.
const GitHubInterval = 60 * time.Second

// githubKeep is how many intervals a project's last good PRs and issue
// count stay reported once collecting them fails.
const githubKeep = 5

// GitHubOptions configure the header's GitHub collection (A29), which runs
// beside the team poll.
type GitHubOptions struct {
	// Gh is gh's absolute path (github.Find). Empty means gh was not found:
	// with a repo to collect, that is logged once and no PRs or issues are
	// reported.
	Gh string
	// Env is gh's whole environment; nil is the daemon's own, which carries
	// what gh's keyring login needs.
	Env []string
	// Interval is how often every repo is collected; zero is GitHubInterval.
	Interval time.Duration
	// ticks, when set, stands in for the interval's ticker (tests).
	ticks <-chan time.Time
	// rounds, when set, is sent each round's end, once every repo's calls
	// and notices are done (tests).
	rounds chan<- struct{}
}

func (g GitHubOptions) interval() time.Duration {
	if g.Interval <= 0 {
		return GitHubInterval
	}
	return g.Interval
}

// headerCache is each project's GitHub state as last collected. The
// collector writes it and the team poll copies it into each snapshot, so
// the snapshot op never runs gh, and GitHub is asked once an interval
// however many dashboards read.
type headerCache struct {
	mu   sync.Mutex
	keep time.Duration
	by   map[string]header
}

// header is one project's last good PRs and issue count, and when each was
// read; a zero time is never.
type header struct {
	prs      []snapshot.PR
	prsAt    time.Time
	issues   int
	issuesAt time.Time
}

func newHeaderCache(g GitHubOptions) *headerCache {
	return &headerCache{keep: githubKeep * g.interval(), by: map[string]header{}}
}

func (c *headerCache) putPRs(project string, prs []snapshot.PR, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	h := c.by[project]
	h.prs, h.prsAt = prs, at
	c.by[project] = h
}

func (c *headerCache) putIssues(project string, n int, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	h := c.by[project]
	h.issues, h.issuesAt = n, at
	c.by[project] = h
}

// fill copies project's header into s: what was read within keep of now,
// and nothing older, so stale state is shown for at most githubKeep
// intervals and then not at all. With nothing to copy, s is left as it is.
func (c *headerCache) fill(project string, s *snapshot.Snapshot, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	h, ok := c.by[project]
	if !ok {
		return
	}
	fresh := func(at time.Time) bool { return !at.IsZero() && now.Sub(at) <= c.keep }
	var prs []snapshot.PR
	if fresh(h.prsAt) {
		prs = slices.Clone(h.prs)
	}
	var issues *int
	if fresh(h.issuesAt) {
		issues = new(h.issues)
	}
	if prs == nil && issues == nil {
		return
	}
	if s.Project == nil {
		s.Project = &snapshot.Project{}
	}
	s.Project.PRs, s.Project.Issues = prs, issues
}

// collectGitHub collects every configured repo's PRs and open-issue count
// now and every interval until ctx ends, into c. The repos are read once,
// here, as the team poll reads its project. A failed call is logged when a
// repo starts failing, or fails differently, and when it recovers, not
// every interval; its last good state stays reported (fill). With no repo
// to collect, no gh, or a configuration that does not resolve, it collects
// nothing. After both calls, each successful PR list goes to notices (A45).
func (o Options) collectGitHub(ctx context.Context, c *headerCache, notices *githubNotices) {
	g := o.Team.GitHub
	repos, err := settings.Headers(o.Team.ConfigDir)
	if err != nil {
		o.Logger.Warn().Err(err).Msg("github: herdr-agentisan.toml does not resolve; the header reports no PRs or issues until it is fixed and the daemon restarts")
		return
	}
	if len(repos) == 0 {
		return
	}
	if g.Gh == "" {
		o.Logger.Warn().Msg("github: gh is not on the daemon's PATH; the header reports no PRs or issues until it is installed and the daemon restarts")
		return
	}
	names := make([]string, 0, len(repos))
	for _, h := range repos {
		names = append(names, h.GitHub)
	}
	o.Logger.Info().Strs("repos", names).Dur("interval", g.interval()).Msg("github: collecting")
	ticks := g.ticks
	if ticks == nil {
		tick := time.NewTicker(g.interval())
		defer tick.Stop()
		ticks = tick.C
	}
	gh := github.Runner{Bin: g.Gh, Env: g.Env}
	failing := map[string]string{}
	for {
		for _, h := range repos {
			open, err := gh.OpenPRs(ctx, h.GitHub)
			if ctx.Err() != nil {
				return
			}
			listed := o.noteGitHub(failing, h, "prs", err)
			if listed {
				c.putPRs(h.Project, open.Headers(), o.Now())
			}
			n, err := gh.OpenIssues(ctx, h.GitHub)
			if ctx.Err() != nil {
				return
			}
			if o.noteGitHub(failing, h, "issues", err) {
				c.putIssues(h.Project, n, o.Now())
			}
			if listed {
				notices.collected(ctx, gh, h, open)
			}
		}
		if g.rounds != nil {
			select {
			case <-ctx.Done():
				return
			case g.rounds <- struct{}{}:
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticks:
		}
	}
}

// noteGitHub logs one call's outcome when it changes, and reports whether
// it succeeded. failing holds each repo's and call's last logged failure.
func (o Options) noteGitHub(failing map[string]string, h settings.Header, call string, err error) bool {
	key := h.Project + "\x00" + call
	last, was := failing[key]
	if err == nil {
		if was {
			delete(failing, key)
			o.Logger.Info().Str("project", h.Project).Str("repo", h.GitHub).Str("call", call).Msg("github: collecting again")
		}
		return true
	}
	if msg := err.Error(); !was || msg != last {
		failing[key] = msg
		o.Logger.Warn().Err(err).Str("project", h.Project).Str("repo", h.GitHub).Str("call", call).
			Msg("github: collection failed; the last good state stays reported for 5 intervals")
	}
	return false
}
