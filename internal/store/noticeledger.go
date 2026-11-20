package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// The notice admission ledger (NERD-5533 B0a.2, migration 0009) records
// each notice once, before anything tells it: a notice admitted once is
// never admitted again. It is dormant: nothing in production calls it yet,
// it posts nothing to the board and wakes no reader, and it never writes a
// github_* table.

// ErrNoNotice means no notice with that capture ID is pending: none was
// admitted, or it was imported, or it is settled.
var ErrNoNotice = errors.New("no such pending notice")

// The states AdmitNotice writes. A pending notice is settled later, as
// posted or dropped.
const (
	noticePending  = "pending"
	noticeImported = "imported"
)

// The ledger's sources: GitHub's, the one admitted yet, and the daemon's,
// which the metadata counts (noticemeta.go) and B0a.4 admits.
const (
	noticeFromGitHub = "github"
	noticeFromDaemon = "daemon"
)

// NoticeAdmission is what AdmitNotice did with a notice.
type NoticeAdmission struct {
	// CaptureID is the stored identity's: the notice's own, or, for a
	// duplicate, the first capture's.
	CaptureID string
	// State is the stored identity's state.
	State string
	// Duplicate is a notice already admitted, by its capture ID or by its
	// receipt. Nothing was written.
	Duplicate bool
	// Imported is a notice github_seen already holds: it was recorded as
	// imported, with no payload, and is never told again.
	Imported bool
}

// noticeCapture is a valid GitHub envelope's canonical encoding, by its
// length and its SHA-256, and the capture ID that digest makes.
type noticeCapture struct {
	bytes  int64
	digest string
	id     string
}

// captureOf validates e as B0a.1 does and returns its capture.
func captureOf(e NoticeEnvelope) (noticeCapture, error) {
	encoded, err := githubNoticeEncoding(e)
	if err != nil {
		return noticeCapture{}, err
	}
	sum := sha256.Sum256(encoded)
	digest := hex.EncodeToString(sum[:])
	return noticeCapture{bytes: int64(len(encoded)), digest: digest, id: "gh1:" + digest}, nil
}

// ledgerFailed is a failed statement of the ledger's. It names what was
// being done, never the notice: no error of the ledger's carries its input.
func ledgerFailed(what string, err error) error {
	return fmt.Errorf("%w: %s: %w", ErrStore, what, err)
}

const (
	selectNoticeByID      = "SELECT state FROM notice_identities WHERE capture_id = ?"
	selectNoticeByReceipt = `SELECT capture_id, state FROM notice_identities
WHERE project = ?1 AND repo = ?2 AND pr = ?3 AND kind = ?4 AND receipt_key = ?5`
	// The import is exact: = on github_seen's columns compares under their
	// BINARY collation, with no case folding and no prefix.
	selectNoticeSeen = `SELECT EXISTS (SELECT 1 FROM github_seen
WHERE project = ?1 AND repo = ?2 AND pr = ?3 AND kind = ?4 AND key = ?5)`
	insertNoticeIdentity = `INSERT INTO notice_identities (capture_id, project, source, kind, repo, pr, receipt_key, state, admitted_at)
VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9)`
	insertNoticePayload = `INSERT INTO notice_payloads (capture_id, version, receipt_version, subject, worker, observed_at, text, bytes, digest)
VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9)`
)

