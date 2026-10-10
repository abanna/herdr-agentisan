package report

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"slices"
	"strings"
)

// The Codex ctx adapter (ADR-001 A22). Codex runs `report codex` as an async
// PostToolUse and Stop hook and passes the hook's JSON on stdin; ctx itself
// keeps the token contract Statusline writes.
const (
	// MaxCodexHookBytes bounds the hook payload kept from stdin. A PostToolUse
	// payload carries the tool's input and output, so it can be large; one
	// over the cap reports nothing, and the next hook tries again.
	MaxCodexHookBytes = 4 << 20
	// MaxTranscriptTail bounds how much of the end of the session's rollout
	// is read for its latest token count. Codex writes one after every model
	// response, so it is rarely more than a few kilobytes from the end; the
	// longest rollout line measured was 2.6 MB.
	MaxTranscriptTail = 4 << 20
	// codexBaselineTokens is Codex's BASELINE_TOKENS: context the TUI counts
	// as always in use, taken off both the window and the usage.
	codexBaselineTokens = 12000
)

var (
	// ErrSubagent means the hook fired inside a Codex subagent, whose
	// transcript is not the pane's session.
	ErrSubagent = errors.New("codex hook fired inside a subagent")
	// ErrCodexDaemon means a Codex app-server is among the reporting
	// process's ancestors: the session runs in a shared server, not in the
	// pane's own Codex. The hook then carries that server's environment and
	// ancestry, which belong to whichever process started it, so
	// HERDR_PANE_ID and the lineage can both lead to another pane.
	ErrCodexDaemon = errors.New("codex session runs in a shared app-server, not in the pane's codex")
	// ErrHostUnverified means the process hosting the Codex session cannot
	// be checked: no lineage (no /proc), no cmdline reader, an ancestor
	// whose cmdline is unreadable, or one whose pid was reissued mid-check.
	ErrHostUnverified = errors.New("cannot verify which process hosts the codex session")
)

// CodexHook is the part of a Codex hook's stdin JSON a report needs.
type CodexHook struct {
	// TranscriptPath is the session's rollout file; empty when Codex keeps
	// none, as for an ephemeral session.
	TranscriptPath string
	// AgentID is set only when the hook fired inside a subagent.
	AgentID string
}

