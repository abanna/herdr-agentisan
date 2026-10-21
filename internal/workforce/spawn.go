package workforce

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/settings"
	"github.com/abanna/herdr-agentisan/internal/store"
)

// SpawnRequest is worker spawn: Count new workers of type Group.
type SpawnRequest struct {
	Selector
	Group string `json:"group"`
	Count int    `json:"count"`
	// Model overrides the type's model for these workers.
	Model string `json:"model,omitempty"`
}

// Spawned is one worker whose agent came up.
type Spawned struct {
	ID         string `json:"id"`
	Group      string `json:"group"`
	PaneID     string `json:"pane_id"`
	Generation int64  `json:"generation"`
	// Agent is herdr's kind for it: claude or codex.
	Agent string `json:"agent"`
}

// SpawnResult is worker spawn's answer.
type SpawnResult struct {
	Project string    `json:"project"`
	Workers []Spawned `json:"workers"`
	// Warnings are what the boss should fix that did not stop the spawn,
	// such as a Codex worker that will report no ctx.
	Warnings []string `json:"warnings,omitempty"`
}

// Spawn adds req.Count workers of one type to a tabs team, refused whole
// before anything opens past the group's slots or on a failed preflight (A34).
// Each gets the lowest free id, a tab of its own (the team's workspace first
// when it has none) and its agent; one whose agent does not come up has its
// pane closed and its row failed, named in ErrSpawnFailed; the others stay.
func (s *Service) Spawn(ctx context.Context, req SpawnRequest) (SpawnResult, error) {
	if req.Count < 1 {
		return SpawnResult{}, fmt.Errorf("%w: --count %d is below 1", ErrBadRequest, req.Count)
	}
	if req.Group == "" {
		return SpawnResult{}, fmt.Errorf("%w: --group is required", ErrBadRequest)
	}
	if err := settings.CheckModel(req.Model); err != nil {
		return SpawnResult{}, fmt.Errorf("%w: --model: %w", ErrBadRequest, err)
	}
	// The preflight can take seconds, so it runs before the lock: it reads
	// the host, not the team.
	_, p, _, err := s.resolve(ctx, req.Selector)
	if err != nil {
		return SpawnResult{}, err
	}
	if err := s.preflight(ctx, p.Repo); err != nil {
		return SpawnResult{}, err
	}
	pending, st, err := s.planAndOpen(ctx, req.Selector, func(st state) ([]planned, error) {
		if st.profile.Repo != p.Repo {
			return nil, fmt.Errorf("%w: the repo changed from %s to %s during the preflight; spawn again", ErrPreflight, p.Repo, st.profile.Repo)
		}
		g, ok := st.profile.Groups[req.Group]
		if !ok || g.DescribesOnly {
			return nil, fmt.Errorf("%w: %q is not a group of %s; configured: %s", ErrUnknownGroup, req.Group, st.team.Project, strings.Join(configured(st.profile), ", "))
		}
		// Count is compared with what is left, so no count overflows the sum.
		if occ := st.occupied(req.Group); req.Count > g.Slots-occ {
			return nil, fmt.Errorf("%w: %s is capped at %d slots (herdr-agentisan.toml): %d occupied, %d requested", ErrSlotCap, req.Group, g.Slots, occ, req.Count)
		}
		ids, err := nextIDs(st.team.Short, req.Group, req.Count, st.taken())
		if err != nil {
			return nil, err
		}
		return plan(st.profile, req.Group, choice{model: cmp.Or(req.Model, st.profile.AgentFor(req.Group).Model)}, ids)
	})
	if err != nil {
		return SpawnResult{}, err
	}
	workers, err := s.bringUp(ctx, st.profile.Spawn, pending)
	return SpawnResult{Project: st.team.Project, Workers: workers, Warnings: s.codexWarnings(pending)}, err
}

// codexWarnings warns when a Codex worker was planned and Codex's ctx hooks
// are not installed (A22, A43): the worker runs, but reports no ctx.
func (s *Service) codexWarnings(ws []opened) []string {
	if s.CodexHooks == nil || !slices.ContainsFunc(ws, func(w opened) bool { return w.launch.kind == codexKind }) {
		return nil
	}
	switch installed, err := s.CodexHooks(); {
	case err != nil:
		return []string{fmt.Sprintf("could not check Codex's ctx hooks: %v", err)}
	case !installed:
		return []string{"Codex's ctx hooks are not installed, so a Codex worker reports no ctx: run `herdr-agentisan codex hooks`, then trust them once in Codex"}
	}
	return nil
}

