package bossshift_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/bossshift"
)

const (
	attempt = "20261124-000000-abc123"
	endMark = "\n<!-- end shift round -->\n"
)

func sum(content string) string {
	d := sha256.Sum256([]byte(content))
	return hex.EncodeToString(d[:])
}

func account(id string, n int, body string) string {
	return fmt.Sprintf("attempt: %s\nround: %d\n\n%s%s", id, n, body, endMark)
}

func reply(id string, n int, decision, digest, body string) string {
	return fmt.Sprintf("attempt: %s\nround: %d\ndecision: %s\naccount: %s\n\n%s%s", id, n, decision, digest, body, endMark)
}

type shiftDir string

func newShiftDir(t *testing.T) shiftDir {
	t.Helper()
	dir := filepath.Join(t.TempDir(), attempt)
	require.NoError(t, os.Mkdir(dir, 0o700))
	return shiftDir(dir)
}

func (d shiftDir) put(t *testing.T, name, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(string(d), name), []byte(content), 0o600))
}

// pair writes round n's account and a ready reply that carries its digest.
func (d shiftDir) pair(t *testing.T, n int, decision string) string {
	t.Helper()
	acc := account(attempt, n, "Account.")
	d.put(t, fmt.Sprintf("account-%d.md", n), acc)
	d.put(t, fmt.Sprintf("reply-%d.md", n), reply(attempt, n, decision, sum(acc), "Reviewed."))
	return sum(acc)
}

func (d shiftDir) wait(mutate func(*bossshift.WaitRequest)) (bossshift.RoundVerdict, error) {
	req := bossshift.WaitRequest{Record: bossshift.Record{Dir: string(d), Attempt: attempt}}
	if mutate != nil {
		mutate(&req)
	}
	return bossshift.Wait(context.Background(), req)
}

func TestWaitPublishedAccountCarriesItsDigest(t *testing.T) {
	t.Parallel()
	d := newShiftDir(t)
	acc := account(attempt, 1, "Account.")
	d.put(t, "account-1.md", acc)
	v, err := d.wait(nil)
	require.NoError(t, err)
	require.Equal(t, bossshift.RoundVerdict{
		Status: "published", Attempt: attempt, Round: 1, Path: filepath.Join(string(d), "account-1.md"), SHA256: sum(acc), Text: acc,
	}, v)
}

func TestWaitIgnoresAnotherAttemptsRounds(t *testing.T) {
	t.Parallel()
	d := newShiftDir(t)
	d.put(t, "account-1.md", account("20261124-000000-other1", 1, "Account."))
	v, err := d.wait(nil)
	require.ErrorIs(t, err, bossshift.ErrWaitTimeout)
	require.Equal(t, "timeout", v.Status)
	d.put(t, "account-1.md", account(attempt, 1, "Account."))
	d.put(t, "reply-1.md", reply("20261124-000000-other1", 1, "revise", sum(account(attempt, 1, "Account.")), "Reviewed."))
	_, err = d.wait(func(r *bossshift.WaitRequest) { r.Round, r.Reply = 1, true })
	require.ErrorIs(t, err, bossshift.ErrWaitTimeout, "a reply of another attempt answers nothing")
}

func TestWaitIgnoresUnusableRoundFiles(t *testing.T) {
	t.Parallel()
	good := account(attempt, 1, "Account.")
	for name, content := range map[string]string{
		"oversized":      account(attempt, 1, strings.Repeat("x", 129<<10)),
		"invalid UTF-8":  account(attempt, 1, "bad \xff"),
		"no end marker":  strings.TrimSuffix(good, endMark),
		"empty body":     account(attempt, 1, "  "),
		"another round":  account(attempt, 2, "Account."),
		"no header":      "Account.\n" + endMark,
		"no blank line":  strings.Replace(good, "\n\n", "\n", 1),
		"text after end": good + "more\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			d := newShiftDir(t)
			d.put(t, "account-1.md", content)
			_, err := d.wait(nil)
			require.ErrorIs(t, err, bossshift.ErrWaitTimeout)
		})
	}
	t.Run("a link or a directory", func(t *testing.T) {
		t.Parallel()
		d := newShiftDir(t)
		d.put(t, "elsewhere", good)
		require.NoError(t, os.Symlink(filepath.Join(string(d), "elsewhere"), filepath.Join(string(d), "account-1.md")))
		require.NoError(t, os.Mkdir(filepath.Join(string(d), "account-2.md"), 0o700))
		_, err := d.wait(nil)
		require.ErrorIs(t, err, bossshift.ErrWaitTimeout)
		_, err = d.wait(func(r *bossshift.WaitRequest) { r.Round = 2 })
		require.ErrorIs(t, err, bossshift.ErrWaitTimeout)
	})
	for name, content := range map[string]string{
		"unknown decision": reply(attempt, 1, "maybe", sum(good), "Reviewed."),
		"no digest":        strings.Replace(reply(attempt, 1, "ready", sum(good), "Reviewed."), "account: "+sum(good)+"\n", "", 1),
		"short digest":     reply(attempt, 1, "ready", sum(good)[:40], "Reviewed."),
	} {
		t.Run("reply with "+name, func(t *testing.T) {
			t.Parallel()
			d := newShiftDir(t)
			d.put(t, "account-1.md", good)
			d.put(t, "reply-1.md", content)
			_, err := d.wait(func(r *bossshift.WaitRequest) { r.Round, r.Reply = 1, true })
			require.ErrorIs(t, err, bossshift.ErrWaitTimeout)
		})
	}
}

