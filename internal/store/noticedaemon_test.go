package store_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/notice"
	"github.com/abanna/herdr-agentisan/internal/store"
)

// The daemon's notices (NERD-5533 B0a.4-ids): AdmitNotice allocates each an
// ID, nt1:<ULID>:<digest>, and PendingNotice re-checks it.

// daemonAt is when the daemon notice tests admit. daemonULID is the ULID
// drawn at daemonAt from fixedEntropy, worked out apart from the store, with
// Python's integers: the millisecond 1794916801000, then the bytes 0 to 9.
var daemonAt = time.Date(2026, 11, 17, 12, 0, 1, 0, time.UTC)

const daemonULID = "01M7MP1PF8000G40R40M30E209"

// nt1Pattern is a daemon notice's ID: nt1:, a ULID whose first character is
// 0 to 7, in Crockford's base32 without I, L, O or U, then the digest.
var nt1Pattern = regexp.MustCompile(`^nt1:[0-7][0-9A-HJKMNP-TV-Z]{25}:[0-9a-f]{64}$`)

// daemonNotice is a valid daemon notice, kind turn_ended of project ha,
// subject w-1 and worker ha-coder-1, with one line of text, changed by each
// change.
func daemonNotice(change ...func(*store.NoticeEnvelope)) store.NoticeEnvelope {
	e := store.NoticeEnvelope{
		Version: 1, Project: "ha", Source: "daemon", Kind: "turn_ended", Subject: "w-1", Worker: "ha-coder-1",
		ObservedAt: daemonAt.Add(-time.Second), Text: "ha-coder-1 finished a turn",
	}
	for _, c := range change {
		c(&e)
	}
	return e
}

// admitDaemon admits e at at, which must be admitted pending, and returns
// its ID.
func admitDaemon(t *testing.T, s *store.Store, e store.NoticeEnvelope, at time.Time) string {
	t.Helper()
	got, err := s.AdmitNotice(t.Context(), e, at)
	require.NoError(t, err)
	require.Equal(t, store.NoticeAdmission{CaptureID: got.CaptureID, State: "pending"}, got)
	return got.CaptureID
}

// fixedEntropy is the bytes 0 to 9: the random part of one ID.
func fixedEntropy() io.Reader { return bytes.NewReader([]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}) }

// repeating reads its ten bytes from the start at every read, so every ID
// drawn from it has the same random part.
type repeating [10]byte

func (r repeating) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = r[i%len(r)]
	}
	return len(p), nil
}

// counting counts what is read through it.
type counting struct {
	r io.Reader
	n atomic.Int64
}

func (c *counting) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

// failing is entropy that cannot be read.
type failing struct{ err error }

func (f failing) Read([]byte) (int, error) { return 0, f.err }

// ulidMillis is the millisecond an nt1 ID's ULID holds: its first ten
// characters, 50 bits whose top two are 0.
func ulidMillis(id string) int64 {
	var ms int64
	for _, c := range id[4:14] {
		ms = ms<<5 | int64(strings.IndexRune("0123456789ABCDEFGHJKMNPQRSTVWXYZ", c))
	}
	return ms
}

