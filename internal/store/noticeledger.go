package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// The notice admission ledger (NERD-5533 B0a.2, migration 0009) records
// each notice once, before anything tells it: a GitHub notice admitted once
// is never admitted again; the ledger does not dedupe a daemon notice. It is
// dormant: nothing in production calls it yet, it posts nothing to the board
// and wakes no reader, and it never writes a github_* table.

// ErrNoNotice means no notice with that capture ID is pending: none was
// admitted, or it was imported, or it is settled.
var ErrNoNotice = errors.New("no such pending notice")

// The states AdmitNotice writes. A pending notice is settled later, as
// posted or dropped.
const (
	noticePending  = "pending"
	noticeImported = "imported"
)

// The sources AdmitNotice admits: GitHub's, each notice identified by its
// receipt and the ID its content derives, and the daemon's, each by an ID
// the store allocates (B0a.4). A team post is never a notice: it goes to
// the board by Post, which records its author.
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
// what it did. It admits a GitHub notice and a daemon notice; a team post,
// or any other source, is ErrInvalid.
//
// A GitHub notice:
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
// A daemon notice (B0a.4):
//
//   - e must be a valid version-1 daemon envelope (daemonNoticeEncoding)
//     with no CaptureID and no receipt, its project empty or a line, and at
//     a time the store can hold, not before 1970; anything else is
//     ErrInvalid, and no ID is drawn.
//   - The store allocates its ID, nt1:<ULID>:<digest>: the ULID at's
//     millisecond and 80 random bits, the digest its encoding's SHA-256.
//   - It is never a Duplicate: two equal notices are two identities. What
//     keeps a daemon notice from being told twice after a restart is the
//     daemon's memory of what it recorded, not the ID. An ID drawn twice is
//     ErrStore, and nothing is written.
//   - It is recorded pending, with its payload, unless it would exceed a
//     quota: it faces what a pending GitHub notice faces but Q3, since it
//     has no pull request, and a refusal writes nothing.
//
// It writes no table but the ledger's and wakes no reader.
func (s *Store) AdmitNotice(ctx context.Context, e NoticeEnvelope, at time.Time) (NoticeAdmission, error) {
	switch {
	case e.Source == noticeFromDaemon:
		return s.admitDaemonNotice(ctx, e, at)
	// A GitHub notice with no project can never be imported, nor settled:
	// github_seen refuses an empty project.
	case e.Source != noticeFromGitHub || e.Project == "":
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

// The daemon's notices (B0a.4). The nt1 encoding is frozen: once a daemon
// notice is admitted, a changed encoding would fail its pending row.
const (
	// nt1Prefix opens a daemon notice's ID, nt1:<ULID>:<digest>.
	nt1Prefix = "nt1:"
	// maxDaemonKind is the longest kind, in bytes: notice_identities.kind's.
	maxDaemonKind = 64
	// maxDaemonEncoding bounds an nt1 encoding. Valid input makes at most
	// 9,070 bytes: 16, 6 lengths of 4, a project, subject and worker of 256,
	// the source's 6, a kind of 64 and a text of MaxBody.
	maxDaemonEncoding = 9216
)

// githubNoticeKinds are GitHub's kinds (A45), which no daemon notice has.
var githubNoticeKinds = []string{"merged", "ci_failed", "ci_passed", "dirty", "approved", "review"}

// nt1Layout is e's version-1 daemon encoding, nt1, unchecked: the version,
// then ObservedAt's nanoseconds as their two's-complement bits, each an
// unsigned 64-bit big-endian word; then the project, source, kind,
// subject, worker and text, each an unsigned 32-bit big-endian byte length
// and its bytes.
func nt1Layout(e NoticeEnvelope) []byte {
	out := make([]byte, 0, 16+6*4+len(e.Project)+len(e.Source)+len(e.Kind)+len(e.Subject)+len(e.Worker)+len(e.Text))
	out = binary.BigEndian.AppendUint64(out, e.Version)
	out = binary.BigEndian.AppendUint64(out, uint64(e.ObservedAt.UnixNano())) // #nosec G115 -- signed nanoseconds use their two's-complement bits
	for _, field := range []string{e.Project, e.Source, e.Kind, e.Subject, e.Worker, e.Text} {
		out = binary.BigEndian.AppendUint32(out, uint32(len(field))) // #nosec G115 -- a checked field is at most MaxBody bytes
		out = append(out, field...)
	}
	return out
}

// daemonNoticeEncoding checks e as a daemon notice's content, then returns
// its nt1 encoding. A version-1 daemon envelope with no receipt; a kind of
// 1 to 64 bytes, a line and none of GitHub's; a project, subject and
// worker each empty or a line; a text Send would queue (CheckBody); and an
// ObservedAt the store's nanoseconds hold. Anything else is noticeInvalid,
// which carries none of it. It reads nothing else of e, CaptureID included.
func daemonNoticeEncoding(e NoticeEnvelope) ([]byte, error) {
	if e.Version != 1 || e.Source != noticeFromDaemon || e.Receipt != nil {
		return nil, noticeInvalid()
	}
	if len(e.Kind) > maxDaemonKind || checkLine("kind", e.Kind, false) != nil || slices.Contains(githubNoticeKinds, e.Kind) {
		return nil, noticeInvalid()
	}
	for _, field := range []string{e.Project, e.Subject, e.Worker} {
		if checkLine("notice", field, true) != nil {
			return nil, noticeInvalid()
		}
	}
	if CheckBody(e.Text) != nil {
		return nil, noticeInvalid()
	}
	if _, err := nanos(e.ObservedAt.UTC()); err != nil {
		return nil, noticeInvalid()
	}
	out := nt1Layout(e)
	if len(out) > maxDaemonEncoding {
		return nil, noticeInvalid()
	}
	return out, nil
}

// isNT1 reports whether id is a daemon notice's ID for digest:
// nt1:<ULID>:<digest>, the ULID 26 characters of Crockford's base32 in
// capitals, the first 0 to 7, as newULID makes them.
func isNT1(id, digest string) bool {
	rest, ok := strings.CutPrefix(id, nt1Prefix)
	if !ok {
		return false
	}
	ulid, suffix, ok := strings.Cut(rest, ":")
	if !ok || len(ulid) != 26 || ulid[0] > '7' || suffix != digest {
		return false
	}
	for i := range len(ulid) {
		if strings.IndexByte(crockford, ulid[i]) < 0 {
			return false
		}
	}
	return true
}

// admitDaemonNotice is AdmitNotice's daemon path. It checks e and at before
// it draws anything, allocates the notice's ID, and records it pending,
// with its payload, past the quotas, in one transaction. No error carries
// e, at or a callee's error that would.
func (s *Store) admitDaemonNotice(ctx context.Context, e NoticeEnvelope, at time.Time) (NoticeAdmission, error) {
	if e.CaptureID != "" {
		return NoticeAdmission{}, noticeInvalid()
	}
	encoded, err := daemonNoticeEncoding(e)
	if err != nil {
		return NoticeAdmission{}, err
	}
	// A ULID holds milliseconds since 1970.
	ns, err := nanos(at)
	if err != nil || ns < 0 {
		return NoticeAdmission{}, noticeInvalid()
	}
	sum := sha256.Sum256(encoded)
	digest, size := hex.EncodeToString(sum[:]), int64(len(encoded))
	ulid, err := newULID(at, s.noticeEntropy)
	if err != nil {
		// Only the entropy can fail here, and its error is not handed on.
		return NoticeAdmission{}, fmt.Errorf("%w: draw a notice ID", ErrStore)
	}
	id := nt1Prefix + ulid + ":" + digest
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return NoticeAdmission{}, ledgerFailed("admit a notice", err)
	}
	defer tx.Rollback() //nolint:errcheck // a no-op once committed; a refusal writes nothing to commit
	if err := checkNoticeQuotas(ctx, tx, s.quotas, e.Project, nil, size, true, ns); err != nil {
		return NoticeAdmission{}, err
	}
	// An ID drawn twice fails the primary key: ErrStore, never a Duplicate.
	if _, err := tx.ExecContext(ctx, insertNoticeIdentity, id, e.Project, e.Source, e.Kind, nil, nil, nil, noticePending, ns); err != nil {
		return NoticeAdmission{}, ledgerFailed("record a notice", err)
	}
	if _, err := tx.ExecContext(ctx, insertNoticePayload, id, e.Version, nil, e.Subject, e.Worker, e.ObservedAt.UnixNano(), e.Text, size, digest); err != nil {
		return NoticeAdmission{}, ledgerFailed("record a notice's payload", err)
	}
	if err := tx.Commit(); err != nil {
		return NoticeAdmission{}, ledgerFailed("admit a notice", err)
	}
	return NoticeAdmission{CaptureID: id, State: noticePending}, nil
}

const selectPendingNotice = `SELECT i.state, i.project, i.source, i.kind, i.repo, i.pr, i.receipt_key,
	p.capture_id, p.version, p.receipt_version, p.subject, p.worker, p.observed_at, p.text, p.bytes, p.digest
FROM notice_identities AS i LEFT JOIN notice_payloads AS p ON p.capture_id = i.capture_id
WHERE i.capture_id = ?`

// PendingNotice is the envelope of the pending notice captureID, its
// receipt included, as it was admitted, with ObservedAt in UTC: a GitHub
// notice's with its CaptureID, a daemon notice's with none, since the
// store allocated it (B0a.4). An unknown ID, or a notice not pending, is
// ErrNoNotice. A pending notice whose payload is missing, whose source is
// neither GitHub's nor the daemon's, or whose rows no longer make the
// capture ID, digest and size they were admitted with, is ErrStore: a row
// edited by hand is never handed on.
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
	// Version 1 is the only one.
	if !payload.Valid || version.Int64 != 1 {
		return NoticeEnvelope{}, tampered
	}
	e := NoticeEnvelope{
		Version: 1, Project: project, Source: source, Kind: kind,
		Subject: subject.String, Worker: worker.String, ObservedAt: time.Unix(0, observed.Int64).UTC(), Text: text.String,
	}
	switch source {
	case noticeFromGitHub:
		if !repo.Valid || !pr.Valid || !key.Valid || receiptVersion.Int64 != 1 {
			return NoticeEnvelope{}, tampered
		}
		e.CaptureID = captureID
		e.Receipt = &NoticeReceipt{Version: 1, Repo: repo.String, PR: pr.Int64, Kind: kind, Key: key.String}
		c, err := captureOf(e)
		if err != nil || c.id != captureID || c.digest != sum.String || c.bytes != size.Int64 {
			return NoticeEnvelope{}, tampered
		}
	case noticeFromDaemon:
		// 0009's CHECKs keep a daemon row's repo, PR and key NULL. It is
		// rebuilt from the stored source, so no other source's row passes.
		if receiptVersion.Valid {
			return NoticeEnvelope{}, tampered
		}
		encoded, err := daemonNoticeEncoding(e)
		if err != nil {
			return NoticeEnvelope{}, tampered
		}
		digest := sha256.Sum256(encoded)
		if hex.EncodeToString(digest[:]) != sum.String || int64(len(encoded)) != size.Int64 || !isNT1(captureID, sum.String) {
			return NoticeEnvelope{}, tampered
		}
	default:
		// A team post is never a notice, however its rows agree.
		return NoticeEnvelope{}, tampered
	}
	return e, nil
}
