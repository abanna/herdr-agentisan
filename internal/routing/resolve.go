package routing

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

// The lookup statuses a task packet may carry: whether the boss found the
// task it is spawning for.
const (
	LookupFound       = "found"
	LookupNotFound    = "not_found"
	LookupUnavailable = "unavailable"
)

// Task is the boss's task packet, the rules' task_packet shape. The plugin
// never looks a task up itself.
type Task struct {
	// TaskID is opaque, such as a Linear key; nil when there is none.
	TaskID             *string  `json:"task_id"`
	LookupStatus       string   `json:"task_lookup_status"`
	Title              string   `json:"title"`
	Requirements       string   `json:"requirements"`
	AcceptanceCriteria []string `json:"acceptance_criteria"`
	// WorkerAssignment is the scope delegated to this worker.
	WorkerAssignment string `json:"worker_assignment"`
	// RequestedRole, when set, must be the worker type's role.
	RequestedRole string `json:"requested_role"`
	// ProviderPreference, when set, must be the worker type's provider.
	ProviderPreference string   `json:"provider_preference"`
	RequiredTools      []string `json:"required_tools"`
	PreviousAttempts   string   `json:"previous_attempts"`
	// UserModelOverride must be nil: the user's override is theirs to set
	// in [routing.overrides], never the boss's to pass.
	UserModelOverride *string `json:"user_model_override"`
}

// check checks a packet against the spawn it rides with; CheckBounds checks
// its size.
func (t *Task) check(role, provider string) error {
	switch t.LookupStatus {
	case LookupFound, LookupNotFound, LookupUnavailable:
	default:
		return fmt.Errorf("%w: task_lookup_status %q is not found, not_found or unavailable", ErrInvalid, t.LookupStatus)
	}
	switch {
	case t.UserModelOverride != nil:
		return fmt.Errorf("%w: the packet sets user_model_override; the user's own override is [routing.overrides] in the shared herdr-agentisan.toml", ErrOverrideRefused)
	case t.RequestedRole != "" && t.RequestedRole != role:
		return fmt.Errorf("%w: the packet's requested_role is not the worker type's role, %s", ErrInvalid, role)
	case t.ProviderPreference != "" && t.ProviderPreference != provider:
		return fmt.Errorf("%w: the packet's provider_preference is not %s, which the worker type runs; set the type's agent command to change it", ErrProviderMismatch, provider)
	}
	return nil
}

