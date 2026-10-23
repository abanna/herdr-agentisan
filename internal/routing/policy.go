package routing

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// Facts are what the user's [routing] says of one binding (A50). A nil field
// says nothing, and the rules' value stands.
type Facts struct {
	// ModelID replaces the rules' id; "" marks the binding unresolved.
	ModelID *string `json:"model_id,omitempty"`
	// Efforts replaces the efforts the rules list, such as the ones the
	// user has verified the client takes.
	Efforts []string `json:"efforts,omitempty"`
	// Enabled false takes the binding out of every decision.
	Enabled *bool `json:"enabled,omitempty"`
}

// Deployment is the user's [routing]: whether routing decides a spawn's
// model, the bindings' facts, the user's own overrides, role to binding, and
// what [routing.jev] says of asking Jev.
type Deployment struct {
	Enabled   bool              `json:"enabled"`
	Bindings  map[string]Facts  `json:"bindings,omitempty"`
	Overrides map[string]string `json:"overrides,omitempty"`
	// Jev is nil while [routing.jev] sets none of its facts, so a
	// deployment without them keeps the version it had before them.
	Jev *JevFacts `json:"jev,omitempty"`
}

// JevFacts are what [routing.jev] says of asking Jev (A57): whether a spawn
// asks, and the thresholds in whole percents, a nil one leaving the rules'.
// The env file the key is read from is no fact of routing's: no decision
// depends on where the key is kept.
type JevFacts struct {
	Enabled        bool `json:"enabled"`
	ConfidencePct  *int `json:"confidence_pct,omitempty"`
	SufficiencyPct *int `json:"sufficiency_pct,omitempty"`
}

// CheckJevPct checks a threshold the user sets in whole percents.
func CheckJevPct(pct int) error {
	if pct < 1 || pct > 100 {
		return fmt.Errorf("%w: %d is not a whole percent from 1 to 100", ErrInvalid, pct)
	}
	return nil
}

// CheckFacts checks the user's facts for one binding.
func (r *Rules) CheckFacts(binding string, f Facts) error {
	b, ok := r.bindings[binding]
	if !ok {
		return fmt.Errorf("%w: %q is not a binding of the rules; they have %s", ErrInvalid, binding, strings.Join(r.BindingNames(), ", "))
	}
	if f.ModelID != nil && *f.ModelID != "" && !modelIDPattern.MatchString(*f.ModelID) {
		return fmt.Errorf("%w: model_id is not a model id: a letter or digit, then letters, digits and . _ - : / @ [ ], at most 128 bytes", ErrInvalid)
	}
	if f.Efforts != nil {
		return checkEfforts(b.Provider, f.Efforts)
	}
	return nil
}

// CheckOverride checks the user's override of one role: a binding the role
// allows, and a premium one only where the rules let the user choose it.
func (r *Rules) CheckOverride(role, binding string) error {
	ro, ok := r.roles[role]
	if !ok {
		return fmt.Errorf("%w: %q is not a role of the rules; they have %s", ErrUnknownRole, role, strings.Join(r.RoleNames(), ", "))
	}
	if !slices.Contains(ro.Allowed, binding) {
		return fmt.Errorf("%w: %s allows %s, not %q", ErrOverrideRefused, role, strings.Join(ro.Allowed, ", "), binding)
	}
	return nil
}

// Policy is the rules with the user's deployment applied: what Resolve
// decides by.
type Policy struct {
	rules     *Rules
	enabled   bool
	bindings  map[string]Binding
	overrides map[string]string
	// jevOn is [routing.jev] enabled; thresholds are the rules', or the
	// user's in their place.
	jevOn      bool
	thresholds thresholds
	version    string
}

// thresholds are what ApplyJev compares Jev's answers with.
type thresholds struct{ confidence, sufficiency float64 }

// New applies a deployment to the rules, checking every fact and override.
// An error names the [routing] key it is about.
func New(r *Rules, d Deployment) (Policy, error) {
	p := Policy{rules: r, enabled: d.Enabled, bindings: maps.Clone(r.bindings), overrides: maps.Clone(d.Overrides)}
	p.thresholds = thresholds{confidence: r.jev.confidence, sufficiency: r.jev.sufficiency}
	if j := d.Jev; j != nil {
		p.jevOn = j.Enabled
		for _, t := range []struct {
			key string
			pct *int
			to  *float64
		}{{"confidence_pct", j.ConfidencePct, &p.thresholds.confidence}, {"sufficiency_pct", j.SufficiencyPct, &p.thresholds.sufficiency}} {
			if t.pct == nil {
				continue
			}
			if err := CheckJevPct(*t.pct); err != nil {
				return Policy{}, fmt.Errorf("routing.jev.%s: %w", t.key, err)
			}
			// pct/100 is the double nearest the percent, as the rules'
			// 0.8 is: a threshold met exactly still escalates.
			*t.to = float64(*t.pct) / 100
		}
	}
	for _, name := range slices.Sorted(maps.Keys(d.Bindings)) {
		f := d.Bindings[name]
		if err := r.CheckFacts(name, f); err != nil {
			return Policy{}, fmt.Errorf("routing.bindings.%s: %w", name, err)
		}
		b := p.bindings[name]
		if f.ModelID != nil {
			b.ModelID = *f.ModelID
		}
		if f.Efforts != nil {
			b.Efforts = slices.Clone(f.Efforts)
		}
		if f.Enabled != nil {
			b.Enabled = *f.Enabled
		}
		p.bindings[name] = b
	}
	for _, role := range slices.Sorted(maps.Keys(d.Overrides)) {
		if err := r.CheckOverride(role, d.Overrides[role]); err != nil {
			return Policy{}, fmt.Errorf("routing.overrides.%s: %w", role, err)
		}
	}
	// A Deployment always encodes: its maps have string keys.
	facts, _ := json.Marshal(d)
	sum := sha256.Sum256([]byte(r.digest + "\n" + string(facts)))
	p.version = fmt.Sprintf("v%d-%s", r.SchemaVersion, hex.EncodeToString(sum[:])[:12])
	return p, nil
}

// Enabled reports whether routing decides a spawn's model; when it does
// not, a decision is still made and recorded, as what routing would have
// chosen.
func (p Policy) Enabled() bool { return p.enabled }

// JevEnabled reports whether a spawn asks Jev about a decision that
// AsksJev: only while routing decides the model and [routing.jev] enabled
// is true, so no call is billed for a decision nobody applies (A57).
func (p Policy) JevEnabled() bool { return p.enabled && p.jevOn }

// Version names the rules and the deployment a decision was made by: the
// rules' schema version, then a digest of both.
func (p Policy) Version() string { return p.version }
