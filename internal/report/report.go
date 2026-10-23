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
	"maps"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

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
	// CodexCtxTTL is Codex's ctx TTL, herdr's longest, set again by every
	// report (A27). Codex hooks fire only in turns and compactions, the only
	// times its context changes, so an idle agent's last count stays true. A
	// Codex that exits leaves it until the TTL runs out or the next Codex in
	// the pane reports.
	CodexCtxTTL = 24 * time.Hour
	// CtxAtKey is the pane token holding when the ctx was reported, in Unix
	// milliseconds (FormatModelAt), set in every report that sets ctx, so a
	// reader can tell the current agent's ctx from one an agent before it in
	// the pane left (A61): model_at dates only a report with a model.
	CtxAtKey = "ctx_at"
	// MaxStatuslineBytes bounds the statusline JSON read from stdin. Claude
	// sends a few kilobytes; the cap stops a runaway writer from growing the
	// buffer without limit.
	MaxStatuslineBytes = 1 << 20
)

// The model token contract (ADR-001 A56). The agent reports the model it runs
// now in the same report as its ctx, with ctx's TTL: Claude's statusline its
// model.id, a Codex hook its model, so a /model switch shows without a
// restart. The daemon prefers it to the launch model (A39).
const (
	// ModelKey is the pane token holding the model the pane's agent reported
	// it runs, such as claude-sonnet-5-5 or gpt-5.5.
	ModelKey = "model"
	// ModelAtKey is the pane token holding when the model was reported, in
	// Unix milliseconds (FormatModelAt), so a reader can tell the current
	// agent's report from one an agent before it in the pane left: herdr
	// gives no token a time. It is set in every report that sets the model.
	ModelAtKey = "model_at"
)

// Sentinel errors. Callers branch on these with errors.Is.
var (
	// ErrNotInPane means HERDR_SOCKET_PATH or HERDR_PANE_ID is unset or
	// empty: the process is not running in a herdr pane.
	ErrNotInPane = errors.New("not running in a herdr pane")
	// ErrNoContext means there is no context use to report yet: Claude's
	// statusline carries no percentage, as before Claude has measured one,
	// or a Codex session has no transcript, or no token count with a usable
	// context window in the tail of it. Statusline and Codex return it only
	// when there is no model to report either.
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
	// Now is the clock a stage report's stage_at reads; nil is time.Now.
	Now func() time.Time
}

// now is the time p's clock reads.
func (p Pane) now() time.Time {
	if p.Now == nil {
		return time.Now()
	}
	return p.Now()
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
	PaneFinder
}

// PaneFinder is the slice of the herdr client ResolvePane asks.
type PaneFinder interface {
	PaneProcessInfo(ctx context.Context, paneID string) (herdr.ProcessInfo, error)
	ListPanes(ctx context.Context) ([]herdr.PaneInfo, error)
}

// Reading is what an agent's statusline or hook gives a report: its context
// use, when it has measured one, and the model it runs, when it names one.
type Reading struct {
	// Ctx is the context use from 0 to 100; HasCtx says there is one.
	Ctx    int
	HasCtx bool
	// Model is the model, already ValidModel's; "" when there is none.
	Model string
}

// Result is what a report pushed, and where.
type Result struct {
	Ctx    int
	HasCtx bool
	Model  string
	// PaneID is the pane the tokens landed on: Pane.PaneID, or the pane's
	// current id when Pane.PaneID is stale or an alias.
	PaneID string
}

// ValidModel is m when it may be a row's model, else "" (ADR-001 A39, A56):
// it is printable UTF-8 of at most MaxValueChars code points, a herdr token
// value's limit, and does not start with "-", as a flag would. U+FFFD, which
// invalid bytes decode to, is not printable here.
func ValidModel(m string) string {
	if m == "" || strings.HasPrefix(m, "-") || !utf8.ValidString(m) || utf8.RuneCountInString(m) > MaxValueChars {
		return ""
	}
	for _, r := range m {
		if !unicode.IsPrint(r) || r == utf8.RuneError {
			return ""
		}
	}
	return m
}