// planned is one worker about to be opened.
type planned struct {
	id, group string
	launch    launch
}

// opened is one worker whose tab is open and whose row is bound.
type opened struct {
	planned
	paneID, terminalID string
	generation         int64
}

// planAndOpen holds the workforce lock while it reads the team, plans with
// planFn, opens each tab and binds its row as starting, so no other op reuses
// an id or counts past a cap; the agents start after, outside the lock.
func (s *Service) planAndOpen(ctx context.Context, sel Selector, planFn func(state) ([]planned, error)) ([]opened, state, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.load(ctx, sel)
	if err != nil {
		return nil, state{}, err
	}
	plans, err := planFn(st)
	if err != nil {
		return nil, st, err
	}
	out := make([]opened, 0, len(plans))
	for _, p := range plans {
		o, err := s.open(ctx, &st, p)
		if err != nil {
			return out, st, errors.Join(err, s.abandon(ctx, out))
		}
		out = append(out, o)
	}
	return out, st, nil
}

// open opens one worker's tab, or the team's workspace when it has none yet
// (its first tab then becomes the worker's), and binds its row.
func (s *Service) open(ctx context.Context, st *state, p planned) (opened, error) {
	env := map[string]string{WorkerEnv: p.id}
	var (
		c   herdr.Created
		err error
	)
	if st.workspace == "" {
		c, err = s.Herdr.CreateWorkspace(ctx, herdr.WorkspaceCreate{Cwd: st.profile.Repo, Label: st.team.Project, Env: env})
		if err != nil {
			return opened{}, herdrFailed("create the team's workspace "+st.team.Project, err)
		}
		st.workspace = c.WorkspaceID
		if err := s.Herdr.RenameTab(ctx, c.TabID, p.id); err != nil {
			return opened{}, errors.Join(herdrFailed("label "+p.id+"'s tab", err), s.closeQuietly(ctx, c.RootPane.PaneID, c.RootPane.TerminalID))
		}
	} else {
		c, err = s.Herdr.CreateTab(ctx, herdr.TabCreate{WorkspaceID: st.workspace, Cwd: st.profile.Repo, Label: p.id, Env: env})
		if err != nil {
			return opened{}, herdrFailed("open "+p.id+"'s tab", err)
		}
	}
	gen, err := s.Store.BindWorker(ctx, store.Worker{LogicalID: p.id, Project: st.team.Project, Group: p.group, PaneID: c.RootPane.PaneID, TerminalID: c.RootPane.TerminalID}, s.now())
	if err != nil {
		return opened{}, errors.Join(fmt.Errorf("record %s: %w", p.id, err), s.closeQuietly(ctx, c.RootPane.PaneID, c.RootPane.TerminalID))
	}
	return opened{planned: p, paneID: c.RootPane.PaneID, terminalID: c.RootPane.TerminalID, generation: gen}, nil
}

// abandon closes the panes of workers opened before a failure, and marks
// their rows failed.
func (s *Service) abandon(ctx context.Context, ws []opened) error {
	errs := make([]error, 0, len(ws))
	for _, w := range ws {
		errs = append(errs, s.fail(ctx, w))
	}
	return errors.Join(errs...)
}

// cleanupBudget bounds the cleanup of a failed worker, which runs even when
// the op's own deadline is what failed it.
const cleanupBudget = 10 * time.Second

// fail closes w's pane and marks its row failed on a context of its own, so
// an op out of time still leaves no pane open and no row starting.
func (s *Service) fail(ctx context.Context, w opened) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupBudget)
	defer cancel()
	return errors.Join(s.closeQuietly(ctx, w.paneID, w.terminalID), ignoreGone(s.Store.SetWorkerStatus(ctx, w.row(), store.StatusFailed, s.now())))
}

// row is the workers row w bound.
func (w opened) row() store.Worker { return store.Worker{LogicalID: w.id, Generation: w.generation} }

