package store

import (
	"context"
	"fmt"
	"slices"
	"time"
)

// GitHubRetention is how long a row of the GitHub notices' state is kept
// after the collector last observed it (ADR-001 A45).
const GitHubRetention = 30 * 24 * time.Hour

// GitHubEvent is one key of a repo's seen-set: an event its boss was told,
// or one its team skips.
type GitHubEvent struct {
	PR   int
	Kind string
	// Key tells one event of a kind from the next: a merge commit, a head
	// commit, or "" for once per PR.
	Key string
}

// GitHubState is what the store holds of one project's repo.
type GitHubState struct {
	// Seeded is a repo whose first successful collection was recorded.
	Seeded bool
	// Open are the PRs open at its last successful collection, by number.
	Open []int
	// Seen are the events recorded.
	Seen map[GitHubEvent]bool
}

// GitHubUpdate is one change to a project's repo, written in one
// transaction.
type GitHubUpdate struct {
	Project, Repo string
	// Collected is a successful collection: the repo is marked seeded, or
	// its mark observed again.
	Collected bool
	// Open are PRs the collection found open: each is recorded, or its row
	// and its events' rows are observed again.
	Open []int
	// Seen are events recorded as told, or observed again.
	Seen []GitHubEvent
	// Resolved are PRs merged or closed: their open rows go.
	Resolved []int
}

// GitHub returns what the store holds of project's repo.
func (s *Store) GitHub(ctx context.Context, project, repo string) (GitHubState, error) {
	out := GitHubState{Open: []int{}, Seen: map[GitHubEvent]bool{}}
	err := s.db.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM github_seeded WHERE project = ? AND repo = ?)", project, repo).Scan(&out.Seeded)
	if err != nil {
		return GitHubState{}, fmt.Errorf("%w: read github state: %w", ErrStore, err)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT 0, pr, '', '' FROM github_open WHERE project = ?1 AND repo = ?2
UNION ALL SELECT 1, pr, kind, key FROM github_seen WHERE project = ?1 AND repo = ?2 ORDER BY 1, 2`, project, repo)
	if err != nil {
		return GitHubState{}, fmt.Errorf("%w: read github state: %w", ErrStore, err)
	}
	defer rows.Close() //nolint:errcheck // read-only; rows.Err reports what matters
	for rows.Next() {
		var seen bool
		var e GitHubEvent
		if err := rows.Scan(&seen, &e.PR, &e.Kind, &e.Key); err != nil {
			return GitHubState{}, fmt.Errorf("%w: read github state: %w", ErrStore, err)
		}
		if seen {
			out.Seen[e] = true
		} else {
			out.Open = append(out.Open, e.PR)
		}
	}
	if err := rows.Err(); err != nil {
		return GitHubState{}, fmt.Errorf("%w: read github state: %w", ErrStore, err)
	}
	return out, nil
}

// UpdateGitHub writes u as of at, in one transaction: all of it, or, on an
// error, none of it.
func (s *Store) UpdateGitHub(ctx context.Context, u GitHubUpdate, at time.Time) error {
	ns, err := nanos(at)
	if err != nil {
		return err
	}
	if err := u.check(); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("%w: update github state: %w", ErrStore, err)
	}
	defer tx.Rollback() //nolint:errcheck // a no-op once committed
	exec := func(query string, args ...any) error {
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return fmt.Errorf("%w: update github state: %w", ErrStore, err)
		}
		return nil
	}
	if u.Collected {
		if err := exec(`INSERT INTO github_seeded (project, repo, first_seen, last_seen) VALUES (?1, ?2, ?3, ?3)
ON CONFLICT DO UPDATE SET last_seen = max(last_seen, excluded.last_seen)`, u.Project, u.Repo, ns); err != nil {
			return err
		}
	}
	for _, pr := range u.Open {
		if err := exec(`INSERT INTO github_open (project, repo, pr, first_seen, last_seen) VALUES (?1, ?2, ?3, ?4, ?4)
ON CONFLICT DO UPDATE SET last_seen = max(last_seen, excluded.last_seen)`, u.Project, u.Repo, pr, ns); err != nil {
			return err
		}
		if err := exec("UPDATE github_seen SET last_seen = max(last_seen, ?4) WHERE project = ?1 AND repo = ?2 AND pr = ?3", u.Project, u.Repo, pr, ns); err != nil {
			return err
		}
	}
	for _, e := range u.Seen {
		if err := exec(`INSERT INTO github_seen (project, repo, pr, kind, key, first_seen, last_seen) VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?6)
ON CONFLICT DO UPDATE SET last_seen = max(last_seen, excluded.last_seen)`, u.Project, u.Repo, e.PR, e.Kind, e.Key, ns); err != nil {
			return err
		}
	}
	for _, pr := range u.Resolved {
		if err := exec("DELETE FROM github_open WHERE project = ? AND repo = ? AND pr = ?", u.Project, u.Repo, pr); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%w: update github state: %w", ErrStore, err)
	}
	return nil
}

// check refuses an update the schema would: no project or repo, a PR below
// 1, or an event with no kind.
func (u GitHubUpdate) check() error {
	prs := slices.Concat(u.Open, u.Resolved)
	for _, e := range u.Seen {
		if e.Kind == "" {
			return fmt.Errorf("%w: a github event of PR %d with no kind", ErrInvalid, e.PR)
		}
		prs = append(prs, e.PR)
	}
	if u.Project == "" || u.Repo == "" {
		return fmt.Errorf("%w: github state with no project or repo (%q, %q)", ErrInvalid, u.Project, u.Repo)
	}
	if slices.ContainsFunc(prs, func(pr int) bool { return pr < 1 }) {
		return fmt.Errorf("%w: a PR number below 1 in %v", ErrInvalid, prs)
	}
	return nil
}

// PruneGitHub deletes the GitHub notices' rows the collector last observed
// more than GitHubRetention before now (A45), and reports how many it
// deleted: a merged PR's events, a closed repo's, a repo no longer
// configured.
func (s *Store) PruneGitHub(ctx context.Context, now time.Time) (int64, error) {
	cutoff, err := nanos(now.Add(-GitHubRetention))
	if err != nil {
		return 0, err
	}
	var n int64
	for _, p := range []struct{ table, query string }{
		{"github_seen", "DELETE FROM github_seen WHERE last_seen < ?"},
		{"github_open", "DELETE FROM github_open WHERE last_seen < ?"},
		{"github_seeded", "DELETE FROM github_seeded WHERE last_seen < ?"},
	} {
		res, err := s.db.ExecContext(ctx, p.query, cutoff)
		if err != nil {
			return n, fmt.Errorf("%w: prune %s: %w", ErrStore, p.table, err)
		}
		d, err := res.RowsAffected()
		if err != nil {
			return n, fmt.Errorf("%w: prune %s: %w", ErrStore, p.table, err)
		}
		n += d
	}
	return n, nil
}
