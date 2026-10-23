package store_test

import (
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/abanna/herdr-agentisan/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func githubEnvelope() store.NoticeEnvelope {
	return store.NoticeEnvelope{
		Version: 1, Source: "github", Kind: "approved", ObservedAt: time.Unix(0, 1), Text: "x",
		Receipt: &store.NoticeReceipt{Version: 1, Repo: "a/b", PR: 1, Kind: "approved"},
	}
}

func TestGitHubNoticeCaptureIDTypes(t *testing.T) {
	for _, tc := range []struct {
		value  any
		fields []string
	}{
		{store.NoticeReceipt{}, []string{"Version:uint64", "Repo:string", "PR:int64", "Kind:string", "Key:string"}},
		{store.NoticeEnvelope{}, []string{"Version:uint64", "CaptureID:string", "Project:string", "Source:string", "Kind:string", "Subject:string", "Worker:string", "ObservedAt:time.Time", "Text:string", "Receipt:*store.NoticeReceipt"}},
	} {
		typ := reflect.TypeOf(tc.value)
		require.Equal(t, len(tc.fields), typ.NumField())
		for i, want := range tc.fields {
			field := typ.Field(i)
			assert.Equal(t, want, field.Name+":"+field.Type.String())
		}
	}
}

func TestGitHubNoticeCaptureIDCanonicalVectors(t *testing.T) {
	// Frozen digests from an independent Python struct.pack/hashlib oracle.
	// Fields are project, subject, worker, repo, key, text, all deliberately distinct.
	for _, tc := range []struct {
		kind   string
		pr     int64
		ns     int64
		fields [6]string
		digest string
	}{
		{"approved", 1, 1, [6]string{"", "", "", "a/b", "", "x"}, "d844ad3a3d9a92025e90f6802d95944911121a8d617f822cc2f0281bb3dc106a"},
		{"merged", math.MaxInt32, -1, [6]string{"p", "s", "w", "o/r", strings.Repeat("Ab", 20), "body"}, "dd1c746003d2ff91a5e8fcc3610f3177bf88b81ebb827ff09ae089625f5eff04"},
		{"ci_failed", 2, 0, [6]string{"left", "right", "worker", "owner/repo", strings.Repeat("a", 64), "line\ttext"}, "4286da562a6f4e7bf4b26d8b8a8b1a08b15080f27f3152968890db9a8f147931"},
		{"ci_passed", 3, math.MaxInt64, [6]string{"prj", "subject", "wrkr", "own/name", strings.Repeat("B", 40), "ok"}, "d6f52f0c5653f800fb7676b4b6e7278e159c314f6b19a1690fdb2e761cc538cb"},
		{"dirty", 4, math.MinInt64, [6]string{"é", "主体", "worker", "a/é", strings.Repeat("c", 40), "emoji 👩\u200d💻"}, "6a3df3021baed3d876c89b7c7845b450982796ab0bb4b25554abf20a7883c399"},
		{"review", 5, 1000000001, [6]string{"p", "s", "w", "o/r", "alice 2026-10-23T00:00:00.123456789Z", "review"}, "f55455cd01aed2f1bb317507ac11b9b498bc4392f2ff58e73fb2e336136720d6"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			e := githubEnvelope()
			e.Kind, e.Receipt.Kind, e.Receipt.PR, e.ObservedAt = tc.kind, tc.kind, tc.pr, time.Unix(0, tc.ns)
			e.Project, e.Subject, e.Worker = tc.fields[0], tc.fields[1], tc.fields[2]
			e.Receipt.Repo, e.Receipt.Key, e.Text = tc.fields[3], tc.fields[4], tc.fields[5]
			before, receipt := e, *e.Receipt
			for range 2 {
				id, err := store.GitHubNoticeCaptureID(e)
				require.NoError(t, err)
				assert.Equal(t, "gh1:"+tc.digest, id)
			}
			assert.Equal(t, before, e)
			assert.Equal(t, receipt, *e.Receipt)
			e.ObservedAt = e.ObservedAt.In(time.FixedZone("caller zone", 3600))
			e.CaptureID = "\xff" + strings.Repeat("ignored", 1<<16)
			id, err := store.GitHubNoticeCaptureID(e)
			require.NoError(t, err)
			assert.Equal(t, "gh1:"+tc.digest, id)
		})
	}
	for _, pair := range [][2]string{{"é", "e\u0301"}, {"ab", "a"}} {
		a, b := githubEnvelope(), githubEnvelope()
		a.Project, b.Project = pair[0], pair[1]
		if pair[0] == "ab" {
			a.Subject, b.Subject = "c", "bc"
		}
		x, err := store.GitHubNoticeCaptureID(a)
		require.NoError(t, err)
		y, err := store.GitHubNoticeCaptureID(b)
		require.NoError(t, err)
		assert.NotEqual(t, x, y, "no Unicode normalization or ambiguous concatenation")
	}
	base := githubEnvelope()
	id, err := store.GitHubNoticeCaptureID(base)
	require.NoError(t, err)
	for _, change := range []func(*store.NoticeEnvelope){
		func(e *store.NoticeEnvelope) { e.Project = "p" },
		func(e *store.NoticeEnvelope) { e.Subject = "s" },
		func(e *store.NoticeEnvelope) { e.Worker = "w" },
		func(e *store.NoticeEnvelope) { e.Text = "xx" },
		func(e *store.NoticeEnvelope) { e.Receipt.Repo = "a/c" },
		func(e *store.NoticeEnvelope) { e.Receipt.PR = 2 },
		func(e *store.NoticeEnvelope) { e.ObservedAt = e.ObservedAt.Add(time.Nanosecond) },
	} {
		e, receipt := base, *base.Receipt
		e.Receipt = &receipt
		change(&e)
		other, err := store.GitHubNoticeCaptureID(e)
		require.NoError(t, err)
		assert.NotEqual(t, id, other)
	}
}

