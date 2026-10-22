package workforce

import (
	"fmt"
	"regexp"
	"strconv"

	"github.com/abanna/herdr-agentisan/internal/store"
)

// agentName is herdr's agent-name grammar (src/app/agents.rs valid_agent_name
// at v0.9.3), which a logical id, also its agent's name, must match.
var agentName = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// LogicalID is the n-th worker of type typ in the team with short code
// short: <short>-<type>-<n>, its tab's label and its agent's name.
func LogicalID(short, typ string, n int) string {
	return short + "-" + typ + "-" + strconv.Itoa(n)
}

// checkID refuses an id herdr would refuse as an agent name.
func checkID(id string) error {
	if !agentName.MatchString(id) {
		return fmt.Errorf("%w: %q must match %s (herdr's agent names, at most 32 characters); shorten the type or the project's short code", ErrInvalidID, id, agentName)
	}
	return nil
}

// taken is every name a new worker's id must not be: the labels of the
// team's tabs, every agent name on the server (herdr's names are global), and
// every live row's logical id but a lost one's, which spawn may reuse (A35).
func (st state) taken() map[string]bool {
	out := map[string]bool{}
	for _, t := range st.layout.Tabs {
		if t.WorkspaceID == st.workspace && st.workspace != "" {
			out[t.Label] = true
		}
	}
	for _, a := range st.layout.Agents {
		if a.Name != "" {
			out[a.Name] = true
		}
	}
	for _, r := range st.rows {
		if r.Status != store.StatusLost {
			out[r.LogicalID] = true
		}
	}
	return out
}

// nextIDs returns count ids of type typ, each the lowest n from 1 not in
// taken, and marks them taken; a retired worker's id is free again.
func nextIDs(short, typ string, count int, taken map[string]bool) ([]string, error) {
	out := make([]string, 0, count)
	for n := 1; len(out) < count; n++ {
		id := LogicalID(short, typ, n)
		if taken[id] {
			continue
		}
		if err := checkID(id); err != nil {
			return nil, err
		}
		taken[id] = true
		out = append(out, id)
	}
	return out, nil
}
