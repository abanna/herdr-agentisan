package snapshot_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/snapshot"
)

const valid = `{
  "v": 1,
  "at": "2026-10-09T17:19:00Z",
  "team": "agentisan",
  "boss": {"name": "boss", "pane_id": "w1:p1", "status": "working", "model": "opus", "ctx": 23, "item": "#867 release", "stage": "review"},
  "groups": [
    {"name": "coders", "description": "write code", "agents": [
      {"name": "pee01", "pane_id": "w2:p1", "status": "working", "model": "claude-opus-4-1", "ctx": 38, "item": "r2", "stage": "J1 re-run"},
      {"name": "pee02", "pane_id": "w2:p2", "status": "idle", "model": "opus"}
    ]},
    {"name": "qa", "agents": []}
  ]
}`

// FileSource is what the dashboard polls; this fails to compile if it ever
// stops satisfying the interface.
var _ snapshot.Source = snapshot.FileSource{}

func TestDecodeReadsTheContract(t *testing.T) {
	t.Parallel()

	got, err := snapshot.Decode(strings.NewReader(valid))
	require.NoError(t, err)

	want := snapshot.Snapshot{
		V:    snapshot.Version,
		At:   time.Date(2026, 10, 9, 17, 19, 0, 0, time.UTC),
		Team: "agentisan",
		Boss: &snapshot.Agent{
			Name: "boss", PaneID: "w1:p1", Status: snapshot.StatusWorking, Model: "opus",
			Ctx: new(23), Item: "#867 release", Stage: "review",
		},
		Groups: []snapshot.Group{
			{Name: "coders", Description: "write code", Agents: []snapshot.Agent{
				{Name: "pee01", PaneID: "w2:p1", Status: snapshot.StatusWorking, Model: "claude-opus-4-1", Ctx: new(38), Item: "r2", Stage: "J1 re-run"},
				{Name: "pee02", PaneID: "w2:p2", Status: snapshot.StatusIdle, Model: "opus"},
			}},
			{Name: "qa", Agents: []snapshot.Agent{}},
		},
	}
	assert.True(t, want.At.Equal(got.At))
	got.At = want.At
	assert.Equal(t, want, got)
}

// TestDecodeAcceptsEveryContractValue walks the edges the contract allows: each
// status, ctx at both ends of its range, a missing ctx and a snapshot with no
// boss and no groups.
func TestDecodeAcceptsEveryContractValue(t *testing.T) {
	t.Parallel()

	agent := func(fields string) string {
		return `{"v":1,"team":"t","groups":[{"name":"g","agents":[{"name":"a","pane_id":"w1:p1","model":"opus",` + fields + `}]}]}`
	}
	tests := map[string]string{
		"working":         agent(`"status":"working"`),
		"idle":            agent(`"status":"idle"`),
		"blocked":         agent(`"status":"blocked"`),
		"done":            agent(`"status":"done"`),
		"ready":           agent(`"status":"ready"`),
		"unknown":         agent(`"status":"unknown"`),
		"ctx 0":           agent(`"status":"idle","ctx":0`),
		"ctx 100":         agent(`"status":"idle","ctx":100`),
		"ctx missing":     agent(`"status":"idle"`),
		"no boss, groups": `{"v":1,"team":"t","groups":[]}`,
		"groups omitted":  `{"v":1,"team":"t"}`,
		// JSON whitespace includes CR: a writer on another platform frames
		// the same document.
		"CRLF framing and a final CRLF": "{\"v\":1,\r\n\"team\":\"t\",\r\n\"groups\":[]}\r\n",
		"a final newline":               "{\"v\":1,\"team\":\"t\",\"groups\":[]}\n",
		"many small agents":             manyAgents(2000),
		"exactly MaxBytes":              padTo(`{"v":1,"team":"t","groups":[]}`, snapshot.MaxBytes),
	}
	for name, in := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := snapshot.Decode(strings.NewReader(in))
			require.NoError(t, err)
		})
	}
}