// modelOf is the model raw names, a JSON string ValidModel keeps, else "".
// Anything else, a number or an object, is no model rather than malformed
// input: the agent owns the field's shape, and a change to it must not
// silence ctx.
func modelOf(raw json.RawMessage) string {
	var m string
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	return ValidModel(m)
}

// FormatModelAt is t as a model_at or ctx_at value: whole Unix milliseconds, in
// decimal. Milliseconds, not stage_at's seconds, so a report made within a
// second of its agent's start is never read as older than the agent.
func FormatModelAt(t time.Time) string { return strconv.FormatInt(t.UnixMilli(), 10) }

// ParseModelAt reads a model_at value as FormatModelAt writes it: a positive
// count of Unix milliseconds in canonical decimal, with no sign, space or
// leading zero. Anything else is no model_at, and a model without one is
// never taken as current.
func ParseModelAt(v string) (time.Time, bool) {
	return parseMillis(v)
}

// ParseCtxAt reads a ctx_at value, which FormatModelAt writes too, by
// ParseModelAt's rules: anything else is no ctx_at (A61).
func ParseCtxAt(v string) (time.Time, bool) {
	return parseMillis(v)
}

// parseMillis reads a positive count of Unix milliseconds in canonical
// decimal.
func parseMillis(v string) (time.Time, bool) {
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 || strconv.FormatInt(n, 10) != v {
		return time.Time{}, false
	}
	return time.UnixMilli(n), true
}

// ParseContext reads Claude's statusline JSON from r and returns
// .context_window.used_percentage rounded to the nearest integer, halves
// away from zero. The range is checked before rounding, so 100.4 is
// malformed rather than 100. A statusline with a model but no percentage is
// ErrNoContext here; ParseStatusline reads both.
func ParseContext(r io.Reader) (int, error) {
	rd, err := ParseStatusline(r)
	if err != nil {
		return 0, err
	}
	if !rd.HasCtx {
		return 0, ErrNoContext
	}
	return rd.Ctx, nil
}

