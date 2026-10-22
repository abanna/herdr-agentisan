package routing

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"regexp"
	"slices"
	"strings"
	"sync"
)

// v1JSON is the user's routing rules v1 (2026-10-10), byte for byte as the
// user gave them: the rules are the spec.
//
//go:embed rules/jev-routing-v1.json
var v1JSON []byte

// RulesSchemaVersion is the only schema_version of the rules this binary reads.
const RulesSchemaVersion = 1

// routingOwner is who the rules say routes: the plugin.
const routingOwner = "herdr_plugin"

// providerEfforts are the reasoning efforts each provider's client takes:
// Claude Code's --effort, low to max, and Codex's model_reasoning_effort,
// minimal to xhigh. A binding lists only its own provider's.
var providerEfforts = map[string][]string{
	Claude: {"low", "medium", "high", "xhigh", "max"},
	Codex:  {"minimal", "low", "medium", "high", "xhigh"},
}

// namePattern is the alphabet of a binding's or a role's name. Names become
// keys of the user's [routing] tables and appear in every decision.
var namePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// modelIDPattern is the alphabet of a model id: it goes to the agent as the
// one argument after --model, so it never starts with a dash and holds no
// space. Brackets allow a context suffix such as [1m].
var modelIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@\[\]-]{0,127}$`)

// Selection is how a role picks its binding.
type Selection string

// The selections the rules define.
const (
	// SelectFixed is always the role's default.
	SelectFixed Selection = "fixed"
	// SelectExplicit is the default unless an override names a binding.
	SelectExplicit Selection = "explicit"
	// SelectJevComplexity asks Jev how demanding the assignment is (a later
	// part); until then it is the default.
	SelectJevComplexity Selection = "jev_complexity"
)

// Binding is one model a role can run, and the efforts it takes.
type Binding struct {
	Name string
	// ModelID is the id the agent is started with; "" is unresolved, and
	// never means "the latest".
	ModelID string
	// Provider is claude or codex: the prefix of the binding's name, as the
	// rules name every binding.
	Provider string
	// Efforts are the reasoning efforts it takes, the first being what a
	// role without a default effort runs it at.
	Efforts []string
	// Premium is a binding named *_premium: only the user's own override
	// chooses one.
	Premium bool
	// Enabled is false when the user's deployment switches it off.
	Enabled bool
}

// Role is one role of the rules.
type Role struct {
	Name    string
	Allowed []string
	// Default is the role's one default; Defaults its default per
	// provider. The rules set exactly one of the two.
	Default  string
	Defaults map[string]string
	// DefaultEffort is "" when the rules give none.
	DefaultEffort string
	Selection     Selection
	// PremiumAllowed is false when the rules forbid the role a premium
	// binding even by the user's choice.
	PremiumAllowed bool
}

// Rules are the user's routing rules, decoded and checked.
type Rules struct {
	SchemaVersion int
	// Status is the rules' own status, such as draft_unvalidated.
	Status   string
	bindings map[string]Binding
	roles    map[string]Role
	// digest is the sha256 of the rules as given, for the policy version.
	digest string
}

// rulesJSON is the rules' document. Strict: a key not named here fails, so a
// rule added to a later version is never silently ignored.
type rulesJSON struct {
	SchemaVersion int                    `json:"schema_version"`
	Status        string                 `json:"status"`
	RoutingOwner  string                 `json:"routing_owner"`
	Bindings      map[string]bindingJSON `json:"bindings"`
	Roles         map[string]roleJSON    `json:"roles"`
	// Jev's request and thresholds are read by a later part; the rest is
	// for the people and agents who read the rules.
	TaskPacket         json.RawMessage `json:"task_packet"`
	JevRequestTemplate json.RawMessage `json:"jev_request_template"`
	Policy             json.RawMessage `json:"policy"`
	Activation         json.RawMessage `json:"activation"`
	BindingNotes       json.RawMessage `json:"binding_notes"`
	ModelSources       json.RawMessage `json:"model_sources"`
}

type bindingJSON struct {
	ModelID string   `json:"model_id"`
	Alias   string   `json:"alias"`
	Efforts []string `json:"efforts"`
}

type roleJSON struct {
	Allowed        []string          `json:"allowed"`
	Default        string            `json:"default"`
	Defaults       map[string]string `json:"defaults"`
	DefaultEffort  string            `json:"default_effort"`
	Selection      Selection         `json:"selection"`
	PremiumAllowed *bool             `json:"premium_allowed"`
	// Role instructions, which stay in agentisan-skills (D9): read past.
	PremiumRequires               json.RawMessage `json:"premium_requires"`
	Scope                         json.RawMessage `json:"scope"`
	CleanupRequires               json.RawMessage `json:"cleanup_requires"`
	OutputRequires                json.RawMessage `json:"output_requires"`
	Skills                        json.RawMessage `json:"skills"`
	AutomaticEscalation           json.RawMessage `json:"automatic_escalation"`
	FindingsRequire               json.RawMessage `json:"findings_require"`
	SecurityOutputPolicy          json.RawMessage `json:"security_output_policy"`
	HostedReviewerModelAssumption json.RawMessage `json:"hosted_reviewer_model_assumption"`
	SessionModelStable            json.RawMessage `json:"session_model_stable"`
}

// v1 decodes the embedded rules once.
var v1 = sync.OnceValues(func() (*Rules, error) { return ParseRules(v1JSON) })

// V1 is the embedded rules v1. Like regexp.MustCompile it panics when they
// do not parse, which cannot happen at run time: they are compiled in, and
// TestV1 decodes them.
func V1() *Rules {
	r, err := v1()
	if err != nil {
		panic(err)
	}
	return r
}

// ParseRules decodes and checks one rules document.
func ParseRules(raw []byte) (*Rules, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var doc rulesJSON
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("%w: the rules do not decode: %w", ErrInvalid, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: the rules hold more than one JSON value", ErrInvalid)
	}
	if doc.SchemaVersion != RulesSchemaVersion {
		return nil, fmt.Errorf("%w: the rules' schema_version is %d; this binary reads %d", ErrInvalid, doc.SchemaVersion, RulesSchemaVersion)
	}
	if doc.RoutingOwner != routingOwner {
		return nil, fmt.Errorf("%w: the rules' routing_owner is %q, not %s", ErrInvalid, doc.RoutingOwner, routingOwner)
	}
	sum := sha256.Sum256(raw)
	r := &Rules{SchemaVersion: doc.SchemaVersion, Status: doc.Status, bindings: map[string]Binding{}, roles: map[string]Role{}, digest: hex.EncodeToString(sum[:])}
	if len(doc.Bindings) == 0 || len(doc.Roles) == 0 {
		return nil, fmt.Errorf("%w: the rules need bindings and roles", ErrInvalid)
	}
	for _, name := range slices.Sorted(maps.Keys(doc.Bindings)) {
		b, err := bindingOf(name, doc.Bindings[name])
		if err != nil {
			return nil, err
		}
		r.bindings[name] = b
	}
	for _, name := range slices.Sorted(maps.Keys(doc.Roles)) {
		role, err := r.roleOf(name, doc.Roles[name])
		if err != nil {
			return nil, err
		}
		r.roles[name] = role
	}
	return r, nil
}

func bindingOf(name string, raw bindingJSON) (Binding, error) {
	if !namePattern.MatchString(name) {
		return Binding{}, fmt.Errorf("%w: binding %q: a name must match %s", ErrInvalid, name, namePattern)
	}
	provider, _, _ := strings.Cut(name, "_")
	if provider != Claude && provider != Codex {
		return Binding{}, fmt.Errorf("%w: binding %s: its name must start with claude_ or codex_, which says its provider", ErrInvalid, name)
	}
	if raw.ModelID != "" && !modelIDPattern.MatchString(raw.ModelID) {
		return Binding{}, fmt.Errorf("%w: binding %s: model_id %q is not a model id", ErrInvalid, name, raw.ModelID)
	}
	if err := checkEfforts(provider, raw.Efforts); err != nil {
		return Binding{}, fmt.Errorf("binding %s: %w", name, err)
	}
	return Binding{Name: name, ModelID: raw.ModelID, Provider: provider, Efforts: slices.Clone(raw.Efforts), Premium: strings.HasSuffix(name, "_premium"), Enabled: true}, nil
}

// checkEfforts checks a binding's efforts: at least one, each one its
// provider's client takes, none twice.
func checkEfforts(provider string, efforts []string) error {
	if len(efforts) == 0 {
		return fmt.Errorf("%w: efforts is empty", ErrInvalid)
	}
	takes := providerEfforts[provider]
	for i, e := range efforts {
		if !slices.Contains(takes, e) {
			return fmt.Errorf("%w: effort %q is not one %s takes: %s", ErrInvalid, e, provider, strings.Join(takes, ", "))
		}
		if slices.Contains(efforts[:i], e) {
			return fmt.Errorf("%w: effort %s is listed twice", ErrInvalid, e)
		}
	}
	return nil
}

func (r *Rules) roleOf(name string, raw roleJSON) (Role, error) {
	fail := func(format string, args ...any) (Role, error) {
		return Role{}, fmt.Errorf("%w: role %s: %s", ErrInvalid, name, fmt.Sprintf(format, args...))
	}
	if !namePattern.MatchString(name) {
		return fail("a name must match %s", namePattern)
	}
	role := Role{Name: name, Allowed: slices.Clone(raw.Allowed), Default: raw.Default, Defaults: maps.Clone(raw.Defaults), DefaultEffort: raw.DefaultEffort, Selection: raw.Selection, PremiumAllowed: raw.PremiumAllowed == nil || *raw.PremiumAllowed}
	switch role.Selection {
	case SelectFixed, SelectExplicit, SelectJevComplexity:
	default:
		return fail("selection %q is not fixed, explicit or jev_complexity", role.Selection)
	}
	if len(role.Allowed) == 0 {
		return fail("allowed is empty")
	}
	for i, b := range role.Allowed {
		binding, ok := r.bindings[b]
		switch {
		case !ok:
			return fail("allowed names %q, which is not a binding", b)
		case slices.Contains(role.Allowed[:i], b):
			return fail("allowed lists %s twice", b)
		case binding.Premium && !role.PremiumAllowed:
			return fail("allowed lists the premium %s, and premium_allowed is false", b)
		}
	}
	if (role.Default == "") == (len(role.Defaults) == 0) {
		return fail("set exactly one of default and defaults")
	}
	defaults := map[string]string{"": role.Default}
	if role.Default == "" {
		defaults = role.Defaults
	}
	for _, provider := range slices.Sorted(maps.Keys(defaults)) {
		b := defaults[provider]
		binding := r.bindings[b]
		switch {
		case !slices.Contains(role.Allowed, b):
			return fail("default %q is not in allowed", b)
		case binding.Premium:
			return fail("default %s is premium, which only the user's override chooses", b)
		case provider != "" && binding.Provider != provider:
			return fail("the %s default %s is a %s binding", provider, b, binding.Provider)
		case role.DefaultEffort != "" && !slices.Contains(binding.Efforts, role.DefaultEffort):
			return fail("default_effort %q is not one %s takes", role.DefaultEffort, b)
		}
	}
	return role, nil
}

// BindingNames are the rules' bindings, sorted.
func (r *Rules) BindingNames() []string { return slices.Sorted(maps.Keys(r.bindings)) }

// Provider is the provider a binding of the rules runs under; "" for a name
// the rules do not have.
func (r *Rules) Provider(binding string) string { return r.bindings[binding].Provider }

// RoleNames are the rules' roles, sorted.
func (r *Rules) RoleNames() []string { return slices.Sorted(maps.Keys(r.roles)) }
