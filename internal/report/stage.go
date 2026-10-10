package report

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/abanna/herdr-agentisan/internal/herdr"
)

// The item and stage token contract (ADR-001 token contract, amended by A5).
// Agentisan is the only writer of both keys (D4, A6): it runs `report stage`
// on every pipeline step transition.
const (
	// ItemKey is the pane token holding the ticket the agent works on, such
	// as NERD-5253.
	ItemKey = "item"
	// StageKey is the pane token holding the agentisan pipeline step the
	// agent is in, such as build_test.
	StageKey = "stage"
	// StageTTL is herdr's longest TTL, 24 h, set again by every report (A5).
	// The tokens also leave with their pane, but a session that exits while
	// its shell keeps the pane would otherwise leave them there for good.
	StageTTL = 24 * time.Hour
	// MaxValueChars is herdr's limit on a token value, in Unicode characters
	// (code points). herdr cuts a longer value short; CheckValue refuses it.
	MaxValueChars = 80
)

// ErrInvalidValue means an item or stage value is one herdr would not store
// exactly as sent. Nothing is reported rather than a value herdr changes.
var ErrInvalidValue = errors.New("invalid token value")

// StageResult is what a stage report pushed, and where.
type StageResult struct {
	Item  string
	Stage string
	// PaneID is the pane the tokens landed on: Pane.PaneID, or the pane's
	// current id when Pane.PaneID is stale or an alias.
	PaneID string
}

// CheckValue reports whether v can be an item or stage value: one herdr
// stores exactly as sent and every client can draw. herdr trims a value,
// drops its control characters, cuts it at MaxValueChars characters and
// takes a blank one as "clear the token", so each of those is refused here
// rather than changed there. A value must be valid UTF-8 (encoding/json would
// replace a bad byte), at most MaxValueChars characters, without leading or
// trailing white space, and printable throughout: letters, marks, numbers,
// punctuation, symbols and the ASCII space. That also refuses the format
// characters herdr would keep (a bidi override, a zero-width space), line
// and paragraph separators, other spaces, and private-use or unassigned code
// points.
//
// The error names the reason, never the value: it goes to the debug log, and
// the value may be long or carry terminal escapes.
func CheckValue(v string) error {
	if v == "" {
		return fmt.Errorf("%w: empty", ErrInvalidValue)
	}
	if !utf8.ValidString(v) {
		return fmt.Errorf("%w: not valid UTF-8", ErrInvalidValue)
	}
	if n := utf8.RuneCountInString(v); n > MaxValueChars {
		return fmt.Errorf("%w: %d characters, more than %d", ErrInvalidValue, n, MaxValueChars)
	}
	if strings.TrimSpace(v) != v {
		return fmt.Errorf("%w: leading or trailing white space", ErrInvalidValue)
	}
	for i, r := range v {
		if !unicode.IsPrint(r) {
			return fmt.Errorf("%w: unprintable character %U at byte %d", ErrInvalidValue, r, i)
		}
	}
	return nil
}

// Stage pushes the item and stage tokens to the pane the reporting process
// runs in, in one report with source agentisan and the StageTTL, and returns
// what it pushed where. Outside a pane it returns ErrNotInPane, and for a
// value CheckValue refuses it returns ErrInvalidValue prefixed with the key;
// neither calls herdr.
//
// The pane is resolved exactly as Statusline resolves it (NERD-5268): a stale
// or reissued HERDR_PANE_ID never receives another pane's tokens. Run by a
// Codex tool command (CODEX_THREAD_ID), it first refuses Codex's network
// sandbox and any lineage that is not the pane's own Codex (A25), before
// calling herdr. One
// herdr.CallTimeout bounds the whole report, the resolution included.
func Stage(ctx context.Context, rep Reporter, pane Pane, item, stage string) (StageResult, error) {
	if pane.SocketPath == "" || pane.PaneID == "" {
		return StageResult{}, ErrNotInPane
	}
	if err := CheckValue(item); err != nil {
		return StageResult{}, fmt.Errorf("%s: %w", ItemKey, err)
	}
	if err := CheckValue(stage); err != nil {
		return StageResult{}, fmt.Errorf("%s: %w", StageKey, err)
	}
	if err := pane.checkCodex(); err != nil {
		return StageResult{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, herdr.CallTimeout)
	defer cancel()
	target, err := resolvePane(ctx, rep, pane)
	if err != nil {
		return StageResult{}, err
	}
	err = rep.ReportPaneMetadata(ctx, herdr.PaneMetadata{
		PaneID:    target,
		Source:    Source,
		Tokens:    map[string]string{ItemKey: item, StageKey: stage},
		TTLMillis: uint64(StageTTL / time.Millisecond),
	})
	if err != nil {
		return StageResult{}, fmt.Errorf("report %s and %s to %s: %w", ItemKey, StageKey, target, err)
	}
	return StageResult{Item: item, Stage: stage, PaneID: target}, nil
}