// storedPayload is the size and digest stored for id.
func storedPayload(t *testing.T, s *store.Store, id string) (int64, string) {
	t.Helper()
	var (
		size   int64
		digest string
	)
	require.NoError(t, store.DB(s).QueryRowContext(t.Context(), "SELECT bytes, digest FROM notice_payloads WHERE capture_id = ?", id).Scan(&size, &digest))
	return size, digest
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	require.NoError(t, err)
	return b
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// The frozen vectors (section 2.4): each envelope, its encoding and its
// SHA-256, as the packet pins them. V3's encoding, too long to print, is
// built below from its parts and pinned by its length.
var (
	v1Notice = store.NoticeEnvelope{
		Version: 1, Project: "ha", Source: "daemon", Kind: "turn_ended", Subject: "w-1", Worker: "ha-coder-1",
		ObservedAt: time.Date(2026, 11, 17, 12, 0, 0, 123456789, time.UTC), Text: "ha-coder-1 finished a turn",
	}
	v2Notice = store.NoticeEnvelope{
		Version: 1, Source: "daemon", Kind: "started",
		ObservedAt: time.Date(1969, 7, 20, 20, 17, 40, 0, time.UTC), Text: "line one\nline two",
	}
	v3Text   = strings.Repeat(strings.Repeat("a", 63)+"\n", 128)
	v3Notice = store.NoticeEnvelope{
		Version: 1, Project: "agentisan", Source: "daemon", Kind: "ctx_ceiling", Subject: "w-9", Worker: "ha-qa-2",
		ObservedAt: time.Unix(0, math.MaxInt64).UTC(), Text: v3Text,
	}
	v4Notice = store.NoticeEnvelope{
		Version: 1, Project: "ha", Source: "daemon", Kind: "blocked", Subject: "w-2", Worker: "ha-qa-1",
		ObservedAt: time.Date(2026, 11, 17, 18, 30, 0, 5, time.UTC), Text: "ha-qa-1 is blocked ◆ on a prompt — answer it",
	}
)

const (
	v1Hex    = "000000000000000118e8d351e0bd4d15000000026861000000066461656d6f6e0000000a7475726e5f656e64656400000003772d310000000a68612d636f6465722d310000001a68612d636f6465722d312066696e69736865642061207475726e"
	v1Digest = "cd4ebc04a0cf3366d8ff97fe65d0a94603100dee3c73973e53a5dd93ce27b98f"
	v2Hex    = "0000000000000001ffcd9cb0f36de80000000000000000066461656d6f6e00000007737461727465640000000000000000000000116c696e65206f6e650a6c696e652074776f"
	v2Digest = "4f780b0306873f01b185e5a7673f3f38301cde7c0e6e3e34d21ee03b5001f2a4"
	v3Digest = "b5d30314a4b39dba80c07c8493cafafea03b651d01f75b97d06895fba5cea188"
	v4Hex    = "000000000000000118e8e89a16119005000000026861000000066461656d6f6e00000007626c6f636b656400000003772d320000000768612d71612d310000003068612d71612d3120697320626c6f636b656420e29786206f6e20612070726f6d707420e2809420616e73776572206974"
	v4Digest = "508a46f2ab12872f4299faf00ff12d2cd2aeea7cc1c693ecf56088788e206e62"
)

// v3Encoding is V3's encoding, built from the packet's layout by hand: the
// version and the largest nanosecond, then each field's 32-bit length and
// bytes.
func v3Encoding(t *testing.T) []byte {
	t.Helper()
	out := mustHex(t, "0000000000000001"+"7fffffffffffffff")
	for _, field := range []string{"agentisan", "daemon", "ctx_ceiling", "w-9", "ha-qa-2", v3Text} {
		out = binary.BigEndian.AppendUint32(out, uint32(len(field)))
		out = append(out, field...)
	}
	return out
}

// TestDaemonNoticeVectors (V1 to V4): the nt1 encoding is frozen. Each
// vector's bytes are its literal, whose length and SHA-256 are the pinned
// ones, and the store, at daemonAt with fixed entropy, admits it as
// nt1:<daemonULID>:<digest>, with that size and digest stored.
func TestDaemonNoticeVectors(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	require.Len(t, []rune(v4Notice.Text), 44)
	for _, v := range []struct {
		name     string
		e        store.NoticeEnvelope
		encoding []byte
		size     int
		digest   string
	}{
		{"V1", v1Notice, mustHex(t, v1Hex), 97, v1Digest},
		{"V2", v2Notice, mustHex(t, v2Hex), 70, v2Digest},
		{"V3", v3Notice, v3Encoding(t), 8268, v3Digest},
		{"V4", v4Notice, mustHex(t, v4Hex), 113, v4Digest},
	} {
		require.Len(t, v.encoding, v.size, v.name)
		require.Equal(t, v.digest, sha256Hex(v.encoding), "%s: the literal is the pinned digest's", v.name)
		store.SetNoticeEntropy(s, fixedEntropy())
		id := admitDaemon(t, s, v.e, daemonAt)
		assert.Equal(t, "nt1:"+daemonULID+":"+v.digest, id, v.name)
		size, digest := storedPayload(t, s, id)
		assert.Equal(t, int64(v.size), size, v.name)
		assert.Equal(t, v.digest, digest, v.name)
	}
}

// TestDaemonNoticeGetsAnAllocatedID (S1): a daemon notice's ID is nt1, its
// ULID holding the admission's millisecond; two equal notices are two
// identities, and the totals move by two. Many at once are as many.
func TestDaemonNoticeGetsAnAllocatedID(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	db := store.DB(s)
	at := daemonAt.Add(123*time.Millisecond + 456*time.Microsecond)
	e := daemonNotice()
	first, second := admitDaemon(t, s, e, at), admitDaemon(t, s, e, at)
	for _, id := range []string{first, second} {
		assert.Regexp(t, nt1Pattern, id)
		assert.Equal(t, at.UnixMilli(), ulidMillis(id), "the ULID holds the admission's millisecond")
		var state, source string
		require.NoError(t, db.QueryRowContext(t.Context(), "SELECT state, source FROM notice_identities WHERE capture_id = ?", id).Scan(&state, &source))
		assert.Equal(t, []string{"pending", "daemon"}, []string{state, source})
	}
	var admitted, observed int64
	require.NoError(t, db.QueryRowContext(t.Context(),
		"SELECT i.admitted_at, p.observed_at FROM notice_identities AS i JOIN notice_payloads AS p USING (capture_id) WHERE capture_id = ?",
		first).Scan(&admitted, &observed))
	assert.Equal(t, at.UnixNano(), admitted, "admitted_at is the admission's nanosecond, not the observed time")
	assert.Equal(t, e.ObservedAt.UnixNano(), observed)
	assert.NotEqual(t, first, second, "no dedupe by content")
	assert.Equal(t, first[31:], second[31:], "the same content has the same digest")
	assert.Equal(t, [2]int{2, 2}, ledgerRows(t, s))
	projects, all := noticeTotals(t, db)
	assert.Equal(t, map[string]int64{"ha": 2}, projects)
	assert.Equal(t, int64(2), all)
	assertNoticeTotals(t, db)

	const many = 16
	ids := make(chan string, many)
	var wg sync.WaitGroup
	for range many {
		wg.Go(func() {
			got, err := s.AdmitNotice(t.Context(), e, at)
			assert.NoError(t, err)
			ids <- got.CaptureID
		})
	}
	wg.Wait()
	close(ids)
	seenIDs := map[string]bool{first: true, second: true}
	for id := range ids {
		assert.Regexp(t, nt1Pattern, id)
		assert.False(t, seenIDs[id], "each admission its own ID")
		seenIDs[id] = true
	}
	assert.Len(t, seenIDs, 2+many)
	assert.Equal(t, [2]int{2 + many, 2 + many}, ledgerRows(t, s))
	assertNoticeTotals(t, db)
}

// TestDaemonNoticeIsCheckedAsAPost (S2): a daemon notice is checked before
// anything is drawn: each refusal is ErrInvalid, writes nothing, leaves the
// totals and reads no entropy; each edge of the checks is admitted.
func TestDaemonNoticeIsCheckedAsAPost(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	db := store.DB(s)
	valid := admitDaemon(t, s, daemonNotice(), daemonAt)
	draws := &counting{r: rand.Reader}
	store.SetNoticeEntropy(s, draws)
	rows, projects := ledgerRows(t, s), queryRows(t, db, "SELECT * FROM notice_counts ORDER BY project")
	refused := func(name string, e store.NoticeEnvelope, at time.Time) {
		t.Helper()
		got, err := s.AdmitNotice(t.Context(), e, at)
		require.ErrorIs(t, err, store.ErrInvalid, name)
		assert.NotErrorIs(t, err, store.ErrStore, name)
		assert.Equal(t, store.NoticeAdmission{}, got, name)
		assert.Zero(t, draws.n.Load(), "%s: no entropy is drawn", name)
		assert.Equal(t, rows, ledgerRows(t, s), name)
		assert.Equal(t, projects, queryRows(t, db, "SELECT * FROM notice_counts ORDER BY project"), name)
	}
	for name, change := range map[string]func(*store.NoticeEnvelope){
		"a valid nt1 ID supplied": func(e *store.NoticeEnvelope) { e.CaptureID = valid },
		"a GitHub ID supplied":    func(e *store.NoticeEnvelope) { e.CaptureID = admissible(t).CaptureID },
		"some ID supplied":        func(e *store.NoticeEnvelope) { e.CaptureID = "x" },
		"a receipt": func(e *store.NoticeEnvelope) {
			e.Receipt = &store.NoticeReceipt{Version: 1, Repo: ledgerRepo, PR: 50, Kind: "turn_ended"}
		},
		"version 0":                      func(e *store.NoticeEnvelope) { e.Version = 0 },
		"version 2":                      func(e *store.NoticeEnvelope) { e.Version = 2 },
		"the largest version":            func(e *store.NoticeEnvelope) { e.Version = math.MaxUint64 },
		"no kind":                        func(e *store.NoticeEnvelope) { e.Kind = "" },
		"a kind of 65 bytes":             func(e *store.NoticeEnvelope) { e.Kind = strings.Repeat("k", 65) },
		"a kind holding a control":       func(e *store.NoticeEnvelope) { e.Kind = "turn\x01ended" },
		"a kind holding a line break":    func(e *store.NoticeEnvelope) { e.Kind = "turn\nended" },
		"GitHub's kind review":           func(e *store.NoticeEnvelope) { e.Kind = "review" },
		"GitHub's kind merged":           func(e *store.NoticeEnvelope) { e.Kind = "merged" },
		"a project of 257 bytes":         func(e *store.NoticeEnvelope) { e.Project = strings.Repeat("p", 257) },
		"a project holding a control":    func(e *store.NoticeEnvelope) { e.Project = "h\ta" },
		"a project not UTF-8":            func(e *store.NoticeEnvelope) { e.Project = "h\xff" },
		"a subject of 257 bytes":         func(e *store.NoticeEnvelope) { e.Subject = strings.Repeat("s", 257) },
		"a subject holding a control":    func(e *store.NoticeEnvelope) { e.Subject = "w-\x7f1" },
		"a subject holding a format":     func(e *store.NoticeEnvelope) { e.Subject = "\ufeffw-1" },
		"a worker of 257 bytes":          func(e *store.NoticeEnvelope) { e.Worker = strings.Repeat("w", 257) },
		"a worker holding a control":     func(e *store.NoticeEnvelope) { e.Worker = "ha-coder-1\n" },
		"no text":                        func(e *store.NoticeEnvelope) { e.Text = "" },
		"a text of 8,193 bytes":          func(e *store.NoticeEnvelope) { e.Text = strings.Repeat("a", 8193) },
		"a text holding a NUL":           func(e *store.NoticeEnvelope) { e.Text = "a\x00b" },
		"a text not UTF-8":               func(e *store.NoticeEnvelope) { e.Text = "caf\xe9" },
		"a text ending mid-character":    func(e *store.NoticeEnvelope) { e.Text = "turn \xc3" },
		"a text holding a surrogate":     func(e *store.NoticeEnvelope) { e.Text = "a\xed\xa0\x80b" },
		"a text mixing UTF-8 and not":    func(e *store.NoticeEnvelope) { e.Text = "é\xff" },
		"a project ending mid-character": func(e *store.NoticeEnvelope) { e.Project = "h\xc3" },
		"observed at the zero time":      func(e *store.NoticeEnvelope) { e.ObservedAt = time.Time{} },
		"observed in 2263":               func(e *store.NoticeEnvelope) { e.ObservedAt = time.Date(2263, 1, 1, 0, 0, 0, 0, time.UTC) },
		"observed before the first time": func(e *store.NoticeEnvelope) { e.ObservedAt = time.Unix(0, math.MinInt64).Add(-time.Nanosecond) },
	} {
		refused(name, daemonNotice(change), daemonAt)
	}
	// Each line field refuses each malformed value as the others do: bytes
	// that are not UTF-8, a cut or surrogate sequence, Latin-1, a mix, NUL,
	// a carriage return, a byte-order mark and a tab.
	for _, bad := range []string{"\xff", "\xc3", "\xed\xa0\x80", "caf\xe9", "é\xff", "a\x00b", "a\rb", "\ufeffa", "a\tb"} {
		for field, set := range map[string]func(*store.NoticeEnvelope){
			"kind":    func(e *store.NoticeEnvelope) { e.Kind = bad },
			"project": func(e *store.NoticeEnvelope) { e.Project = bad },
			"subject": func(e *store.NoticeEnvelope) { e.Subject = bad },
			"worker":  func(e *store.NoticeEnvelope) { e.Worker = bad },
		} {
			refused(fmt.Sprintf("%s %q", field, bad), daemonNotice(set), daemonAt)
		}
	}
	for name, at := range map[string]time.Time{
		"admitted 1 ms before 1970":   time.Unix(0, 0).Add(-time.Millisecond),
		"admitted 1 ns before 1970":   time.Unix(0, -1),
		"admitted at the zero time":   {},
		"admitted in 2263":            time.Date(2263, 1, 1, 0, 0, 0, 0, time.UTC),
		"admitted past the last time": time.Unix(0, math.MaxInt64).Add(time.Nanosecond),
	} {
		refused(name, daemonNotice(), at)
	}

	for name, tc := range map[string]struct {
		e  store.NoticeEnvelope
		at time.Time
	}{
		"a kind of 64 bytes":                 {daemonNotice(func(e *store.NoticeEnvelope) { e.Kind = strings.Repeat("k", 64) }), daemonAt},
		"a text of 8,192 bytes with breaks":  {daemonNotice(func(e *store.NoticeEnvelope) { e.Text = v3Text }), daemonAt},
		"no project":                         {daemonNotice(func(e *store.NoticeEnvelope) { e.Project = "" }), daemonAt},
		"no subject and no worker":           {daemonNotice(func(e *store.NoticeEnvelope) { e.Subject, e.Worker = "", "" }), daemonAt},
		"a kind not ASCII":                   {daemonNotice(func(e *store.NoticeEnvelope) { e.Kind = "tour_terminé" }), daemonAt},
		"GitHub's kind in capitals, another": {daemonNotice(func(e *store.NoticeEnvelope) { e.Kind = "MERGED" }), daemonAt},
		"fields of 256 bytes": {daemonNotice(func(e *store.NoticeEnvelope) {
			e.Project, e.Subject, e.Worker = strings.Repeat("é", 128), strings.Repeat("s", 256), strings.Repeat("w", 256)
		}), daemonAt},
		"admitted at 1970":           {daemonNotice(), time.Unix(0, 0)},
		"admitted at the last time":  {daemonNotice(), time.Unix(0, math.MaxInt64)},
		"observed at the first time": {daemonNotice(func(e *store.NoticeEnvelope) { e.ObservedAt = time.Unix(0, math.MinInt64) }), daemonAt},
	} {
		got, err := s.AdmitNotice(t.Context(), tc.e, tc.at)
		require.NoError(t, err, name)
		assert.Regexp(t, nt1Pattern, got.CaptureID, name)
		assert.Equal(t, "pending", got.State, name)
	}
	assertNoticeTotals(t, db)
}

// TestDaemonNoticeSizeAndDigest (S3): the stored size and digest are the
// encoding's, the digest is the ID's suffix, and each tampering, alone,
// makes PendingNotice ErrStore, a rewrite consistent with itself included.
func TestDaemonNoticeSizeAndDigest(t *testing.T) {
	t.Parallel()
	// another is V1 with another text, its encoding built here by hand:
	// V1's literal less its text, then the new text's length and bytes.
	const anotherText = "ha-coder-1 finished two turns"
	v1 := mustHex(t, v1Hex)
	another := binary.BigEndian.AppendUint32(slices.Clone(v1[:len(v1)-4-len(v1Notice.Text)]), uint32(len(anotherText)))
	another = append(another, anotherText...)
	for name, tc := range map[string]struct {
		edit string
		args []any
	}{
		"the project":              {edit: "UPDATE notice_identities SET project = 'hb'"},
		"the kind":                 {edit: "UPDATE notice_identities SET kind = 'blocked'"},
		"the subject":              {edit: "UPDATE notice_payloads SET subject = 'w-2'"},
		"the worker":               {edit: "UPDATE notice_payloads SET worker = 'ha-coder-2'"},
		"the text":                 {edit: "UPDATE notice_payloads SET text = 'x'"},
		"the time":                 {edit: "UPDATE notice_payloads SET observed_at = observed_at + 1"},
		"the size":                 {edit: "UPDATE notice_payloads SET bytes = bytes + 1"},
		"the digest":               {edit: "UPDATE notice_payloads SET digest = ?", args: []any{v2Digest}},
		"the receipt version":      {edit: "UPDATE notice_payloads SET receipt_version = 1"},
		"another text, whole":      {edit: "UPDATE notice_payloads SET text = ?, bytes = ?, digest = ?", args: []any{anotherText, len(another), sha256Hex(another)}},
		"the payload gone":         {edit: "DELETE FROM notice_payloads"},
		"text not UTF-8":           {edit: "UPDATE notice_payloads SET text = CAST(X'FF' AS TEXT)"},
		"text cut mid-character":   {edit: "UPDATE notice_payloads SET text = CAST(X'C3' AS TEXT)"},
		"text holding a surrogate": {edit: "UPDATE notice_payloads SET text = CAST(X'61EDA08062' AS TEXT)"},
		"text holding a NUL":       {edit: "UPDATE notice_payloads SET text = CAST(X'610062' AS TEXT)"},
		"subject not UTF-8":        {edit: "UPDATE notice_payloads SET subject = CAST(X'FF' AS TEXT)"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := open(t, dbPath(t))
			store.SetNoticeEntropy(s, fixedEntropy())
			id := admitDaemon(t, s, v1Notice, daemonAt)
			size, digest := storedPayload(t, s, id)
			require.Equal(t, int64(len(v1)), size)
			require.Equal(t, v1Digest, digest)
			require.True(t, strings.HasSuffix(id, ":"+digest), "the digest is the ID's suffix")
			_, err := store.DB(s).ExecContext(t.Context(), tc.edit, tc.args...)
			require.NoError(t, err)
			got, err := s.PendingNotice(t.Context(), id)
			require.ErrorIs(t, err, store.ErrStore)
			assert.Equal(t, store.NoticeEnvelope{}, got)
		})
	}
}