// TestDecodeRefuses: the contract is strict. A version this build does not
// speak is ErrUnsupportedVersion, even when it carries fields v1 lacks, and
// everything else that breaks the contract is ErrInvalid.
func TestDecodeRefuses(t *testing.T) {
	t.Parallel()

	agent := func(fields string) string {
		return `{"v":1,"team":"t","groups":[{"name":"g","agents":[{` + fields + `}]}]}`
	}
	tests := map[string]struct {
		in       string
		want     error
		contains []string // the error names what broke the contract
	}{
		"v missing":                        {in: `{"team":"t","groups":[]}`, want: snapshot.ErrUnsupportedVersion},
		"v 0":                              {in: `{"v":0,"team":"t","groups":[]}`, want: snapshot.ErrUnsupportedVersion},
		"v 2":                              {in: `{"v":2,"team":"t","groups":[]}`, want: snapshot.ErrUnsupportedVersion},
		"v 2 with a field v1 lacks":        {in: `{"v":2,"team":"t","groups":[],"projects":[]}`, want: snapshot.ErrUnsupportedVersion},
		"v is a string":                    {in: `{"v":"1","team":"t","groups":[]}`, want: snapshot.ErrInvalid},
		"unknown top-level field":          {in: `{"v":1,"team":"t","groups":[],"extra":1}`, want: snapshot.ErrInvalid},
		"unknown agent field":              {in: agent(`"name":"a","status":"idle","colour":"red"`), want: snapshot.ErrInvalid},
		"unknown status":                   {in: agent(`"name":"a","status":"asleep"`), want: snapshot.ErrInvalid},
		"empty status":                     {in: agent(`"name":"a","status":""`), want: snapshot.ErrInvalid},
		"status missing":                   {in: agent(`"name":"a"`), want: snapshot.ErrInvalid},
		"ctx -1":                           {in: agent(`"name":"a","status":"idle","ctx":-1`), want: snapshot.ErrInvalid},
		"ctx 101":                          {in: agent(`"name":"a","status":"idle","ctx":101`), want: snapshot.ErrInvalid},
		"ctx fractional":                   {in: agent(`"name":"a","status":"idle","ctx":42.5`), want: snapshot.ErrInvalid},
		"empty agent name":                 {in: agent(`"name":"","status":"idle"`), want: snapshot.ErrInvalid},
		"agent name missing":               {in: agent(`"status":"idle"`), want: snapshot.ErrInvalid},
		"boss with an empty name":          {in: `{"v":1,"team":"t","boss":{"name":"","status":"idle"},"groups":[]}`, want: snapshot.ErrInvalid},
		"boss ctx out of range":            {in: `{"v":1,"team":"t","boss":{"name":"boss","status":"idle","ctx":140},"groups":[]}`, want: snapshot.ErrInvalid},
		"boss with an unknown status":      {in: `{"v":1,"team":"t","boss":{"name":"boss","status":"zzz"},"groups":[]}`, want: snapshot.ErrInvalid},
		"empty group name":                 {in: `{"v":1,"team":"t","groups":[{"name":"","agents":[]}]}`, want: snapshot.ErrInvalid},
		"not JSON":                         {in: `not json`, want: snapshot.ErrInvalid},
		"empty input":                      {in: ``, want: snapshot.ErrInvalid},
		"an empty object":                  {in: `{}`, want: snapshot.ErrUnsupportedVersion},
		"an array, not an object":          {in: `[1]`, want: snapshot.ErrInvalid},
		"trailing data":                    {in: `{"v":1,"team":"t","groups":[]} {"v":1}`, want: snapshot.ErrInvalid},
		"trailing closing brace":           {in: `{"v":1,"team":"t","groups":[]}}`, want: snapshot.ErrInvalid},
		"larger than the snapshot ceiling": {in: `{"v":1,"team":"` + strings.Repeat("x", snapshot.MaxBytes) + `","groups":[]}`, want: snapshot.ErrInvalid},
		"one byte past MaxBytes":           {in: padTo(`{"v":1,"team":"t","groups":[]}`, snapshot.MaxBytes+1), want: snapshot.ErrInvalid, contains: []string{"larger than"}},
		// encoding/json matches keys case-insensitively and keeps the last of
		// a duplicate; the contract is exact about both.
		"v in another case":            {in: `{"V":1,"team":"t","groups":[]}`, want: snapshot.ErrInvalid, contains: []string{`"V"`}},
		"a key in another case":        {in: `{"v":1,"TEAM":"t","groups":[]}`, want: snapshot.ErrInvalid, contains: []string{`"TEAM"`}},
		"an agent key in another case": {in: agent(`"name":"a","Status":"idle"`), want: snapshot.ErrInvalid, contains: []string{`"Status"`}},
		"a duplicate key":              {in: `{"v":1,"team":"a","team":"b","groups":[]}`, want: snapshot.ErrInvalid, contains: []string{`"team"`, "twice"}},
		"a duplicate v":                {in: `{"v":1,"v":1,"team":"t","groups":[]}`, want: snapshot.ErrInvalid, contains: []string{`"v"`, "twice"}},
		"a duplicate agent key":        {in: agent(`"name":"a","status":"idle","name":"b"`), want: snapshot.ErrInvalid, contains: []string{`"name"`, "twice"}},
		"status in another case":       {in: agent(`"name":"a","status":"Idle"`), want: snapshot.ErrInvalid, contains: []string{`"Idle"`}},
		"v negative":                   {in: `{"v":-1}`, want: snapshot.ErrUnsupportedVersion},
		"v past int64":                 {in: `{"v":99999999999999999999,"team":"t","groups":[]}`, want: snapshot.ErrInvalid},
		"v not an integer":             {in: `{"v":1.0,"team":"t","groups":[]}`, want: snapshot.ErrInvalid},
		"ctx past int64":               {in: agent(`"name":"a","status":"idle","ctx":1e30`), want: snapshot.ErrInvalid},
		"a byte-order mark":            {in: "\xef\xbb\xbf" + `{"v":1,"team":"t","groups":[]}`, want: snapshot.ErrInvalid},
		"a UTF-16 document":            {in: utf16le(`{"v":1,"team":"t","groups":[]}`), want: snapshot.ErrInvalid},
		"a raw NUL in a string":        {in: "{\"v\":1,\"team\":\"t\x00\",\"groups\":[]}", want: snapshot.ErrInvalid},
		// encoding/json refuses nesting past 10,000 levels; the object itself
		// is one, so 9,999 arrays reach the limit and 10,000 pass it. Either
		// way the value is no team name.
		"nesting at the JSON depth limit": {in: nested(9999), want: snapshot.ErrInvalid},
		"nesting one level past it":       {in: nested(10000), want: snapshot.ErrInvalid, contains: []string{"depth"}},
		"the offending agent is named": {
			in:   `{"v":1,"team":"t","groups":[{"name":"coders","agents":[{"name":"pee07","status":"idle","ctx":180}]}]}`,
			want: snapshot.ErrInvalid, contains: []string{"coders", "pee07", "180"},
		},
		"the version is named": {in: `{"v":3}`, want: snapshot.ErrUnsupportedVersion, contains: []string{"v 3", "v1"}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := snapshot.Decode(strings.NewReader(tc.in))
			require.ErrorIs(t, err, tc.want)
			for _, s := range tc.contains {
				assert.ErrorContains(t, err, s)
			}
		})
	}
}