// closeQuietly closes a pane while it still shows terminal (A35); one whose
// terminal is gone is no failure.
func (s *Service) closeQuietly(ctx context.Context, paneID, terminal string) error {
	_, err := s.closeProven(ctx, target{paneID: paneID, terminal: terminal})
	return err
}

// bringUp starts every opened worker's agent at once and waits for each.
// The ones up are marked active; each other is failed.
func (s *Service) bringUp(ctx context.Context, cfg settings.Spawn, ws []opened) ([]Spawned, error) {
	errs := make([]error, len(ws))
	var wg sync.WaitGroup
	for i, w := range ws {
		wg.Go(func() { errs[i] = s.startAndWait(ctx, cfg, w) })
	}
	wg.Wait()
	// The rows settle on a context of their own, as fail's cleanup does: a
	// sibling that ran the op to its deadline must not fail a worker that is up.
	settle, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupBudget)
	defer cancel()
	out := []Spawned{}
	var failed []error
	for i, w := range ws {
		if errs[i] == nil {
			errs[i] = s.Store.SetWorkerStatus(settle, w.row(), store.StatusActive, s.now())
		}
		if errs[i] != nil {
			failed = append(failed, fmt.Errorf("%s: %w", w.id, errors.Join(errs[i], s.fail(ctx, w))))
			continue
		}
		out = append(out, Spawned{ID: w.id, Group: w.group, PaneID: w.paneID, Generation: w.generation, Agent: w.launch.kind})
	}
	if len(failed) > 0 {
		return out, fmt.Errorf("%w: %w", ErrSpawnFailed, errors.Join(failed...))
	}
	return out, nil
}

// codexKind is herdr's kind for Codex.
const codexKind = "codex"

// noDaemon runs Codex embedded, off the shared app-server, which A22 needs
// for its ctx; Codex refuses the flag twice, so it is added only when absent.
const noDaemon = "--no-daemon"

// launch is how one worker's agent is started: herdr's kind, which picks its
// executable, and the arguments after it.
type launch struct {
	kind string
	args []string
}

// choice is the model and effort ("" for none) a spawn's agents start with:
// --model or the type's, until NERD-5289's routing chooses at plan's one call.
type choice struct {
	model, effort string
}

// effortPattern is an effort, such as high or xhigh: safe in any shell.
var effortPattern = regexp.MustCompile(`^[a-z]{1,16}$`)

// plan builds each id's launch: the kind is the command's bare executable,
// which herdr runs from the shell's PATH, and the arguments the command's own,
// --no-daemon for Codex if absent, c's model and effort and, for Claude only,
// --name with the id (herdr names a Codex).
func plan(p *settings.Profile, typ string, c choice, ids []string) ([]planned, error) {
	agent := p.AgentFor(typ)
	if err := settings.CheckAgentCommand(agent.Command); err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrConfig, typ, err)
	}
	// herdr types both into the pane's shell (A46): checked where used,
	// whoever chose them, and never echoed.
	if err := settings.CheckModel(c.model); err != nil {
		return nil, fmt.Errorf("%w: %s's agent model: %w", ErrConfig, typ, err)
	}
	if c.effort != "" && !effortPattern.MatchString(c.effort) {
		return nil, fmt.Errorf("%w: %s's effort is not 1 to 16 lowercase letters", ErrConfig, typ)
	}
	// herdr runs its own executable for the kind from the shell's PATH, so
	// a path would be silently swapped for whatever claude or codex is
	// there: refused instead.
	kind := filepath.Base(agent.Command[0])
	if kind != agent.Command[0] {
		return nil, fmt.Errorf("%w: %s's agent command names the path %s; herdr runs %s from the shell's PATH, so name it bare", ErrConfig, typ, agent.Command[0], kind)
	}
	out := make([]planned, 0, len(ids))
	for _, id := range ids {
		args := slices.Clone(agent.Command[1:])
		if kind == codexKind && !slices.Contains(args, noDaemon) {
			args = append(args, noDaemon)
		}
		if c.model != "" {
			args = append(args, "--model", c.model)
		}
		switch {
		case c.effort == "":
		case kind == codexKind:
			args = append(args, "-c", `model_reasoning_effort="`+c.effort+`"`)
		default:
			args = append(args, "--effort", c.effort)
		}
		if kind == "claude" {
			args = append(args, "--name", id)
		}
		out = append(out, planned{id: id, group: typ, launch: launch{kind: kind, args: args}})
	}
	return out, nil
}