// TestDaemonNoticeRoundTrip (S4): PendingNotice returns a daemon notice as
// it was admitted, field for field, with no CaptureID and no receipt, its
// time in UTC.
func TestDaemonNoticeRoundTrip(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	zone := time.FixedZone("UTC-6", -6*3600)
	inZone := v2Notice
	inZone.ObservedAt = inZone.ObservedAt.In(zone)
	asGiven := daemonNotice(func(e *store.NoticeEnvelope) {
		e.Project, e.Subject, e.Worker = "é-team'; DROP TABLE notice_payloads; --", "w-1 %_*[?]", "wörker \"quoted\""
		e.Text = "\ufeffa BOM-led text\r\nwith CRLF, a lone \r and no final break"
	})
	for _, e := range []store.NoticeEnvelope{v2Notice, v3Notice, v4Notice, inZone, asGiven} {
		id := admitDaemon(t, s, e, daemonAt)
		got, err := s.PendingNotice(t.Context(), id)
		require.NoError(t, err)
		assert.True(t, e.ObservedAt.Equal(got.ObservedAt), "%v read back as %v", e.ObservedAt, got.ObservedAt)
		assert.Equal(t, time.UTC, got.ObservedAt.Location())
		e.ObservedAt, got.ObservedAt = time.Time{}, time.Time{}
		assert.Equal(t, e, got)
		_, err = s.PendingNotice(t.Context(), strings.ToLower(id))
		require.ErrorIs(t, err, store.ErrNoNotice, "an ID differing in case is another")
	}
}

