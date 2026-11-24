package team_test

import (
	"bytes"
	"encoding/json"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/report"
	"github.com/abanna/herdr-agentisan/internal/snapshot"
	"github.com/abanna/herdr-agentisan/internal/store"
	"github.com/abanna/herdr-agentisan/internal/team"
)

// at is the instant every snapshot test builds at.
var at = time.Date(2026, 10, 10, 9, 30, 0, 0, time.UTC)

// pane is a pane in ws carrying tokens: an agent of kind agent ("" for a
// plain shell) at status.
func pane(id, ws, agent, status string, tokens map[string]string) herdr.PaneInfo {
	return herdr.PaneInfo{PaneID: id, WorkspaceID: ws, Agent: agent, AgentStatus: status, Tokens: tokens}
}

// oneRow builds the model of one pane in the live team's first coder's tab,
// named name by agent.list when name is not "", and returns the row the
// snapshot gives it, less its tab, which it checks is the pane's.
func oneRow(t *testing.T, p herdr.PaneInfo, name string) snapshot.Agent {
	t.Helper()
	l := teamOf([]herdr.PaneInfo{inTeam(p, "coder-1")})
	if name != "" {
		l.Agents = []herdr.AgentInfo{{PaneID: p.PaneID, Name: name}}
	}
	s := team.Snapshot(team.Build(l, liveTeam, nil), liveProject, at, nil, nil)
	require.Len(t, s.Groups, 1)
	require.Len(t, s.Groups[0].Agents, 1)
	row := s.Groups[0].Agents[0]
	assert.Equal(t, liveWS+":coder-1", row.TabID, "the row carries its pane's tab")
	row.TabID = ""
	return row
}

//go:fix inline

// TestSnapshotRowNames: a row is named as v12 names it
// (herdr-dashboard-v12.py:180-205): agent.list's name, else the pane's
// label, else its pane id. Only an agent.list name is one agent.focus takes
// (herdr 0.9.3 src/app/terminal_targets.rs:74-104), so every other row is
// marked unnamed and the dashboard jumps to it by pane id.
func TestSnapshotRowNames(t *testing.T) {
	t.Parallel()

	labelled := func(p herdr.PaneInfo, label string) herdr.PaneInfo { p.Label = label; return p }
	tests := map[string]struct {
		pane herdr.PaneInfo
		name string
		want snapshot.Agent
	}{
		"a named agent": {
			pane: pane("wN:p1", "", "claude", "working", nil), name: "pee01",
			want: snapshot.Agent{Name: "pee01", PaneID: "wN:p1", Status: snapshot.StatusWorking, Kind: "claude"},
		},
		"a name beats a label": {
			pane: labelled(pane("wN:p1", "", "claude", "working", nil), "notes"), name: "pee01",
			want: snapshot.Agent{Name: "pee01", PaneID: "wN:p1", Status: snapshot.StatusWorking, Kind: "claude"},
		},
		"an unnamed agent with a label": {
			pane: labelled(pane("wN:p2", "", "claude", "idle", nil), "scratch"),
			want: snapshot.Agent{Name: "scratch", Unnamed: true, PaneID: "wN:p2", Status: snapshot.StatusIdle, Kind: "claude"},
		},
		"an unnamed agent without a label": {
			pane: pane("wN:p2", "", "codex", "idle", nil),
			want: snapshot.Agent{Name: "wN:p2", Unnamed: true, PaneID: "wN:p2", Status: snapshot.StatusIdle, Kind: "codex"},
		},
		"a labelled shell": {
			pane: labelled(pane("wN:p3", "", "", "unknown", nil), "notes"),
			want: snapshot.Agent{Name: "notes", Unnamed: true, Shell: true, PaneID: "wN:p3", Status: snapshot.StatusUnknown},
		},
		"an unlabelled shell": {
			pane: pane("wN:p3", "", "", "unknown", nil),
			want: snapshot.Agent{Name: "wN:p3", Unnamed: true, Shell: true, PaneID: "wN:p3", Status: snapshot.StatusUnknown},
		},
		// A label of white space alone would draw as a blank name.
		"a blank label is no label": {
			pane: labelled(pane("wN:p3", "", "", "unknown", nil), " \t"),
			want: snapshot.Agent{Name: "wN:p3", Unnamed: true, Shell: true, PaneID: "wN:p3", Status: snapshot.StatusUnknown},
		},
		// The label is kept as the user wrote it; the dashboard makes text
		// safe to draw.
		"a non-ASCII label with a control character": {
			pane: labelled(pane("wN:p3", "", "", "unknown", nil), "メモ\x1b[2J"),
			want: snapshot.Agent{Name: "メモ\x1b[2J", Unnamed: true, Shell: true, PaneID: "wN:p3", Status: snapshot.StatusUnknown},
		},
		// Nothing normalises, trims or re-encodes a label: the bytes the
		// user gave herdr are the name. Encoding the snapshot as JSON makes
		// invalid UTF-8 valid (TestSnapshotDecodesStrictly).
		"a label with invalid UTF-8": {
			pane: labelled(pane("wN:p3", "", "", "unknown", nil), "no\xfftes"),
			want: snapshot.Agent{Name: "no\xfftes", Unnamed: true, Shell: true, PaneID: "wN:p3", Status: snapshot.StatusUnknown},
		},
		"a label with a truncated multibyte": {
			pane: labelled(pane("wN:p3", "", "", "unknown", nil), "notes\xe2\x82"),
			want: snapshot.Agent{Name: "notes\xe2\x82", Unnamed: true, Shell: true, PaneID: "wN:p3", Status: snapshot.StatusUnknown},
		},
		"a NUL in a label": {
			pane: labelled(pane("wN:p3", "", "", "unknown", nil), "no\x00tes"),
			want: snapshot.Agent{Name: "no\x00tes", Unnamed: true, Shell: true, PaneID: "wN:p3", Status: snapshot.StatusUnknown},
		},
		"a byte-order mark before a label": {
			pane: labelled(pane("wN:p3", "", "", "unknown", nil), "\xef\xbb\xbfnotes"),
			want: snapshot.Agent{Name: "\xef\xbb\xbfnotes", Unnamed: true, Shell: true, PaneID: "wN:p3", Status: snapshot.StatusUnknown},
		},
		"an NFD label is kept as written": {
			pane: labelled(pane("wN:p3", "", "", "unknown", nil), "cafe\xcc\x81"),
			want: snapshot.Agent{Name: "cafe\xcc\x81", Unnamed: true, Shell: true, PaneID: "wN:p3", Status: snapshot.StatusUnknown},
		},
		"an option-like label": {
			pane: labelled(pane("wN:p3", "", "", "unknown", nil), "--help"),
			want: snapshot.Agent{Name: "--help", Unnamed: true, Shell: true, PaneID: "wN:p3", Status: snapshot.StatusUnknown},
		},
		"a label with a line break": {
			pane: labelled(pane("wN:p3", "", "", "unknown", nil), "a\r\nb"),
			want: snapshot.Agent{Name: "a\r\nb", Unnamed: true, Shell: true, PaneID: "wN:p3", Status: snapshot.StatusUnknown},
		},
		"a label of line breaks alone is no label": {
			pane: labelled(pane("wN:p3", "", "", "unknown", nil), "\r\n"),
			want: snapshot.Agent{Name: "wN:p3", Unnamed: true, Shell: true, PaneID: "wN:p3", Status: snapshot.StatusUnknown},
		},
		// A label equal to another agent's name is still a label: the row
		// is unnamed, so a jump goes by its own pane id.
		"a label that looks like an agent name": {
			pane: labelled(pane("wN:p3", "", "", "unknown", nil), "pee01"),
			want: snapshot.Agent{Name: "pee01", Unnamed: true, Shell: true, PaneID: "wN:p3", Status: snapshot.StatusUnknown},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, oneRow(t, tc.pane, tc.name))
		})
	}
}