// ParseCodexHook reads a Codex hook payload from r, and reads r to its end
// whatever it finds, so Codex's write of the payload always completes rather
// than meeting a closed pipe. (Codex 0.161 tolerates a closed pipe too: a
// broken pipe on a hook's stdin counts as written.) Fields other than
// transcript_path and agent_id are Codex's to add or change, and ignored.
func ParseCodexHook(r io.Reader) (CodexHook, error) {
	// Run by hand in a pane, stdin is the terminal: reading it would wait
	// for Ctrl-D with nothing on screen to say so.
	if isCharDevice(r) {
		return CodexHook{}, fmt.Errorf("%w: stdin is a terminal or other character device, not a piped codex hook payload", ErrMalformed)
	}
	raw, err := io.ReadAll(io.LimitReader(r, MaxCodexHookBytes+1))
	if err != nil {
		return CodexHook{}, fmt.Errorf("%w: read: %w", ErrMalformed, err)
	}
	if len(raw) > MaxCodexHookBytes {
		// Codex closes stdin once the payload is written and kills a hook
		// that outlives its timeout, so draining ends.
		_, _ = io.Copy(io.Discard, r)
		return CodexHook{}, fmt.Errorf("%w: hook payload exceeds %d bytes", ErrMalformed, MaxCodexHookBytes)
	}
	var in struct {
		TranscriptPath *string `json:"transcript_path"`
		AgentID        *string `json:"agent_id"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return CodexHook{}, fmt.Errorf("%w: %w", ErrMalformed, err)
	}
	var h CodexHook
	if in.TranscriptPath != nil {
		h.TranscriptPath = *in.TranscriptPath
	}
	if in.AgentID != nil {
		h.AgentID = *in.AgentID
	}
	return h, nil
}

// CheckCodexHost returns nil only when every process of the lineage was
// read again and none is a Codex app-server, which is what a hook run by the
// pane's own Codex (codex --no-daemon) looks like. A lineage through an
// app-server is ErrCodexDaemon; one that cannot be checked, including a nil
// lineage, is ErrHostUnverified. Either way a report must go nowhere.
func (l *Lineage) CheckCodexHost() error {
	if l == nil || l.stat == nil || l.cmdline == nil || len(l.Procs) == 0 {
		return fmt.Errorf("%w: no lineage with a cmdline reader", ErrHostUnverified)
	}
	for _, p := range l.Procs {
		argv, err := l.cmdline(p.PID)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrHostUnverified, err)
		}
		// The arguments count only if pid is still the process the lineage
		// read: a reissued pid's arguments say nothing about the hook.
		if s, err := l.stat(p.PID); err != nil || s.Start != p.Start {
			return fmt.Errorf("%w: pid %d is no longer the process the lineage read", ErrHostUnverified, p.PID)
		}
		// The daemon runs `codex app-server ...`; an IDE's server may run it
		// through the node launcher, one argument later.
		if len(argv) > 1 && slices.Contains(argv[1:], "app-server") {
			return fmt.Errorf("%w: pid %d runs %q", ErrCodexDaemon, p.PID, strings.Join(argv, " "))
		}
	}
	return nil
}

// TranscriptContext returns the context use Codex's TUI shows as "Context
// N% used", from the latest token count in the tail of the rollout at path.
// path must be absolute and name a regular file, or a link to one.
func TranscriptContext(path string) (int, error) {
	if !filepath.IsAbs(path) {
		return 0, fmt.Errorf("%w: transcript path %q is not absolute", ErrMalformed, path)
	}
	f, err := openNonBlocking(path)
	if err != nil {
		return 0, fmt.Errorf("%w: %w", ErrMalformed, err)
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return 0, fmt.Errorf("%w: stat transcript: %w", ErrMalformed, err)
	}
	if !fi.Mode().IsRegular() {
		return 0, fmt.Errorf("%w: transcript %s is not a regular file", ErrMalformed, path)
	}
	tail, err := readTail(f, fi.Size())
	if err != nil {
		return 0, err
	}
	return latestContext(tail)
}

// readTail returns the whole lines among the last MaxTranscriptTail bytes
// of r, which is size bytes long. It reads one byte more, the one before the
// window, to tell whether the window starts a line or cuts one; a cut line
// is left out, never parsed.
func readTail(r io.ReaderAt, size int64) ([]byte, error) {
	start := max(size-MaxTranscriptTail, 0)
	from := max(start-1, 0)
	buf := make([]byte, size-from)
	n, err := r.ReadAt(buf, from)
	// The file can shrink under the read; what was read is still lines.
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: read transcript: %w", ErrMalformed, err)
	}
	buf = buf[:n]
	if start > 0 {
		nl := bytes.IndexByte(buf, '\n')
		if nl < 0 {
			return nil, nil
		}
		buf = buf[nl+1:]
	}
	return buf, nil
}

// codexUsage is one token usage record of a Codex token_count event.
type codexUsage struct {
	InputTokens           int64 `json:"input_tokens"`
	CachedInputTokens     int64 `json:"cached_input_tokens"`
	CacheWriteInputTokens int64 `json:"cache_write_input_tokens"`
	OutputTokens          int64 `json:"output_tokens"`
	ReasoningOutputTokens int64 `json:"reasoning_output_tokens"`
	TotalTokens           int64 `json:"total_tokens"`
}

// codexTokenInfo is a token_count event's info: the session's cumulative
// usage, the last model response's usage, which is the context in use, and
// the model's usable window.
type codexTokenInfo struct {
	Total  *codexUsage `json:"total_token_usage"`
	Last   *codexUsage `json:"last_token_usage"`
	Window *int64      `json:"model_context_window"`
}

// latestContext scans tail backwards for the newest token count Codex
// recorded with usage, skipping every line that is not one: other events, a
// line still being written, and counts with no info yet.
func latestContext(tail []byte) (int, error) {
	for _, line := range slices.Backward(bytes.Split(tail, []byte{'\n'})) {
		if !bytes.Contains(line, []byte(`"token_count"`)) {
			continue
		}
		var ev struct {
			Type    string `json:"type"`
			Payload struct {
				Type string          `json:"type"`
				Info *codexTokenInfo `json:"info"`
			} `json:"payload"`
		}
		if json.Unmarshal(line, &ev) != nil || ev.Type != "event_msg" || ev.Payload.Type != "token_count" ||
			ev.Payload.Info == nil || ev.Payload.Info.Last == nil {
			continue
		}
		return ev.Payload.Info.percent()
	}
	return 0, fmt.Errorf("%w: no codex token count in the last %d bytes of the transcript", ErrNoContext, MaxTranscriptTail)
}

// percent is Codex's "Context N% used" (codex-rs rust-v0.161.0
// tui/src/token_usage.rs): the baseline comes off the window and the usage,
// the REMAINING share is rounded half away from zero, and used is 100 minus
// it. The window is already the model's usable share of its context.
//
// One deliberate difference: when the context overflows, Codex records the
// full window as the session total with nothing else counted and leaves the
// last usage near 0 (fill_to_context_window), so the TUI shows almost nothing
// used just as the context fills. That record reads 100 here, the value
// rotation needs.
func (i codexTokenInfo) percent() (int, error) {
	if i.Window == nil || *i.Window <= codexBaselineTokens {
		return 0, fmt.Errorf("%w: codex token count has no usable context window", ErrNoContext)
	}
	window := *i.Window
	if i.Total != nil && *i.Total == (codexUsage{TotalTokens: window}) {
		return 100, nil
	}
	effective := window - codexBaselineTokens
	// max before subtracting, so an extreme negative count cannot wrap.
	used := max(i.Last.TotalTokens, codexBaselineTokens) - codexBaselineTokens
	remaining := max(effective-used, 0)
	share := min(max(float64(remaining)/float64(effective)*100, 0), 100)
	return 100 - int(math.Round(share)), nil
}

// Codex pushes the ctx token from a Codex hook's payload in r: the latest
// token count in the session's transcript, as the TUI's "Context N% used"
// shows it, set on the pane the session's Codex runs in. It reads r to its
// end first, then reports nothing outside a pane, from a subagent, from a
// session it cannot place (CheckCodexHost), or with no count to report.
//
// One herdr.CallTimeout bounds the herdr calls, finding the pane included.
func Codex(ctx context.Context, rep Reporter, pane Pane, r io.Reader) (Result, error) {
	hook, err := ParseCodexHook(r)
	if pane.SocketPath == "" || pane.PaneID == "" {
		return Result{}, ErrNotInPane
	}
	if err != nil {
		return Result{}, err
	}
	if hook.AgentID != "" {
		return Result{}, fmt.Errorf("%w: agent %s", ErrSubagent, hook.AgentID)
	}
	if err := pane.Lineage.CheckCodexHost(); err != nil {
		return Result{}, err
	}
	if hook.TranscriptPath == "" {
		return Result{}, fmt.Errorf("%w: the codex session keeps no transcript", ErrNoContext)
	}
	pct, err := TranscriptContext(hook.TranscriptPath)
	if err != nil {
		return Result{}, err
	}
	return pushCtx(ctx, rep, pane, pct)
}
