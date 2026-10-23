package workforce

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/abanna/herdr-agentisan/internal/debugger"
	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/routing"
	"github.com/abanna/herdr-agentisan/internal/settings"
	"github.com/abanna/herdr-agentisan/internal/store"
)

// SpawnRequest is worker spawn: Count new workers of type Group.
type SpawnRequest struct {
	Selector
	Group string `json:"group"`
	Count int    `json:"count"`
	// Model overrides the type's model for these workers. While routing is
	// on (A50) it must name a binding the type's role allows, by its name or
	// model id.
	Model string `json:"model,omitempty"`
	// Effort overrides the effort they start at.
	Effort string `json:"effort,omitempty"`
	// Task is the boss's task packet, which routing reads; nil for none.
	Task *routing.Task `json:"task,omitempty"`
}

// Spawned is one worker whose agent came up.
type Spawned struct {
	ID         string `json:"id"`
	Group      string `json:"group"`
	PaneID     string `json:"pane_id"`
	Generation int64  `json:"generation"`
	// Agent is herdr's kind for it: claude or codex.
	Agent string `json:"agent"`
	// Binding, Model and Effort are what routing chose for it, and
	// FallbackReason why that was the role's default; empty while routing
	// is off.
	Binding        string         `json:"binding,omitempty"`
	Model          string         `json:"model,omitempty"`
	Effort         string         `json:"effort,omitempty"`
	FallbackReason routing.Reason `json:"fallback_reason,omitempty"`
}

// Routed is one routing decision, applied or not, and the workers it was
// made for: every worker of a spawn, or of one group team up fills.
type Routed struct {
	// Op is the command that asked: "worker spawn" or "team up".
	Op             string
	Project, Group string
	IDs            []string
	// Decision is nil when routing, off, would have refused the spawn:
	// Refused says why.
	Decision *routing.Decision
	Refused  error
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
	if req.Effort != "" && !effortPattern.MatchString(req.Effort) {
		return SpawnResult{}, fmt.Errorf("%w: --effort is not 1 to 16 lowercase letters", ErrBadRequest)
	}
	if req.Task != nil {
		if err := req.Task.CheckBounds(); err != nil {
			return SpawnResult{}, fmt.Errorf("%w: %w", ErrBadRequest, err)
		}
	}
	// The preflight can take seconds, so it runs before the lock: it reads
	// the host, not the team. So does a debugger's launch, which reads the
	// env file's name and stats it, and every MCP config above the repo.
	_, p, _, err := s.resolve(ctx, req.Selector)
	if err != nil {
		return SpawnResult{}, err
	}
	var dbg *debugger.Command
	if g, ok := p.Groups[req.Group]; ok && !g.DescribesOnly && req.Group == settings.DebuggerType {
		c, err := s.debuggerLaunch(p.Repo)
		if err != nil {
			return SpawnResult{}, configFailed(err)
		}
		dbg = &c
	}
	if err := s.preflight(ctx, p.Repo); err != nil {
		return SpawnResult{}, err
	}
	// Routing also runs before the lock: Jev's answer can take seconds.
	d, refused, err := s.route(p, req)
	if err != nil {
		return SpawnResult{}, err
	}
	if d != nil {
		*d = s.askJev(ctx, p, *d, req.Task)
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
		c := choice{model: cmp.Or(req.Model, st.profile.AgentFor(req.Group).Model), effort: req.Effort}
		return planRouted(st.profile, req.Group, c, d, ids, dbg)
	})
	if err != nil {
		return SpawnResult{}, err
	}
	s.tellRouted(ctx, Routed{Op: "worker spawn", Project: st.team.Project, Group: req.Group, Decision: d, Refused: refused}, pending)
	workers, err := s.bringUp(ctx, st.profile.Spawn, pending)
	for i := range workers {
		routedResult(&workers[i], d)
	}
	return SpawnResult{Project: st.team.Project, Workers: workers, Warnings: s.warnings(pending)}, err
}