// TestSnapshotRowStatus: herdr's agent_status, mapped one to one onto the
// contract's; "ready" for a pane carrying a non-empty handoff token,
// whatever its status; and "unknown", the contract's status for a pane with
// no agent, for a plain shell.
func TestSnapshotRowStatus(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		pane herdr.PaneInfo
		want snapshot.Status
	}{
		"working":                   {pane: pane("wN:p1", "", "claude", "working", nil), want: snapshot.StatusWorking},
		"idle":                      {pane: pane("wN:p1", "", "claude", "idle", nil), want: snapshot.StatusIdle},
		"done":                      {pane: pane("wN:p1", "", "claude", "done", nil), want: snapshot.StatusDone},
		"blocked":                   {pane: pane("wN:p1", "", "claude", "blocked", nil), want: snapshot.StatusBlocked},
		"unknown":                   {pane: pane("wN:p1", "", "claude", "unknown", nil), want: snapshot.StatusUnknown},
		"a status from a newer one": {pane: pane("wN:p1", "", "claude", "napping", nil), want: snapshot.StatusUnknown},
		"no status at all":          {pane: pane("wN:p1", "", "claude", "", nil), want: snapshot.StatusUnknown},
		"a status in another case":  {pane: pane("wN:p1", "", "claude", "Working", nil), want: snapshot.StatusUnknown},
		"a handoff token":           {pane: pane("wN:p1", "", "claude", "done", map[string]string{"handoff": "⟳ READY TO RESTART"}), want: snapshot.StatusReady},
		"a handoff while working":   {pane: pane("wN:p1", "", "claude", "working", map[string]string{"handoff": "x"}), want: snapshot.StatusReady},
		"an empty handoff token":    {pane: pane("wN:p1", "", "claude", "idle", map[string]string{"handoff": ""}), want: snapshot.StatusIdle},
		"a shell":                   {pane: pane("wN:p1", "", "", "unknown", nil), want: snapshot.StatusUnknown},
		// herdr reports a status for every pane; a shell's is not an
		// agent's, whatever it says.
		"a shell herdr calls idle": {pane: pane("wN:p1", "", "", "idle", nil), want: snapshot.StatusUnknown},
		"a shell with a handoff":   {pane: pane("wN:p1", "", "", "unknown", map[string]string{"handoff": "x"}), want: snapshot.StatusReady},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, oneRow(t, tc.pane, "").Status)
		})
	}
	t.Run("a named agent herdr detects no kind of", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, snapshot.StatusWorking, oneRow(t, pane("wN:p1", "", "", "working", nil), "pee01").Status,
			"agent.list names only agent terminals, so a name makes the pane an agent")
	})
}

// TestSnapshotRowShell: a row is a shell exactly when no agent runs in its
// pane, herdr detecting none and knowing none by name (ADR-001 A38). An
// unnamed agent is not one, so the dashboard can tell the two apart.
func TestSnapshotRowShell(t *testing.T) {
	t.Parallel()

	handoff := map[string]string{"handoff": "x"}
	tests := map[string]struct {
		pane       herdr.PaneInfo
		name       string
		shell      bool
		wantStatus snapshot.Status
	}{
		"a named agent":                          {pane: pane("wN:p1", "", "claude", "working", nil), name: "pee01", wantStatus: snapshot.StatusWorking},
		"a named agent herdr detects not":        {pane: pane("wN:p1", "", "", "working", nil), name: "pee01", wantStatus: snapshot.StatusWorking},
		"an unnamed agent":                       {pane: pane("wN:p2", "", "claude", "idle", nil), wantStatus: snapshot.StatusIdle},
		"an unnamed agent herdr cannot classify": {pane: pane("wN:p2", "", "claude", "unknown", nil), wantStatus: snapshot.StatusUnknown},
		"a plain shell":                          {pane: pane("wN:p3", "", "", "unknown", nil), shell: true, wantStatus: snapshot.StatusUnknown},
		"a shell herdr calls idle":               {pane: pane("wN:p3", "", "", "idle", nil), shell: true, wantStatus: snapshot.StatusUnknown},
		"a shell with a handoff":                 {pane: pane("wN:p3", "", "", "unknown", handoff), shell: true, wantStatus: snapshot.StatusReady},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			row := oneRow(t, tc.pane, tc.name)
			assert.Equal(t, tc.shell, row.Shell)
			assert.Equal(t, tc.wantStatus, row.Status)
			doc := snapshot.Snapshot{V: snapshot.Version, Team: "t", Groups: []snapshot.Group{{Name: "coders", Agents: []snapshot.Agent{row}}}}
			assert.NoError(t, doc.Validate(), "a shell row is unnamed, as the contract requires")
		})
	}
}