// TestDaemonNoticeFacesTheQuotas (S5): each quota but Q3 refuses a daemon
// notice at its limit, with its own error, writing nothing. With Q3 at 0, a
// daemon notice is admitted, and a GitHub notice is refused.
func TestDaemonNoticeFacesTheQuotas(t *testing.T) {
	t.Parallel()
	size := int64(len(mustHex(t, v1Hex))) // daemonNotice's size, as V1's
	in := func(project string) store.NoticeEnvelope {
		return daemonNotice(func(e *store.NoticeEnvelope) { e.Project = project })
	}
	for quota, tc := range map[int]struct {
		admitted []string
		set      func(*store.NoticeQuotas)
		next     string
	}{
		1: {[]string{"ha", "ha"}, func(q *store.NoticeQuotas) { q.PendingPerProject = 2 }, "ha"},
		2: {[]string{"ha", "hb"}, func(q *store.NoticeQuotas) { q.PendingInStore = 2 }, "hc"},
		4: {[]string{"ha"}, func(q *store.NoticeQuotas) { q.BytesPerProject = 2*size - 1 }, "ha"},
		5: {[]string{"ha"}, func(q *store.NoticeQuotas) { q.BytesInStore = 2*size - 1 }, "hb"},
		6: {[]string{"ha", "ha"}, func(q *store.NoticeQuotas) { q.IdentitiesPerProject = 2 }, "ha"},
		7: {[]string{"ha", "hb"}, func(q *store.NoticeQuotas) { q.IdentitiesInStore = 2 }, "hc"},
		8: {[]string{"ha", "ha"}, func(q *store.NoticeQuotas) { q.AdmissionsPerHour = 2 }, "ha"},
	} {
		t.Run(fmt.Sprintf("Q%d", quota), func(t *testing.T) {
			t.Parallel()
			f := newQuotaFixture(t)
			if quota == 8 {
				admitDaemon(t, f.s, in("ha"), daemonAt.Add(-store.NoticeRateWindow))
			}
			for _, project := range tc.admitted {
				admitDaemon(t, f.s, in(project), daemonAt)
			}
			f.set(tc.set)
			f.refused(in(tc.next), daemonAt, quota)
		})
	}
	t.Run("Q3", func(t *testing.T) {
		t.Parallel()
		f := newQuotaFixture(t)
		f.set(func(q *store.NoticeQuotas) { q.PendingPerPR = 0 })
		admitDaemon(t, f.s, daemonNotice(), daemonAt)
		f.refused(f.notice("ha", 50), ledgerAt, 3)
	})
	// Q8's window edge. daemonNotice's ObservedAt is a second before
	// daemonAt, so a window counted from it, or a row admitted at it, moves
	// the edge.
	for name, tc := range map[string]struct {
		earlier time.Time
		refused bool
	}{
		"a window before":           {daemonAt.Add(-store.NoticeRateWindow), false},
		"a window less 1 ns before": {daemonAt.Add(-store.NoticeRateWindow + time.Nanosecond), true},
	} {
		t.Run("Q8 edge, "+name, func(t *testing.T) {
			t.Parallel()
			f := newQuotaFixture(t)
			admitDaemon(t, f.s, daemonNotice(), tc.earlier)
			f.set(func(q *store.NoticeQuotas) { q.AdmissionsPerHour = 1 })
			if tc.refused {
				f.refused(daemonNotice(), daemonAt, 8)
				return
			}
			admitDaemon(t, f.s, daemonNotice(), daemonAt)
		})
	}
}

