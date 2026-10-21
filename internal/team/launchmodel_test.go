package team_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/snapshot"
	"github.com/abanna/herdr-agentisan/internal/team"
)

// TestLaunchModel walks the matcher (ADR-001 A39): which process is the
// agent, which flags name its model, and which values a row may carry.
func TestLaunchModel(t *testing.T) {
	t.Parallel()
	proc := func(pid uint32, argv ...string) herdr.Process {
		return herdr.Process{PID: pid, Name: "x", Argv: argv}
	}
	claude := func(args ...string) []herdr.Process {
		return []herdr.Process{proc(10, append([]string{"/home/u/.local/bin/claude"}, args...)...)}
	}
	codex := func(args ...string) []herdr.Process {
		return []herdr.Process{proc(20, append([]string{"codex"}, args...)...)}
	}
	type want struct {
		model string
		pid   uint32
		found bool
	}
	tests := map[string]struct {
		procs []herdr.Process
		want  want
	}{
		// Claude: --model X or --model=X, whatever else it was started with.
		"claude --model X":             {procs: claude("--model", "claude-opus-5-5", "--name", "ha-qa-1"), want: want{"claude-opus-5-5", 10, true}},
		"claude --model=X":             {procs: claude("--name", "a", "--model=claude-opus-5-5"), want: want{"claude-opus-5-5", 10, true}},
		"claude with an alias":         {procs: claude("--model", "opus"), want: want{"opus", 10, true}},
		"claude by a relative name":    {procs: []herdr.Process{proc(11, "claude", "--model", "sonnet")}, want: want{"sonnet", 11, true}},
		"claude with no flags":         {procs: claude(), want: want{"", 10, true}},
		"claude: the last flag wins":   {procs: claude("--model", "a", "--model=b"), want: want{"b", 10, true}},
		"claude: an invalid last flag": {procs: claude("--model", "a", "--model="), want: want{"", 10, true}},
		"claude: nothing after --":     {procs: claude("--", "--model", "x"), want: want{"", 10, true}},
		"claude: before -- only":       {procs: claude("--model", "a", "--", "--model", "b"), want: want{"a", 10, true}},
		"claude: a flag with no value": {procs: claude("--model"), want: want{"", 10, true}},
		"claude: an empty value":       {procs: claude("--model", ""), want: want{"", 10, true}},
		"claude: an empty = value":     {procs: claude("--model="), want: want{"", 10, true}},
		"claude: a value led by -":     {procs: claude("--model", "--name", "x"), want: want{"", 10, true}},
		"claude: a lone - value":       {procs: claude("--model=-"), want: want{"", 10, true}},
		"claude has no -m":             {procs: claude("-m", "opus"), want: want{"", 10, true}},
		"claude: --fallback-model":     {procs: claude("--fallback-model", "sonnet"), want: want{"", 10, true}},
		"claude: a longer flag":        {procs: claude("--modelx=y", "--model-x", "y"), want: want{"", 10, true}},
		"claude in a prompt":           {procs: claude("use --model opus"), want: want{"", 10, true}},
		"another program named like claude": {
			procs: []herdr.Process{proc(12, "/bin/claude-code", "--model", "x"), proc(13, "xclaude", "--model", "x")},
		},
		// Codex: those, and -m X, -mX or -m=X; the node launcher's flags
		// start after its script.
		"codex -m X":                    {procs: codex("-m", "gpt-5.5"), want: want{"gpt-5.5", 20, true}},
		"codex -mX":                     {procs: codex("-mgpt-5.5"), want: want{"gpt-5.5", 20, true}},
		"codex -m=X":                    {procs: codex("-m=gpt-5.5"), want: want{"gpt-5.5", 20, true}},
		"codex --model X":               {procs: codex("--model", "o3"), want: want{"o3", 20, true}},
		"codex --model=X":               {procs: codex("--model=o3"), want: want{"o3", 20, true}},
		"codex under exec":              {procs: codex("exec", "-m", "o3", "fix it"), want: want{"o3", 20, true}},
		"codex with no -m":              {procs: codex("--no-daemon"), want: want{"", 20, true}},
		"codex: -m with none":           {procs: codex("-m"), want: want{"", 20, true}},
		"codex: -m=":                    {procs: codex("-m="), want: want{"", 20, true}},
		"codex: -m -":                   {procs: codex("-m", "-"), want: want{"", 20, true}},
		"codex: -m-x":                   {procs: codex("-m-x"), want: want{"", 20, true}},
		"codex: -c model= is not read":  {procs: codex("-c", "model=o3", "--profile", "fast"), want: want{"", 20, true}},
		"codex: --model-provider":       {procs: codex("--model-provider", "oss"), want: want{"", 20, true}},
		"codex: last wins across forms": {procs: codex("-m", "a", "--model=b", "-mc"), want: want{"c", 20, true}},
		"codex: nothing after --":       {procs: codex("--", "-m", "o3"), want: want{"", 20, true}},
		"codex native by path":          {procs: []herdr.Process{proc(21, "/x/vendor/bin/codex", "-m", "o3")}, want: want{"o3", 21, true}},
		"the node launcher":             {procs: []herdr.Process{proc(22, "node", "/x/bin/codex", "-m", "o3")}, want: want{"o3", 22, true}},
		"the node launcher's codex.js":  {procs: []herdr.Process{proc(23, "/usr/bin/node", "codex.js", "--model=o3")}, want: want{"o3", 23, true}},
		"the launcher's script is no flag": {
			procs: []herdr.Process{proc(24, "node", "/x/bin/codex")}, want: want{"", 24, true},
		},
		"node running something else": {procs: []herdr.Process{proc(25, "node", "/x/cli.js", "--model", "x")}},
		"node alone":                  {procs: []herdr.Process{proc(26, "node")}},
		// Which process: the first either rule accepts, in herdr's order.
		"the launcher before its child": {
			procs: []herdr.Process{proc(30, "node", "/x/bin/codex", "-m", "o3"), proc(31, "/x/vendor/codex", "-m", "o3")},
			want:  want{"o3", 30, true},
		},
		"a shell, then the agent": {
			procs: []herdr.Process{proc(32, "/bin/zsh"), proc(33, "claude", "--model", "opus")},
			want:  want{"opus", 33, true},
		},
		"the first agent, even with no model": {
			procs: []herdr.Process{proc(34, "claude"), proc(35, "codex", "-m", "o3")},
			want:  want{"", 34, true},
		},
		"argv herdr could not read":   {procs: []herdr.Process{{PID: 36, Name: "claude"}}},
		"an empty argv":               {procs: []herdr.Process{{PID: 37, Name: "claude", Argv: []string{}}}},
		"an empty argv[0]":            {procs: []herdr.Process{proc(38, "", "--model", "x")}},
		"no processes":                {},
		"a pid herdr reports as null": {procs: []herdr.Process{proc(0, "claude", "--model", "x")}, want: want{"x", 0, true}},
		// The value: printable UTF-8 of at most 80 code points.
		"80 ASCII code points":     {procs: claude("--model", strings.Repeat("a", 80)), want: want{strings.Repeat("a", 80), 10, true}},
		"81 ASCII code points":     {procs: claude("--model", strings.Repeat("a", 81)), want: want{"", 10, true}},
		"80 non-ASCII code points": {procs: claude("--model", strings.Repeat("\u00e9", 80)), want: want{strings.Repeat("\u00e9", 80), 10, true}},
		"81 non-ASCII code points": {procs: claude("--model", strings.Repeat("\u00e9", 81)), want: want{"", 10, true}},
		"a space":                  {procs: claude("--model", "Opus 5"), want: want{"Opus 5", 10, true}},
		"a replacement character":  {procs: claude("--model", "opus\ufffd"), want: want{"", 10, true}},
		"invalid UTF-8":            {procs: claude("--model", "opus\xff"), want: want{"", 10, true}},
		"a tab":                    {procs: claude("--model", "a\tb"), want: want{"", 10, true}},
		"a newline":                {procs: claude("--model", "a\nb"), want: want{"", 10, true}},
		"an escape sequence":       {procs: claude("--model", "\x1b[31mopus"), want: want{"", 10, true}},
		"a NUL":                    {procs: claude("--model", "a\x00b"), want: want{"", 10, true}},
		"a zero-width space":       {procs: claude("--model", "op\u200bus"), want: want{"", 10, true}},
		"a no-break space":         {procs: claude("--model", "op\u00a0us"), want: want{"", 10, true}},
		"a bidi override":          {procs: claude("--model", "\u202eopus"), want: want{"", 10, true}},
		"a line separator":         {procs: claude("--model", "a\u2028b"), want: want{"", 10, true}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			model, pid, found := team.LaunchModel(tc.procs)
			assert.Equal(t, tc.want, want{model, pid, found})
		})
	}
}