// TestSnapshotRowKind: a row's agent is the one herdr detects in its pane,
// kept as herdr sent it, the boss's included (ADR-001 A68): no fallback
// from a name or the launch, so a shell and a named pane herdr detects
// nothing in carry none. Whatever herdr sends, the row is one the decoder
// accepts.
func TestSnapshotRowKind(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		pane herdr.PaneInfo
		name string
		want string
	}{
		"a claude":                        {pane: pane("wN:p1", "", "claude", "working", nil), name: "pee01", want: "claude"},
		"a codex":                         {pane: pane("wN:p1", "", "codex", "working", nil), name: "pee01", want: "codex"},
		"an unnamed codex":                {pane: pane("wN:p2", "", "codex", "unknown", nil), want: "codex"},
		"a kind herdr adds":               {pane: pane("wN:p2", "", "gemini", "idle", nil), want: "gemini"},
		"a kind with a control character": {pane: pane("wN:p2", "", "co\x1bdex", "idle", nil), want: "co\x1bdex"},
		// Kept as herdr sent it, whatever its bytes: the dashboard makes
		// it safe to draw, and JSON makes it valid UTF-8.
		"a non-ASCII kind":                {pane: pane("wN:p2", "", "コーデックス", "idle", nil), want: "コーデックス"},
		"a kind with invalid UTF-8":       {pane: pane("wN:p2", "", "co\xffdex", "idle", nil), want: "co\xffdex"},
		"a decomposed kind":               {pane: pane("wN:p2", "", "cafe\u0301", "idle", nil), want: "cafe\u0301"},
		"a kind like an option":           {pane: pane("wN:p2", "", "--codex", "idle", nil), want: "--codex"},
		"a kind with white space":         {pane: pane("wN:p2", "", " codex\r\n", "idle", nil), want: " codex\r\n"},
		"a kind with a NUL":               {pane: pane("wN:p2", "", "co\x00dex", "idle", nil), want: "co\x00dex"},
		"a kind in another case":          {pane: pane("wN:p2", "", "Codex", "idle", nil), want: "Codex"},
		"a truncated multibyte":           {pane: pane("wN:p2", "", "co\xe2\x82", "idle", nil), want: "co\xe2\x82"},
		"a BOM before a kind":             {pane: pane("wN:p2", "", "\ufeffcodex", "idle", nil), want: "\ufeffcodex"},
		"mixed encodings":                 {pane: pane("wN:p2", "", "é\xffコ", "idle", nil), want: "é\xffコ"},
		"a named agent herdr detects not": {pane: pane("wN:p1", "", "", "working", nil), name: "pee01"},
		"a plain shell":                   {pane: pane("wN:p3", "", "", "unknown", nil)},
		"a shell with a handoff":          {pane: pane("wN:p3", "", "", "unknown", map[string]string{"handoff": "x"})},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			row := oneRow(t, tc.pane, tc.name)
			assert.Equal(t, tc.want, row.Kind)
			raw, err := json.Marshal(snapshot.Snapshot{V: snapshot.Version, Team: "t", Groups: []snapshot.Group{{Name: "coders", Agents: []snapshot.Agent{row}}}})
			require.NoError(t, err)
			got, err := snapshot.Decode(bytes.NewReader(raw))
			require.NoError(t, err, "the decoder accepts every row the daemon writes")
			var want string // as JSON carries it: valid UTF-8, a U+FFFD for each invalid byte
			raw, err = json.Marshal(tc.want)
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(raw, &want))
			assert.Equal(t, want, got.Groups[0].Agents[0].Kind)
		})
	}
	t.Run("the boss", func(t *testing.T) {
		t.Parallel()
		l := teamOf([]herdr.PaneInfo{inTeam(pane("wJ:p1", "", "codex", "working", nil), "boss")})
		l.Agents = []herdr.AgentInfo{{PaneID: "wJ:p1", Name: "boss"}}
		s := team.Snapshot(team.Build(l, liveTeam, nil), liveProject, at, nil, nil)
		require.NotNil(t, s.Boss)
		assert.Equal(t, "codex", s.Boss.Kind)
	})
}