// TestTheCeilingPassesAFullHour (S13): with Q8 at 0, a daemon ctx_ceiling
// is admitted, while a daemon ctx_hard and a GitHub notice are refused at
// Q8; the ceiling counts in the daemon's hour. It skips Q8 alone: with Q1
// at 0 too, a ceiling is refused at Q1.
func TestTheCeilingPassesAFullHour(t *testing.T) {
	t.Parallel()
	f := newQuotaFixture(t)
	f.set(func(q *store.NoticeQuotas) { q.AdmissionsPerHour = 0 })
	ceiling := func() store.NoticeEnvelope {
		return daemonNotice(func(e *store.NoticeEnvelope) { e.Kind = string(notice.CtxCeiling) })
	}
	admitDaemon(t, f.s, ceiling(), daemonAt)
	f.refused(daemonNotice(func(e *store.NoticeEnvelope) { e.Kind = string(notice.CtxHard) }), daemonAt, 8)
	f.refused(f.notice("ha", 50), daemonAt, 8)
	st, err := f.s.NoticeStats(t.Context(), "ha", daemonAt)
	require.NoError(t, err)
	assert.Equal(t, int64(1), st.DaemonLastHour, "the ceiling counts")

	f.set(func(q *store.NoticeQuotas) { q.PendingPerProject = 0 })
	f.refused(ceiling(), daemonAt, 1)
}