// route asks routing for the spawn's model and effort (A50), on the profile
// the preflight checked: one decision for every worker of the spawn. While
// routing is off the spawn launches as it would without it, so the decision
// is what routing would have chosen, and a refusal is only recorded, as
// refused; a malformed task packet is refused either way.
func (s *Service) route(p *settings.Profile, req SpawnRequest) (d *routing.Decision, refused, err error) {
	if g, ok := p.Groups[req.Group]; !ok || g.DescribesOnly {
		return nil, nil, fmt.Errorf("%w: %q is not a group of %s; configured: %s", ErrUnknownGroup, req.Group, p.Name, strings.Join(configured(p), ", "))
	}
	policy, err := p.Policy()
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %w", ErrConfig, err)
	}
	role, err := routing.RoleFor(req.Group)
	var dec routing.Decision
	if err == nil {
		r := routing.Request{Role: role, Provider: kindOf(p.AgentFor(req.Group)), Task: req.Task, Type: req.Group}
		if policy.Enabled() {
			r.Model, r.Effort = req.Model, req.Effort
		}
		dec, err = policy.Resolve(r, s.now())
	}
	switch {
	case err == nil:
		return &dec, nil, nil
	case errors.Is(err, routing.ErrInvalid):
		return nil, nil, fmt.Errorf("%w: %w", ErrBadRequest, err)
	case !policy.Enabled():
		return nil, err, nil //nolint:wrapcheck // recorded as routing's own refusal
	}
	return nil, nil, err //nolint:wrapcheck // routing's sentinels are the refusal, each with its own code
}

// rowOf is what a worker's row records of d: nil unless d chose its launch.
func rowOf(d *routing.Decision) *store.Routing {
	if d == nil || !d.Applied {
		return nil
	}
	r := &store.Routing{
		Role: d.Role, Provider: d.Provider, Binding: d.SelectedBinding, ModelID: d.ModelID, Effort: d.Effort,
		AssignmentDigest: d.AssignmentDigest, PolicyVersion: d.PolicyVersion, RoutedAt: d.Timestamp,
	}
	if o := d.Override; o != nil {
		r.OverrideBinding, r.OverrideBy, r.OverrideEffort = o.Binding, o.By, o.Effort
	}
	return r
}

// planRouted plans ids of typ with base's launch, or with the one d chose
// when routing applied it, and gives each planned row d's record (A50); dbg
// is a debugger's launch, as plan takes it.
// Spawn and team up plan through it, so both launch what routing chose.
func planRouted(p *settings.Profile, typ string, base choice, d *routing.Decision, ids []string, dbg *debugger.Command) ([]planned, error) {
	if d != nil && d.Applied {
		base = choice{model: d.ModelID, effort: d.Effort, provider: d.Provider}
	}
	ps, err := plan(p, typ, base, ids, dbg)
	for i := range ps {
		ps[i].routing = rowOf(d)
	}
	return ps, err
}

// tellRouted hands s.Routed r with the ids of the workers in ws that are of
// r's group; with none, it tells nothing.
func (s *Service) tellRouted(ctx context.Context, r Routed, ws []opened) {
	for _, w := range ws {
		if w.group == r.Group {
			r.IDs = append(r.IDs, w.id)
		}
	}
	if s.Routed != nil && len(r.IDs) > 0 {
		s.Routed(ctx, r)
	}
}

// routedResult names in w the binding, model, effort and fallback reason d
// launched it with, when routing applied d.
func routedResult(w *Spawned, d *routing.Decision) {
	if d != nil && d.Applied {
		w.Binding, w.Model, w.Effort, w.FallbackReason = d.SelectedBinding, d.ModelID, d.Effort, d.FallbackReason
	}
}

// kindOf is the agent an agent command runs: its executable's name.
func kindOf(a settings.Agent) string {
	if len(a.Command) == 0 {
		return ""
	}
	return filepath.Base(a.Command[0])
}

// warnings are what the boss should fix about the workers in ws that did
// not stop them: first each Claude group that starts without the turn
// hooks, then Codex's hooks, which are the machine's.
func (s *Service) warnings(ws []opened) []string {
	return append(settingsWarnings(ws), s.codexWarnings(ws)...)
}