// TestSnapshotRowCtx: ctx is the token as `report statusline` writes it, a
// bare integer from 0 to 100 (ADR-001 token contract). Anything else, from
// another writer or a stale format, is left out rather than guessed at, and
// so is a ctx on a pane herdr detects no agent in (A27).
func TestSnapshotRowCtx(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		ctx  string
		want *int
	}{
		"0":                        {ctx: "0", want: new(0)},
		"42":                       {ctx: "42", want: new(42)},
		"100, the ceiling":         {ctx: "100", want: new(100)},
		"101, one past it":         {ctx: "101", want: nil},
		"-1":                       {ctx: "-1", want: nil},
		"-0":                       {ctx: "-0", want: nil},
		"a plus sign":              {ctx: "+5", want: nil},
		"a leading zero":           {ctx: "07", want: nil},
		"surrounding space":        {ctx: " 5", want: nil},
		"a percent sign":           {ctx: "5%", want: nil},
		"a fraction":               {ctx: "42.5", want: nil},
		"empty":                    {ctx: "", want: nil},
		"not a number":             {ctx: "abc", want: nil},
		"past int64":               {ctx: "99999999999999999999", want: nil},
		"non-ASCII digits":         {ctx: "٤٢", want: nil},
		"a NUL inside":             {ctx: "4\x002", want: nil},
		"invalid UTF-8":            {ctx: "4\xff", want: nil},
		"a trailing newline":       {ctx: "42\n", want: nil},
		"a trailing CRLF":          {ctx: "42\r\n", want: nil},
		"a truncated multibyte":    {ctx: "4\xe2\x82", want: nil},
		"full-width digits":        {ctx: "４２", want: nil},
		"a byte-order mark before": {ctx: "\xef\xbb\xbf42", want: nil},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			row := oneRow(t, pane("wN:p1", "", "claude", "working", map[string]string{"ctx": tc.ctx}), "pee01")
			assert.Equal(t, tc.want, row.Ctx)
		})
	}
	t.Run("no ctx token", func(t *testing.T) {
		t.Parallel()
		assert.Nil(t, oneRow(t, pane("wN:p1", "", "claude", "working", nil), "pee01").Ctx)
	})
	t.Run("no agent detected", func(t *testing.T) {
		t.Parallel()
		row := oneRow(t, pane("wN:p1", "", "", "unknown", map[string]string{"ctx": "42"}), "pee01")
		assert.Nil(t, row.Ctx, "a ctx a Codex left in a pane agent.list still names is ignored (ADR-001 A27)")
	})
}

// TestSnapshotRowItemAndStage: item and stage are the tokens as `report
// stage` (or any writer) left them, at most 80 characters as herdr caps a
// token value (src/app/api_helpers.rs:208); an absent one is left out.
func TestSnapshotRowItemAndStage(t *testing.T) {
	t.Parallel()

	long := string(bytes.Repeat([]byte("x"), 80))
	tests := map[string]map[string]string{
		"both":                   {"item": "NERD-5255", "stage": "build_test"},
		"item only":              {"item": "NERD-5255"},
		"stage only":             {"stage": "review"},
		"neither":                {},
		"non-ASCII":              {"item": "チケット", "stage": "レビュー"},
		"control characters":     {"item": "a\x1b[31mb", "stage": "c\nd"},
		"herdr's longest values": {"item": long, "stage": long},
		"invalid UTF-8":          {"item": "a\xffb"},
	}
	for name, tokens := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			row := oneRow(t, pane("wN:p1", "", "claude", "working", tokens), "pee01")
			assert.Equal(t, tokens["item"], row.Item)
			assert.Equal(t, tokens["stage"], row.Stage)
			assert.Nil(t, row.StageStartedAt, "no clock: no start time")
		})
	}
}

// TestSnapshotRowTabAndIdle: a row carries pane.list's tab, the boss's and
// each worker's own, and, for an agent at rest, when the idle clock first saw it resting, in UTC (ADR-001
// A37). Working, blocked and unknown agents carry no rest, and neither does
// a plain shell, whatever status herdr gives it; with no clock nothing does.
func TestSnapshotRowTabAndIdle(t *testing.T) {
	t.Parallel()

	layout := teamOf([]herdr.PaneInfo{
		inTeam(pane("wJ:p1", "", "claude", "idle", nil), "boss"),
		inTeam(pane("wN:p1", "", "claude", "idle", nil), "coder-1"),
		inTeam(pane("wN:p2", "", "claude", "done", map[string]string{"handoff": "⟳ READY"}), "coder-2"),
		inTeam(pane("wN:p3", "", "claude", "working", nil), "coder-3"),
		inTeam(pane("wN:p4", "", "claude", "blocked", nil), "coder-4"),
		inTeam(pane("wN:p5", "", "", "idle", nil), "coder-5"),
		inTeam(pane("wN:p6", "", "codex", "unknown", nil), "coder-6"),
	})
	m := team.Build(layout, liveTeam, nil)
	rested := at.In(time.FixedZone("x", -7*3600))
	clock := &team.IdleClock{}
	clock.Observe(m, rested)
	clock.Observe(m, at.Add(time.Minute))

	s := team.Snapshot(m, liveProject, at.Add(time.Minute), nil, clock)
	require.NotNil(t, s.Boss)
	require.Len(t, s.Groups, 1)
	rows := append([]snapshot.Agent{s.Boss.Agent}, s.Groups[0].Agents...)
	want := map[string]struct {
		tab     string
		resting bool
	}{
		"wJ:p1": {"wT:boss", true}, "wN:p1": {"wT:coder-1", true}, "wN:p2": {"wT:coder-2", true},
		"wN:p3": {"wT:coder-3", false}, "wN:p4": {"wT:coder-4", false}, "wN:p5": {"wT:coder-5", false}, "wN:p6": {"wT:coder-6", false},
	}
	require.Len(t, rows, len(want))
	for _, r := range rows {
		w := want[r.PaneID]
		assert.Equal(t, w.tab, r.TabID, r.PaneID)
		if !w.resting {
			assert.Nil(t, r.IdleSince, r.PaneID)
			continue
		}
		require.NotNil(t, r.IdleSince, r.PaneID)
		assert.True(t, r.IdleSince.Equal(rested), r.PaneID)
		assert.Equal(t, time.UTC, r.IdleSince.Location(), r.PaneID)
	}

	for _, r := range team.Snapshot(m, liveProject, at, nil, nil).Groups[0].Agents {
		assert.Nil(t, r.IdleSince, "no clock: no rest, %s", r.PaneID)
	}
}

