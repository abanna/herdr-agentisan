// Package report pushes an agent's own state to herdr as pane tokens
// (ADR-001 D4): the component that knows a value is the one that reports it,
// and nothing scrapes a screen for it.
//
// It owns the token contract for the keys it writes — key, format, source and
// TTL — so a cobra command today and a hook tomorrow write them identically.
package report

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"time"

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/plugin"
)

// The ctx token contract (ADR-001, token contract).
const (
	// Source is the metadata source every agentisan report uses.
	Source = "agentisan"
	// CtxKey is the pane token holding the agent's context use. Its value is
	// a bare integer from 0 to 100: herdr's gt/lt colour rules only match a
	// value that parses completely as a number, so never "42%".
	CtxKey = "ctx"
	// CtxTTL is three times Claude's 60 s statusline refresh: one missed
	// refresh keeps the token, a writer that stopped lets it expire.
	CtxTTL = 180 * time.Second
	// MaxStatuslineBytes bounds the statusline JSON read from stdin. Claude
	// sends a few kilobytes; the cap stops a runaway writer from growing the
	// buffer without limit.
	MaxStatuslineBytes = 1 << 20
)

// Sentinel errors. Callers branch on these with errors.Is.
var (
	// ErrNotInPane means HERDR_SOCKET_PATH or HERDR_PANE_ID is unset or
	// empty: the process is not running in a herdr pane.
	ErrNotInPane = errors.New("not running in a herdr pane")
	// ErrNoContext means the statusline carries no context percentage, as
	// before Claude has measured one.
	ErrNoContext = errors.New("statusline has no context percentage")
	// ErrMalformed means the statusline input is unusable: a terminal rather
	// than piped JSON, unreadable, over MaxStatuslineBytes, not one JSON
	// object, or a percentage that is not a JSON number from 0 to 100.
	ErrMalformed = errors.New("malformed statusline input")
)

// Pane is where a report goes: herdr's socket and the pane the reporting
// process runs in. Both come from the pane's shell, not from a plugin
// invocation, which is why they are not part of plugin.Env.
type Pane struct {
	SocketPath string
	PaneID     string
}

// PaneFrom reads HERDR_SOCKET_PATH and HERDR_PANE_ID through lookup
// (os.LookupEnv in production). Taking the lookup keeps tests from ever
// seeing a developer's live herdr variables.
func PaneFrom(lookup func(string) (string, bool)) Pane {
	sock, _ := lookup("HERDR_SOCKET_PATH")
	pane, _ := lookup("HERDR_PANE_ID")
	return Pane{SocketPath: sock, PaneID: pane}
}

// Reporter is the slice of the herdr client a report needs.
type Reporter interface {
	ReportPaneMetadata(ctx context.Context, m herdr.PaneMetadata) error
}

// ParseContext reads Claude's statusline JSON from r and returns
// .context_window.used_percentage rounded to the nearest integer, halves
// away from zero. The range is checked before rounding, so 100.4 is
// malformed rather than 100.
func ParseContext(r io.Reader) (int, error) {
	// Run by hand in a pane, stdin is the terminal: reading it would wait
	// for Ctrl-D with nothing on screen to say so.
	if isCharDevice(r) {
		return 0, fmt.Errorf("%w: stdin is a terminal or other character device, not piped statusline JSON", ErrMalformed)
	}
	raw, err := io.ReadAll(io.LimitReader(r, MaxStatuslineBytes+1))
	if err != nil {
		return 0, fmt.Errorf("%w: read: %w", ErrMalformed, err)
	}
	if len(raw) > MaxStatuslineBytes {
		return 0, fmt.Errorf("%w: input exceeds %d bytes", ErrMalformed, MaxStatuslineBytes)
	}
	// Unknown fields are ignored on purpose: Claude adds statusline fields
	// between versions, and rejecting them would silence ctx on an upgrade.
	var in struct {
		ContextWindow *struct {
			UsedPercentage *float64 `json:"used_percentage"`
		} `json:"context_window"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return 0, fmt.Errorf("%w: %w", ErrMalformed, err)
	}
	if in.ContextWindow == nil || in.ContextWindow.UsedPercentage == nil {
		return 0, ErrNoContext
	}
	pct := *in.ContextWindow.UsedPercentage
	if pct < 0 || pct > 100 {
		return 0, fmt.Errorf("%w: used_percentage %v is outside 0..100", ErrMalformed, pct)
	}
	return int(math.Round(pct)), nil
}

// isCharDevice reports whether r is a file that is a character device, such
// as a terminal or /dev/null. Either way there is no statusline to read.
func isCharDevice(r io.Reader) bool {
	f, ok := r.(interface{ Stat() (os.FileInfo, error) })
	if !ok {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// Statusline pushes the ctx token for pane from Claude's statusline JSON in
// r, and returns the value it pushed. Outside a pane it returns ErrNotInPane
// before reading r; on unusable input it returns before calling herdr. The
// call is bounded by plugin.CallTimeout: the caller runs it in the background
// on every statusline refresh, and a herdr that never answered must not keep
// one process per refresh alive.
func Statusline(ctx context.Context, rep Reporter, pane Pane, r io.Reader) (int, error) {
	if pane.SocketPath == "" || pane.PaneID == "" {
		return 0, ErrNotInPane
	}
	pct, err := ParseContext(r)
	if err != nil {
		return 0, err
	}

	ctx, cancel := context.WithTimeout(ctx, plugin.CallTimeout)
	defer cancel()
	err = rep.ReportPaneMetadata(ctx, herdr.PaneMetadata{
		PaneID:    pane.PaneID,
		Source:    Source,
		Tokens:    map[string]string{CtxKey: strconv.Itoa(pct)},
		TTLMillis: uint64(CtxTTL / time.Millisecond),
	})
	if err != nil {
		return 0, fmt.Errorf("report %s: %w", CtxKey, err)
	}
	return pct, nil
}
