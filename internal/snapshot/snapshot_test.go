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
  "project": {"color": "#bd93f9", "prs": [
    {"number": 878, "ci": "pending", "codex": "pending", "merge": "blocked"},
    {"number": 886, "ci": "success", "codex": "approved", "merge": "clean"}
  ], "issues": 14, "slots": {"used": 1, "total": 2}},
  "boss": {"name": "boss", "pane_id": "w1:p1", "status": "working", "model": "opus", "ctx": 23, "item": "#867 release", "stage": "review",
           "stage_started_at": "2026-10-09T17:07:00Z", "watchers": 4, "started_at": "2026-10-09T15:05:00Z"},
  "groups": [
    {"name": "coders", "description": "write code", "agents": [
      {"name": "pee01", "pane_id": "w2:p1", "status": "working", "model": "claude-opus-4-1", "ctx": 38, "item": "r2", "stage": "J1 re-run", "stage_started_at": "2026-10-09T17:12:30Z"},
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

	utc := func(h, m, sec int) *time.Time { return new(time.Date(2026, 10, 9, h, m, sec, 0, time.UTC)) }
	want := snapshot.Snapshot{
		V:    snapshot.Version,
		At:   time.Date(2026, 10, 9, 17, 19, 0, 0, time.UTC),
		Team: "agentisan",
		Project: &snapshot.Project{
			Color: "#bd93f9",
			PRs: []snapshot.PR{
				{Number: 878, CI: snapshot.CIPending, Codex: snapshot.CodexPending, Merge: snapshot.MergeBlocked},
				{Number: 886, CI: snapshot.CISuccess, Codex: snapshot.CodexApproved, Merge: snapshot.MergeClean},
			},
			Issues: new(14),
			Slots:  &snapshot.Slots{Used: 1, Total: 2},
		},
		Boss: &snapshot.Boss{
			Agent: snapshot.Agent{
				Name: "boss", PaneID: "w1:p1", Status: snapshot.StatusWorking, Model: "opus",
				Ctx: new(23), Item: "#867 release", Stage: "review", StageStartedAt: utc(17, 7, 0),
			},
			Watchers:  new(4),
			StartedAt: utc(15, 5, 0),
		},
		Groups: []snapshot.Group{
			{Name: "coders", Description: "write code", Agents: []snapshot.Agent{
				{Name: "pee01", PaneID: "w2:p1", Status: snapshot.StatusWorking, Model: "claude-opus-4-1", Ctx: new(38), Item: "r2", Stage: "J1 re-run", StageStartedAt: utc(17, 12, 30)},
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
	project := func(fields string) string { return `{"v":1,"team":"t","project":{` + fields + `}}` }
	pr := func(field string) string { return project(`"prs":[` + prWith(field) + `]`) }
	boss := func(fields string) string {
		return `{"v":1,"team":"t","boss":{"name":"boss","status":"working",` + fields + `}}`
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
		// The optional fields of the project, the boss and each agent, at
		// every value their domains allow.
		"ci pending":                 pr(`"ci":"pending"`),
		"ci success":                 pr(`"ci":"success"`),
		"ci failure":                 pr(`"ci":"failure"`),
		"ci unknown":                 pr(`"ci":"unknown"`),
		"codex pending":              pr(`"codex":"pending"`),
		"codex approved":             pr(`"codex":"approved"`),
		"codex findings":             pr(`"codex":"findings"`),
		"codex unknown":              pr(`"codex":"unknown"`),
		"merge clean":                pr(`"merge":"clean"`),
		"merge blocked":              pr(`"merge":"blocked"`),
		"merge behind":               pr(`"merge":"behind"`),
		"merge dirty":                pr(`"merge":"dirty"`),
		"merge draft":                pr(`"merge":"draft"`),
		"merge unstable":             pr(`"merge":"unstable"`),
		"merge has_hooks":            pr(`"merge":"has_hooks"`),
		"merge unknown":              pr(`"merge":"unknown"`),
		"pr number 1":                pr(`"number":1`),
		"an empty project":           project(``),
		"no open PRs":                project(`"prs":[]`),
		"PRs not reported":           project(`"prs":null`),
		"issues 0":                   project(`"issues":0`),
		"slots 0 of 0":               project(`"slots":{"used":0,"total":0}`),
		"slots all used":             project(`"slots":{"used":3,"total":3}`),
		"a lower-case colour":        project(`"color":"#bd93f9"`),
		"an upper-case colour":       project(`"color":"#BD93F9"`),
		"watchers 0":                 boss(`"watchers":0`),
		"a boss start time":          boss(`"started_at":"2026-10-09T15:05:00Z"`),
		"a boss stage start time":    boss(`"stage_started_at":"2026-10-09T15:05:00Z"`),
		"a stage start time":         agent(`"status":"idle","stage_started_at":"2026-10-09T17:12:30Z"`),
		"a time with an offset":      agent(`"status":"idle","stage_started_at":"2026-10-09T17:12:30+05:30"`),
		"a time with -00:00":         agent(`"status":"idle","stage_started_at":"2026-10-09T17:12:30-00:00"`),
		"a time with a fraction":     agent(`"status":"idle","stage_started_at":"2026-10-09T17:12:30.123456789Z"`),
		"the first RFC 3339 instant": agent(`"status":"idle","stage_started_at":"0000-01-01T00:00:00Z"`),
		"the last RFC 3339 instant":  agent(`"status":"idle","stage_started_at":"9999-12-31T23:59:59.999999999Z"`),
		"a null stage start time":    agent(`"status":"idle","stage_started_at":null`),
		"a fraction of 30 digits":    agent(`"status":"idle","stage_started_at":"2026-10-09T17:12:30.123456789012345678901234567890Z"`),
		"many PRs":                   manyPRs(500),
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
	project := func(fields string) string { return `{"v":1,"team":"t","project":{` + fields + `}}` }
	pr := func(field string) string { return project(`"prs":[` + prWith(field) + `]`) }
	boss := func(fields string) string {
		return `{"v":1,"team":"t","boss":{"name":"boss","status":"working",` + fields + `}}`
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

		// The project: closed enums, positive PR numbers, counts that are
		// never negative, slots that never exceed their total, #rrggbb.
		"pr number 0":                     {in: pr(`"number":0`), want: snapshot.ErrInvalid, contains: []string{"number 0"}},
		"pr number -1":                    {in: pr(`"number":-1`), want: snapshot.ErrInvalid},
		"pr number fractional":            {in: pr(`"number":1.5`), want: snapshot.ErrInvalid},
		"pr number past int64":            {in: pr(`"number":1e30`), want: snapshot.ErrInvalid},
		"pr number missing":               {in: project(`"prs":[{"ci":"success","codex":"approved","merge":"clean"}]`), want: snapshot.ErrInvalid},
		"a duplicate pr number":           {in: project(`"prs":[` + prWith(`"number":7`) + `,` + prWith(`"number":7`) + `]`), want: snapshot.ErrInvalid, contains: []string{"#7", "twice"}},
		"unknown ci":                      {in: pr(`"ci":"green"`), want: snapshot.ErrInvalid, contains: []string{`"green"`}},
		"ci in another case":              {in: pr(`"ci":"SUCCESS"`), want: snapshot.ErrInvalid},
		"empty ci":                        {in: pr(`"ci":""`), want: snapshot.ErrInvalid},
		"unknown codex":                   {in: pr(`"codex":"thumbs_up"`), want: snapshot.ErrInvalid},
		"codex in another case":           {in: pr(`"codex":"Approved"`), want: snapshot.ErrInvalid},
		"unknown merge":                   {in: pr(`"merge":"conflicting"`), want: snapshot.ErrInvalid},
		"merge in another case":           {in: pr(`"merge":"CLEAN"`), want: snapshot.ErrInvalid},
		"an unknown pr key":               {in: pr(`"title":"x"`), want: snapshot.ErrInvalid, contains: []string{`"title"`}},
		"a pr key in another case":        {in: project(`"prs":[{"Number":1,"ci":"success","codex":"approved","merge":"clean"}]`), want: snapshot.ErrInvalid, contains: []string{`"Number"`}},
		"a duplicate pr key":              {in: project(`"prs":[{"number":1,"number":2,"ci":"success","codex":"approved","merge":"clean"}]`), want: snapshot.ErrInvalid, contains: []string{"twice"}},
		"issues -1":                       {in: project(`"issues":-1`), want: snapshot.ErrInvalid, contains: []string{"issues -1"}},
		"issues fractional":               {in: project(`"issues":1.5`), want: snapshot.ErrInvalid},
		"issues past int64":               {in: project(`"issues":1e30`), want: snapshot.ErrInvalid},
		"slots used -1":                   {in: project(`"slots":{"used":-1,"total":2}`), want: snapshot.ErrInvalid},
		"slots total -1":                  {in: project(`"slots":{"used":0,"total":-1}`), want: snapshot.ErrInvalid},
		"slots used past total":           {in: project(`"slots":{"used":3,"total":2}`), want: snapshot.ErrInvalid, contains: []string{"3/2"}},
		"slots without used":              {in: project(`"slots":{"total":2}`), want: snapshot.ErrInvalid, contains: []string{`"used"`}},
		"slots without total":             {in: project(`"slots":{"used":0}`), want: snapshot.ErrInvalid, contains: []string{`"total"`}},
		"an unknown slots key":            {in: project(`"slots":{"used":0,"total":2,"held":1}`), want: snapshot.ErrInvalid},
		"a duplicate slots key":           {in: project(`"slots":{"used":0,"used":1,"total":2}`), want: snapshot.ErrInvalid},
		"colour without #":                {in: project(`"color":"bd93f9"`), want: snapshot.ErrInvalid, contains: []string{"bd93f9"}},
		"colour of three digits":          {in: project(`"color":"#fff"`), want: snapshot.ErrInvalid},
		"colour with alpha":               {in: project(`"color":"#bd93f9ff"`), want: snapshot.ErrInvalid},
		"colour by name":                  {in: project(`"color":"purple"`), want: snapshot.ErrInvalid},
		"colour not hex":                  {in: project(`"color":"#gg0000"`), want: snapshot.ErrInvalid},
		"colour with an escape":           {in: project(`"color":"#bd93f9\u001b"`), want: snapshot.ErrInvalid},
		"an unknown project key":          {in: project(`"repo":"x"`), want: snapshot.ErrInvalid, contains: []string{`"repo"`}},
		"a project key in another case":   {in: project(`"Issues":1`), want: snapshot.ErrInvalid, contains: []string{`"Issues"`}},
		"a project that is not an object": {in: `{"v":1,"team":"t","project":[]}`, want: snapshot.ErrInvalid},

		// The boss alone reports watchers and a start time.
		"watchers -1":                 {in: boss(`"watchers":-1`), want: snapshot.ErrInvalid, contains: []string{"watchers -1"}},
		"watchers fractional":         {in: boss(`"watchers":0.5`), want: snapshot.ErrInvalid},
		"watchers past int64":         {in: boss(`"watchers":1e30`), want: snapshot.ErrInvalid},
		"watchers on a group agent":   {in: agent(`"name":"a","status":"idle","watchers":1`), want: snapshot.ErrInvalid, contains: []string{`"watchers"`}},
		"started_at on a group agent": {in: agent(`"name":"a","status":"idle","started_at":"2026-10-09T15:05:00Z"`), want: snapshot.ErrInvalid, contains: []string{`"started_at"`}},
		"a boss key in another case":  {in: boss(`"Watchers":1`), want: snapshot.ErrInvalid, contains: []string{`"Watchers"`}},
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

// manyPRs is a valid snapshot whose project has n open PRs.
func manyPRs(n int) string {
	prs := make([]string, n)
	for i := range prs {
		prs[i] = prWith(fmt.Sprintf(`"number":%d`, i+1))
	}
	return `{"v":1,"team":"t","project":{"prs":[` + strings.Join(prs, ",") + `]}}`
}

// prWith is a valid PR object with field replacing the one of the same key.
func prWith(field string) string {
	fields := map[string]string{"number": `"number":1`, "ci": `"ci":"success"`, "codex": `"codex":"approved"`, "merge": `"merge":"clean"`}
	key, _, _ := strings.Cut(strings.Trim(field, `"`), `"`)
	if _, ok := fields[key]; ok {
		fields[key] = field
	} else {
		fields["extra"] = field
	}
	parts := make([]string, 0, len(fields))
	for _, k := range []string{"number", "ci", "codex", "merge", "extra"} {
		if f, ok := fields[k]; ok {
			parts = append(parts, f)
		}
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// TestDecodeRefusesTimesThatAreNotRFC3339: every time in the contract (the
// snapshot's at, the boss's start, each agent's stage start) is an RFC 3339
// date-time with its two-digit fields, an upper-case T and zone, a '.'
// before any fraction and an offset within ±23:59. Go's time package accepts
// some of these; the contract does not.
func TestDecodeRefusesTimesThatAreNotRFC3339(t *testing.T) {
	t.Parallel()

	at := func(lit string) string { return `{"v":1,"team":"t","at":` + lit + `}` }
	start := func(lit string) string {
		return `{"v":1,"team":"t","boss":{"name":"boss","status":"idle","started_at":` + lit + `}}`
	}
	stage := func(lit string) string {
		return `{"v":1,"team":"t","groups":[{"name":"g","agents":[{"name":"a","status":"idle","stage_started_at":` + lit + `}]}]}`
	}
	tests := map[string]string{
		"a date without a time":       `"2026-10-09"`,
		"a time without a zone":       `"2026-10-09T17:19:00"`,
		"a space for T":               `"2026-10-09 17:19:00Z"`,
		"lower-case t and z":          `"2026-10-09t17:19:00z"`,
		"a one-digit hour":            `"2026-10-09T7:19:00Z"`,
		"hour 24":                     `"2026-10-09T24:00:00Z"`,
		"february 30":                 `"2026-02-30T00:00:00Z"`,
		"a leap second":               `"2026-12-31T23:59:60Z"`,
		"an offset past 23 hours":     `"2026-10-09T17:19:00+24:00"`,
		"a comma before the fraction": `"2026-10-09T17:19:00,5Z"`,
		"a five-digit year":           `"+10000-01-01T00:00:00Z"`,
		"an empty time":               `""`,
		"a time as a number":          `1760030340`,
		"a time as an object":         `{}`,
		"a time with a NUL":           `"2026-10-09T17:19:00Z\u0000"`,
		"trailing text":               `"2026-10-09T17:19:00Z later"`,
		"a trailing newline":          `"2026-10-09T17:19:00Z\n"`,
		"full-width digits":           `"２０２６-10-09T17:19:00Z"`,
		"a time with invalid UTF-8":   "\"2026-10-09T17:19:00Z\xff\"",
		"a truncated multibyte":       "\"2026-10-09T17:19:00Z\xe2\x82\"",
		"an escaped lone surrogate":   `"2026-10-09T17:19:00Z\ud800"`,
		"a byte-order mark":           "\"\xef\xbb\xbf2026-10-09T17:19:00Z\"",
		"a negative year":             `"-0001-01-01T00:00:00Z"`,
	}
	for name, lit := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for field, doc := range map[string]string{"at": at(lit), "started_at": start(lit), "stage_started_at": stage(lit)} {
				_, err := snapshot.Decode(strings.NewReader(doc))
				require.ErrorIs(t, err, snapshot.ErrInvalid, "%s %s", field, lit)
			}
		})
	}
}

// TestEnumsValid: each of the PR enums accepts its own values and nothing
// else, the empty string included.
func TestEnumsValid(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		valid func(string) bool
		good  []string
	}{
		"ci":    {valid: func(s string) bool { return snapshot.CIState(s).Valid() }, good: []string{"pending", "success", "failure", "unknown"}},
		"codex": {valid: func(s string) bool { return snapshot.CodexState(s).Valid() }, good: []string{"pending", "approved", "findings", "unknown"}},
		"merge": {valid: func(s string) bool { return snapshot.MergeState(s).Valid() }, good: []string{"clean", "blocked", "behind", "dirty", "draft", "unstable", "has_hooks", "unknown"}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for _, s := range tc.good {
				assert.True(t, tc.valid(s), s)
				assert.False(t, tc.valid(strings.ToUpper(s)), strings.ToUpper(s))
			}
			// Spellings that are not the contract's: other words, padding,
			// a byte-order mark, an invalid byte, a NUL, a newline and a
			// full-width letter.
			for _, s := range []string{"", "ok", "working", " pending", "\ufeffpending", "pend\xffing", "pending\xe2\x82", "pending\x00", "pending\n", "ｐending"} {
				assert.False(t, tc.valid(s), "%q", s)
			}
		})
	}
}

func TestValidColor(t *testing.T) {
	t.Parallel()

	tests := map[string]bool{
		"#bd93f9": true, "#BD93F9": true, "#000000": true,
		"": false, "bd93f9": false, "#fff": false, "#bd93f9ff": false, "#gg0000": false,
		"12": false, "purple": false, "#bd93f9\n": false, " #bd93f9": false,
		"#ｂd93f9": false, "#bd93f\xff": false, "#bd93f\xe9": false, "#bd93\xe2\x82": false,
		"\ufeff#bd93f9": false, "#bd93f9\x00": false,
	}
	for in, want := range tests {
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, want, snapshot.ValidColor(in))
		})
	}
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