// TestSnapshotRowStage: a row carries its stage's stage_at, in UTC, and
// whether it is stale, by the idle clock's stretches (ADR-001 A42), the
// boss's included: here the boss and wN:p1 worked a turn that reported
// nothing, wN:p2 reported at its end, wN:p3's agent is gone and wN:p4 has
// no stage_at. With no clock no stage ages but the one whose agent is gone.
func TestSnapshotRowStage(t *testing.T) {
	t.Parallel()
	before := time.Unix(at.Unix()-3600, 0)
	after := time.Unix(at.Unix()+5, 0)
	layout := func(status string) team.Layout {
		tokens := func(reported time.Time) map[string]string {
			out := map[string]string{"stage": "build_test"}
			if !reported.IsZero() {
				out["stage_at"] = strconv.FormatInt(reported.Unix(), 10)
			}
			return out
		}
		l := teamOf([]herdr.PaneInfo{
			inTeam(pane("wJ:p1", "", "claude", status, tokens(before)), "boss"),
			inTeam(pane("wN:p1", "", "claude", status, tokens(before)), "coder-1"),
			inTeam(pane("wN:p2", "", "claude", status, tokens(after)), "coder-2"),
			inTeam(pane("wN:p3", "", "", "unknown", tokens(before)), "coder-3"),
			inTeam(pane("wN:p4", "", "claude", status, tokens(time.Time{})), "coder-4"),
		})
		l.Agents = []herdr.AgentInfo{{Name: "gone", PaneID: "wN:p3"}}
		return l
	}
	clock := &team.IdleClock{}
	var m team.Model
	for i, status := range []string{"idle", "working", "idle", "idle"} {
		m = team.Build(layout(status), liveTeam, nil)
		clock.Observe(m, at.Add(time.Duration(i)*3*time.Second))
	}
	now := at.Add(time.Minute)

	type state struct {
		at    *time.Time
		stale bool
	}
	rowsOf := func(s snapshot.Snapshot) map[string]state {
		require.NotNil(t, s.Boss)
		require.Len(t, s.Groups, 1)
		out := map[string]state{}
		for _, r := range append([]snapshot.Agent{s.Boss.Agent}, s.Groups[0].Agents...) {
			if r.StageAt != nil {
				assert.Equal(t, time.UTC, r.StageAt.Location(), r.PaneID)
			}
			out[r.PaneID] = state{r.StageAt, r.StageStale}
		}
		return out
	}
	utc := func(t time.Time) *time.Time { u := t.UTC(); return &u }
	assert.Equal(t, map[string]state{
		"wJ:p1": {utc(before), true}, "wN:p1": {utc(before), true}, "wN:p2": {utc(after), false},
		"wN:p3": {utc(before), true}, "wN:p4": {nil, false},
	}, rowsOf(team.Snapshot(m, liveProject, now, nil, clock)))
	assert.Equal(t, map[string]state{
		"wJ:p1": {utc(before), false}, "wN:p1": {utc(before), false}, "wN:p2": {utc(after), false},
		"wN:p3": {utc(before), true}, "wN:p4": {nil, false},
	}, rowsOf(team.Snapshot(m, liveProject, now, nil, nil)))
}

// typesLayout is the live team with one worker's tab per type,
// <short>-<type>-1, each with one agent, listed in the order given.
func typesLayout(types ...string) team.Layout {
	var panes []herdr.PaneInfo
	for i, typ := range types {
		panes = append(panes, inTeam(pane("w"+string(rune('a'+i))+":p1", "", "claude", "idle", nil), typ+"-1"))
	}
	return teamOf(panes)
}

func groupNames(s snapshot.Snapshot) []string {
	var out []string
	for _, g := range s.Groups {
		out = append(out, g.Name)
	}
	return out
}

func TestBossSnapshotOmitsMismatchedBindingAndOversizedStage(t *testing.T) {
	t.Parallel()
	pane := team.Pane{ID: "pane", TerminalID: "current", Stage: strings.Repeat("s", snapshot.MaxBossFieldBytes+1), StageAt: at}
	worker := team.Worker{ID: "al-coder-1", Bound: true, Pane: pane}
	project := team.Project{Name: "alpha", Short: "al", Groups: []team.Group{{Name: "coder", Workers: []team.Worker{worker}}}}
	rows := map[string]store.Worker{worker.ID: {Project: project.Name, TerminalID: "previous", Generation: 2}}
	multibyte := strings.Repeat("調", report.MaxValueChars)
	project.Groups[0].Workers = append(project.Groups[0].Workers, team.Worker{Pane: team.Pane{ID: "pane2", Stage: multibyte, StageAt: at}})
	workers := team.BossSnapshot(project, at, nil, nil, rows).Groups[0].Workers
	assert.Equal(t, &multibyte, workers[1].Stage, "a stage the reporter accepts keeps its value")
	assert.Equal(t, &at, workers[1].StageAt)
	got := workers[0]
	assert.Nil(t, got.ID)
	assert.Nil(t, got.Generation)
	assert.Nil(t, got.Stage)
	assert.Nil(t, got.StageAt)
	assert.False(t, got.StageStale)
}