// startAndWait brings one worker's agent up, within ctx: it waits for the
// shell prompt when one is configured (herdr only sees which program has the
// terminal, and a shell asking a question still has it), starts the agent,
// retrying a busy pane, then waits for herdr to report it ready.
func (s *Service) startAndWait(ctx context.Context, cfg settings.Spawn, w opened) error {
	keys := keySender{s: s, w: w, cfg: cfg, started: time.Now()}
	if cfg.ShellPrompt != "" {
		if err := s.waitForPrompt(ctx, cfg, w, &keys); err != nil {
			return err
		}
	}
	if err := s.start(ctx, w, &keys); err != nil {
		return err
	}
	if err := s.waitReady(ctx, w); err != nil || w.launch.kind != codexKind {
		return err
	}
	return s.checkHookReview(ctx, w)
}

// codexStartupHint names the startup screens a new Codex can stop at.
const codexStartupHint = ": Codex may be at its hook review or its directory-trust prompt; start `codex --no-daemon` once in a herdr pane in the repo, trust the hooks and the folder, then spawn again"

// hookReview matches Codex's startup hook review, which herdr does not report
// as blocked: a worker there looks ready, and a prompt typed into it picks a
// choice.
const hookReview = `Hooks need review|Trust all and continue`

// hookReviewWait is how long the screen is read for the hook review.
const hookReviewWait = 500 * time.Millisecond

// checkHookReview fails a Codex worker whose screen shows the hook review.
func (s *Service) checkHookReview(ctx context.Context, w opened) error {
	_, err := s.Herdr.WaitForOutput(ctx, w.paneID, hookReview, hookReviewWait)
	switch {
	case err == nil:
		return fmt.Errorf("%s is at Codex's hook review%s", w.id, codexStartupHint)
	case errors.Is(err, herdr.ErrWaitTimeout):
		return nil
	}
	return herdrFailed("read "+w.id+"'s screen", err)
}

// keySender sends the configured keys to a worker's pane at most once, and
// only when a program other than the shell has its terminal: keys typed at
// the shell itself would answer whatever the shell is asking.
type keySender struct {
	s       *Service
	w       opened
	cfg     settings.Spawn
	started time.Time
	sent    bool
}

// maybe sends the keys when they are due.
func (k *keySender) maybe(ctx context.Context) {
	if k.sent || len(k.cfg.ShellReadyKeys) == 0 || time.Since(k.started) < k.cfg.ShellReadyAfter {
		return
	}
	info, err := k.s.Herdr.PaneProcessInfo(ctx, k.w.paneID)
	if err != nil || info.ShellPID == 0 || info.ForegroundProcessGroupID == info.ShellPID {
		return
	}
	k.sent = true
	_ = k.s.Herdr.SendKeys(ctx, k.w.paneID, k.cfg.ShellReadyKeys)
}

// waitForPrompt waits for the shell prompt, sending the keys when due.
func (s *Service) waitForPrompt(ctx context.Context, cfg settings.Spawn, w opened, keys *keySender) error {
	step := max(cfg.ShellReadyAfter, time.Second)
	for {
		wait := step
		if d, ok := ctx.Deadline(); ok {
			wait = min(wait, time.Until(d))
		}
		if wait <= 0 {
			return fmt.Errorf("no line of %s's screen matched spawn.shell_prompt %q in time: %w", w.paneID, cfg.ShellPrompt, context.DeadlineExceeded)
		}
		_, err := s.Herdr.WaitForOutput(ctx, w.paneID, cfg.ShellPrompt, wait)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, herdr.ErrWaitTimeout):
			keys.maybe(ctx)
		case ctx.Err() != nil:
			return fmt.Errorf("no line of %s's screen matched spawn.shell_prompt %q in time: %w", w.paneID, cfg.ShellPrompt, ctx.Err())
		default:
			return herdrFailed(fmt.Sprintf("wait for %s's shell prompt, spawn.shell_prompt %q", w.paneID, cfg.ShellPrompt), err)
		}
	}
}

