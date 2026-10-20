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
	// ErrNoContext means there is no context use to report yet: Claude's
	// statusline carries no percentage, as before Claude has measured one,
	// or a Codex session has no transcript, or no token count with a usable
	// context window in the tail of it.
	ErrNoContext = errors.New("no context percentage to report")
	// ErrMalformed means the input is unusable: a terminal rather than piped
	// JSON, unreadable, over its size cap, not one JSON object, or a
	// percentage that is not a JSON number from 0 to 100; or, for Codex, a
	// transcript path that is not absolute or not a readable regular file.
	ErrMalformed = errors.New("malformed report input")
	// ErrPaneUnresolved means no pane runs the reporting process: its lineage
	// reaches no pane's shell or foreground job. That is the case when the
	// statusline exited first and the report was re-parented to init.
	ErrPaneUnresolved = errors.New("no pane runs the reporting process")
	// ErrAmbiguousPane means more than one pane claims the reporting process.
	// Nothing is reported rather than guessing which.
	ErrAmbiguousPane = errors.New("more than one pane runs the reporting process")
)

// Pane is where a report goes: herdr's socket and the pane the reporting
// process runs in. They come from the pane's shell, not from a plugin
// invocation, which is why they are not part of plugin.Env.
type Pane struct {
	SocketPath string
	// PaneID is HERDR_PANE_ID. herdr sets it when the process spawns and
	// never updates it, so a pane moved to another workspace has a new id
	// and this one is stale.
	PaneID string
	// Lineage is the reporting process and its ancestors (SelfLineage). A
	// report lands only on a pane one of them runs in. Nil means the OS cannot
	// read a lineage: the report then trusts PaneID unchecked.
	Lineage *Lineage
	// CodexThread is CODEX_THREAD_ID, which Codex sets on every tool command
	// it runs: a report that carries it runs under Codex, and must find the
	// pane's own Codex among its ancestors before it reports (A25).
	CodexThread string
	// CodexNetworkSandboxed is CODEX_SANDBOX_NETWORK_DISABLED set: Codex ran
	// the command in a sandbox whose seccomp filter denies every connect,
	// herdr's socket included.
	CodexNetworkSandboxed bool
}

// PaneFrom reads HERDR_SOCKET_PATH, HERDR_PANE_ID, CODEX_THREAD_ID and
// CODEX_SANDBOX_NETWORK_DISABLED through lookup (os.LookupEnv in
// production). Taking the lookup keeps tests from ever seeing a developer's
// live herdr variables.
func PaneFrom(lookup func(string) (string, bool)) Pane {
	sock, _ := lookup("HERDR_SOCKET_PATH")
	pane, _ := lookup("HERDR_PANE_ID")
	thread, _ := lookup("CODEX_THREAD_ID")
	sandboxed, _ := lookup("CODEX_SANDBOX_NETWORK_DISABLED")
	return Pane{SocketPath: sock, PaneID: pane, CodexThread: thread, CodexNetworkSandboxed: sandboxed != ""}
}

// Reporter is the slice of the herdr client a report needs: the report
// itself, and the queries that find the pane it belongs on.
type Reporter interface {
	ReportPaneMetadata(ctx context.Context, m herdr.PaneMetadata) error
	PaneProcessInfo(ctx context.Context, paneID string) (herdr.ProcessInfo, error)
	ListPanes(ctx context.Context) ([]herdr.PaneInfo, error)
}

// Result is what a report pushed, and where.
type Result struct {
	Ctx int
	// PaneID is the pane the token landed on: Pane.PaneID, or the pane's
	// current id when Pane.PaneID is stale or an alias.
	PaneID string
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

// Statusline pushes the ctx token from Claude's statusline JSON in r to the
// pane the reporting process runs in, and returns what it pushed where.
// Outside a pane it returns ErrNotInPane before reading r; on unusable input
// it returns before calling herdr.
//
// One herdr.CallTimeout bounds the whole report, finding the pane included:
// the caller runs it in the background on every statusline refresh, and a
// herdr that never answered must not keep one process per refresh alive.
func Statusline(ctx context.Context, rep Reporter, pane Pane, r io.Reader) (Result, error) {
	if pane.SocketPath == "" || pane.PaneID == "" {
		return Result{}, ErrNotInPane
	}
	pct, err := ParseContext(r)
	if err != nil {
		return Result{}, err
	}
	return pushCtx(ctx, rep, pane, pct)
}

// pushCtx sets the ctx token to pct on the pane the reporting process runs
// in. One herdr.CallTimeout bounds the whole push, finding the pane included.
func pushCtx(ctx context.Context, rep Reporter, pane Pane, pct int) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, herdr.CallTimeout)
	defer cancel()
	target, err := resolvePane(ctx, rep, pane)
	if err != nil {
		return Result{}, err
	}
	err = rep.ReportPaneMetadata(ctx, herdr.PaneMetadata{
		PaneID:    target,
		Source:    Source,
		Tokens:    map[string]string{CtxKey: strconv.Itoa(pct)},
		TTLMillis: uint64(CtxTTL / time.Millisecond),
	})
	if err != nil {
		return Result{}, fmt.Errorf("report %s to %s: %w", CtxKey, target, err)
	}
	return Result{Ctx: pct, PaneID: target}, nil
}

// resolvePane returns the current id of the pane the reporting process runs
// in (NERD-5268). Without a lineage it can only trust pane.PaneID.
//
// It checks pane.PaneID first: a pane that never moved costs that one query.
// An id that answers proves nothing by itself, because after a live handoff
// herdr can reissue a closed workspace's ids to new panes; it counts only if
// one of the pane's processes is in the lineage. Otherwise every pane is
// asked, and the report goes to the one pane that runs the process, or
// nowhere.
func resolvePane(ctx context.Context, rep Reporter, pane Pane) (string, error) {
	if pane.Lineage == nil {
		return pane.PaneID, nil
	}

	info, err := rep.PaneProcessInfo(ctx, pane.PaneID)
	switch {
	case err == nil && pane.Lineage.runsIn(info):
		return info.PaneID, nil
	case err != nil && !errors.Is(err, herdr.ErrPaneNotFound):
		return "", fmt.Errorf("check pane %s: %w", pane.PaneID, err)
	}

	panes, err := rep.ListPanes(ctx)
	if err != nil {
		return "", fmt.Errorf("find the reporting pane: %w", err)
	}
	var found []string
	for _, p := range panes {
		info, err := rep.PaneProcessInfo(ctx, p.PaneID)
		if errors.Is(err, herdr.ErrPaneNotFound) {
			continue // closed since the list
		}
		if err != nil {
			return "", fmt.Errorf("check pane %s: %w", p.PaneID, err)
		}
		if pane.Lineage.runsIn(info) {
			found = append(found, info.PaneID)
		}
	}
	switch len(found) {
	case 0:
		return "", fmt.Errorf("%w: lineage %v, HERDR_PANE_ID %s", ErrPaneUnresolved, pane.Lineage.PIDs(), pane.PaneID)
	case 1:
		return found[0], nil
	default:
		return "", fmt.Errorf("%w: %v", ErrAmbiguousPane, found)
	}
}