func TestGitHubNoticeCaptureIDEnvelopeValidation(t *testing.T) {
	bad := map[string]func(*store.NoticeEnvelope){
		"version zero":      func(e *store.NoticeEnvelope) { e.Version = 0 },
		"version two":       func(e *store.NoticeEnvelope) { e.Version = 2 },
		"version future":    func(e *store.NoticeEnvelope) { e.Version = math.MaxUint64 },
		"source empty":      func(e *store.NoticeEnvelope) { e.Source = "" },
		"watch":             func(e *store.NoticeEnvelope) { e.Source, e.Kind = "watch", "blocked" },
		"ctx":               func(e *store.NoticeEnvelope) { e.Source, e.Kind = "ctx", "ctx_soft" },
		"source upper":      func(e *store.NoticeEnvelope) { e.Source = "GitHub" },
		"source future":     func(e *store.NoticeEnvelope) { e.Source = "future" },
		"kind empty":        func(e *store.NoticeEnvelope) { e.Kind, e.Receipt.Kind = "", "" },
		"kind future":       func(e *store.NoticeEnvelope) { e.Kind, e.Receipt.Kind = "future", "future" },
		"kind delimiter":    func(e *store.NoticeEnvelope) { e.Kind, e.Receipt.Kind = "approved|merged", "approved|merged" },
		"kind upper":        func(e *store.NoticeEnvelope) { e.Kind, e.Receipt.Kind = "APPROVED", "APPROVED" },
		"text empty":        func(e *store.NoticeEnvelope) { e.Text = "" },
		"text long":         func(e *store.NoticeEnvelope) { e.Text = strings.Repeat("é", 256) + "x" },
		"text invalid UTF8": func(e *store.NoticeEnvelope) { e.Text = "\xff" },
		"text NUL":          func(e *store.NoticeEnvelope) { e.Text = "a\x00b" },
		"text CR":           func(e *store.NoticeEnvelope) { e.Text = "a\rb" },
		"text LF":           func(e *store.NoticeEnvelope) { e.Text = "a\nb" },
		"time zero":         func(e *store.NoticeEnvelope) { e.ObservedAt = time.Time{} },
		"time underflow":    func(e *store.NoticeEnvelope) { e.ObservedAt = time.Unix(0, math.MinInt64).Add(-time.Nanosecond) },
		"time overflow":     func(e *store.NoticeEnvelope) { e.ObservedAt = time.Unix(0, math.MaxInt64).Add(time.Nanosecond) },
	}
	for field := range 3 {
		for _, value := range []string{strings.Repeat("é", 128) + "x", "\xff", "\xc3", "\xed\xa0\x80", "é\xff", "a\nb", "a\r\nb", "a\rb", "a\x00b", "\u200d", "\ufeff", "\u0085"} {
			t.Run("line field", func(t *testing.T) {
				e := githubEnvelope()
				switch field {
				case 0:
					e.Project = value
				case 1:
					e.Subject = value
				case 2:
					e.Worker = value
				}
				id, err := store.GitHubNoticeCaptureID(e)
				require.ErrorIs(t, err, store.ErrInvalid)
				assert.Empty(t, id)
			})
		}
	}
	for name, change := range bad {
		t.Run(name, func(t *testing.T) {
			e := githubEnvelope()
			change(&e)
			id, err := store.GitHubNoticeCaptureID(e)
			require.ErrorIs(t, err, store.ErrInvalid)
			assert.NotEqual(t, store.ErrInvalid, err, "sentinel must be wrapped")
			assert.Empty(t, id)
		})
	}
}