// settingsWarnings warns once for each group whose Claude workers start
// without the turn hooks, since their command passes a --settings of its own
// (A42): the worker runs, but reports its item and stage only through hooks
// the user installed.
func settingsWarnings(ws []opened) []string {
	var groups, out []string
	for _, w := range ws {
		if w.launch.ownSettings && !slices.Contains(groups, w.group) {
			groups = append(groups, w.group)
			out = append(out, fmt.Sprintf("%s's agent command passes a --settings of its own, and Claude Code applies only one, so its workers start without the turn hooks: each reports its item and stage only through hooks you installed", w.group))
		}
	}
	return out
}

// codexWarnings warns when a Codex worker was planned and Codex's ctx hooks
// are not installed (A22, A43), or its turn hooks (A42): the worker runs, but
// reports no ctx, or no item or stage.
func (s *Service) codexWarnings(ws []opened) []string {
	if !slices.ContainsFunc(ws, func(w opened) bool { return w.launch.kind == codexKind }) {
		return nil
	}
	var out []string
	if s.CodexHooks != nil {
		switch installed, err := s.CodexHooks(); {
		case err != nil:
			out = append(out, fmt.Sprintf("could not check Codex's ctx hooks: %v", err))
		case !installed:
			out = append(out, "Codex's ctx hooks are not installed, so a Codex worker reports no ctx: run `herdr-agentisan codex hooks`, then trust them once in Codex")
		}
	}
	if s.CodexTurnHooks != nil {
		switch installed, err := s.CodexTurnHooks(); {
		case err != nil:
			out = append(out, fmt.Sprintf("could not check Codex's turn hooks: %v", err))
		case !installed:
			out = append(out, "Codex's turn hooks are not installed, so a Codex worker reports no item or stage: run `herdr-agentisan codex hooks --turn`, then trust them once in Codex")
		}
	}
	return out
}

// debuggerLaunch builds a debugger worker's launch in repo (A32), from
// s.Debugger, the config dir and the env file its configuration names.
func (s *Service) debuggerLaunch(repo string) (debugger.Command, error) {
	l := s.Debugger
	if l.Root == "" || l.Bin == "" {
		return debugger.Command{}, fmt.Errorf("%w: the daemon was not given the plugin's checkout and binary", debugger.ErrInvalid)
	}
	if err := debugger.CheckRoot(l.Root); err != nil {
		return debugger.Command{}, err //nolint:wrapcheck // the debugger's own error, wrapped by configFailed
	}
	lookup := s.LookupEnv
	if lookup == nil {
		lookup = os.LookupEnv
	}
	keys, err := debugger.KeyFiles(s.ConfigDir, lookup)
	if err != nil {
		return debugger.Command{}, err //nolint:wrapcheck // the debugger's own error, wrapped by configFailed
	}
	l.Repo, l.ConfigDir, l.KeyFiles = repo, s.ConfigDir, keys
	return l.Command() //nolint:wrapcheck // the debugger's own error, wrapped by configFailed
}

// planned is one worker about to be opened.
type planned struct {
	id, group string
	launch    launch
	// routing is the decision its row records; nil when routing did not
	// choose its launch.
	routing *store.Routing
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
		o, left, err := s.open(ctx, &st, p)
		if err != nil {
			return out, st, errors.Join(err, s.abandon(ctx, out, left))
		}
		out = append(out, o)
	}
	return out, st, nil
}