func TestWaitAdvancesPastARevisedRound(t *testing.T) {
	t.Parallel()
	d := newShiftDir(t)
	d.pair(t, 1, "revise")
	v, err := d.wait(nil)
	require.ErrorIs(t, err, bossshift.ErrWaitTimeout, "round 1 was revised: the next account is awaited")
	require.Equal(t, 2, v.Round)
	d.put(t, "account-2.md", account(attempt, 2, "Account two."))
	v, err = d.wait(nil)
	require.NoError(t, err)
	require.Equal(t, 2, v.Round)
	v, err = d.wait(func(r *bossshift.WaitRequest) { r.Round = 1 })
	require.NoError(t, err, "an explicit round is read as it is")
	require.Equal(t, 1, v.Round)
	v, err = d.wait(func(r *bossshift.WaitRequest) { r.Round, r.Reply = 1, true })
	require.NoError(t, err)
	require.Equal(t, "revise", v.Decision)
}

func TestWaitReportsAbortedDoneAndExhausted(t *testing.T) {
	t.Parallel()
	d := newShiftDir(t)
	d.pair(t, 1, "revise")
	d.pair(t, 2, "revise")
	d.pair(t, 3, "revise")
	for name, req := range map[string]func(*bossshift.WaitRequest){
		"the next account": nil,
		"the reply":        func(r *bossshift.WaitRequest) { r.Round, r.Reply = 3, true },
	} {
		v, err := d.wait(req)
		require.NoError(t, err, name)
		require.Equal(t, "exhausted", v.Status, name)
		require.Equal(t, 3, v.Round, name)
	}
	d.put(t, "DONE", "12:00:00\n")
	v, err := d.wait(nil)
	require.NoError(t, err)
	require.Equal(t, "done", v.Status)
	d.put(t, "ABORTED", "why\n")
	v, err = d.wait(nil)
	require.NoError(t, err)
	require.Equal(t, "aborted", v.Status, "an abort is reported before any round")
}

func TestWaitTimesOutAndStopsWithItsContext(t *testing.T) {
	t.Parallel()
	d := newShiftDir(t)
	started := time.Now()
	_, err := d.wait(func(r *bossshift.WaitRequest) { r.Timeout = 300 * time.Millisecond })
	require.ErrorIs(t, err, bossshift.ErrWaitTimeout)
	require.GreaterOrEqual(t, time.Since(started), 300*time.Millisecond)
	go func() { time.Sleep(100 * time.Millisecond); d.put(t, "account-1.md", account(attempt, 1, "Late.")) }()
	v, err := d.wait(func(r *bossshift.WaitRequest) { r.Timeout = 5 * time.Second })
	require.NoError(t, err, "a publication that lands while waiting is found")
	require.Equal(t, "published", v.Status)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = bossshift.Wait(ctx, bossshift.WaitRequest{Record: bossshift.Record{Dir: string(newShiftDir(t)), Attempt: attempt}, Timeout: time.Minute})
	require.ErrorIs(t, err, context.Canceled)
}

func TestWaitAndBriefRefuseUnusableRequests(t *testing.T) {
	t.Parallel()
	d := newShiftDir(t)
	role := filepath.Join(t.TempDir(), "boss-shift.md")
	require.NoError(t, os.WriteFile(role, []byte("role"), 0o600))
	for name, rec := range map[string]bossshift.Record{
		"relative directory": {Dir: "relative/" + attempt, Attempt: attempt},
		"bad attempt":        {Dir: string(d), Attempt: "../x"},
		"another folder":     {Dir: string(d), Attempt: "20261124-000000-other1"},
		"missing directory":  {Dir: filepath.Join(t.TempDir(), attempt), Attempt: attempt},
	} {
		_, err := bossshift.Wait(context.Background(), bossshift.WaitRequest{Record: rec})
		require.ErrorIs(t, err, bossshift.ErrRecord, "wait: "+name)
		_, err = bossshift.Brief(rec, role)
		require.ErrorIs(t, err, bossshift.ErrRecord, "brief: "+name)
	}
	rec := bossshift.Record{Dir: string(d), Attempt: attempt}
	for name, req := range map[string]bossshift.WaitRequest{
		"round past three":    {Record: rec, Round: 4},
		"negative round":      {Record: rec, Round: -1},
		"reply without round": {Record: rec, Reply: true},
		"negative timeout":    {Record: rec, Timeout: -time.Second},
		"timeout past 5m":     {Record: rec, Timeout: 5*time.Minute + time.Second},
	} {
		_, err := bossshift.Wait(context.Background(), req)
		require.ErrorIs(t, err, bossshift.ErrRecord, name)
	}
	_, err := bossshift.Brief(rec, "relative.md")
	require.ErrorIs(t, err, bossshift.ErrRecord)
	_, err = bossshift.Brief(rec, filepath.Join(string(d), "missing.md"))
	require.ErrorIs(t, err, bossshift.ErrRecord)
	b, err := bossshift.Brief(rec, role)
	require.NoError(t, err)
	require.Equal(t, bossshift.Briefing{Record: rec, Role: role}, b)
}

