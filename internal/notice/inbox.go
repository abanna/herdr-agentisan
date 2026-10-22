package notice

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// The inbox's notices (A44): each message goes to its team's boss as a
// message the daemon sends, appended to the one still queued for that boss,
// so a boss that is away gets one notice, not a pile. Its body is a header,
// which counts the notices cut for size, then one line per notice, oldest
// first: "[agentisan] notices (3 earlier dropped):\nha-coder-2 is blocked".
// A message the daemon sends starts "[agentisan" and takes no reply
// (docs/messaging.md); the count is in the header, always the first line
// and always the daemon's, so no notice's line can pass for it.

// headerOf matches a notice body's header, and holds its count.
var headerOf = regexp.MustCompile(`^\[agentisan\] notices(?: \(([1-9][0-9]*) earlier dropped\))?:$`)

// AppendTo is queued, the body of the notice still queued for m's boss, with
// m's notices after its own, or m's body alone when queued is "". The oldest
// lines are cut, and counted in the header, until the body fits MaxBody,
// the inbox's cap; the newest line always stays. It reports false when
// queued is not a body AppendTo wrote, which is never appended to.
func (m Message) AppendTo(queued string) (string, bool) {
	dropped, lines := m.Dropped, []string{}
	if queued != "" {
		head, rest, _ := strings.Cut(queued, "\n")
		sub := headerOf.FindStringSubmatch(head)
		if sub == nil {
			return "", false
		}
		if sub[1] != "" {
			n, err := strconv.Atoi(sub[1])
			if err != nil {
				return "", false
			}
			dropped += n
		}
		if rest != "" {
			lines = strings.Split(rest, "\n")
		}
	}
	for _, l := range m.Lines {
		lines = append(lines, inboxLine(l))
	}
	for len(lines) > 1 && inboxSize(dropped, lines) > MaxBody {
		lines, dropped = lines[1:], dropped+1
	}
	return strings.Join(append([]string{inboxHeader(dropped)}, lines...), "\n"), true
}

func inboxHeader(dropped int) string {
	if dropped == 0 {
		return "[agentisan] notices:"
	}
	return fmt.Sprintf("[agentisan] notices (%d earlier dropped):", dropped)
}

func inboxSize(dropped int, lines []string) int {
	n := len(inboxHeader(dropped))
	for _, l := range lines {
		n += 1 + len(l)
	}
	return n
}

// inboxLine is l as one line of a body: valid UTF-8, with every control
// character a space, so no line breaks early and no NUL, which the inbox
// refuses, is kept, and at most MaxLine bytes.
func inboxLine(l string) string {
	l = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(l, "�"))
	return cut(l, MaxLine)
}