// open opens one worker's tab, or the team's workspace when it has none yet
// (its first tab then becomes the worker's), and binds its row. When the rest
// of an open fails, left is the pane it opened, for abandon to close.
func (s *Service) open(ctx context.Context, st *state, p planned) (opened, *herdr.Created, error) {
	env := maps.Clone(p.launch.env)
	if env == nil {
		env = map[string]string{}
	}
	env[WorkerEnv] = p.id
	var (
		c   herdr.Created
		err error
	)
	if st.workspace == "" {
		c, err = s.Herdr.CreateWorkspace(ctx, herdr.WorkspaceCreate{Cwd: st.profile.Repo, Label: st.team.Project, Env: env})
		if err != nil {
			return opened{}, nil, herdrFailed("create the team's workspace "+st.team.Project, err)
		}
		st.workspace = c.WorkspaceID
		if err := s.Herdr.RenameTab(ctx, c.TabID, p.id); err != nil {
			return opened{}, &c, herdrFailed("label "+p.id+"'s tab", err)
		}
	} else {
		c, err = s.Herdr.CreateTab(ctx, herdr.TabCreate{WorkspaceID: st.workspace, Cwd: st.profile.Repo, Label: p.id, Env: env})
		if err != nil {
			return opened{}, nil, herdrFailed("open "+p.id+"'s tab", err)
		}
	}
	gen, err := s.Store.BindWorker(ctx, store.Worker{LogicalID: p.id, Project: st.team.Project, Group: p.group, PaneID: c.RootPane.PaneID, TerminalID: c.RootPane.TerminalID, Routing: p.routing}, s.now())
	if err != nil {
		return opened{}, &c, fmt.Errorf("record %s: %w", p.id, err)
	}
	return opened{planned: p, paneID: c.RootPane.PaneID, terminalID: c.RootPane.TerminalID, generation: gen}, nil, nil
}

// abandon closes the panes of workers opened before a failure, and marks
// their rows failed, and closes left, the pane the failed open opened with no
// row bound, if any: all at once on one settle context. What failed may be
// the op's own context ending, at a shutdown's cancel or the deadline, and a
// close on that context would fail at once, leaving a pane no row binds whose
// label takes the id; one budget for all of them keeps the lock, and a
// daemon's stop, waiting the settle budget at most (NERD-5350).
func (s *Service) abandon(ctx context.Context, ws []opened, left *herdr.Created) error {
	settle, cancel := settling(ctx)
	defer cancel()
	var leftErr error
	var wg sync.WaitGroup
	if left != nil {
		wg.Go(func() { leftErr = s.closeQuietly(settle, left.RootPane.PaneID, left.RootPane.TerminalID) })
	}
	errs := s.failAll(settle, ws)
	wg.Wait()
	return errors.Join(append(errs, leftErr)...)
}

// settleBudget bounds what an op does once its own deadline may have passed:
// marking the workers that came up active, then cleaning up every other at
// once. It fits in the 5 s the daemon's exchange leaves a reply past the op's
// budget, so an op its deadline ended still answers (QA #78 N1).
const settleBudget = 4 * time.Second

// settling is the context that work runs on, of its own: it runs even when
// the op's deadline is what failed the workers.
func settling(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), settleBudget)
}

// failAll marks each worker's row failed and closes its pane, all at once on
// ctx, so no row is left starting and no pane open; it returns each one's
// error, in ws's order. The row comes first: the store answers in
// milliseconds, while a slow herdr can hold the close past the whole budget,
// and a row left starting would hold its slot until the boss retires it.
func (s *Service) failAll(ctx context.Context, ws []opened) []error {
	errs := make([]error, len(ws))
	var wg sync.WaitGroup
	for i, w := range ws {
		wg.Go(func() {
			failed := ignoreGone(s.Store.SetWorkerStatus(ctx, w.row(), store.StatusFailed, s.now()))
			errs[i] = errors.Join(failed, s.closeQuietly(ctx, w.paneID, w.terminalID))
		})
	}
	wg.Wait()
	return errs
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
	// The rows settle on a context of their own: a sibling that ran the op to
	// its deadline must not fail a worker that is up. Every worker up is
	// marked active before any cleanup runs.
	settle, cancel := settling(ctx)
	defer cancel()
	out := []Spawned{}
	var down []opened
	var why []error
	for i, w := range ws {
		if errs[i] == nil {
			errs[i] = s.Store.SetWorkerStatus(settle, w.row(), store.StatusActive, s.now())
		}
		if errs[i] != nil {
			down, why = append(down, w), append(why, errs[i])
			continue
		}
		out = append(out, Spawned{ID: w.id, Group: w.group, PaneID: w.paneID, Generation: w.generation, Agent: w.launch.kind})
	}
	if len(down) == 0 {
		return out, nil
	}
	failed := s.failAll(settle, down)
	for i, w := range down {
		failed[i] = fmt.Errorf("%s: %w", w.id, errors.Join(why[i], failed[i]))
	}
	return out, fmt.Errorf("%w: %w", ErrSpawnFailed, errors.Join(failed...))
}

