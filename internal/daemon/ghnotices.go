package daemon

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/abanna/herdr-agentisan/internal/github"
	"github.com/abanna/herdr-agentisan/internal/notice"
	"github.com/abanna/herdr-agentisan/internal/settings"
	"github.com/abanna/herdr-agentisan/internal/store"
)

// githubLookups bounds how many gh calls one collection makes per repo
// beyond the header's two, each of up to 15 s: the PRs that left the open
// list, lowest number first, then the new reviews' counts. The rest wait
// for the next interval.
const githubLookups = 10

// noticeQueue carries notices from a source on another goroutine to the
// team poll's, which adds them to its Batcher before each flush.
type noticeQueue struct {
	mu sync.Mutex
	ns []notice.Notice
}

func (q *noticeQueue) add(ns []notice.Notice) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.ns = append(q.ns, ns...)
}

func (q *noticeQueue) take() []notice.Notice {
	q.mu.Lock()
	defer q.mu.Unlock()
	ns := q.ns
	q.ns = nil
	return ns
}

// githubNotices tells each project's boss what its repo shows (ADR-001 A45):
// after each successful open-PR list, the collector diffs it against
// state.db and hands what is new to the team poll. An event is recorded as
// told only once the sink accepted it, in its notice's ack, so a restart, a
// refused message or a failed write tells it again rather than never.
type githubNotices struct {
	o  Options
	st *store.Store
	n  *notices
	mu sync.Mutex
	// pending holds each event handed to the poll and not yet acknowledged,
	// until its project's gap plus one interval: by then the Batcher has
	// sent it, or lost it to a refused message or the size cut, and it is
	// told again.
	pending map[githubKey]time.Time
	// full and failing are the collector's own: the repos whose last list
	// came back at the limit, and each lookup's last logged failure.
	full    map[string]bool
	failing map[string]string
	// betweenReads, when set, runs between collected's read of what is held
	// and its read of state.db: a test's acknowledgement landing there.
	betweenReads func()
}

// githubKey is one event of one project's repo.
type githubKey struct {
	project, repo string
	e             store.GitHubEvent
}

func (o Options) newGitHubNotices(st *store.Store, n *notices) *githubNotices {
	return &githubNotices{o: o, st: st, n: n, pending: map[githubKey]time.Time{}, full: map[string]bool{}, failing: map[string]string{}}
}

// collected diffs h's successful list, open, against state.db. A repo's first
// one is the seed: everything true then is recorded, and nothing is told;
// the reviews' first is theirs, in a repo seeded before they were told. A
// PR the last list held and this one does not is looked up, unless the list
// may be cut off at the limit, and then each new review, for its counts.
func (g *githubNotices) collected(ctx context.Context, gh github.Runner, h settings.Header, open github.Open) {
	log := g.o.Logger.With().Str("project", h.Project).Str("repo", h.GitHub).Logger()
	// held is read before the state: an event acknowledged in between is
	// then in one or the other, never told twice. Only this goroutine adds
	// to pending, so nothing held is missing from it.
	held := g.held()
	if g.betweenReads != nil {
		g.betweenReads()
	}
	state, err := g.st.GitHub(ctx, h.Project, h.GitHub)
	if err != nil {
		log.Warn().Err(err).Msg("github: notices: state.db not read; nothing is told this interval")
		return
	}
	events := notice.GitHubEvents(open.PRs)
	reviews := notice.GitHubReviews(open.PRs)
	u := store.GitHubUpdate{Project: h.Project, Repo: h.GitHub, Collected: true}
	for _, p := range open.PRs {
		u.Open = append(u.Open, p.Number)
	}
	if !state.Seeded {
		for _, e := range events {
			u.Seen = append(u.Seen, seenOf(e))
		}
	}
	seedReviews := !state.Seeded || !state.Kinds[string(notice.Review)]
	if seedReviews {
		for _, r := range reviews {
			u.Seen = append(u.Seen, seenOf(r.GitHubEvent))
		}
		u.SeedKinds = []string{string(notice.Review)}
	}
	if err := g.st.UpdateGitHub(ctx, u, g.o.Now()); err != nil {
		log.Warn().Err(err).Msg("github: notices: state.db not written; nothing is told this interval")
		return
	}
	if !state.Seeded {
		log.Info().Int("prs", len(u.Open)).Msg("github: notices start from this collection; what is true now is not told")
		return
	}
	if seedReviews {
		log.Info().Int("reviews", len(reviews)).Msg("github: review notices start from this collection; the reviews there now are not told")
		reviews = nil
	}
	looked := 0
	if g.complete(h, open.Full, log) {
		events = append(events, g.left(ctx, gh, h, state.Open, u.Open, held, &looked, log)...)
	}
	events = append(events, g.reviewed(ctx, gh, h, reviews, state.Seen, held, &looked, log)...)
	g.mu.Lock()
	defer g.mu.Unlock()
	until := g.o.Now().Add(g.n.configs.For(h.Project).Gap + g.o.Team.GitHub.interval())
	var ns []notice.Notice
	for _, e := range events {
		k := githubKey{h.Project, h.GitHub, seenOf(e)}
		if held[k] || state.Seen[k.e] {
			continue
		}
		g.pending[k] = until
		ns = append(ns, notice.Notice{Project: h.Project, Subject: h.GitHub + "#" + strconv.Itoa(e.PR), Kind: e.Kind, Text: e.Text, Ack: g.ack(ctx, k)})
	}
	g.n.github.add(ns)
}

