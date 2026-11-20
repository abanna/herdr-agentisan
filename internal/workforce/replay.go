package workforce

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/abanna/herdr-agentisan/internal/store"
)

// superseded is a replayed worker's status once its id is bound again at a
// later generation than its request's: the id is another worker's now.
const superseded = "superseded"

// Replay is the answer a done record replays (A73): stored text is
// history, and every fact about a worker now is read from the record's
// links, never from the stored answer. For ok it is the stored SpawnResult,
// replayed, answered at the first answer's time and with the key spent,
// each worker as first answered but for its status and briefs now and its
// pane id only while its row is live; a superseded worker has neither. For
// a refusal it is a *RefusalError of the stored code, the key spent, its message
// between the replay's prefix and what each planned worker is now, its
// cause ErrReplayed. A record still running is ErrSpawnUnsettled. It reads
// nothing but r.
func Replay(r store.SpawnRecord) (SpawnResult, error) {
	if r.State != "done" {
		return SpawnResult{}, fmt.Errorf("%w: replay a request still running", ErrSpawnUnsettled)
	}
	answered := r.FinishedAt.UTC().Format(time.RFC3339)
	if r.Answer.Code != "ok" {
		return SpawnResult{}, &RefusalError{
			Code: r.Answer.Code,
			Message: "replayed (first answered " + answered + "); its ids and pane ids are as they were then and may since have been reused; act on none of them: " +
				r.Answer.Message + "; " + nowList(r),
			RetryKey: r.Key, KeyState: KeySpent, Err: ErrReplayed,
		}
	}
	var res SpawnResult
	if err := json.Unmarshal(r.Answer.Result, &res); err != nil {
		return SpawnResult{}, fmt.Errorf("replay the stored answer: %w", err)
	}
	links := map[string]store.SpawnLink{}
	for _, l := range r.Planned {
		if l.Bound {
			links[l.LogicalID] = l
		}
	}
	for i, w := range res.Workers {
		w.PaneID, w.Status, w.Briefs = "", superseded, nil
		if l := links[w.ID]; l.Row != nil {
			n := l.Briefs
			w.Status, w.Briefs = l.Row.Status, &n
			if l.Row.RetiredAt.IsZero() && l.Row.Status != store.StatusLost {
				w.PaneID = l.Row.PaneID
			}
		}
		res.Workers[i] = w
	}
	res.RetryKey, res.KeyState, res.Replayed, res.AnsweredAt = r.Key, KeySpent, true, answered
	return res, nil
}

// nowList is what each id r planned is now, as a replayed refusal, a
// mismatch and an interrupted request's message end it: each bound one by
// id, its linked generation, its row's status and its briefs, or superseded
// once its id is bound again; the ones never bound under the key counted,
// never named, since another spawn may hold them now.
func nowList(r store.SpawnRecord) string {
	parts := make([]string, 0, len(r.Planned))
	never := 0
	for _, l := range r.Planned {
		switch {
		case !l.Bound:
			never++
		case l.Row == nil:
			parts = append(parts, fmt.Sprintf("%s (generation %d) %s", l.LogicalID, l.Generation, superseded))
		default:
			parts = append(parts, fmt.Sprintf("%s (generation %d) %s, %s", l.LogicalID, l.Generation, l.Row.Status, counted(l.Briefs, "brief", "briefs")))
		}
	}
	if never > 0 {
		parts = append(parts, counted(never, "planned id", "planned ids")+" never bound")
	}
	return "now: " + strings.Join(parts, "; ")
}

// counted is n with one or many, as n says.
func counted(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}