// TestDaemonNoticeHasNoEffect (S6): admitting a daemon notice, or refusing
// one, writes no table but the ledger's and wakes no board reader.
func TestDaemonNoticeHasNoEffect(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	ctx := t.Context()
	seedWorker(t, s, "w", "w14:p2", 1)
	send(t, s, worker("w"), boss, "before the ledger")
	post(t, s, teamPost("ha", "w", "before the ledger"))
	require.NoError(t, s.UpdateGitHub(ctx, store.GitHubUpdate{Project: "ha", Repo: ledgerRepo, Collected: true, SeedKinds: []string{"review"}, Open: []int{50}}, ledgerSeen))
	seen(t, s, "ha", store.GitHubEvent{PR: 50, Kind: "merged", Key: headOID})
	before := otherTables(t, store.DB(s))
	require.Len(t, before["github_seen"], 1)
	require.Len(t, before["board_posts"], 1)
	require.Len(t, before["messages"], 1)
	woken := s.BoardChanged()

	admitDaemon(t, s, daemonNotice(), daemonAt)
	admitDaemon(t, s, daemonNotice(func(e *store.NoticeEnvelope) { e.Project = "" }), daemonAt)
	_, err := s.AdmitNotice(ctx, daemonNotice(func(e *store.NoticeEnvelope) { e.Kind = "merged" }), daemonAt)
	require.ErrorIs(t, err, store.ErrInvalid)
	store.SetNoticeQuotas(s, store.NoticeQuotas{})
	_, err = s.AdmitNotice(ctx, daemonNotice(), daemonAt)
	require.ErrorIs(t, err, store.ErrNoticeQuota)

	assert.Equal(t, before, otherTables(t, store.DB(s)), "no table but the ledger's changed")
	assert.Equal(t, [2]int{2, 2}, ledgerRows(t, s))
	select {
	case <-woken:
		t.Fatal("a daemon notice woke the board's readers")
	default:
	}
}

// TestDaemonNoticeIsAtomic (S7): a payload that cannot be written leaves no
// identity and the totals as they were; so do a cancelled admission and
// entropy that cannot be read.
func TestDaemonNoticeIsAtomic(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	db := store.DB(s)
	admitDaemon(t, s, daemonNotice(), daemonAt)
	projects, all := noticeTotals(t, db)
	_, err := db.ExecContext(t.Context(), "CREATE TEMP TRIGGER refuse_payload BEFORE INSERT ON main.notice_payloads BEGIN SELECT RAISE(ABORT, 'refused by the test'); END")
	require.NoError(t, err)

	_, err = s.AdmitNotice(t.Context(), daemonNotice(), daemonAt)
	require.ErrorIs(t, err, store.ErrStore)
	assert.Equal(t, [2]int{1, 1}, ledgerRows(t, s), "the identity went with its payload")
	afterProjects, afterAll := noticeTotals(t, db)
	assert.Equal(t, projects, afterProjects)
	assert.Equal(t, all, afterAll)

	_, err = db.ExecContext(t.Context(), "DROP TRIGGER temp.refuse_payload")
	require.NoError(t, err)
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = s.AdmitNotice(cancelled, daemonNotice(), daemonAt)
	require.ErrorIs(t, err, store.ErrStore)
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, [2]int{1, 1}, ledgerRows(t, s), "a cancelled admission writes nothing")

	store.SetNoticeEntropy(s, failing{errors.New("no entropy")})
	_, err = s.AdmitNotice(t.Context(), daemonNotice(), daemonAt)
	require.ErrorIs(t, err, store.ErrStore)
	assert.Equal(t, [2]int{1, 1}, ledgerRows(t, s), "entropy that cannot be read writes nothing")
	store.SetNoticeEntropy(s, rand.Reader)
	admitDaemon(t, s, daemonNotice(), daemonAt)
	assertNoticeTotals(t, db)
}