// TestSnapshotGroupOrder: one group per resolver group, in the order the
// resolver gives, then the rest by name. A tabs team's resolver gives the
// team model's working order (team.TypeOrder); another resolver gives its
// own, and the builder hard-codes none. Names match exactly, as the
// resolver matches them.
func TestSnapshotGroupOrder(t *testing.T) {
	t.Parallel()

	types := []string{"coder", "qa", "precheck", "review", "research", "librarian", "debugger"}
	model := team.TypeOrder()
	tests := map[string]struct {
		groups []string
		order  []string
		want   []string
	}{
		"the team model's order":      {groups: []string{"librarian", "research", "review", "qa", "precheck", "coder"}, order: model, want: []string{"coder", "precheck", "qa", "review", "research", "librarian"}},
		"debugger after librarian":    {groups: []string{"debugger", "alpha", "librarian", "coder"}, order: model, want: []string{"coder", "librarian", "debugger", "alpha"}},
		"the rest follow by name":     {groups: []string{"zeta", "beta", "qa", "alpha"}, order: model, want: []string{"qa", "alpha", "beta", "zeta"}},
		"a single group":              {groups: []string{"research"}, order: model, want: []string{"research"}},
		"no worker tabs":              {groups: nil, order: model, want: nil},
		"a resolver's own order":      {groups: []string{"debugger", "coder", "librarian", "qa", "review", "research", "precheck"}, order: types, want: types},
		"no order: by name":           {groups: []string{"qa", "coder", "clerk"}, order: nil, want: []string{"clerk", "coder", "qa"}},
		"an order naming absent ones": {groups: []string{"research"}, order: types, want: []string{"research"}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m := team.Build(typesLayout(tc.groups...), liveTeam, nil)
			for i := range m.Projects {
				m.Projects[i].Order = tc.order
			}
			s := team.Snapshot(m, liveProject, at, nil, nil)
			assert.Equal(t, tc.want, groupNames(s))
			assert.NotNil(t, s.Groups, "groups is a list, never null")
		})
	}
	// A tab's type is lower case ASCII, so the exact match is shown on a
	// model built by hand: "Coder" is not coder.
	for name, tc := range map[string]struct{ groups, want []string }{
		"another case is another one": {groups: []string{"Coder", "coder", "QA"}, want: []string{"coder", "Coder", "QA"}},
		"non-ASCII names":             {groups: []string{"ü", "a", "coder"}, want: []string{"coder", "a", "ü"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			pr := team.Project{Name: liveProject, Order: model}
			for _, g := range tc.groups {
				pr.Groups = append(pr.Groups, team.Group{Name: g})
			}
			assert.Equal(t, tc.want, groupNames(team.Snapshot(team.Model{Projects: []team.Project{pr}}, liveProject, at, nil, nil)))
		})
	}
	t.Run("a tabs team's own order is the team model's", func(t *testing.T) {
		t.Parallel()
		m := team.Build(typesLayout("debugger", "zeta", "librarian", "qa", "review", "coder", "research", "precheck"), liveTeam, nil)
		s := team.Snapshot(m, liveProject, at, nil, nil)
		assert.Equal(t, append(team.TypeOrder(), "zeta"), groupNames(s))
	})
}

// TestSnapshotBoss: the boss is an agent pane in the boss's tab. The
// contract has one, so with several the first by pane id is it, and a shell
// there (this dashboard's own pane, say) never is. No agent in the boss's
// tab, or no boss's tab, leaves the boss out: nothing the daemon does not
// know is invented.
func TestSnapshotBoss(t *testing.T) {
	t.Parallel()

	boss := []herdr.TabInfo{liveTab("boss")}
	tests := map[string]struct {
		tabs   []herdr.TabInfo
		panes  []herdr.PaneInfo
		agents []herdr.AgentInfo
		want   *snapshot.Boss
	}{
		"one boss": {
			tabs: boss, panes: []herdr.PaneInfo{inTeam(pane("wJ:p1", "", "claude", "working", map[string]string{"ctx": "23", "item": "#867", "stage": "review"}), "boss")},
			agents: []herdr.AgentInfo{{PaneID: "wJ:p1", Name: "boss"}},
			want:   &snapshot.Boss{Agent: snapshot.Agent{Name: "boss", PaneID: "wJ:p1", TabID: "wT:boss", Status: snapshot.StatusWorking, Kind: "claude", Ctx: new(23), Item: "#867", Stage: "review"}},
		},
		"the first agent pane by pane id, past a shell": {
			tabs:   boss,
			panes:  []herdr.PaneInfo{inTeam(pane("wJ:p3", "", "claude", "idle", nil), "boss"), inTeam(pane("wJ:p1", "", "", "unknown", nil), "boss"), inTeam(pane("wJ:p2", "", "claude", "working", nil), "boss")},
			agents: []herdr.AgentInfo{{PaneID: "wJ:p2", Name: "boss"}, {PaneID: "wJ:p3", Name: "boss-next"}},
			want:   &snapshot.Boss{Agent: snapshot.Agent{Name: "boss", PaneID: "wJ:p2", TabID: "wT:boss", Status: snapshot.StatusWorking, Kind: "claude"}},
		},
		"an unnamed boss": {
			tabs: boss, panes: []herdr.PaneInfo{inTeam(pane("wJ:p1", "", "claude", "idle", nil), "boss")},
			want: &snapshot.Boss{Agent: snapshot.Agent{Name: "wJ:p1", Unnamed: true, PaneID: "wJ:p1", TabID: "wT:boss", Status: snapshot.StatusIdle, Kind: "claude"}},
		},
		"only shells in the boss's tab": {tabs: boss, panes: []herdr.PaneInfo{inTeam(pane("wJ:p1", "", "", "unknown", nil), "boss")}},
		"an empty boss's tab":           {tabs: boss},
		"no boss's tab":                 {tabs: []herdr.TabInfo{liveTab("coder-1")}, panes: []herdr.PaneInfo{inTeam(pane("wN:p1", "", "claude", "idle", nil), "coder-1")}},
		"the boss is in no group": {
			tabs: append(slices.Clone(boss), liveTab("coder-1")), panes: []herdr.PaneInfo{inTeam(pane("wJ:p1", "", "claude", "idle", nil), "boss")},
			want: &snapshot.Boss{Agent: snapshot.Agent{Name: "wJ:p1", Unnamed: true, PaneID: "wJ:p1", TabID: "wT:boss", Status: snapshot.StatusIdle, Kind: "claude"}},
		},
		"two boss's tabs, one boss": {
			tabs:   append(slices.Clone(boss), herdr.TabInfo{TabID: "wT:tK", WorkspaceID: liveWS, Label: liveShort + "-boss"}),
			panes:  []herdr.PaneInfo{inTeam(pane("wK:p1", "", "claude", "idle", nil), "tK"), inTeam(pane("wJ:p1", "", "claude", "working", nil), "boss")},
			agents: []herdr.AgentInfo{{PaneID: "wJ:p1", Name: "boss"}, {PaneID: "wK:p1", Name: "boss2"}},
			want:   &snapshot.Boss{Agent: snapshot.Agent{Name: "boss", PaneID: "wJ:p1", TabID: "wT:boss", Status: snapshot.StatusWorking, Kind: "claude"}},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			l := team.Layout{Workspaces: []herdr.WorkspaceInfo{{WorkspaceID: liveWS, Label: liveProject}}, Tabs: tc.tabs, Panes: tc.panes, Agents: tc.agents}
			s := team.Snapshot(team.Build(l, liveTeam, nil), liveProject, at, nil, nil)
			assert.Equal(t, tc.want, s.Boss)
			for _, g := range s.Groups {
				for _, a := range g.Agents {
					assert.NotEqual(t, "wJ", a.PaneID[:2], "a pane in the boss's tab is never a group's row")
				}
			}
		})
	}
}

