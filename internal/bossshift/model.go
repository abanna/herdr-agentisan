// Package bossshift verifies the candidate's model for
// the interim boss handoff script (NERD-5262).
package bossshift

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"time"

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/report"
)

// ErrModelProof means the current candidate did not prove the requested model.
var ErrModelProof = errors.New("boss shift model unverified")

// Candidate pins the pane and terminal from the split. After the first process
// read, PID and Start pin that Claude. PinnedAt records the pin in Unix milliseconds.
type Candidate struct {
	Pane, Terminal, Name, Model string
	LaunchedAt, PinnedAt        int64
	PID                         uint32
	Start                       uint64
}

// ModelVerdict reports the observation and any process identity independently
// of whether the model passed. Start uses the OS process table's raw unit.
type ModelVerdict struct {
	Requested string `json:"requested_model"`
	Observed  string `json:"observed_model"`
	Reason    string `json:"reason"`
	PID       uint32 `json:"pid"`
	Start     uint64 `json:"start"`
}

func (v ModelVerdict) refuse(reason string) (ModelVerdict, error) {
	v.Reason = reason
	return v, fmt.Errorf("%w: %s", ErrModelProof, reason)
}

const claudeProgram = "claude"

func (v ModelVerdict) unavailable(err error) (ModelVerdict, error) {
	v.Reason = "candidate_unavailable"
	return v, fmt.Errorf("%w: %w", ErrModelProof, err)
}

var fableID = regexp.MustCompile(`^claude-fable-[0-9]+-[0-9]+(-[0-9]{8})?(\[1m\])?$`)

// CheckModel reads only the named Claude's pane tokens as model evidence.
// Launch argv and dashboard models cannot attest the model in use.
func CheckModel(ctx context.Context, c herdr.Client, target Candidate) (ModelVerdict, error) {
	return checkModel(ctx, c, target, report.SystemStat(), report.SystemStartedAt(), time.Now)
}

func checkModel(ctx context.Context, c herdr.Client, target Candidate, stat report.StatFunc, wall report.StartedAtFunc, clock func() time.Time) (ModelVerdict, error) {
	v := ModelVerdict{Requested: target.Model}
	launch := time.UnixMilli(target.LaunchedAt)
	if target.Pane == "" || target.Terminal == "" || target.Name == "" || report.ValidModel(target.Model) == "" ||
		target.LaunchedAt <= 0 || (target.PID == 0) != (target.Start == 0) ||
		(target.PID == 0) != (target.PinnedAt == 0) || target.PinnedAt != 0 && target.PinnedAt < target.LaunchedAt {
		return v.refuse("invalid_candidate")
	}
	ctx, cancel := context.WithTimeout(ctx, herdr.CallTimeout)
	defer cancel()

	agent, err := c.GetAgent(ctx, target.Name)
	if err != nil {
		return v.unavailable(err)
	}
	if agent.PaneID != target.Pane || agent.TerminalID != target.Terminal || agent.Name != target.Name {
		return v.refuse("candidate_changed")
	}
	if agent.Agent != claudeProgram || agent.LaunchPending {
		return v.refuse("claude_unavailable")
	}
	info, err := c.PaneProcessInfo(ctx, target.Pane)
	if err != nil {
		return v.unavailable(err)
	}
	pid := claudePID(info.ForegroundProcesses)
	if info.PaneID != target.Pane {
		return v.refuse("candidate_changed")
	}
	if pid == 0 || stat == nil || wall == nil {
		return v.refuse("claude_unavailable")
	}
	proc, err := stat(int(pid)) // #nosec G115 -- OS PID fits in int
	if err != nil || proc.Dead || proc.Start == 0 {
		return v.refuse("claude_unavailable")
	}
	if target.PID != 0 && (target.PID != pid || target.Start != proc.Start) {
		return v.refuse("candidate_changed")
	}
	started, err := wall(proc.Start)
	if err != nil || started.IsZero() {
		return v.refuse("claude_unavailable")
	}
	pane, err := c.GetPane(ctx, target.Pane)
	if err != nil {
		return v.unavailable(err)
	}
	if pane.PaneID != target.Pane || pane.TerminalID != target.Terminal || pane.Agent != claudeProgram {
		return v.refuse("candidate_changed")
	}
	current, err := stat(int(pid)) // #nosec G115 -- OS PID fits in int
	if err != nil || current.Dead || current.Start != proc.Start {
		return v.refuse("candidate_changed")
	}

	v.PID, v.Start = pid, proc.Start
	v.Observed = report.ValidModel(pane.Tokens[report.ModelKey])
	if v.Observed == "" {
		return v.refuse("missing_model")
	}
	stamp, ok := report.ParseModelAt(pane.Tokens[report.ModelAtKey])
	now := clock()
	if !ok || stamp.After(now) {
		return v.refuse("invalid_report_time")
	}
	// The Linux boot time rounds down to seconds. Compare the resulting
	// start at the reporter's millisecond precision (NERD-5262).
	if stamp.Before(launch) || stamp.Before(started.Truncate(time.Millisecond)) || now.Sub(stamp) >= report.CtxTTL {
		return v.refuse("stale_report")
	}
	if target.PID == 0 {
		return v.refuse("awaiting_pin")
	}
	// Start time can round down by a second. Observe the candidate first,
	// then require a report beyond the pin and the rounding window (NERD-5262).
	if !stamp.After(time.UnixMilli(target.PinnedAt).Add(time.Second)) {
		return v.refuse("report_before_pin")
	}
	fableAlias := target.Model == "fable" && fableID.MatchString(v.Observed)
	if target.Model != v.Observed && !fableAlias {
		return v.refuse("wrong_model")
	}
	v.Reason = "verified"
	return v, nil
}

func claudePID(procs []herdr.Process) uint32 {
	var pid uint32
	for _, p := range procs {
		if p.PID == 0 || len(p.Argv) == 0 || filepath.Base(p.Argv[0]) != claudeProgram {
			continue
		}
		if pid != 0 {
			return 0
		}
		pid = p.PID
	}
	return pid
}
