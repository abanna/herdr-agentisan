package report

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/abanna/herdr-agentisan/internal/herdr"
)

// The turn hooks (ADR-001 A42). Claude and Codex run `report turn` as an
// async UserPromptSubmit and Stop hook and pass the hook's JSON on stdin. The
// agent states its own item and stage in lines of its text, and the hook
// carries them to its pane under the item and stage token contract.
const (
	// MaxTurnHookBytes bounds the hook payload read from stdin: it carries a
	// whole prompt or final message. One over the cap reports nothing.
	MaxTurnHookBytes = 4 << 20
	// EventPromptSubmit and EventStop are the hook events a turn report
	// reads, named alike by both agents.
	EventPromptSubmit = "UserPromptSubmit"
	EventStop         = "Stop"
	// ItemPrefix opens the line that states the item: `item: NERD-5334`.
	ItemPrefix = "item:"
)

// StatusPrefixes open the status line that ends a final message: `BOSS:`
// for a worker reporting to its boss, `STATUS:` for a pane with no boss
// above it.
var StatusPrefixes = []string{"BOSS:", "STATUS:"}

// ErrNothingStated means the turn states no item and, for a Stop, ends on no
// status line. Nothing is reported, so a stage a pipeline step set stays.
var ErrNothingStated = errors.New("the turn states no item or stage")

// TurnHook is the part of a Claude or Codex hook's stdin JSON a turn report
// needs. Both agents send these fields under the same names.
type TurnHook struct {
	// Event is hook_event_name.
	Event string
	// Text is a UserPromptSubmit's prompt, or a Stop's final message
	// (last_assistant_message, which Codex sends as null when there is none).
	Text string
	// AgentID is set only when the hook fired inside a subagent.
	AgentID string
}