// digest identifies the assignment without carrying it: the sha256 of the
// assignment, the requirements and each acceptance criterion, each as its
// length and its bytes, so no two assignments share one, whatever bytes
// they hold.
func (t *Task) digest() string {
	h := sha256.New()
	for _, s := range slices.Concat([]string{t.WorkerAssignment, t.Requirements}, t.AcceptanceCriteria) {
		// A hash never fails a write.
		_, _ = fmt.Fprintf(h, "%d:%s", len(s), s)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// Request is one spawn's ask of routing.
type Request struct {
	// Role is the worker type's role (RoleFor).
	Role string
	// Provider is the agent the worker type's command runs, claude or
	// codex. A decision never changes it.
	Provider string
	// Task is the boss's task packet; nil when it sent none.
	Task *Task
	// Model is the boss's override: a binding's name, or its model id. ""
	// asks for none.
	Model string
	// Effort is the boss's effort override; "" asks for none.
	Effort string
	// Type is the worker type asking, which a refusal names; "" names none.
	Type string
}

// worker is how a refusal names the worker type asking.
func (r Request) worker() string {
	if r.Type == "" {
		return "the worker type"
	}
	return "the worker type " + r.Type
}

// Override is an override a decision followed.
type Override struct {
	// Binding is the binding it named; "" when only the effort was set.
	Binding string `json:"binding,omitempty"`
	// By is whose the binding is: "user" for [routing.overrides], "boss"
	// for a spawn's --model.
	By string `json:"by,omitempty"`
	// Effort is the boss's --effort.
	Effort string `json:"effort,omitempty"`
}

// Reason is why a decision took the role's default where the role's
// selection would otherwise ask Jev, or what Jev's answer kept it at.
type Reason string

// The reasons.
const (
	ReasonNone Reason = ""
	// ReasonTaskNotFound: no packet, or the boss did not find the task.
	ReasonTaskNotFound Reason = "task_not_found"
	// ReasonLookupUnavailable: the boss could not look the task up.
	ReasonLookupUnavailable Reason = "lookup_unavailable"
	// ReasonInsufficientAssignment: the packet's assignment is empty.
	ReasonInsufficientAssignment Reason = "insufficient_assignment"
	// ReasonJevDisabled: the assignment is one Jev would classify, and no
	// answer of Jev's is applied (AsksJev): Jev is off, or not asked yet.
	ReasonJevDisabled Reason = "jev_disabled"
	// ReasonJevNoChange: no answer could change the binding, so Jev is not
	// asked: a codex coder's default is already its strong binding at high.
	ReasonJevNoChange Reason = "jev_no_change"
	// ReasonJevFailure: Jev gave no usable answer; jev_error says why.
	ReasonJevFailure Reason = "jev_failure"
	// ReasonJevKeyUnavailable: Jev was not asked, for want of the TypeSafe
	// key (ErrJevKey); jev_error says why.
	ReasonJevKeyUnavailable Reason = "jev_key_unavailable"
	// ReasonJevInsufficient: Jev found the information insufficient to
	// classify the assignment, below the sufficiency threshold.
	ReasonJevInsufficient Reason = "jev_insufficient"
	// ReasonJevUnknown: Jev classified the assignment as unknown.
	ReasonJevUnknown Reason = "jev_unknown"
	// ReasonJevLowConfidence: Jev's complexity is below the confidence
	// threshold. Low confidence never escalates.
	ReasonJevLowConfidence Reason = "jev_low_confidence"
	// ReasonEscalationIneligible: Jev found the assignment demanding, and
	// a filter drops the strong binding it would move to.
	ReasonEscalationIneligible Reason = "escalation_ineligible"
)

// Unchecked are the rules' hard filters the plugin has no data for yet:
// every decision lists them, until the user's activation defines them.
var Unchecked = []string{"tool_compatibility", "context_headroom", "availability", "spending_limits"}

// The overriders.
const (
	byUser = "user"
	byBoss = "boss"
)

// Decision is one routing decision and its audit record: the rules'
// audit_fields, then the selection, whether it was applied and the filters
// left unchecked. It never carries the assignment's text.
type Decision struct {
	TaskID           *string `json:"task_id"`
	TaskLookupStatus string  `json:"task_lookup_status"`
	AssignmentDigest string  `json:"assignment_digest"`
	Role             string  `json:"role"`
	Provider         string  `json:"provider"`
	SelectedBinding  string  `json:"selected_binding"`
	ModelID          string  `json:"model_id"`
	Effort           string  `json:"effort"`
	PolicyVersion    string  `json:"policy_version"`
	// JevModelReturned and Answers are Jev's (ApplyJev), empty unless it
	// answered.
	JevModelReturned string          `json:"jev_model_returned"`
	Answers          json.RawMessage `json:"answers"`
	FallbackReason   Reason          `json:"fallback_reason"`
	// JevError is why Jev gave no usable answer: its error, which never
	// carries the key; "" unless the reason is jev_failure.
	JevError  string    `json:"jev_error"`
	Override  *Override `json:"override"`
	Timestamp time.Time `json:"timestamp"`
	Selection Selection `json:"selection"`
	// Applied is false while routing is off: the decision is what it
	// would have chosen.
	Applied   bool     `json:"applied"`
	Unchecked []string `json:"unchecked"`
}

// Resolve decides a spawn's binding and effort. In order: the role, the
// provider and the packet are checked; the user's override, else the boss's
// --model, else the role's default for the provider, is chosen; then the
// effort, the boss's or the role's; then the filters with data (a model id,
// the binding enabled, the effort supported). An override that fails is
// refused, never swapped for another binding, and a default that fails is
// ErrBlocked. Routing never changes the provider.
func (p Policy) Resolve(req Request, now time.Time) (Decision, error) {
	role, ok := p.rules.roles[req.Role]
	if !ok {
		return Decision{}, fmt.Errorf("%w: %q is not a role of the rules", ErrUnknownRole, req.Role)
	}
	if req.Provider != Claude && req.Provider != Codex {
		return Decision{}, fmt.Errorf("%w: %s runs %q, which is neither claude nor codex", ErrProviderMismatch, req.worker(), req.Provider)
	}
	d := Decision{Role: role.Name, Provider: req.Provider, PolicyVersion: p.version, TaskLookupStatus: LookupNotFound, Timestamp: now.UTC(), Selection: role.Selection, Applied: p.enabled, Unchecked: slices.Clone(Unchecked)}
	if req.Task != nil {
		if err := req.Task.check(role.Name, req.Provider); err != nil {
			return Decision{}, err
		}
		d.TaskID, d.TaskLookupStatus, d.AssignmentDigest = req.Task.TaskID, req.Task.LookupStatus, req.Task.digest()
	}
	name, err := p.choose(role, req, &d)
	if err != nil {
		return Decision{}, err
	}
	b := p.bindings[name]
	if req.Effort != "" {
		if !slices.Contains(b.Efforts, req.Effort) {
			return Decision{}, fmt.Errorf("%w: --effort %q: %s takes %s", ErrOverrideRefused, req.Effort, name, strings.Join(b.Efforts, ", "))
		}
		if d.Override == nil {
			d.Override = &Override{}
		}
		d.Override.Effort = req.Effort
	}
	d.Effort = effortFor(role, b, req.Effort)
	if why := ineligible(b, d.Effort); why != "" {
		if d.Override != nil && d.Override.Binding != "" {
			return Decision{}, fmt.Errorf("%w: %s, which the %s chose for %s, %s", ErrOverrideRefused, name, whose(d.Override.By), role.Name, why)
		}
		return Decision{}, fmt.Errorf("%w: %s's default for %s, %s, %s", ErrBlocked, role.Name, req.Provider, name, why)
	}
	if (d.Override == nil || d.Override.Binding == "") && role.Selection == SelectJevComplexity {
		d.FallbackReason = jevReason(req.Task)
		if up, effort, ok := p.escalation(role, req.Provider, d.Override); d.FallbackReason == ReasonJevDisabled && (!ok || up == name && effort == d.Effort) {
			d.FallbackReason = ReasonJevNoChange
		}
	}
	d.SelectedBinding, d.ModelID = name, b.ModelID
	return d, nil
}

// choose picks the binding: the user's override, the boss's, or the role's
// default for the provider. An override sets d.Override.
func (p Policy) choose(role Role, req Request, d *Decision) (string, error) {
	user := p.overrides[role.Name]
	name, by := user, byUser
	if req.Model != "" {
		boss, err := p.named(role, req.Model)
		if err != nil {
			return "", err
		}
		switch {
		case user != "" && boss != user:
			return "", fmt.Errorf("%w: --model %q: the user's [routing.overrides] sets %s to %s", ErrOverrideRefused, req.Model, role.Name, user)
		case user == "" && p.bindings[boss].Premium:
			return "", fmt.Errorf("%w: %s is premium, which only the user chooses, in [routing.overrides]", ErrOverrideRefused, boss)
		}
		name, by = boss, byBoss
		if user != "" {
			by = byUser
		}
	}
	if name == "" {
		return p.defaultFor(role, req.Provider)
	}
	if b := p.bindings[name]; b.Provider != req.Provider {
		return "", fmt.Errorf("%w: %s, which the %s chose for %s, is a %s binding, and %s runs %s", ErrProviderMismatch, name, whose(by), role.Name, b.Provider, req.worker(), req.Provider)
	}
	d.Override = &Override{Binding: name, By: by}
	return name, nil
}

// named finds the binding the boss's --model names among the role's
// allowed ones: by its name, else by its model id.
func (p Policy) named(role Role, model string) (string, error) {
	hits := []string{model}
	if !slices.Contains(role.Allowed, model) {
		hits = slices.DeleteFunc(slices.Clone(role.Allowed), func(n string) bool { return p.bindings[n].ModelID != model })
	}
	switch {
	case len(hits) == 0:
		return "", fmt.Errorf("%w: --model %q is not a binding %s allows, by name or model id: %s", ErrOverrideRefused, model, role.Name, strings.Join(role.Allowed, ", "))
	case len(hits) > 1:
		return "", fmt.Errorf("%w: --model %q is the model id of %s; name the binding", ErrOverrideRefused, model, strings.Join(hits, " and "))
	}
	return hits[0], nil
}

// defaultFor is the role's default for the provider: its default for that
// provider, or its one default when that runs under the provider. With
// neither the role is blocked under the provider (the user, 2026-10-22: no
// fallback), and the refusal names what the role allows.
func (p Policy) defaultFor(role Role, provider string) (string, error) {
	if name, ok := role.Defaults[provider]; ok {
		return name, nil
	}
	if role.Default != "" && p.bindings[role.Default].Provider == provider {
		return role.Default, nil
	}
	under := slices.DeleteFunc(slices.Clone(role.Allowed), func(n string) bool { return p.bindings[n].Provider != provider })
	allows := fmt.Sprintf("under %s it allows %s, as an override", provider, strings.Join(under, ", "))
	if len(under) == 0 {
		allows = fmt.Sprintf("it allows only %s, none under %s", strings.Join(role.Allowed, ", "), provider)
	}
	return "", fmt.Errorf("%w: %s has no default for %s, and routing never changes the provider; %s", ErrBlocked, role.Name, provider, allows)
}

// effortFor is the effort a binding runs at: the boss's, else the role's
// default, else the binding's first, which every binding has.
func effortFor(role Role, b Binding, asked string) string {
	switch {
	case asked != "":
		return asked
	case role.DefaultEffort != "":
		return role.DefaultEffort
	}
	return b.Efforts[0]
}

// ineligible says why a binding cannot run at an effort, "" when it can:
// the rules' hard filters the plugin has data for.
func ineligible(b Binding, effort string) string {
	switch {
	case b.ModelID == "":
		return "has no model_id (unresolved)"
	case !b.Enabled:
		return "is disabled in [routing.bindings]"
	case !slices.Contains(b.Efforts, effort):
		return fmt.Sprintf("does not take effort %s (it takes %s)", effort, strings.Join(b.Efforts, ", "))
	}
	return ""
}

// jevReason is why a jev_complexity role took its default.
func jevReason(t *Task) Reason {
	if t == nil {
		return ReasonTaskNotFound
	}
	switch {
	case t.LookupStatus == LookupNotFound:
		return ReasonTaskNotFound
	case t.LookupStatus == LookupUnavailable:
		return ReasonLookupUnavailable
	case strings.TrimSpace(t.WorkerAssignment) == "":
		return ReasonInsufficientAssignment
	}
	return ReasonJevDisabled
}

// whose names an overrider in an error.
func whose(by string) string {
	if by == byUser {
		return "user's [routing.overrides]"
	}
	return "boss's --model"
}
