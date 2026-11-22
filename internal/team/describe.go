package team

import "github.com/abanna/herdr-agentisan/internal/snapshot"

// Describe sets each group's description in s from descriptions, the
// project's configured ones (ADR-001 A36), keyed by the group's name exactly
// as the snapshot names it, such as coder. Keys match byte for byte.
func Describe(s *snapshot.Snapshot, descriptions map[string]string) {
	for i := range s.Groups {
		s.Groups[i].Description = descriptions[s.Groups[i].Name]
	}
}
