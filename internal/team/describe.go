package team

import "github.com/abanna/herdr-agentisan/internal/snapshot"

// Describe sets each group's description in s from descriptions, the
// project's configured ones (ADR-001 A36), keyed by the group's name exactly
// as the snapshot names it: a tabs team's worker type, such as coder, and a ◆
// space's name, such as coders. Nothing is folded or trimmed, so a name
// descriptions does not hold byte for byte gets none.
func Describe(s *snapshot.Snapshot, descriptions map[string]string) {
	for i := range s.Groups {
		s.Groups[i].Description = descriptions[s.Groups[i].Name]
	}
}