func seenOf(e notice.GitHubEvent) store.GitHubEvent {
	return store.GitHubEvent{PR: e.PR, Kind: string(e.Kind), Key: e.Key}
}

// complete reports whether a list holds every open PR: one at the limit may
// not. It logs when a repo's list starts coming back at the limit.
func (g *githubNotices) complete(h settings.Header, full bool, log zerolog.Logger) bool {
	key := h.Project + "\x00" + h.GitHub
	if full && !g.full[key] {
		log.Info().Int("limit", github.PRLimit).Msg("github: the open list is at its limit; PRs that leave it are not looked up until it is not")
	}
	g.full[key] = full
	return !full
}

// left looks up the PRs was held and now does not, but those whose merge is
// already pending, counting each in looked: a merged one is an event, a
// closed one leaves the open set silently, and one still open, or a lookup
// that failed, stays for the next interval.
func (g *githubNotices) left(ctx context.Context, gh github.Runner, h settings.Header, was, now []int, held map[githubKey]bool, looked *int, log zerolog.Logger) []notice.GitHubEvent {
	merging := map[int]bool{}
	for k := range held {
		if k.project == h.Project && k.repo == h.GitHub && k.e.Kind == string(notice.Merged) {
			merging[k.e.PR] = true
		}
	}
	var out []notice.GitHubEvent
	for _, pr := range was {
		if slices.Contains(now, pr) || merging[pr] {
			continue
		}
		if *looked == githubLookups {
			break
		}
		*looked++
		s, err := gh.PRView(ctx, h.GitHub, pr)
		if ctx.Err() != nil {
			return nil
		}
		key := h.Project + "\x00" + h.GitHub + "\x00" + strconv.Itoa(pr)
		if err != nil {
			if g.failing[key] != err.Error() {
				g.failing[key] = err.Error()
				log.Warn().Err(err).Int("pr", pr).Msg("github: a PR that left the open list was not looked up; it is looked up again each interval")
			}
			continue
		}
		delete(g.failing, key)
		switch s.State {
		case github.StateMerged:
			out = append(out, notice.GitHubMerged(h.GitHub, pr, s.MergeCommit))
		case github.StateClosed:
			if err := g.st.UpdateGitHub(ctx, store.GitHubUpdate{Project: h.Project, Repo: h.GitHub, Resolved: []int{pr}}, g.o.Now()); err != nil {
				log.Warn().Err(err).Int("pr", pr).Msg("github: notices: a closed PR not written; it is looked up again")
			}
		}
	}
	return out
}

// reviewed looks up each review neither told nor held, for its counts,
// while looked leaves calls: one whose lookup failed waits for the next
// interval, and one the answer lacks is told without counts, since asking
// again would not find it. A review whose login is not one, or that its
// project skips, is handed over with no lookup.
func (g *githubNotices) reviewed(ctx context.Context, gh github.Runner, h settings.Header, reviews []notice.GitHubReview, seen map[store.GitHubEvent]bool, held map[githubKey]bool, looked *int, log zerolog.Logger) []notice.GitHubEvent {
	skip := g.n.configs.For(h.Project).Skip[notice.Review]
	var out []notice.GitHubEvent
	for _, r := range reviews {
		k := githubKey{h.Project, h.GitHub, seenOf(r.GitHubEvent)}
		if held[k] || seen[k.e] {
			continue
		}
		if skip || r.Review.Login == "" {
			out = append(out, r.GitHubEvent)
			continue
		}
		if *looked == githubLookups {
			continue
		}
		*looked++
		d, err := gh.Review(ctx, h.GitHub, r.PR, r.Review)
		if ctx.Err() != nil {
			return nil
		}
		key := h.Project + "\x00" + h.GitHub + "\x00" + strconv.Itoa(r.PR) + "\x00" + r.Key
		switch {
		case errors.Is(err, github.ErrNoReview):
			delete(g.failing, key)
			out = append(out, r.GitHubEvent)
		case err != nil:
			if g.failing[key] != err.Error() {
				g.failing[key] = err.Error()
				log.Warn().Err(err).Int("pr", r.PR).Str("reviewer", r.Review.Login).Msg("github: a new review's counts were not read; they are read again each interval")
			}
		default:
			delete(g.failing, key)
			out = append(out, notice.GitHubReviewed(r, d))
		}
	}
	return out
}

// held is every event pending, once those past their time are dropped.
func (g *githubNotices) held() map[githubKey]bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.o.Now()
	out := map[githubKey]bool{}
	for k, until := range g.pending {
		if !now.Before(until) {
			delete(g.pending, k)
			continue
		}
		out[k] = true
	}
	return out
}

// ack records k as told, a merge with its PR resolved, once its notice is
// settled. It runs on the team poll's goroutine, with the team's context. A
// write that fails is logged, and k is told again.
func (g *githubNotices) ack(ctx context.Context, k githubKey) func() {
	return func() {
		u := store.GitHubUpdate{Project: k.project, Repo: k.repo, Seen: []store.GitHubEvent{k.e}}
		if k.e.Kind == string(notice.Merged) {
			u.Resolved = []int{k.e.PR}
		}
		if err := g.st.UpdateGitHub(ctx, u, g.o.Now()); err != nil {
			g.o.Logger.Warn().Err(err).Str("project", k.project).Str("repo", k.repo).Int("pr", k.e.PR).Str("kind", k.e.Kind).
				Msg("github: a notice was told but not recorded; it is told again")
		}
		g.mu.Lock()
		defer g.mu.Unlock()
		delete(g.pending, k)
	}
}
