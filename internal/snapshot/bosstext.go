package snapshot

import (
	"fmt"
	"io"
	"strconv"
	"time"
)

// MaxBossTextBytes is the maximum size of the one-page text view.
const MaxBossTextBytes = 4 << 10

// WriteText prints the snapshot with source ages and counts any rows that
// the text byte budget leaves out.
func (s BossSnapshot) WriteText(w io.Writer) error {
	out := fmt.Appendf(nil, "Team %s at %s\n", bossText(s.Team), s.At.Format(time.RFC3339))
	out = fmt.Appendf(out, "Team poll (herdr, state.db) %s\n", s.TeamPoll.textAt(s.At))
	out = fmt.Appendf(out, "Board (state.db) %s", s.Board.textAt(s.At))
	if s.Board.State == Fresh {
		out = fmt.Appendf(out, " · mark %d · newest %d · unread %d", *s.Board.Mark, *s.Board.Newest, *s.Board.Unread)
	}
	out = append(out, '\n')
	switch {
	case s.Boss != nil:
		out = append(out, "Boss "...)
		out = appendBossRow(out, s.Boss.BossRow)
		out = appendBossTime(out, " started=", s.Boss.StartedAt)
		out = append(out, '\n')
	case s.MissingBossName != nil:
		out = fmt.Appendf(out, "Boss unknown (no agent named %s)\n", *s.MissingBossName)
	default:
		out = append(out, "Boss unknown\n"...)
	}

	textOmitted := 0
	for _, g := range s.Groups {
		header := "\n" + bossText(g.Name) + "\n"
		for i, worker := range g.Workers {
			line := appendBossRow([]byte("  "), worker.BossRow)
			if worker.ID != nil {
				line = fmt.Appendf(line, " id=%s/%d", bossText(*worker.ID), *worker.Generation)
			}
			// Leave room for the omission line even when the row just fits.
			if len(out)+len(header)+len(line)+1 > MaxBossTextBytes-96 {
				textOmitted += len(g.Workers) - i
				break
			}
			out = append(out, header...)
			header = ""
			out = append(out, line...)
			out = append(out, '\n')
		}
	}
	if s.WorkersOmitted > 0 || textOmitted > 0 {
		out = fmt.Appendf(out, "\n%d workers omitted (%d from poll, %d from text)\n", s.WorkersOmitted+textOmitted, s.WorkersOmitted, textOmitted)
	}
	if _, err := w.Write(out); err != nil {
		return fmt.Errorf("write boss snapshot: %w", err)
	}
	return nil
}

func (s BossSource) textAt(now time.Time) string {
	if s.AsOf == nil || s.AsOf.After(now) {
		return string(s.State) + " · age unknown"
	}
	return fmt.Sprintf("%s · %s old", s.State, now.Sub(*s.AsOf).Round(time.Millisecond))
}

func bossText(s string) string {
	runes := []rune(s)
	if len(runes) > 32 {
		s = string(runes[:32]) + "…"
	}
	return strconv.QuoteToASCII(s)
}

func appendBossRow(out []byte, r BossRow) []byte {
	out = fmt.Appendf(out, "%s %s pane=%s", bossText(r.Name), r.Status, bossText(r.PaneID))
	for _, f := range []struct {
		prefix string
		value  *string
	}{{" agent=", r.Agent}, {" item=", r.Item}, {" stage=", r.Stage}} {
		if f.value != nil {
			out = fmt.Appendf(out, "%s%s", f.prefix, bossText(*f.value))
		}
	}
	if r.Ctx == nil {
		out = append(out, " ctx=?"...)
	} else {
		out = fmt.Appendf(out, " ctx=%d%%", *r.Ctx)
	}
	if r.StageStale {
		out = append(out, " stage_stale"...)
	}
	out = appendBossTime(out, " stage_at=", r.StageAt)
	return appendBossTime(out, " idle_since=", r.IdleSince)
}

func appendBossTime(out []byte, prefix string, at *time.Time) []byte {
	if at != nil {
		return fmt.Appendf(out, "%s%s", prefix, at.Format(time.RFC3339))
	}
	return out
}