// AdmitNotice records e in the ledger as of at, in one transaction, and says
// what it did. Only a GitHub notice is admitted yet.
//
//   - e must be a valid version-1 GitHub envelope (GitHubNoticeCaptureID)
//     with a project, its CaptureID the one GitHubNoticeCaptureID derives,
//     and at a time the store can hold; anything else is ErrInvalid.
//   - A notice already admitted, by its capture ID or by its receipt, is a
//     Duplicate: the first capture wins, with its stored ID and state, and
//     nothing is written.
//   - A notice whose receipt github_seen holds, compared exactly, was told
//     before the ledger: it is recorded imported, with no payload.
//     github_seen is only read.
//   - Anything else is recorded pending, with its payload.
//   - A pending notice that would exceed a quota, or an import that would
//     exceed Q6 to Q8, is refused with that quota's error, which wraps
//     ErrNoticeQuota; nothing is written.
//
// It writes no table but the ledger's and wakes no reader.
func (s *Store) AdmitNotice(ctx context.Context, e NoticeEnvelope, at time.Time) (NoticeAdmission, error) {
	// A GitHub notice with no project can never be imported, nor settled:
	// github_seen refuses an empty project.
	if e.Source != noticeFromGitHub || e.Project == "" {
		return NoticeAdmission{}, noticeInvalid()
	}
	c, err := captureOf(e)
	if err != nil {
		return NoticeAdmission{}, err
	}
	if e.CaptureID != c.id {
		return NoticeAdmission{}, fmt.Errorf("%w: notice capture ID", ErrInvalid)
	}
	ns, err := nanos(at)
	if err != nil {
		return NoticeAdmission{}, noticeInvalid() // nanos would format at
	}
	r := e.Receipt
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return NoticeAdmission{}, ledgerFailed("admit a notice", err)
	}
	defer tx.Rollback() //nolint:errcheck // a no-op once committed; a duplicate writes nothing to commit
	got := NoticeAdmission{CaptureID: c.id, Duplicate: true}
	switch err := tx.QueryRowContext(ctx, selectNoticeByID, c.id).Scan(&got.State); {
	case err == nil:
		return got, nil
	case !errors.Is(err, sql.ErrNoRows):
		return NoticeAdmission{}, ledgerFailed("look a notice up", err)
	}
	switch err := tx.QueryRowContext(ctx, selectNoticeByReceipt, e.Project, r.Repo, r.PR, e.Kind, r.Key).Scan(&got.CaptureID, &got.State); {
	case err == nil:
		return got, nil
	case !errors.Is(err, sql.ErrNoRows):
		return NoticeAdmission{}, ledgerFailed("look a notice's receipt up", err)
	}
	var seen bool
	if err := tx.QueryRowContext(ctx, selectNoticeSeen, e.Project, r.Repo, r.PR, e.Kind, r.Key).Scan(&seen); err != nil {
		return NoticeAdmission{}, ledgerFailed("read the github seen-set", err)
	}
	if err := checkNoticeQuotas(ctx, tx, s.quotas, e.Project, e.Receipt, c.bytes, !seen, ns); err != nil {
		return NoticeAdmission{}, err
	}
	got = NoticeAdmission{CaptureID: c.id, State: noticePending}
	if seen {
		got.State, got.Imported = noticeImported, true
	}
	if _, err := tx.ExecContext(ctx, insertNoticeIdentity, c.id, e.Project, e.Source, e.Kind, r.Repo, r.PR, r.Key, got.State, ns); err != nil {
		return NoticeAdmission{}, ledgerFailed("record a notice", err)
	}
	if !seen {
		_, err := tx.ExecContext(ctx, insertNoticePayload, c.id, e.Version, r.Version, e.Subject, e.Worker, e.ObservedAt.UnixNano(), e.Text, c.bytes, c.digest)
		if err != nil {
			return NoticeAdmission{}, ledgerFailed("record a notice's payload", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return NoticeAdmission{}, ledgerFailed("admit a notice", err)
	}
	return got, nil
}

const selectPendingNotice = `SELECT i.state, i.project, i.source, i.kind, i.repo, i.pr, i.receipt_key,
	p.capture_id, p.version, p.receipt_version, p.subject, p.worker, p.observed_at, p.text, p.bytes, p.digest
FROM notice_identities AS i LEFT JOIN notice_payloads AS p ON p.capture_id = i.capture_id
WHERE i.capture_id = ?`

// PendingNotice is the envelope of the pending notice captureID, its
// receipt included, as it was admitted, with ObservedAt in UTC. An unknown
// ID, or a notice not pending, is ErrNoNotice. A pending notice whose
// payload is missing, or whose rows no longer make the capture ID, digest
// and size they were admitted with, is ErrStore: a row edited by hand is
// never handed on.
func (s *Store) PendingNotice(ctx context.Context, captureID string) (NoticeEnvelope, error) {
	var (
		state, project, source, kind                   string
		repo, key, payload, subject, worker, text, sum sql.NullString
		pr, version, receiptVersion, observed, size    sql.NullInt64
	)
	err := s.db.QueryRowContext(ctx, selectPendingNotice, captureID).Scan(&state, &project, &source, &kind, &repo, &pr, &key,
		&payload, &version, &receiptVersion, &subject, &worker, &observed, &text, &size, &sum)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return NoticeEnvelope{}, fmt.Errorf("%w: by that capture ID", ErrNoNotice)
	case err != nil:
		return NoticeEnvelope{}, ledgerFailed("read a pending notice", err)
	case state != noticePending:
		return NoticeEnvelope{}, fmt.Errorf("%w: that notice is %s", ErrNoNotice, state)
	}
	tampered := fmt.Errorf("%w: a pending notice no longer matches what was admitted", ErrStore)
	// Only GitHub notices are admitted yet, and version 1 is the only one.
	if !payload.Valid || source != noticeFromGitHub || !repo.Valid || !pr.Valid || !key.Valid || version.Int64 != 1 || receiptVersion.Int64 != 1 {
		return NoticeEnvelope{}, tampered
	}
	e := NoticeEnvelope{
		Version: 1, CaptureID: captureID, Project: project, Source: source, Kind: kind,
		Subject: subject.String, Worker: worker.String, ObservedAt: time.Unix(0, observed.Int64).UTC(), Text: text.String,
		Receipt: &NoticeReceipt{Version: 1, Repo: repo.String, PR: pr.Int64, Kind: kind, Key: key.String},
	}
	c, err := captureOf(e)
	if err != nil || c.id != captureID || c.digest != sum.String || c.bytes != size.Int64 {
		return NoticeEnvelope{}, tampered
	}
	return e, nil
}