func TestGitHubNoticeCaptureIDReceiptGrammar(t *testing.T) {
	for name, change := range map[string]func(*store.NoticeReceipt){
		"version zero":       func(r *store.NoticeReceipt) { r.Version = 0 },
		"version two":        func(r *store.NoticeReceipt) { r.Version = 2 },
		"version future":     func(r *store.NoticeReceipt) { r.Version = math.MaxUint64 },
		"repo missing slash": func(r *store.NoticeReceipt) { r.Repo = "ab" },
		"repo extra slash":   func(r *store.NoticeReceipt) { r.Repo = "a/b/c" },
		"repo empty owner":   func(r *store.NoticeReceipt) { r.Repo = "/b" },
		"repo empty name":    func(r *store.NoticeReceipt) { r.Repo = "a/" },
		"repo empty":         func(r *store.NoticeReceipt) { r.Repo = "" },
		"repo long":          func(r *store.NoticeReceipt) { r.Repo = "a/" + strings.Repeat("x", 255) },
		"repo UTF8":          func(r *store.NoticeReceipt) { r.Repo = "a/\xff" },
		"repo control":       func(r *store.NoticeReceipt) { r.Repo = "a/\t" },
		"repo format":        func(r *store.NoticeReceipt) { r.Repo = "a/\u200d" },
		"PR negative":        func(r *store.NoticeReceipt) { r.PR = -1 },
		"PR zero":            func(r *store.NoticeReceipt) { r.PR = 0 },
		"PR high":            func(r *store.NoticeReceipt) { r.PR = math.MaxInt32 + 1 },
		"mismatch":           func(r *store.NoticeReceipt) { r.Kind = "merged" },
		"approved key":       func(r *store.NoticeReceipt) { r.Key = "unexpected" },
	} {
		t.Run(name, func(t *testing.T) {
			e := githubEnvelope()
			change(e.Receipt)
			id, err := store.GitHubNoticeCaptureID(e)
			require.ErrorIs(t, err, store.ErrInvalid)
			assert.Empty(t, id)
		})
	}
	for _, kind := range []string{"merged", "ci_failed", "ci_passed", "dirty"} {
		for _, key := range []string{strings.Repeat("ab", 20), strings.Repeat("AB", 32)} {
			e := githubEnvelope()
			e.Kind, e.Receipt.Kind, e.Receipt.Key = kind, kind, key
			id, err := store.GitHubNoticeCaptureID(e)
			require.NoError(t, err)
			assert.Len(t, id, 68)
		}
	}
	e := githubEnvelope()
	e.Receipt = nil
	_, err := store.GitHubNoticeCaptureID(e)
	require.ErrorIs(t, err, store.ErrInvalid)
	for _, kind := range []string{"merged", "ci_failed", "ci_passed", "dirty", "review"} {
		keys := []string{"", strings.Repeat("a", 39), strings.Repeat("g", 40), strings.Repeat("a", 41), strings.Repeat("a", 63), strings.Repeat("a", 65), strings.Repeat("a", 257)}
		if kind == "review" {
			keys = []string{"", "alice", " 2026-10-23T00:00:00Z", "a b 2026-10-23T00:00:00Z", "a\tb 2026-10-23T00:00:00Z", "a\u00a0b 2026-10-23T00:00:00Z", "a\u200db 2026-10-23T00:00:00Z", "\xff 2026-10-23T00:00:00Z", strings.Repeat("a", 129) + " 2026-10-23T00:00:00Z", "a invalid", "a 2026-10-23T00:00:00+00:00", "a 2026-10-23T00:00:00.100Z", "a 2026-10-23T00:00:00Z extra"}
		}
		for _, key := range keys {
			e := githubEnvelope()
			e.Kind, e.Receipt.Kind, e.Receipt.Key = kind, kind, key
			id, err := store.GitHubNoticeCaptureID(e)
			if kind == "merged" && key == "" {
				require.NoError(t, err)
				assert.NotEmpty(t, id)
			} else {
				require.ErrorIs(t, err, store.ErrInvalid)
				assert.Empty(t, id)
			}
		}
	}
	// Conservative valid maximum: 32 numeric + 32 prefixes + five 256-byte
	// fields + 512 text + 6 source + 9 kind = 1871 < 2048; equality is unreachable.
	e = githubEnvelope()
	e.Kind, e.Receipt.Kind = "review", "review"
	e.Project, e.Subject, e.Worker = strings.Repeat("é", 128), strings.Repeat("x", 256), strings.Repeat("x", 256)
	e.Receipt.Repo = "a/" + strings.Repeat("é", 127)
	e.Receipt.Key, e.Text = strings.Repeat("a", 128)+" 2026-10-23T00:00:00.123456789Z", strings.Repeat("é", 256)
	id, err := store.GitHubNoticeCaptureID(e)
	require.NoError(t, err)
	assert.Len(t, id, 68)
}