// manyAgents is a valid snapshot of n small agents in one group.
func manyAgents(n int) string {
	var b strings.Builder
	b.WriteString(`{"v":1,"team":"t","groups":[{"name":"g","agents":[`)
	for i := range n {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"name":"a%d","status":"idle"}`, i)
	}
	b.WriteString(`]}]}`)
	return b.String()
}

// padTo pads doc with trailing JSON whitespace to exactly n bytes.
func padTo(doc string, n int) string {
	return doc + strings.Repeat(" ", n-len(doc))
}

// utf16le is s encoded as UTF-16, little-endian, without a byte-order mark.
func utf16le(s string) string {
	var b strings.Builder
	for _, u := range utf16.Encode([]rune(s)) {
		b.WriteByte(byte(u))
		b.WriteByte(byte(u >> 8))
	}
	return b.String()
}

// nested is a snapshot whose team is depth nested empty arrays.
func nested(depth int) string {
	return `{"v":1,"team":` + strings.Repeat("[", depth) + strings.Repeat("]", depth) + `,"groups":[]}`
}

// TestDecodeText: string values (the team, group names, agent names) arrive
// as the text the reader holds. Bytes that are not UTF-8 become U+FFFD;
// control characters are kept, for the renderer to neutralise.
func TestDecodeText(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		literal string // the team value, as JSON
		want    string
	}{
		"non-ASCII text":                     {literal: `"日本語 ✔"`, want: "日本語 ✔"},
		"invalid UTF-8 becomes U+FFFD":       {literal: "\"a\xffb\"", want: "a\ufffdb"},
		"a truncated multibyte sequence":     {literal: "\"a\xe2\x82\"", want: "a\ufffd\ufffd"},
		"Latin-1 bytes become U+FFFD":        {literal: "\"caf\xe9\"", want: "caf\ufffd"},
		"mixed valid and invalid bytes":      {literal: "\"é\xff日\"", want: "é\ufffd日"},
		"an escaped lone surrogate":          {literal: `"a\ud800b"`, want: "a\ufffdb"},
		"an escaped NUL is kept":             {literal: `"a\u0000b"`, want: "a\x00b"},
		"a newline and a tab are kept":       {literal: `"a\nb\tc"`, want: "a\nb\tc"},
		"an escape sequence is kept as text": {literal: `"a\u001b[2Jb"`, want: "a\x1b[2Jb"},
		"a byte-order mark inside is kept":   {literal: "\"\xef\xbb\xbfa\"", want: "\ufeffa"},
		"a decomposed name is kept":          {literal: `"cafe\u0301"`, want: "cafe\u0301"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			// The same literal as the team, a group's name and an agent's
			// name: every string field goes through the same decoder, and
			// Validate accepts each spelling as a name.
			l := tc.literal
			got, err := snapshot.Decode(strings.NewReader(
				`{"v":1,"team":` + l + `,"groups":[{"name":` + l + `,"agents":[{"name":` + l + `,"status":"idle"}]}]}`))
			require.NoError(t, err)
			assert.Equal(t, tc.want, got.Team)
			require.Len(t, got.Groups, 1)
			assert.Equal(t, tc.want, got.Groups[0].Name)
			require.Len(t, got.Groups[0].Agents, 1)
			assert.Equal(t, tc.want, got.Groups[0].Agents[0].Name)
		})
	}
}

func TestStatusValid(t *testing.T) {
	t.Parallel()

	tests := map[snapshot.Status]bool{
		snapshot.StatusWorking: true,
		snapshot.StatusIdle:    true,
		snapshot.StatusBlocked: true,
		snapshot.StatusDone:    true,
		snapshot.StatusReady:   true,
		snapshot.StatusUnknown: true,
		"":                     false,
		"Working":              false,
		"asleep":               false,
	}
	for status, want := range tests {
		t.Run(string(status), func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, want, status.Valid())
		})
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
}

// TestFileSourceRereadsOnEveryCall: the fixture source stands in for the
// daemon, so an edited file must show up on the next poll without a restart.
func TestFileSourceRereadsOnEveryCall(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "snap.json")
	src := snapshot.FileSource{Path: path}

	writeFile(t, path, `{"v":1,"team":"first","groups":[]}`)
	got, err := src.Snapshot(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "first", got.Team)

	writeFile(t, path, `{"v":1,"team":"second","groups":[]}`)
	got, err = src.Snapshot(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "second", got.Team)
}

func TestFileSourceFailures(t *testing.T) {
	t.Parallel()

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	tests := map[string]struct {
		body string // written to the file unless empty
		ctx  context.Context
		want error
	}{
		"missing file":      {ctx: context.Background(), want: snapshot.ErrUnavailable},
		"invalid contents":  {body: `{"v":1,"team":"t","groups":[],"x":1}`, ctx: context.Background(), want: snapshot.ErrInvalid},
		"unsupported":       {body: `{"v":9}`, ctx: context.Background(), want: snapshot.ErrUnsupportedVersion},
		"cancelled context": {body: `{"v":1,"team":"t","groups":[]}`, ctx: cancelled, want: context.Canceled},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "snap.json")
			if tc.body != "" {
				writeFile(t, path, tc.body)
			}
			_, err := snapshot.FileSource{Path: path}.Snapshot(tc.ctx)
			require.ErrorIs(t, err, tc.want)
		})
	}
}