// TestFillModels: every row, the boss's too, gets its pane's model; a row
// whose pane has none gets none, and an empty map or no boss is no matter.
func TestFillModels(t *testing.T) {
	t.Parallel()
	s := func() *snapshot.Snapshot {
		return &snapshot.Snapshot{
			Boss: &snapshot.Boss{Agent: snapshot.Agent{Name: "boss", PaneID: "wJ:p1"}},
			Groups: []snapshot.Group{
				{Name: "coder", Agents: []snapshot.Agent{{Name: "a", PaneID: "w1:p1"}, {Name: "b", PaneID: "w1:p2"}}},
				{Name: "qa", Agents: []snapshot.Agent{{Name: "c", PaneID: "w1:p3"}}},
				{Name: "empty", Agents: []snapshot.Agent{}},
			},
		}
	}
	got := s()
	team.FillModels(got, map[string]string{"wJ:p1": "opus", "w1:p1": "claude-opus-5-5", "w1:p3": "o3", "w9:p9": "elsewhere"})
	want := s()
	want.Boss.Agent.Model = "opus"
	want.Groups[0].Agents[0].Model = "claude-opus-5-5"
	want.Groups[1].Agents[0].Model = "o3"
	assert.Equal(t, want, got)

	none := s()
	team.FillModels(none, nil)
	assert.Equal(t, s(), none, "no models: no row has one")

	noBoss := s()
	noBoss.Boss = nil
	team.FillModels(noBoss, map[string]string{"w1:p2": "sonnet"})
	assert.Nil(t, noBoss.Boss)
	assert.Equal(t, "sonnet", noBoss.Groups[0].Agents[1].Model)
}