func TestGitHubNoticeCaptureIDEncodingClasses(t *testing.T) {
	for _, field := range []string{"project", "subject", "worker", "source", "kind", "receipt_kind", "repo", "key", "text"} {
		for _, tc := range []struct {
			name  string
			value string
		}{
			{"malformed", "\xff"},
			{"truncated", "\xe2\x82"},
			{"surrogate bytes", "\xed\xa0\x80"},
			{"Latin1", "caf\xe9"},
			{"UTF16", "a\x00b\x00"},
			{"mixed", "é\xff"},
			{"NUL", "a\x00b"},
			{"CRLF", "a\r\nb"},
			{"CR", "a\rb"},
			{"BOM", "\ufeff"},
		} {
			t.Run(field+"/"+tc.name, func(t *testing.T) {
				e := githubEnvelope()
				switch field {
				case "project":
					e.Project = tc.value
				case "subject":
					e.Subject = tc.value
				case "worker":
					e.Worker = tc.value
				case "source":
					e.Source = tc.value
				case "kind":
					e.Kind, e.Receipt.Kind = tc.value, tc.value
				case "receipt_kind":
					e.Receipt.Kind = tc.value
				case "repo":
					e.Receipt.Repo = "a/" + tc.value
				case "key":
					e.Kind, e.Receipt.Kind = "review", "review"
					e.Receipt.Key = tc.value + " 2026-10-23T00:00:00Z"
				case "text":
					e.Text = tc.value
				}
				before, receipt := e, *e.Receipt
				id, err := store.GitHubNoticeCaptureID(e)
				if field == "text" && tc.name == "BOM" {
					require.NoError(t, err, "text permits format characters")
					assert.Len(t, id, 68)
				} else {
					require.ErrorIs(t, err, store.ErrInvalid)
					assert.Empty(t, id)
				}
				assert.Equal(t, before, e)
				assert.Equal(t, receipt, *e.Receipt)
			})
		}
	}
}

func TestGitHubNoticeCaptureIDBoundedInvalidInput(t *testing.T) {
	large := strings.Repeat("body-must-stay-private", 1<<16)
	for _, field := range []string{"source", "kind", "receipt_kind", "project", "subject", "worker", "text", "repo", "key", "zone", "capture_id"} {
		e := githubEnvelope()
		switch field {
		case "source":
			e.Source = large
		case "kind":
			e.Kind = large
		case "receipt_kind":
			e.Receipt.Kind = large
		case "project":
			e.Project = large
		case "subject":
			e.Subject = large
		case "worker":
			e.Worker = large
		case "text":
			e.Text = large
		case "repo":
			e.Receipt.Repo = large
		case "key":
			e.Receipt.Key = large
		case "capture_id":
			e.CaptureID = large
		case "zone":
			e.ObservedAt = time.Time{}.In(time.FixedZone(large, 0))
		}
		result := testing.Benchmark(func(b *testing.B) {
			for b.Loop() {
				_, _ = store.GitHubNoticeCaptureID(e)
			}
		})
		assert.Less(t, result.AllocedBytesPerOp(), int64(4096), field)
		id, err := store.GitHubNoticeCaptureID(e)
		if field == "capture_id" {
			require.NoError(t, err)
			assert.Equal(t, "gh1:d844ad3a3d9a92025e90f6802d95944911121a8d617f822cc2f0281bb3dc106a", id)
			continue
		}
		require.ErrorIs(t, err, store.ErrInvalid)
		assert.Empty(t, id)
		assert.Less(t, len(err.Error()), 100)
		assert.NotContains(t, err.Error(), "body-must-stay-private")
		assert.NotContains(t, err.Error(), e.Receipt.Repo)
	}
}
