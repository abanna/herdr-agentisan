package trail

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/abanna/herdr-agentisan/internal/telemetry"
)

// Source is the trail's data stream pattern: what the collector writes
// (docs/telemetry.md) and the only source a trail query reads.
const Source = "logs-agentisan.otel-*"

// The bounds of a worker trail's window.
const (
	MinSince = time.Minute
	MaxSince = 7 * 24 * time.Hour
)

// maxID bounds a worker or pane id, in bytes.
const maxID = 128

// RowLimit caps a worker trail at its newest changes. The live check's
// worker changed about 9 times an hour, so a busy day fits, and a longer
// window says when it was cut.
const RowLimit = 1000

// fromTrail matches a query that reads the trail and nothing else: after
// the pattern only a pipe or the end may follow, so neither a second
// source (a comma, with or without spaces) nor METADATA can.
var fromTrail = regexp.MustCompile(`(?i)^\s*FROM\s+logs-agentisan\.otel-\*\s*(\||$)`)

// secondSource matches LOOKUP JOIN and ENRICH, which read another index, as
// words anywhere: ES|QL takes a comment wherever it takes a space, so even a
// name holding either word is refused, which only ever refuses too much.
var secondSource = regexp.MustCompile(`(?i)\b(LOOKUP|ENRICH)\b`)

// CheckQuery refuses an ES|QL query that does not start FROM the trail, or
// that reads a second index. The read key bounds what any query can reach;
// this keeps the debugger's ad-hoc queries on the trail.
func CheckQuery(esql string) error {
	if !fromTrail.MatchString(esql) {
		return fmt.Errorf("%w: a trail query starts FROM %s", ErrInvalid, Source)
	}
	if secondSource.MatchString(esql) {
		return fmt.Errorf("%w: a trail query reads no second index, so LOOKUP and ENRICH appear nowhere in it", ErrInvalid)
	}
	return nil
}

// Attribute fields, as the collector's otel mapping stores them.
func field(attr string) string { return "attributes." + attr }

// The columns of WorkerQuery's answer, in order.
var workerColumns = []string{
	"@timestamp", field(telemetry.AttrWorkerID), field(telemetry.AttrPaneID), field(telemetry.AttrFrom),
	field(telemetry.AttrTo), field(telemetry.AttrGroup), field(telemetry.AttrWorkerBound),
}

// WorkerQuery is the ES|QL for "what happened to worker X in the last
// hour": the newest RowLimit status changes whose worker id or pane id is
// id, since `since` ago, newest first, so a cut drops the oldest, never the
// newest; WorkerChanges puts them back in order. Matching either id follows
// a worker across its binding, since its events carry the pane id until a
// workers row binds it. since is whole minutes from MinSince to MaxSince.
func WorkerQuery(id string, since time.Duration) (string, error) {
	lit, err := esqlString(id)
	if err != nil {
		return "", err
	}
	if since < MinSince || since > MaxSince || since%time.Minute != 0 {
		return "", fmt.Errorf("%w: --since %s is not whole minutes from %s to %s", ErrInvalid, since, MinSince, MaxSince)
	}
	return fmt.Sprintf(`FROM %s
| WHERE event_name == %q
    AND (%s == %s OR %s == %s)
    AND @timestamp > NOW() - %d minutes
| SORT @timestamp DESC
| KEEP %s
| LIMIT %d`,
		Source, telemetry.EventStatusChanged,
		field(telemetry.AttrWorkerID), lit, field(telemetry.AttrPaneID), lit,
		int(since/time.Minute), strings.Join(workerColumns, ", "), RowLimit), nil
}

// esqlString quotes s as an ES|QL string literal. It refuses an empty or
// overlong s, invalid UTF-8 and control characters other than the three
// escapes ES|QL has.
func esqlString(s string) (string, error) {
	if s == "" || len(s) > maxID || !utf8.ValidString(s) {
		return "", fmt.Errorf("%w: a worker id is 1 to %d bytes of UTF-8", ErrInvalid, maxID)
	}
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if unicode.IsControl(r) {
				return "", fmt.Errorf("%w: a worker id holds no control characters", ErrInvalid)
			}
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String(), nil
}

// Change is one row of a worker trail.
type Change struct {
	At       string `json:"at"`
	WorkerID string `json:"worker_id"`
	PaneID   string `json:"pane_id"`
	From     string `json:"from"`
	To       string `json:"to"`
	Group    string `json:"group"`
	Bound    bool   `json:"bound"`
}

// WorkerChanges reads a WorkerQuery answer, by column name, into its
// changes, oldest first. cut is whether the answer reached RowLimit, so
// older changes in the window may be missing.
func WorkerChanges(t Table) (changes []Change, cut bool, err error) {
	idx := map[string]int{}
	for i, c := range t.Columns {
		idx[c.Name] = i
	}
	for _, name := range workerColumns {
		if _, ok := idx[name]; !ok {
			return nil, false, fmt.Errorf("%w: the answer has no %s column", ErrUnavailable, name)
		}
	}
	str := func(row []any, name string) string {
		s, _ := row[idx[name]].(string)
		return s
	}
	out := make([]Change, 0, len(t.Values))
	for _, row := range t.Values {
		bound, _ := row[idx[field(telemetry.AttrWorkerBound)]].(bool)
		out = append(out, Change{
			At: str(row, "@timestamp"), WorkerID: str(row, field(telemetry.AttrWorkerID)),
			PaneID: str(row, field(telemetry.AttrPaneID)), From: str(row, field(telemetry.AttrFrom)),
			To: str(row, field(telemetry.AttrTo)), Group: str(row, field(telemetry.AttrGroup)), Bound: bound,
		})
	}
	slices.Reverse(out)
	return out, len(out) >= RowLimit, nil
}