// ParseTurnHook reads a turn hook's payload from r, and reads r to its end
// whatever it finds, as ParseCodexHook does. Other fields are the agents' to
// add or change, and ignored.
func ParseTurnHook(r io.Reader) (TurnHook, error) {
	raw, err := readHook(r, MaxTurnHookBytes)
	if err != nil {
		return TurnHook{}, err
	}
	var in struct {
		Event   string `json:"hook_event_name"`
		Prompt  string `json:"prompt"`
		Message string `json:"last_assistant_message"`
		AgentID string `json:"agent_id"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return TurnHook{}, fmt.Errorf("%w: %w", ErrMalformed, err)
	}
	h := TurnHook{Event: in.Event, AgentID: in.AgentID}
	switch in.Event {
	case EventPromptSubmit:
		h.Text = in.Prompt
	case EventStop:
		h.Text = in.Message
	}
	return h, nil
}

// Stated returns the item and stage a turn's text states, each "" when it
// states none, cleaned into a value CheckValue accepts.
//
// A prompt (EventPromptSubmit) states an item only: its first `item:` line,
// as a dispatch carries it. A final message (EventStop) states the stage in
// the last line of its closing paragraph, or the last before that
// paragraph's trailing `item:` lines, when it opens with one of
// StatusPrefixes; and the item in the paragraph's last `item:` line. A line counts once
// its Markdown emphasis and code ticks are dropped, so `**BOSS:** done` is a
// status line, but never inside a sentence or behind a list marker.
func Stated(event, text string) (item, stage string) {
	lines := strings.Split(text, "\n")
	if event == EventPromptSubmit {
		for _, l := range lines {
			if v := lineValue(l, ItemPrefix); v != "" {
				return v, ""
			}
		}
		return "", ""
	}
	end := len(lines)
	for end > 0 && clean(lines[end-1]) == "" {
		end--
	}
	start := end
	for start > 0 && clean(lines[start-1]) != "" {
		start--
	}
	closing := lines[start:end]
	for _, l := range closing {
		if v := lineValue(l, ItemPrefix); v != "" {
			item = v
		}
	}
	// The status line ends the paragraph, or comes just before its trailing
	// item lines.
	for i := len(closing) - 1; i >= 0; i-- {
		if lineValue(closing[i], ItemPrefix) != "" {
			continue
		}
		for _, p := range StatusPrefixes {
			if v := lineValue(closing[i], p); v != "" {
				return item, v
			}
		}
		break
	}
	return item, ""
}

// lineValue returns what follows prefix on line once the line is cleaned,
// cut to MaxValueChars characters; "" when the line does not open with
// prefix or nothing follows it.
func lineValue(line, prefix string) string {
	rest, ok := strings.CutPrefix(clean(line), prefix)
	if !ok {
		return ""
	}
	v := strings.TrimSpace(rest)
	if utf8.RuneCountInString(v) > MaxValueChars {
		v = strings.TrimRight(string([]rune(v)[:MaxValueChars-1]), " ") + "…"
	}
	return v
}

// clean drops Markdown emphasis and code ticks (`*` and the backtick) and
// unprintable characters from s, and turns every run of white space into one
// space, trimmed at the ends: what is left is printable throughout.
func clean(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '*' || r == '`':
			return -1
		case unicode.IsSpace(r):
			return ' '
		case !unicode.IsPrint(r):
			return -1
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

// TurnResult is what a turn report pushed, and where. Item and Stage are ""
// when the turn did not state them; Cleared means the stage was cleared too.
type TurnResult struct {
	Item    string
	Stage   string
	Cleared bool
	// PaneID is the pane the tokens landed on, as for StageResult.
	PaneID string
}

// Turn pushes the item and stage a turn hook's payload on r states (Stated)
// to the pane the reporting process runs in, with source agentisan and the
// StageTTL, and returns what it pushed where. Only the keys stated are set,
// but an item stated with no stage that is not the item the pane carries
// also clears the stage, which belonged to the old item.
//
// Outside a pane it returns ErrNotInPane, inside a subagent ErrSubagent, for
// an event other than EventPromptSubmit and EventStop ErrMalformed, and for a
// turn that states nothing ErrNothingStated; none calls herdr. The pane is
// resolved as Stage resolves it, after the host check checkTurnHost makes.
// One herdr.CallTimeout bounds the whole report.
func Turn(ctx context.Context, rep Reporter, pane Pane, r io.Reader) (TurnResult, error) {
	hook, err := ParseTurnHook(r)
	if pane.SocketPath == "" || pane.PaneID == "" {
		return TurnResult{}, ErrNotInPane
	}
	if err != nil {
		return TurnResult{}, err
	}
	if hook.AgentID != "" {
		return TurnResult{}, fmt.Errorf("%w: agent %s", ErrSubagent, hook.AgentID)
	}
	if hook.Event != EventPromptSubmit && hook.Event != EventStop {
		return TurnResult{}, fmt.Errorf("%w: hook event %q is neither %s nor %s", ErrMalformed, hook.Event, EventPromptSubmit, EventStop)
	}
	item, stage := Stated(hook.Event, hook.Text)
	if item == "" && stage == "" {
		return TurnResult{}, fmt.Errorf("%w: %s", ErrNothingStated, hook.Event)
	}
	if err := pane.checkTurnHost(); err != nil {
		return TurnResult{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, herdr.CallTimeout)
	defer cancel()
	target, err := ResolvePane(ctx, rep, pane)
	if err != nil {
		return TurnResult{}, err
	}
	res := TurnResult{Item: item, Stage: stage, PaneID: target}
	tokens := map[string]string{}
	if stage != "" {
		tokens[StageKey] = stage
	}
	if item != "" {
		tokens[ItemKey] = item
		if stage == "" {
			current, err := paneItem(ctx, rep, target)
			if err != nil {
				return TurnResult{}, err
			}
			if current != item {
				// herdr clears a token reported empty.
				tokens[StageKey], res.Cleared = "", true
			}
		}
	}
	err = rep.ReportPaneMetadata(ctx, herdr.PaneMetadata{
		PaneID: target, Source: Source, Tokens: tokens, TTLMillis: uint64(StageTTL / time.Millisecond),
	})
	if err != nil {
		return TurnResult{}, fmt.Errorf("report %s and %s to %s: %w", ItemKey, StageKey, target, err)
	}
	return res, nil
}

// paneItem returns the item token pane carries, "" when it has none.
func paneItem(ctx context.Context, rep PaneFinder, pane string) (string, error) {
	panes, err := rep.ListPanes(ctx)
	if err != nil {
		return "", fmt.Errorf("read %s's %s: %w", pane, ItemKey, err)
	}
	for _, p := range panes {
		if p.PaneID == pane {
			return p.Tokens[ItemKey], nil
		}
	}
	return "", nil
}

// checkTurnHost is the check a turn report must pass before it calls herdr.
// The payload does not prove which agent ran the hook, so the lineage
// decides: one that may hold a Codex process (mayRunCodex) must pass
// CheckCodexHost, as for `report codex` (A22), so a hook the shared
// app-server ran reports nothing; a command carrying CODEX_THREAD_ID gets
// checkCodex (A25). Any other lineage is placed by ResolvePane alone, as
// Claude's statusline is.
func (p Pane) checkTurnHost() error {
	if p.CodexThread == "" && p.Lineage.mayRunCodex() {
		return p.Lineage.CheckCodexHost()
	}
	return p.checkCodex()
}

// mayRunCodex reports whether a process of the lineage runs Codex or an
// app-server, or may: one whose arguments cannot be read, or a lineage with
// no reader for them, counts, so CheckCodexHost then refuses it. A nil
// lineage, on an OS with no process table reader, does not.
func (l *Lineage) mayRunCodex() bool {
	if l == nil {
		return false
	}
	if l.cmdline == nil {
		return true
	}
	for _, p := range l.Procs {
		argv, err := l.cmdline(p.PID)
		if err != nil || isCodex(argv) || len(argv) > 1 && slices.Contains(argv[1:], "app-server") {
			return true
		}
	}
	return false
}