// ParseStatusline reads Claude's statusline JSON from r: the context use as
// ParseContext rounds it, and .model.id, when it is a string ValidModel
// keeps (A56). It returns ErrNoContext when there is neither, and
// ErrMalformed for unusable input, a percentage that is not a JSON number
// from 0 to 100 included, whatever model it names.
func ParseStatusline(r io.Reader) (Reading, error) {
	// Run by hand in a pane, stdin is the terminal: reading it would wait
	// for Ctrl-D with nothing on screen to say so.
	if isCharDevice(r) {
		return Reading{}, fmt.Errorf("%w: stdin is a terminal or other character device, not piped statusline JSON", ErrMalformed)
	}
	raw, err := io.ReadAll(io.LimitReader(r, MaxStatuslineBytes+1))
	if err != nil {
		return Reading{}, fmt.Errorf("%w: read: %w", ErrMalformed, err)
	}
	if len(raw) > MaxStatuslineBytes {
		return Reading{}, fmt.Errorf("%w: input exceeds %d bytes", ErrMalformed, MaxStatuslineBytes)
	}
	// Unknown fields are ignored on purpose: Claude adds statusline fields
	// between versions, and rejecting them would silence ctx on an upgrade.
	// The model is read loosely for the same reason (modelOf).
	var in struct {
		Model         json.RawMessage `json:"model"`
		ContextWindow *struct {
			UsedPercentage *float64 `json:"used_percentage"`
		} `json:"context_window"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return Reading{}, fmt.Errorf("%w: %w", ErrMalformed, err)
	}
	var rd Reading
	var model struct {
		ID json.RawMessage `json:"id"`
	}
	if json.Unmarshal(in.Model, &model) == nil {
		rd.Model = modelOf(model.ID)
	}
	if in.ContextWindow != nil && in.ContextWindow.UsedPercentage != nil {
		pct := *in.ContextWindow.UsedPercentage
		if pct < 0 || pct > 100 {
			return Reading{}, fmt.Errorf("%w: used_percentage %v is outside 0..100", ErrMalformed, pct)
		}
		rd.Ctx, rd.HasCtx = int(math.Round(pct)), true
	}
	if !rd.HasCtx && rd.Model == "" {
		return Reading{}, ErrNoContext
	}
	return rd, nil
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

// Statusline pushes the ctx token, and the model token with its model_at,
// from Claude's statusline JSON in r to the pane the reporting process runs
// in, and returns what it pushed where. A statusline with only one of them
// pushes that one. Outside a pane it returns ErrNotInPane before reading r;
// on unusable input it returns before calling herdr.
//
// One herdr.CallTimeout bounds the whole report, finding the pane included:
// the caller runs it in the background on every statusline refresh, and a
// herdr that never answered must not keep one process per refresh alive.
func Statusline(ctx context.Context, rep Reporter, pane Pane, r io.Reader) (Result, error) {
	if pane.SocketPath == "" || pane.PaneID == "" {
		return Result{}, ErrNotInPane
	}
	rd, err := ParseStatusline(r)
	if err != nil {
		return Result{}, err
	}
	return push(ctx, rep, pane, rd, CtxTTL)
}

// push sets rd's tokens in one report, each expiring after ttl, on the pane
// the reporting process runs in: ctx with ctx_at when rd has one, and the
// model with model_at when rd has one, each stamped from pane's clock. One
// herdr.CallTimeout bounds the whole push, finding the pane included.
func push(ctx context.Context, rep Reporter, pane Pane, rd Reading, ttl time.Duration) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, herdr.CallTimeout)
	defer cancel()
	target, err := ResolvePane(ctx, rep, pane)
	if err != nil {
		return Result{}, err
	}
	tokens := map[string]string{}
	if rd.HasCtx {
		tokens[CtxKey] = strconv.Itoa(rd.Ctx)
		tokens[CtxAtKey] = FormatModelAt(pane.now())
	}
	if rd.Model != "" {
		tokens[ModelKey] = rd.Model
		tokens[ModelAtKey] = FormatModelAt(pane.now())
	}
	m := herdr.PaneMetadata{PaneID: target, Source: Source, Tokens: tokens}
	// ttl is CtxTTL or CodexCtxTTL; the guard keeps the conversion from
	// wrapping, and a zero ttl would be omitted, never expiring.
	if ttl > 0 {
		m.TTLMillis = uint64(ttl / time.Millisecond)
	}
	err = rep.ReportPaneMetadata(ctx, m)
	if err != nil {
		return Result{}, fmt.Errorf("report %s to %s: %w", strings.Join(slices.Sorted(maps.Keys(tokens)), ", "), target, err)
	}
	return Result{Ctx: rd.Ctx, HasCtx: rd.HasCtx, Model: rd.Model, PaneID: target}, nil
}

// ResolvePane returns the current id of the pane the reporting process runs
// in (NERD-5268). Without a lineage it can only trust pane.PaneID.
//
// It checks pane.PaneID first: a pane that never moved costs that one query.
// An id that answers proves nothing by itself, because after a live handoff
// herdr can reissue a closed workspace's ids to new panes; it counts only if
// one of the pane's processes is in the lineage. Otherwise every pane is
// asked, and the report goes to the one pane that runs the process, or
// nowhere.
func ResolvePane(ctx context.Context, rep PaneFinder, pane Pane) (string, error) {
	if pane.Lineage == nil {
		return pane.PaneID, nil
	}

	info, err := rep.PaneProcessInfo(ctx, pane.PaneID)
	switch {
	case err == nil && pane.Lineage.RunsIn(info):
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
		if pane.Lineage.RunsIn(info) {
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
