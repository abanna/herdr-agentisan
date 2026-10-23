package github

import (
	"cmp"
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The states a review is told in (ADR-001 A45). Any other state GitHub
// prints reads as "".
const (
	ReviewApproved         = "APPROVED"
	ReviewChangesRequested = "CHANGES_REQUESTED"
	ReviewCommented        = "COMMENTED"
	ReviewDismissed        = "DISMISSED"
)

// reviewPending is a review not yet submitted, which only its author sees.
const reviewPending = "PENDING"

// reviewsQuery reads one reviewer's last reviews of a PR, with what the
// boss's notice counts in them (A45): the body, and the inline comments
// with whether each replies to another. The author filter needs a bot's
// login with "[bot]", which GraphQL's login lacks, and a login no account
// has answers no reviews, so both forms are asked at once.
const reviewsQuery = `query($owner: String!, $name: String!, $number: Int!, $user: String!, $bot: String!) { repository(owner: $owner, name: $name) { pullRequest(number: $number) { user: reviews(last: 5, author: $user) { ...reviews } bot: reviews(last: 5, author: $bot) { ...reviews } } } } fragment reviews on PullRequestReviewConnection { nodes { author { login } state submittedAt commit { oid } body comments(first: 100) { totalCount nodes { body replyTo { id } } } } }`

// loginPattern is a GitHub login: at most 39 letters, digits and hyphens,
// never starting with a hyphen. GraphQL prints a bot's with no "[bot]".
var loginPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}$`)

// badgePattern is the image a P-badged finding starts with, such as Codex's
// `![P1 Badge](https://img.shields.io/badge/P1-orange?style=flat)`.
var badgePattern = regexp.MustCompile(`!\[P([0-3]) Badge\]\(`)

// Review is one review as the PR list reads it: who, how and when. The list
// gives no id, so the reviewer and the time are its identity.
type Review struct {
	// Login is the reviewer's, or "" when GitHub's is not a login, such as a
	// deleted account's.
	Login string
	// State is one of the Review states, or "".
	State string
	// Submitted is when it was submitted, in UTC.
	Submitted time.Time
}

// ReviewDetail is one review as the review query reads it: also the commit
// it reviewed and its P-badge counts.
type ReviewDetail struct {
	Review
	// Commit is the reviewed commit's oid, or "" when GitHub's is not one.
	Commit string
	// Badges counts the P0 to P3 badges, by level, in the body and in each
	// inline comment that starts a thread: one that replies may quote one.
	Badges [4]int
	// Comments counts the inline comments, and Read those counted for
	// badges: the first 100.
	Comments, Read int
}

// ghAuthor is a review's author; null for a deleted account.
type ghAuthor struct {
	Login string `json:"login"`
}

// ghReview is one review as gh prints it: in the PR list, the latest of
// each reviewer, with no commit, body or comments; in the review query, one
// of a reviewer's last reviews with all three.
type ghReview struct {
	Author      *ghAuthor `json:"author"`
	State       string    `json:"state"`
	SubmittedAt *string   `json:"submittedAt"`
	Commit      *struct {
		Oid string `json:"oid"`
	} `json:"commit"`
	Body     string `json:"body"`
	Comments struct {
		TotalCount int `json:"totalCount"`
		Nodes      []struct {
			Body    string `json:"body"`
			ReplyTo *struct {
				ID string `json:"id"`
			} `json:"replyTo"`
		} `json:"nodes"`
	} `json:"comments"`
}

// review reads r: a pending review, or one with no submission time in RFC
// 3339, is none. A state or login it does not know reads as "".
func (r ghReview) review() (Review, bool) {
	if r.State == reviewPending || r.SubmittedAt == nil {
		return Review{}, false
	}
	at, err := time.Parse(time.RFC3339, *r.SubmittedAt)
	if err != nil {
		return Review{}, false
	}
	out := Review{Submitted: at.UTC()}
	if r.Author != nil && loginPattern.MatchString(r.Author.Login) {
		out.Login = r.Author.Login
	}
	if slices.Contains([]string{ReviewApproved, ReviewChangesRequested, ReviewCommented, ReviewDismissed}, r.State) {
		out.State = r.State
	}
	return out, true
}

// reviews are the reviews list holds, by submission, then login; nil for
// none.
func reviews(list []ghReview) []Review {
	var out []Review
	for _, r := range list {
		if rv, ok := r.review(); ok {
			out = append(out, rv)
		}
	}
	slices.SortFunc(out, func(a, b Review) int {
		return cmp.Or(a.Submitted.Compare(b.Submitted), strings.Compare(a.Login, b.Login))
	})
	return out
}

// detail is r with its commit and its P-badge counts.
func (r ghReview) detail(rv Review) ReviewDetail {
	out := ReviewDetail{Review: rv, Comments: max(r.Comments.TotalCount, len(r.Comments.Nodes)), Read: len(r.Comments.Nodes)}
	if r.Commit != nil && oidPattern.MatchString(r.Commit.Oid) {
		out.Commit = r.Commit.Oid
	}
	count := func(s string) {
		for _, m := range badgePattern.FindAllStringSubmatch(s, -1) {
			out.Badges[m[1][0]-'0']++
		}
	}
	count(r.Body)
	for _, c := range r.Comments.Nodes {
		if c.ReplyTo == nil {
			count(c.Body)
		}
	}
	return out
}

// ParseReviews reads the review query's answer: the reviews under either
// form of the login, each with its counts, pending ones left out. An answer
// with no PR, or no reviews under either form, is ErrOutput.
func ParseReviews(raw []byte) ([]ReviewDetail, error) {
	type connection struct {
		Nodes []ghReview `json:"nodes"`
	}
	var answer struct {
		Data *struct {
			Repository *struct {
				PullRequest *struct {
					User *connection `json:"user"`
					Bot  *connection `json:"bot"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := decodeOne(raw, &answer); err != nil {
		return nil, err
	}
	if answer.Data == nil || answer.Data.Repository == nil || answer.Data.Repository.PullRequest == nil {
		return nil, fmt.Errorf("%w: no PR in the answer", ErrOutput)
	}
	pr := answer.Data.Repository.PullRequest
	if pr.User == nil || pr.Bot == nil {
		return nil, fmt.Errorf("%w: no reviews in the answer", ErrOutput)
	}
	out := []ReviewDetail{}
	for _, r := range slices.Concat(pr.User.Nodes, pr.Bot.Nodes) {
		if rv, ok := r.review(); ok {
			out = append(out, r.detail(rv))
		}
	}
	return out, nil
}

// Review reads repo's PR n's review r, as the PR list read it, with its
// commit and P-badge counts: one call, matched by login and submission
// time. A review not in the answer is ErrNoReview.
func (r Runner) Review(ctx context.Context, repo string, n int, rv Review) (ReviewDetail, error) {
	if err := checkRepo(repo); err != nil {
		return ReviewDetail{}, err
	}
	if n < 1 {
		return ReviewDetail{}, fmt.Errorf("%w: %d", ErrNumber, n)
	}
	if !loginPattern.MatchString(rv.Login) {
		return ReviewDetail{}, fmt.Errorf("%w: %q", ErrLogin, rv.Login)
	}
	owner, name, _ := strings.Cut(repo, "/")
	out, err := r.run(ctx, "api", "graphql", "-f", "query="+reviewsQuery, "-f", "owner="+owner, "-f", "name="+name,
		"-F", "number="+strconv.Itoa(n), "-f", "user="+rv.Login, "-f", "bot="+rv.Login+"[bot]")
	if err != nil {
		return ReviewDetail{}, err
	}
	ds, err := ParseReviews(out)
	if err != nil {
		return ReviewDetail{}, err
	}
	for _, d := range ds {
		if d.Login == rv.Login && d.Submitted.Equal(rv.Submitted) {
			return d, nil
		}
	}
	return ReviewDetail{}, fmt.Errorf("%w: %s's at %s on #%d", ErrNoReview, rv.Login, rv.Submitted.Format(time.RFC3339), n)
}