// codexKind is herdr's kind for Codex.
const codexKind = "codex"

// noDaemon runs Codex embedded, off the shared app-server, which A22 needs
// for its ctx; Codex refuses the flag twice, so it is added only when absent.
const noDaemon = "--no-daemon"

// launch is how one worker's agent is started: herdr's kind, which picks its
// executable, the arguments after it, and what its tab's shell is given
// besides the worker's id. ownSettings reports a Claude command that passes
// a --settings of its own, so the launch has no turn hooks.
type launch struct {
	kind        string
	args        []string
	env         map[string]string
	ownSettings bool
}

// settingsFlag is Claude Code's --settings. It applies only the last one it
// is given, whole: a launch with two would drop one (Claude Code 2.1.309).
const settingsFlag = "--settings"

// turnHooks is the --settings a Claude worker starts with: A42's two turn
// hooks, each handler as the sample has it
// (docs/config/claude-hooks.example.json). Claude Code adds them to the hooks
// every settings file installs, and runs a handler the user installed too
// only once (Claude Code 2.1.308). herdr single-quotes it in the pane's shell
// (A46), and it holds neither a single quote nor a backslash, which fish
// reads as an escape inside them.
const turnHooks = `{"hooks":{"UserPromptSubmit":[{"hooks":[{"type":"command","command":"herdr-agentisan report turn","timeout":10,"async":true}]}],"Stop":[{"hooks":[{"type":"command","command":"herdr-agentisan report turn","timeout":10,"async":true}]}]}}`

// passesSettings reports whether a Claude command's arguments pass
// --settings, as --settings x or --settings=x; Claude matches a long flag
// whole (A53). Any argument that reads as one counts, a flag's value or a
// prompt included: a launch then goes without the turn hooks, and never has
// two.
func passesSettings(args []string) bool {
	return slices.ContainsFunc(args, func(a string) bool {
		return a == settingsFlag || strings.HasPrefix(a, settingsFlag+"=")
	})
}

