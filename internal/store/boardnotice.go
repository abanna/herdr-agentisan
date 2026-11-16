package store

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// NoticeReceipt is a versioned GitHub event identity, including an empty key.
type NoticeReceipt struct {
	Version uint64
	Repo    string
	PR      int64
	Kind    string
	Key     string
}

// NoticeEnvelope holds frozen capture inputs; deriving an ID changes none of them.
type NoticeEnvelope struct {
	Version    uint64
	CaptureID  string
	Project    string
	Source     string
	Kind       string
	Subject    string
	Worker     string
	ObservedAt time.Time
	Text       string
	Receipt    *NoticeReceipt
}

func noticeInvalid() error { return fmt.Errorf("%w: notice argument", ErrInvalid) }

func checkNoticeReceipt(r NoticeReceipt) bool {
	if r.Version != 1 || r.PR < 1 || r.PR > 1<<31-1 || len(r.Repo) > 256 || len(r.Key) > 256 || checkLine("repo", r.Repo, false) != nil || strings.Count(r.Repo, "/") != 1 {
		return false
	}
	owner, name, _ := strings.Cut(r.Repo, "/")
	if owner == "" || name == "" {
		return false
	}
	switch r.Kind {
	case "approved":
		return r.Key == ""
	case "review":
		login, stamp, ok := strings.Cut(r.Key, " ")
		at, err := time.Parse(time.RFC3339Nano, stamp)
		return ok && login != "" && len(login) <= 128 && utf8.ValidString(login) && !strings.ContainsFunc(login, func(r rune) bool { return invisible(r) || unicode.IsSpace(r) }) && err == nil && at.UTC().Format(time.RFC3339Nano) == stamp
	case "merged":
		if r.Key == "" {
			return true
		}
	case "ci_failed", "ci_passed", "dirty":
	default:
		return false
	}
	if len(r.Key) != 40 && len(r.Key) != 64 {
		return false
	}
	_, err := hex.DecodeString(r.Key)
	return err == nil
}

// githubNoticeEncoding validates before allocating the bounded canonical bytes.
func githubNoticeEncoding(e NoticeEnvelope) ([]byte, error) {
	if e.Version != 1 || e.Source != "github" || e.Receipt == nil || e.Receipt.Kind != e.Kind || !checkNoticeReceipt(*e.Receipt) {
		return nil, noticeInvalid()
	}
	for _, field := range []string{e.Project, e.Subject, e.Worker} {
		if len(field) > 256 || checkLine("notice", field, true) != nil {
			return nil, noticeInvalid()
		}
	}
	if e.Text == "" || len(e.Text) > 512 || !utf8.ValidString(e.Text) || strings.ContainsAny(e.Text, "\x00\r\n") {
		return nil, noticeInvalid()
	}
	// UTC also bounds nanos' invalid-time diagnostic independently of zone names.
	ns, err := nanos(e.ObservedAt.UTC())
	if err != nil {
		return nil, noticeInvalid()
	}
	out := make([]byte, 0, 2048)
	for _, word := range []uint64{e.Version, e.Receipt.Version, uint64(e.Receipt.PR), uint64(ns)} {
		out = binary.BigEndian.AppendUint64(out, word)
	} // #nosec G115 -- PR is bounded above; signed nanoseconds use their two's-complement bits
	for _, field := range []string{e.Project, e.Source, e.Kind, e.Subject, e.Worker, e.Receipt.Repo, e.Receipt.Key, e.Text} {
		out = binary.BigEndian.AppendUint32(out, uint32(len(field))) // #nosec G115 -- validated fields are at most 512 bytes
		out = append(out, field...)
	}
	if len(out) > 2048 {
		return nil, noticeInvalid()
	}
	return out, nil
}

// GitHubNoticeCaptureID validates a complete GitHub envelope except CaptureID,
// which is ignored. It returns gh1: followed by lowercase SHA256 hex of version-1
// canonical bytes. It reads no state, allocates no identity and preserves inputs.
func GitHubNoticeCaptureID(e NoticeEnvelope) (string, error) {
	encoded, err := githubNoticeEncoding(e)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return "gh1:" + hex.EncodeToString(digest[:]), nil
}