// TestSnapshotProject: the team is the project, by name, and its colour,
// when configured, is the project's only reported state: PRs, issues and
// slots stay unreported (NERD-5257 collects them).
func TestSnapshotProject(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		color string
		want  *snapshot.Project
	}{
		"a project with a colour":    {color: "#7c3aed", want: &snapshot.Project{Color: "#7c3aed"}},
		"a project without a colour": {},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m := team.Build(typesLayout("coder"), team.Tabs{Project: liveProject, Short: liveShort, Color: tc.color}, nil)
			s := team.Snapshot(m, liveProject, at, nil, nil)
			assert.Equal(t, liveProject, s.Team)
			assert.Equal(t, tc.want, s.Project)
			assert.Equal(t, []string{"coder"}, groupNames(s))
		})
	}
	t.Run("another project's panes are not the team's", func(t *testing.T) {
		t.Parallel()
		m := team.Build(typesLayout("coder"), team.Tabs{Project: liveProject, Short: liveShort, Color: "#7c3aed"}, nil)
		s := team.Snapshot(m, "b", at, nil, nil)
		assert.Equal(t, "b", s.Team)
		assert.Empty(t, s.Groups)
		assert.Nil(t, s.Boss)
		assert.Nil(t, s.Project, "nor its colour")
	})
}

// TestSnapshotDecodesStrictly: whatever the model holds, the snapshot it
// gives encodes to a document snapshot.Decode accepts byte for byte: a
// worker's tab with no panes is a group with an empty agent list, never
// null, and no team is an empty group list.
func TestSnapshotDecodesStrictly(t *testing.T) {
	t.Parallel()

	full := teamOf([]herdr.PaneInfo{
		inTeam(pane("wJ:p1", "", "claude", "working", map[string]string{"ctx": "23", "stage": "review"}), "boss"),
		inTeam(pane("wN:p1", "", "claude", "working", map[string]string{"ctx": "40", "item": "NERD-1", "stage": "build"}), "coder-1"),
		inTeam(pane("wN:p2", "", "", "unknown", map[string]string{"ctx": "nope"}), "coder-2"),
		inTeam(herdr.PaneInfo{PaneID: "wN:p3", AgentStatus: "unknown", Label: "no\xfftes", Tokens: map[string]string{"item": "a\x00b", "stage": "c\xe2\x82"}}, "coder-3"),
		pane("w2:p1", "w2", "claude", "working", nil),
	}, "research-1")
	full.Workspaces = append(full.Workspaces, herdr.WorkspaceInfo{WorkspaceID: "w2", Label: "notes"})
	full.Agents = []herdr.AgentInfo{{PaneID: "wJ:p1", Name: "boss"}, {PaneID: "wN:p1", Name: "pee01"}}
	clock := &team.StageClock{}
	m := team.Build(full, team.Tabs{Project: "agentisan", Short: liveShort, Color: "#7c3aed"}, nil)
	clock.Observe(m, at)

	tests := map[string]snapshot.Snapshot{
		"a full team":      team.Snapshot(m, "agentisan", at, clock, nil),
		"no team":          team.Snapshot(team.Model{}, "", at, clock, nil),
		"a nil clock":      team.Snapshot(m, "agentisan", at, nil, nil),
		"a local-time now": team.Snapshot(m, "agentisan", at.In(time.FixedZone("x", -7*3600)), clock, nil),
	}
	for name, s := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			raw, err := json.Marshal(s)
			require.NoError(t, err)
			got, err := snapshot.Decode(bytes.NewReader(raw))
			require.NoError(t, err, "%s", raw)
			assert.Equal(t, snapshot.Version, got.V)
			assert.True(t, got.At.Equal(at))
			assert.NotContains(t, string(raw), `"groups":null`)
			assert.NotContains(t, string(raw), `"agents":null`)
		})
	}

	raw, err := json.Marshal(tests["a full team"])
	require.NoError(t, err)
	assert.Contains(t, string(raw), `{"name":"research","agents":[]}`, "an empty worker's tab is an empty list")
	assert.Contains(t, string(raw), `"at":"2026-10-10T09:30:00Z"`, "times are UTC")
}