// withTurnHooks is the debugger's launch args with A42's turn hooks put into
// the one --settings object they pass, its read fence (A32): Claude Code
// applies only the last --settings it is given, whole, so the hooks cannot
// ride a second one (A42). The fence's own keys are kept, each value as it
// was; a launch with no such object, more than one, or one with hooks of its
// own is refused, since its fence is not the one this knows.
func withTurnHooks(args []string) ([]string, error) {
	i := slices.Index(args, settingsFlag)
	n := 0
	for _, a := range args {
		if a == settingsFlag || strings.HasPrefix(a, settingsFlag+"=") {
			n++
		}
	}
	if i < 0 || i+1 == len(args) || n != 1 {
		return nil, fmt.Errorf("%w: it passes %d --settings flags, not one object", ErrConfig, n)
	}
	var fence map[string]json.RawMessage
	if err := json.Unmarshal([]byte(args[i+1]), &fence); err != nil || fence == nil {
		return nil, fmt.Errorf("%w: its --settings is not a JSON object", ErrConfig)
	}
	if _, ok := fence["hooks"]; ok {
		return nil, fmt.Errorf("%w: its --settings has hooks of its own", ErrConfig)
	}
	var hooks struct {
		Hooks json.RawMessage `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(turnHooks), &hooks); err != nil {
		return nil, fmt.Errorf("%w: the turn hooks: %w", ErrConfig, err)
	}
	fence["hooks"] = hooks.Hooks
	one, err := json.Marshal(fence)
	if err != nil {
		return nil, fmt.Errorf("%w: the debugger's --settings: %w", ErrConfig, err)
	}
	out := slices.Clone(args)
	out[i+1] = string(one)
	return out, nil
}

// choice is the model and effort ("" for none) a spawn's agents start with,
// chosen before plan: routing's decision while it is on, else --model or the
// type's agent, and --effort. provider is the agent routing decided for; ""
// when it did not decide.
type choice struct {
	model, effort, provider string
}

// effortPattern is an effort, such as high or xhigh: safe in any shell.
var effortPattern = regexp.MustCompile(`^[a-z]{1,16}$`)

// plan builds each id's launch: the kind is the command's bare executable,
// which herdr runs from the shell's PATH, and the arguments the command's own,
// for the debugger type its launch's (dbg, A32), --no-daemon for Codex if
// absent, its type's sandbox for a Codex whose command chooses none (A59),
// c's model and effort and, for Claude only, --name with the id (herdr names
// a Codex), then the turn hooks as --settings, unless the command passes its
// own; the debugger's go into its launch's one --settings object instead
// (withTurnHooks). A debugger's tab also gets its launch's environment.
func plan(p *settings.Profile, typ string, c choice, ids []string, dbg *debugger.Command) ([]planned, error) {
	agent := p.AgentFor(typ)
	var env map[string]string
	if typ == settings.DebuggerType {
		if dbg == nil {
			return nil, fmt.Errorf("%w: %s: its launch was not built", ErrConfig, typ)
		}
		args, err := withTurnHooks(dbg.Args)
		if err != nil {
			return nil, fmt.Errorf("%w: %s's launch: %w", ErrConfig, typ, err)
		}
		agent.Command = append(agent.Command, args...)
		env = dbg.Env
	}
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
	if c.provider != "" && kind != c.provider {
		return nil, fmt.Errorf("%w: %s's agent changed from %s to %s while routing chose its model; spawn again", ErrConfig, typ, c.provider, kind)
	}
	// own is a command passing a --settings of the user's, which starts
	// without the turn hooks; the debugger's is the plugin's fence, which
	// carries them already.
	hooked := typ == settings.DebuggerType
	own := kind == "claude" && !hooked && passesSettings(agent.Command[1:])
	out := make([]planned, 0, len(ids))
	for _, id := range ids {
		args := slices.Clone(agent.Command[1:])
		if kind == codexKind && !slices.Contains(args, noDaemon) {
			args = append(args, noDaemon)
		}
		if mode := settings.SandboxFor(typ); kind == codexKind && mode != "" && !settings.ChoosesSandbox(args) {
			args = append(args, "--sandbox", mode)
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
			if !own && !hooked {
				args = append(args, settingsFlag, turnHooks)
			}
		}
		out = append(out, planned{id: id, group: typ, launch: launch{kind: kind, args: args, env: env, ownSettings: own}})
	}
	return out, nil
}

// startAndWait brings one worker's agent up, within ctx: it waits for the
// shell prompt when one is configured (herdr only sees which program has the
// terminal, and a shell asking a question still has it), starts the agent,
// retrying a busy pane, then waits for herdr to report it ready and, for a
// Codex, for its screen to show its composer and no hook review (A65).
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
	if err := s.checkComposer(ctx, w); err != nil {
		return err
	}
	return s.checkHookReview(ctx, w)
}

// codexStartupHint names the startup screens a new Codex can stop at, and the
// one step past all three.
const codexStartupHint = ": Codex may be at its update prompt, its hook review or its directory-trust prompt; start `codex --no-daemon` once in a herdr pane in the repo, choose \"Skip until next version\" (or update Codex), trust the hooks and the folder, then spawn again"

// hookReview matches Codex's startup hook review, which herdr does not report
// as blocked: a worker there looks ready, and a prompt typed into it picks a
// choice.
const hookReview = `Hooks need review|Trust all and continue`

// hookReviewWait is how long the screen is read for the hook review.
const hookReviewWait = 500 * time.Millisecond

// composer matches Codex's empty composer (Codex 0.163.0
// tui/src/chatwidget.rs:2079), which a Codex at any other startup screen does
// not show, though herdr reads most of them as idle (A65).
const composer = `Ask Codex to do anything`

// composerWait is how long the screen is read for the composer. A Codex that
// started well shows it before herdr says it is ready, so the read answers at
// once.
const composerWait = 1500 * time.Millisecond

// hookReviewLine is a line of the hook review.
var hookReviewLine = regexp.MustCompile(hookReview)

// checkComposer fails a Codex worker whose screen does not show its composer
// in time, or shows the hook review. herdr answers the first matching line
// from the top, and the review clears the whole screen, so the two are never
// on it together.
func (s *Service) checkComposer(ctx context.Context, w opened) error {
	line, err := s.Herdr.WaitForOutput(ctx, w.paneID, composer+"|"+hookReview, composerWait)
	switch {
	case err == nil && hookReviewLine.MatchString(line):
		return fmt.Errorf("%s is at Codex's hook review%s", w.id, codexStartupHint)
	case err == nil:
		return nil
	case errors.Is(err, herdr.ErrWaitTimeout):
		return fmt.Errorf("%s never showed Codex's composer%s", w.id, codexStartupHint)
	}
	return callFailed(ctx, "read "+w.id+"'s screen", err)
}

// checkHookReview fails a Codex worker whose screen shows the hook review. It
// runs after checkComposer, since Codex's startup draft shows the same
// composer before the review (A65).
func (s *Service) checkHookReview(ctx context.Context, w opened) error {
	_, err := s.Herdr.WaitForOutput(ctx, w.paneID, hookReview, hookReviewWait)
	switch {
	case err == nil:
		return fmt.Errorf("%s is at Codex's hook review%s", w.id, codexStartupHint)
	case errors.Is(err, herdr.ErrWaitTimeout):
		return nil
	}
	return callFailed(ctx, "read "+w.id+"'s screen", err)
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
		case outOfTime(ctx, err) != nil:
			return fmt.Errorf("no line of %s's screen matched spawn.shell_prompt %q in time: %w", w.paneID, cfg.ShellPrompt, outOfTime(ctx, err))
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
				return callFailed(ctx, "re-read "+w.id+"'s pane", err)
			case p.TerminalID != w.terminalID:
				return fmt.Errorf("%s is no longer %s's pane", w.paneID, w.id)
			}
		default:
			return callFailed(ctx, "start "+w.id, err)
		}
	}
}

// outOfTime is why ctx ended a herdr call that failed with err: ctx's own
// error, or context.DeadlineExceeded when the call ran into ctx's deadline
// before ctx's timer marked it done (QA #78 R1); nil when ctx did not end it.
// A call that times out on its own, before ctx's deadline, is herdr's.
func outOfTime(ctx context.Context, err error) error {
	if late := ctx.Err(); late != nil {
		return late //nolint:wrapcheck // the caller wraps it with what ran out of time
	}
	if d, ok := ctx.Deadline(); ok && errors.Is(err, context.DeadlineExceeded) && !time.Now().Before(d) {
		return context.DeadlineExceeded
	}
	return nil
}

// callFailed is a failed herdr call made to do what: out of time when ctx
// ended it, else ErrHerdr.
func callFailed(ctx context.Context, what string, err error) error {
	if late := outOfTime(ctx, err); late != nil {
		return fmt.Errorf("%s: out of time: %w", what, late)
	}
	return herdrFailed(what, err)
}

// waitReady polls agent.get until the agent is ready or fails, as herdr's CLI
// does (src/cli/agent.rs:562-638 at v0.9.3): an error answer, an unreachable
// herdr or a call that ran out of time is polled again until ctx ends, and any
// other failure ends the wait.
func (s *Service) waitReady(ctx context.Context, w opened) error {
	for {
		a, err := s.Herdr.GetAgent(ctx, w.id)
		switch {
		case err == nil:
			done, err := ready(w, a)
			if done || err != nil {
				return err
			}
		case !errors.Is(err, herdr.ErrAPI) && !errors.Is(err, herdr.ErrUnavailable) && !errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil:
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
