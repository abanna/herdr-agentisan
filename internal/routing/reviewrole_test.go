package routing_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/routing"
	"github.com/abanna/herdr-agentisan/internal/settings"
)

// TestReviewRoleAsksWhatRoutingRequires: the review worker's role (ADR-001
// A55) asks of each finding every field the user's rules require of the
// review type's role. Go reads past findings_require, role instructions
// that stay out of the plugin's logic (D9), so the test decodes the rules
// file itself: a rules change fails it until the role follows.
func TestReviewRoleAsksWhatRoutingRequires(t *testing.T) {
	t.Parallel()
	name, err := routing.RoleFor(settings.ReviewType)
	require.NoError(t, err)
	raw, err := os.ReadFile(rulesFile)
	require.NoError(t, err)
	var doc struct {
		Roles map[string]struct {
			FindingsRequire []string `json:"findings_require"`
		} `json:"roles"`
	}
	require.NoError(t, json.Unmarshal(raw, &doc))
	fields := doc.Roles[name].FindingsRequire
	require.NotEmpty(t, fields, "%s requires fields of a finding", name)

	role, err := os.ReadFile(filepath.Join("..", "..", "roles", "review.md"))
	require.NoError(t, err)
	_, format, ok := strings.Cut(string(role), "\n## Your report\n")
	require.True(t, ok, "the role has its report section")
	format = strings.ToLower(strings.Join(strings.Fields(format), " "))
	for _, field := range fields {
		assert.Contains(t, format, strings.ToLower(field)+":", "a finding gives its %s", field)
	}
}