// TestTeamSourceIsRefused (S8): a team post is never a notice. Admitting
// one is ErrInvalid and writes nothing; a pending team row made by hand,
// consistent with itself whether encoded as the team's or as the daemon's,
// is ErrStore from PendingNotice, while the daemon's same row reads back.
func TestTeamSourceIsRefused(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	db := store.DB(s)
	draws := &counting{r: rand.Reader}
	store.SetNoticeEntropy(s, draws)
	got, err := s.AdmitNotice(t.Context(), daemonNotice(func(e *store.NoticeEnvelope) { e.Source = "team" }), daemonAt)
	require.ErrorIs(t, err, store.ErrInvalid)
	assert.Equal(t, store.NoticeAdmission{}, got)
	assert.Equal(t, [2]int{0, 0}, ledgerRows(t, s))
	assert.Zero(t, draws.n.Load())

	// handMade writes a pending row of source, e encoded with e.Source, under
	// an ID with ulid, as a hand edit would.
	handMade := func(ulid, source string, e store.NoticeEnvelope) string {
		encoded := store.NoticeEncoding(e)
		digest := sha256Hex(encoded)
		id := "nt1:" + ulid + ":" + digest
		_, err := db.ExecContext(t.Context(), insertIdentity, id, e.Project, source, e.Kind, nil, nil, nil, "pending", daemonAt.UnixNano(), nil, nil, nil, 0)
		require.NoError(t, err)
		_, err = db.ExecContext(t.Context(), insertPayload, id, 1, nil, e.Subject, e.Worker, e.ObservedAt.UnixNano(), e.Text, len(encoded), digest)
		require.NoError(t, err)
		return id
	}
	for _, encodedAs := range []string{"team", "daemon"} {
		id := handMade(daemonULID, "team", daemonNotice(func(e *store.NoticeEnvelope) { e.Source = encodedAs }))
		got, err := s.PendingNotice(t.Context(), id)
		require.ErrorIs(t, err, store.ErrStore, "a team row encoded as the %s's", encodedAs)
		assert.Equal(t, store.NoticeEnvelope{}, got)
	}
	e := daemonNotice()
	got2, err := s.PendingNotice(t.Context(), handMade("7ZZZZZZZZZZZZZZZZZZZZZZZZZ", "daemon", e))
	require.NoError(t, err, "the daemon's row, made the same way, reads back")
	assert.True(t, e.ObservedAt.Equal(got2.ObservedAt))
	e.ObservedAt, got2.ObservedAt = time.Time{}, time.Time{}
	assert.Equal(t, e, got2)
}

// TestDaemonNoticeErrorsCarryNoInput (S9): no error of a daemon notice's
// carries its content, the time it was admitted at, its time zone, or what
// a callee said.
func TestDaemonNoticeErrorsCarryNoInput(t *testing.T) {
	t.Parallel()
	const mark = "MARK"
	e := daemonNotice(func(e *store.NoticeEnvelope) {
		e.Project, e.Kind, e.Subject, e.Worker, e.Text = "proj"+mark, "kind"+mark, "subj"+mark, "work"+mark, "text"+mark
	})
	s := open(t, dbPath(t))
	var errs []error
	try := func(e store.NoticeEnvelope, at time.Time) {
		t.Helper()
		_, err := s.AdmitNotice(t.Context(), e, at)
		require.Error(t, err)
		errs = append(errs, err)
	}
	for _, change := range []func(*store.NoticeEnvelope){
		func(e *store.NoticeEnvelope) { e.CaptureID = "nt1:" + mark },
		func(e *store.NoticeEnvelope) { e.Text += "\x00" },
		func(e *store.NoticeEnvelope) { e.Text = strings.Repeat(mark, 2049) },
		func(e *store.NoticeEnvelope) { e.Kind += "\n" },
		func(e *store.NoticeEnvelope) { e.Kind += strings.Repeat("k", 64) },
		func(e *store.NoticeEnvelope) { e.Project += "\n" },
		func(e *store.NoticeEnvelope) { e.Subject += "\n" },
		func(e *store.NoticeEnvelope) { e.Worker += strings.Repeat("w", 256) },
		func(e *store.NoticeEnvelope) {
			e.ObservedAt = time.Date(2263, 1, 1, 0, 0, 0, 0, time.FixedZone("zone"+mark, 3600))
		},
		func(e *store.NoticeEnvelope) {
			e.Receipt = &store.NoticeReceipt{Version: 1, Repo: "own" + mark + "/repo", PR: 50}
		},
	} {
		bad := e
		change(&bad)
		try(bad, daemonAt)
	}
	try(e, time.Date(1969, 12, 31, 23, 59, 59, 0, time.FixedZone("zone"+mark, 3600)))
	try(e, time.Date(2263, 1, 1, 0, 0, 0, 0, time.FixedZone("zone"+mark, 3600)))
	store.SetNoticeEntropy(s, failing{errors.New("entropy " + mark)})
	try(e, daemonAt)
	store.SetNoticeEntropy(s, rand.Reader)
	_, err := store.DB(s).ExecContext(t.Context(), "CREATE TEMP TRIGGER refuse_payload BEFORE INSERT ON main.notice_payloads BEGIN SELECT RAISE(ABORT, 'refused by the test'); END")
	require.NoError(t, err)
	try(e, daemonAt)
	_, err = store.DB(s).ExecContext(t.Context(), "DROP TRIGGER temp.refuse_payload")
	require.NoError(t, err)

	id := admitDaemon(t, s, e, daemonAt)
	_, err = store.DB(s).ExecContext(t.Context(), "UPDATE notice_payloads SET text = 'x'")
	require.NoError(t, err)
	_, err = s.PendingNotice(t.Context(), id)
	errs = append(errs, err)
	_, err = s.PendingNotice(t.Context(), "nt1:"+mark)
	errs = append(errs, err)

	for i, err := range errs {
		require.Error(t, err, i)
		assert.NotContains(t, err.Error(), mark, "error %d: %v", i, err)
		assert.NotContains(t, err.Error(), "1969", "error %d: %v", i, err)
		assert.NotContains(t, err.Error(), "2263", "error %d: %v", i, err)
	}
}