// An answered account is one with a reply for its current bytes, ready or revise; the next
// account is the first that is not.
func TestWaitDefaultSkipsOnlyAnsweredAccounts(t *testing.T) {
	t.Parallel()
	d := newShiftDir(t)
	d.pair(t, 1, "ready")
	v, err := d.wait(nil)
	require.ErrorIs(t, err, bossshift.ErrWaitTimeout, "round 1 is answered, whatever the answer")
	require.Equal(t, 2, v.Round)
	d.put(t, "account-2.md", account(attempt, 2, "Account two."))
	v, err = d.wait(nil)
	require.NoError(t, err)
	require.Equal(t, 2, v.Round, "a round published after a model refusal is found")
	// An account rewritten after its reply is awaiting a reply again.
	d = newShiftDir(t)
	d.pair(t, 1, "revise")
	d.put(t, "account-1.md", account(attempt, 1, "Rewritten."))
	v, err = d.wait(nil)
	require.NoError(t, err)
	require.Equal(t, 1, v.Round)
	require.Empty(t, v.Decision)
}

// A reply counts for the bytes it names and no others: a revise written about an account that
// has since been replaced is not an answer to the new one, so it cannot end the review.
func TestWaitIgnoresAReplyForOtherAccountBytes(t *testing.T) {
	t.Parallel()
	d := newShiftDir(t)
	d.pair(t, 1, "revise")
	d.pair(t, 2, "revise")
	d.pair(t, 3, "revise")
	d.put(t, "account-3.md", account(attempt, 3, "Corrected after the model refusal."))
	for name, req := range map[string]func(*bossshift.WaitRequest){
		"the reply":        func(r *bossshift.WaitRequest) { r.Round, r.Reply = 3, true },
		"the next account": nil,
	} {
		v, err := d.wait(req)
		if name == "the reply" {
			require.ErrorIs(t, err, bossshift.ErrWaitTimeout, name)
		} else {
			require.NoError(t, err, name)
		}
		require.NotEqual(t, "exhausted", v.Status, name)
	}
}

// The reply is read before the account it must name, so a replacement that lands between the
// two reads shows as a mismatch: a poll never ends the review on an account that is already gone.
func TestWaitSnapshotIsConsistentAcrossReads(t *testing.T) {
	t.Parallel()
	for name, mutate := range map[string]func(*bossshift.WaitRequest){
		"the reply":        func(r *bossshift.WaitRequest) { r.Round, r.Reply = 3, true },
		"the next account": nil,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			d := newShiftDir(t)
			d.pair(t, 1, "revise")
			d.pair(t, 2, "revise")
			d.pair(t, 3, "revise")
			req := bossshift.WaitRequest{Record: bossshift.Record{Dir: string(d), Attempt: attempt}}
			if mutate != nil {
				mutate(&req)
			}
			v, err := bossshift.WaitBetweenReads(context.Background(), req, func() {
				d.put(t, "account-3.md", account(attempt, 3, "Corrected after the review."))
			})
			require.NotEqual(t, "exhausted", v.Status)
			if name != "the next account" {
				require.ErrorIs(t, err, bossshift.ErrWaitTimeout)
			}
		})
	}
}

// The reviewer is shown the bytes the digest names, not a path that can hold other bytes by the time it is
// opened: the digest of the text a wait returns is the digest it returns.
func TestWaitShowsTheBytesItDigests(t *testing.T) {
	t.Parallel()
	d := newShiftDir(t)
	first := account(attempt, 1, "Draft A.")
	d.put(t, "account-1.md", first)
	v, err := d.wait(nil)
	require.NoError(t, err)
	require.Equal(t, first, v.Text)
	require.Equal(t, sum(v.Text), v.SHA256)
	d.put(t, "account-1.md", account(attempt, 1, "Draft B."))
	require.Equal(t, sum(first), v.SHA256, "what was shown is unchanged by a later publication")
	v, err = d.wait(func(r *bossshift.WaitRequest) { r.Round = 1 })
	require.NoError(t, err)
	require.Equal(t, sum(v.Text), v.SHA256)
	require.Contains(t, v.Text, "Draft B.")
}