// TestStageClock: stage_started_at is when the daemon first saw a pane's
// current stage (ADR-001 A18), keyed by pane id and value. The same value
// keeps its start, however many polls pass; a new value starts again; and a
// pane that drops its stage, or goes, is forgotten, so the same value later
// starts again. Held in memory only: a new clock, as after a daemon restart,
// starts fresh.
func TestStageClock(t *testing.T) {
	t.Parallel()

	l := func(stages map[string]string) team.Model {
		var panes []herdr.PaneInfo
		for _, id := range slices.Sorted(maps.Keys(stages)) {
			var tokens map[string]string
			if v := stages[id]; v != "-" {
				tokens = map[string]string{"stage": v}
			}
			tab := "coder-1" // wN's panes are a coder's, wJ's the boss's
			if id[:2] == "wJ" {
				tab = "boss"
			}
			panes = append(panes, inTeam(pane(id, "", "claude", "working", tokens), tab))
		}
		return team.Build(teamOf(panes), liveTeam, nil)
	}
	t0, t1, t2, t3 := at, at.Add(3*time.Second), at.Add(6*time.Second), at.Add(9*time.Second)
	since := func(c *team.StageClock, id, stage string) time.Time {
		got, ok := c.Since(id, stage)
		if !ok {
			return time.Time{}
		}
		return got
	}

	t.Run("the same value keeps its start", func(t *testing.T) {
		t.Parallel()
		c := &team.StageClock{}
		c.Observe(l(map[string]string{"wN:p1": "build"}), t0)
		c.Observe(l(map[string]string{"wN:p1": "build"}), t1)
		c.Observe(l(map[string]string{"wN:p1": "build"}), t2)
		assert.Equal(t, t0, since(c, "wN:p1", "build"))
	})
	t.Run("a new value resets it", func(t *testing.T) {
		t.Parallel()
		c := &team.StageClock{}
		c.Observe(l(map[string]string{"wN:p1": "build"}), t0)
		c.Observe(l(map[string]string{"wN:p1": "review"}), t1)
		assert.Equal(t, t1, since(c, "wN:p1", "review"))
		_, ok := c.Since("wN:p1", "build")
		assert.False(t, ok, "the old value is gone")
	})
	t.Run("going back to an earlier value starts again", func(t *testing.T) {
		t.Parallel()
		c := &team.StageClock{}
		c.Observe(l(map[string]string{"wN:p1": "build"}), t0)
		c.Observe(l(map[string]string{"wN:p1": "review"}), t1)
		c.Observe(l(map[string]string{"wN:p1": "build"}), t2)
		assert.Equal(t, t2, since(c, "wN:p1", "build"))
	})
	t.Run("a dropped stage is forgotten", func(t *testing.T) {
		t.Parallel()
		c := &team.StageClock{}
		c.Observe(l(map[string]string{"wN:p1": "build"}), t0)
		c.Observe(l(map[string]string{"wN:p1": "-"}), t1)
		_, ok := c.Since("wN:p1", "build")
		assert.False(t, ok)
		c.Observe(l(map[string]string{"wN:p1": "build"}), t2)
		assert.Equal(t, t2, since(c, "wN:p1", "build"))
	})
	t.Run("a pane that goes is forgotten", func(t *testing.T) {
		t.Parallel()
		c := &team.StageClock{}
		c.Observe(l(map[string]string{"wN:p1": "build", "wN:p2": "build"}), t0)
		c.Observe(l(map[string]string{"wN:p2": "build"}), t1)
		c.Observe(l(map[string]string{"wN:p1": "build", "wN:p2": "build"}), t2)
		assert.Equal(t, t2, since(c, "wN:p1", "build"), "back, it starts again")
		assert.Equal(t, t0, since(c, "wN:p2", "build"), "the other pane keeps its start")
	})
	t.Run("panes are timed apart, the boss's too", func(t *testing.T) {
		t.Parallel()
		c := &team.StageClock{}
		c.Observe(l(map[string]string{"wJ:p1": "plan"}), t0)
		c.Observe(l(map[string]string{"wJ:p1": "plan", "wN:p1": "plan"}), t3)
		assert.Equal(t, t0, since(c, "wJ:p1", "plan"))
		assert.Equal(t, t3, since(c, "wN:p1", "plan"))
	})
	t.Run("an empty stage has no start", func(t *testing.T) {
		t.Parallel()
		c := &team.StageClock{}
		c.Observe(l(map[string]string{"wN:p1": ""}), t0)
		_, ok := c.Since("wN:p1", "")
		assert.False(t, ok)
	})
	t.Run("a restart starts fresh", func(t *testing.T) {
		t.Parallel()
		before := &team.StageClock{}
		before.Observe(l(map[string]string{"wN:p1": "build"}), t0)
		after := &team.StageClock{}
		after.Observe(l(map[string]string{"wN:p1": "build"}), t3)
		assert.Equal(t, t3, since(after, "wN:p1", "build"))
	})
	t.Run("the snapshot carries it, in UTC", func(t *testing.T) {
		t.Parallel()
		c := &team.StageClock{}
		local := t0.In(time.FixedZone("x", 5*3600))
		m := l(map[string]string{"wN:p1": "build", "wJ:p1": "plan"})
		c.Observe(m, local)
		c.Observe(m, t2)
		s := team.Snapshot(m, liveProject, t2, c, nil)
		require.NotNil(t, s.Boss)
		require.NotNil(t, s.Boss.StageStartedAt)
		assert.Equal(t, t0, *s.Boss.StageStartedAt)
		assert.Equal(t, time.UTC, s.Boss.StageStartedAt.Location())
		require.NotNil(t, s.Groups[0].Agents[0].StageStartedAt)
		assert.Equal(t, t0, *s.Groups[0].Agents[0].StageStartedAt)
	})
	t.Run("a nil clock observes nothing", func(t *testing.T) {
		t.Parallel()
		var c *team.StageClock
		c.Observe(l(map[string]string{"wN:p1": "build"}), t0)
		_, ok := c.Since("wN:p1", "build")
		assert.False(t, ok)
	})
}

// TestSnapshotLeavesTheBossRuntimeOut: the model's snapshot names the boss
// but leaves its started_at out, however long the stage clock has watched
// its pane: the daemon fills it from the boss agent's process (A47), so a
// daemon restart cannot start it again.
func TestSnapshotLeavesTheBossRuntimeOut(t *testing.T) {
	t.Parallel()
	m := team.Build(teamOf([]herdr.PaneInfo{
		inTeam(pane("wJ:p1", "", "claude", "working", map[string]string{"stage": "plan"}), "boss"),
	}), liveTeam, nil)
	c := &team.StageClock{}
	c.Observe(m, at)
	c.Observe(m, at.Add(time.Hour))
	s := team.Snapshot(m, liveProject, at.Add(time.Hour), c, nil)
	require.NotNil(t, s.Boss)
	assert.Equal(t, "wJ:p1", s.Boss.PaneID)
	require.NotNil(t, s.Boss.StageStartedAt, "the stage clock still times its stage")
	assert.Equal(t, at, *s.Boss.StageStartedAt)
	assert.Nil(t, s.Boss.StartedAt)
}