// TestAnAllocationCollisionIsAnError (S10): an ID drawn twice, the same
// notice at the same millisecond with the same random bits, is ErrStore,
// never a Duplicate, and writes nothing; another text at that millisecond
// is admitted.
func TestAnAllocationCollisionIsAnError(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	store.SetNoticeEntropy(s, repeating{9, 8, 7, 6, 5, 4, 3, 2, 1, 0})
	first := admitDaemon(t, s, daemonNotice(), daemonAt)
	before := ledgerSnapshot(t, s)

	for _, at := range []time.Time{daemonAt, daemonAt.Add(time.Microsecond)} {
		got, err := s.AdmitNotice(t.Context(), daemonNotice(), at)
		require.ErrorIs(t, err, store.ErrStore, "at %v", at)
		assert.NotErrorIs(t, err, store.ErrInvalid)
		assert.NotErrorIs(t, err, store.ErrNoticeQuota)
		assert.Equal(t, store.NoticeAdmission{}, got, "not a Duplicate")
		assert.Equal(t, before, ledgerSnapshot(t, s), "a collision writes nothing")
	}

	other := admitDaemon(t, s, daemonNotice(func(e *store.NoticeEnvelope) { e.Text = "ha-coder-1 finished another turn" }), daemonAt)
	assert.Equal(t, first[:31], other[:31], "the same ULID")
	assert.NotEqual(t, first, other)
	assertNoticeTotals(t, store.DB(s))
}

// TestDaemonNoticeIDMustParse (S11): a pending daemon row whose ID is not
// nt1's grammar is ErrStore, though its digest is right. Each row is
// rewritten in both tables with foreign keys off; the same rewrite to a
// well-formed ID reads back.
func TestDaemonNoticeIDMustParse(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		rewrite func(id string) string
		want    error
	}{
		"a first ULID character 8": {func(id string) string { return id[:4] + "8" + id[5:] }, store.ErrStore},
		"a lowercase ULID":         {func(id string) string { return id[:4] + strings.ToLower(id[4:30]) + id[30:] }, store.ErrStore},
		"a ULID holding U":         {func(id string) string { return id[:5] + "U" + id[6:] }, store.ErrStore},
		"25 ULID characters":       {func(id string) string { return id[:4] + id[5:] }, store.ErrStore},
		"no colon after the ULID":  {func(id string) string { return id[:30] + id[31:] }, store.ErrStore},
		"another prefix":           {func(id string) string { return "nt2" + id[3:] }, store.ErrStore},
		"another well-formed ULID": {func(id string) string { return id[:4] + "7ZZZZZZZZZZZZZZZZZZZZZZZZZ" + id[30:] }, nil},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := open(t, dbPath(t))
			db := store.DB(s)
			store.SetNoticeEntropy(s, fixedEntropy())
			id := admitDaemon(t, s, v1Notice, daemonAt)
			require.Equal(t, "nt1:"+daemonULID+":"+v1Digest, id)
			moved := tc.rewrite(id)
			require.NotEqual(t, id, moved)
			_, err := db.ExecContext(t.Context(), "PRAGMA foreign_keys = OFF")
			require.NoError(t, err)
			for _, rewrite := range []string{
				"UPDATE notice_identities SET capture_id = ?1 WHERE capture_id = ?2",
				"UPDATE notice_payloads SET capture_id = ?1 WHERE capture_id = ?2",
			} {
				_, err := db.ExecContext(t.Context(), rewrite, moved, id)
				require.NoError(t, err, rewrite)
			}
			_, err = db.ExecContext(t.Context(), "PRAGMA foreign_keys = ON")
			require.NoError(t, err)
			require.Empty(t, queryRows(t, db, "PRAGMA foreign_key_check"), "both rows moved")
			got, err := s.PendingNotice(t.Context(), moved)
			if tc.want != nil {
				require.ErrorIs(t, err, tc.want)
				assert.Equal(t, store.NoticeEnvelope{}, got)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, v1Notice.Text, got.Text)
		})
	}
}

// TestNoticeKindsFitTheDaemonSource (C2, the kinds): each of notice.Kinds
// that B0a.1's receipt grammar takes, GitHub's six, is refused as a daemon
// notice, and every other kind is admitted. The store's own ceiling kind,
// which Q8 never refuses, is notice.CtxCeiling.
func TestNoticeKindsFitTheDaemonSource(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	var fromGitHub []string
	for _, kind := range notice.Kinds {
		got, err := s.AdmitNotice(t.Context(), daemonNotice(func(e *store.NoticeEnvelope) { e.Kind = string(kind) }), daemonAt)
		if githubGrammarTakes(string(kind)) {
			fromGitHub = append(fromGitHub, string(kind))
			require.ErrorIs(t, err, store.ErrInvalid, "GitHub's %s", kind)
			assert.Equal(t, store.NoticeAdmission{}, got)
			continue
		}
		require.NoError(t, err, "the daemon's %s", kind)
		assert.Equal(t, "pending", got.State)
	}
	assert.Equal(t, []string{"merged", "ci_failed", "ci_passed", "dirty", "approved", "review"}, fromGitHub)
	assert.Equal(t, string(notice.CtxCeiling), store.NoticeKindCeiling, "the store's ceiling kind is the daemon's")
}

// githubGrammarTakes reports whether B0a.1's grammar takes a GitHub notice
// of kind, with a key of any shape it has: none, a commit, or a login and
// a time.
func githubGrammarTakes(kind string) bool {
	for _, key := range []string{"", headOID, "alice 2026-11-16T08:59:00Z"} {
		_, err := store.GitHubNoticeCaptureID(store.NoticeEnvelope{
			Version: 1, Project: "ha", Source: "github", Kind: kind, ObservedAt: ledgerAt, Text: "a notice",
			Receipt: &store.NoticeReceipt{Version: 1, Repo: ledgerRepo, PR: 50, Kind: kind, Key: key},
		})
		if err == nil {
			return true
		}
	}
	return false
}