// start calls agent.start until the pane's shell takes it.
func (s *Service) start(ctx context.Context, w opened, keys *keySender) error {
	deadline := time.Now().Add(orDefault(s.BusyWindow, DefaultBusyWindow))
	timeout := agentStartTimeout
	if d, ok := ctx.Deadline(); ok {
		timeout = max(min(timeout, time.Until(d)), minAgentStartTimeout)
	}
	for {
		a, err := s.Herdr.StartAgent(ctx, herdr.AgentStart{Name: w.id, Kind: w.launch.kind, PaneID: w.paneID, Args: w.launch.args, Timeout: timeout})
		switch {
		case err == nil:
			if a.TerminalID != w.terminalID {
				return fmt.Errorf("agent.start answered for terminal %s, not %s's", a.TerminalID, w.id)
			}
			return nil
		case errors.Is(err, herdr.ErrPaneBusy) && time.Now().Before(deadline):
			keys.maybe(ctx)
			if err := sleep(ctx, orDefault(s.ReadyPoll, DefaultReadyPoll)); err != nil {
				return fmt.Errorf("%s's shell never took the agent: %w", w.paneID, err)
			}
			// agent.start takes only a pane id, which herdr can reissue, so
			// the pane is re-read before each retry, as herdr's CLI does.
			p, err := s.Herdr.GetPane(ctx, w.paneID)
			switch {
			case err != nil:
				return herdrFailed("re-read "+w.id+"'s pane", err)
			case p.TerminalID != w.terminalID:
				return fmt.Errorf("%s is no longer %s's pane", w.paneID, w.id)
			}
		default:
			return herdrFailed("start "+w.id, err)
		}
	}
}

// waitReady polls agent.get until the agent is ready or fails, as herdr's CLI
// does (src/cli/agent.rs:562-638 at v0.9.3): an error answer or an unreachable
// herdr is polled again until ctx ends, and any other failure ends the wait.
func (s *Service) waitReady(ctx context.Context, w opened) error {
	for {
		a, err := s.Herdr.GetAgent(ctx, w.id)
		switch {
		case err == nil:
			done, err := ready(w, a)
			if done || err != nil {
				return err
			}
		case !errors.Is(err, herdr.ErrAPI) && !errors.Is(err, herdr.ErrUnavailable) && ctx.Err() == nil:
			return herdrFailed("ask herdr about "+w.id, err)
		}
		if err := sleep(ctx, orDefault(s.ReadyPoll, DefaultReadyPoll)); err != nil {
			return fmt.Errorf("%s was not ready in time: %w", w.id, err)
		}
	}
}

// ready reads one agent.get: done with no error when the agent is up, done
// with an error when it never will be, not done while it is still starting.
func ready(w opened, a herdr.Agent) (bool, error) {
	switch {
	case a.TerminalID != w.terminalID, a.Name != w.id:
		return true, fmt.Errorf("the agent named %s no longer owns its terminal", w.id)
	case a.Agent != "" && a.Agent != w.launch.kind:
		return true, fmt.Errorf("expected %s, herdr detected %s", w.launch.kind, a.Agent)
	}
	switch a.AgentStatus {
	case "blocked":
		err := fmt.Errorf("%s is blocked during startup and is not ready for prompts", w.id)
		if w.launch.kind == codexKind {
			err = fmt.Errorf("%w%s", err, codexStartupHint)
		}
		return true, err
	case "unknown":
		return w.launch.kind == codexKind && a.InteractiveReady, nil
	case "idle", "done":
		if a.InteractiveReady {
			return true, nil
		}
		if !a.LaunchPending {
			return true, fmt.Errorf("%s's agent exited before it became interactive", w.id)
		}
	}
	return false, nil
}

// occupied is how many of a type's slots are taken: every pane of the type's
// group running an agent, recorded or not (a worker started by hand counts),
// and every pane a live row binds there, whose agent may still be starting.
func (st state) occupied(typ string) int {
	n := 0
	for _, g := range st.model.Groups {
		if g.Name != typ {
			continue
		}
		for _, w := range g.Workers {
			if w.Bound || w.Pane.IsAgent() {
				n++
			}
		}
	}
	return n
}
