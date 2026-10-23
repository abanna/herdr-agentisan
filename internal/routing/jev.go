package routing

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// Asking Jev is two steps around a call routing never makes (A57): Resolve
// marks a decision Jev should be asked about (AsksJev), the caller sends
// JevRequest's body to TypeSafe with the key it holds, and ApplyJev maps
// the answer, or the failure, onto the decision. Routing never sees the key.

// The template's two questions.
const (
	questionComplexity = "complexity"
	questionSufficient = "information_sufficient"
)

// The complexity choices the template's criteria name.
const (
	ComplexityBounded   = "bounded"
	ComplexityDemanding = "demanding"
	ComplexityUnknown   = "unknown"
)

// escalationEffort is the effort a demanding coder's strong binding runs at:
// "the provider's strong model at high" (the rules' complexity_mapping).
const escalationEffort = "high"

// maxJevError bounds the error a decision records.
const maxJevError = 200

// maxJevModel bounds the model name a decision records.
const maxJevModel = 64

// jevStateKeys are the template's state, in its order: what JevRequest
// fills, so a rules version that asks for more is refused, not half filled.
var jevStateKeys = []string{"worker_assignment", "requirements", "acceptance_criteria", "attempt_evidence"}

// jevModelPattern is the alphabet of the model the template pins.
var jevModelPattern = regexp.MustCompile(`^[a-z][a-z0-9.-]{0,63}$`)

// jevRules are what the rules say of asking Jev: the template's model and
// questions, the thresholds and the timeout.
type jevRules struct {
	model string
	// questions are the template's, compacted, sent as they are.
	questions               json.RawMessage
	confidence, sufficiency float64
	timeout                 time.Duration
}

// jevOf reads the rules' jev_request_template and the numbers of their
// policy, whose prose is for people and is read past.
func jevOf(template, policy json.RawMessage) (jevRules, error) {
	fail := func(format string, args ...any) (jevRules, error) {
		return jevRules{}, fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
	}
	var t struct {
		Model     string                     `json:"model"`
		State     map[string]json.RawMessage `json:"state"`
		Questions json.RawMessage            `json:"questions"`
	}
	if err := decodeStrict(template, &t); err != nil {
		return fail("jev_request_template does not decode: %v", err)
	}
	if !jevModelPattern.MatchString(t.Model) {
		return fail("jev_request_template's model %q is not a model name", t.Model)
	}
	if keys := slices.Sorted(maps.Keys(t.State)); !slices.Equal(keys, slices.Sorted(slices.Values(jevStateKeys))) {
		return fail("jev_request_template's state is %s, not %s", strings.Join(keys, ", "), strings.Join(jevStateKeys, ", "))
	}
	if err := checkQuestions(t.Questions); err != nil {
		return fail("jev_request_template's questions: %v", err)
	}
	var q bytes.Buffer
	// checkQuestions decoded it, so it compacts.
	_ = json.Compact(&q, t.Questions)
	var p struct {
		Confidence  *float64 `json:"confidence_threshold"`
		Sufficiency *float64 `json:"sufficiency_threshold"`
		TimeoutMS   *int     `json:"jev_timeout_ms"`
		Retries     *int     `json:"jev_retries"`
	}
	if err := json.Unmarshal(policy, &p); err != nil {
		return fail("the policy's numbers do not decode: %v", err)
	}
	switch {
	case p.Confidence == nil || *p.Confidence <= 0 || *p.Confidence > 1:
		return fail("the policy's confidence_threshold must be above 0 and at most 1")
	case p.Sufficiency == nil || *p.Sufficiency <= 0 || *p.Sufficiency > 1:
		return fail("the policy's sufficiency_threshold must be above 0 and at most 1")
	case p.TimeoutMS == nil || *p.TimeoutMS < 1 || *p.TimeoutMS > 60_000:
		return fail("the policy's jev_timeout_ms must be 1 to 60000")
	case p.Retries == nil || *p.Retries != 0:
		return fail("the policy's jev_retries must be 0: Jev is asked once")
	}
	return jevRules{model: t.Model, questions: q.Bytes(), confidence: *p.Confidence, sufficiency: *p.Sufficiency, timeout: time.Duration(*p.TimeoutMS) * time.Millisecond}, nil
}

// question is one question of the template.
type question struct {
	Type         string            `json:"type"`
	Instructions string            `json:"instructions"`
	Criteria     map[string]string `json:"criteria"`
}

// checkQuestions checks the template asks exactly what ApplyJev reads: a
// choice of bounded, demanding or unknown, and a noul.
func checkQuestions(raw json.RawMessage) error {
	var qs map[string]question
	if err := decodeStrict(raw, &qs); err != nil {
		return err
	}
	want := map[string]struct {
		typ      string
		criteria []string
	}{
		questionComplexity: {"choice", []string{ComplexityBounded, ComplexityDemanding, ComplexityUnknown}},
		questionSufficient: {"noul", []string{"false", "true"}},
	}
	if keys := slices.Sorted(maps.Keys(qs)); !slices.Equal(keys, slices.Sorted(maps.Keys(want))) {
		return fmt.Errorf("they are %s, not %s and %s", strings.Join(keys, ", "), questionComplexity, questionSufficient)
	}
	for _, name := range slices.Sorted(maps.Keys(want)) {
		q, w := qs[name], want[name]
		switch {
		case q.Type != w.typ:
			return fmt.Errorf("%s is a %q, not a %s", name, q.Type, w.typ)
		case strings.TrimSpace(q.Instructions) == "":
			return fmt.Errorf("%s has no instructions", name)
		case !slices.Equal(slices.Sorted(maps.Keys(q.Criteria)), slices.Sorted(slices.Values(w.criteria))):
			return fmt.Errorf("%s's criteria are not %s", name, strings.Join(w.criteria, ", "))
		}
	}
	return nil
}

// decodeStrict decodes one JSON value, refusing a key v does not name.
func decodeStrict(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(v) //nolint:wrapcheck // the caller says what failed to decode
}

// JevTimeout is how long Jev is given to answer: the rules' jev_timeout_ms.
func (p Policy) JevTimeout() time.Duration { return p.rules.jev.timeout }

// JevRequest is the body Jev is asked with about t: the rules' template,
// its state filled from the packet, the attempt evidence being the
// packet's previous attempts. A packet that is not UTF-8 is refused, so no
// byte of it is silently replaced.
func (p Policy) JevRequest(t *Task) ([]byte, error) {
	if t == nil {
		return nil, fmt.Errorf("%w: there is no task packet to ask Jev about", ErrInvalid)
	}
	for _, s := range slices.Concat([]string{t.WorkerAssignment, t.Requirements, t.PreviousAttempts}, t.AcceptanceCriteria) {
		if !utf8.ValidString(s) {
			return nil, fmt.Errorf("%w: the task packet is not UTF-8", ErrInvalid)
		}
	}
	type state struct {
		WorkerAssignment   string   `json:"worker_assignment"`
		Requirements       string   `json:"requirements"`
		AcceptanceCriteria []string `json:"acceptance_criteria"`
		AttemptEvidence    string   `json:"attempt_evidence"`
	}
	body := struct {
		Model     string          `json:"model"`
		State     state           `json:"state"`
		Questions json.RawMessage `json:"questions"`
	}{
		Model:     p.rules.jev.model,
		State:     state{WorkerAssignment: t.WorkerAssignment, Requirements: t.Requirements, AcceptanceCriteria: append([]string{}, t.AcceptanceCriteria...), AttemptEvidence: t.PreviousAttempts},
		Questions: p.rules.jev.questions,
	}
	// Strings and checked JSON always encode.
	raw, _ := json.Marshal(body)
	return raw, nil
}

// AsksJev reports whether Jev should be asked about d: a jev_complexity
// role's decision with no binding named, whose packet holds an assignment,
// and whose binding escalating could change. Until ApplyJev maps Jev's
// answer onto it, d is the role's default, its reason jev_disabled.
func (d Decision) AsksJev() bool {
	return d.Selection == SelectJevComplexity && d.FallbackReason == ReasonJevDisabled
}

// escalation is the binding and effort a demanding assignment moves a role
// to under provider: the provider's strong binding at high, or at the
// boss's effort when they set one. false when the role allows no such
// binding.
func (p Policy) escalation(role Role, provider string, o *Override) (name, effort string, ok bool) {
	name = provider + "_strong"
	if !slices.Contains(role.Allowed, name) || p.bindings[name].Premium {
		return "", "", false
	}
	effort = escalationEffort
	if o != nil && o.Effort != "" {
		effort = o.Effort
	}
	return name, effort, true
}

// jevChoice and jevNoul are the two answers ApplyJev reads, as a decision
// records them: only these fields, whatever else TypeSafe sends.
type jevChoice struct {
	Type          string             `json:"type"`
	Choice        string             `json:"choice"`
	Confidence    *float64           `json:"confidence"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
}

type jevNoul struct {
	Type string   `json:"type"`
	Noul *float64 `json:"noul"`
}

type jevAnswers struct {
	Complexity jevChoice `json:"complexity"`
	Sufficient jevNoul   `json:"information_sufficient"`
}

// ApplyJev maps Jev's answer onto d, a decision that asks Jev; any other is
// returned as it is. model and answers are what TypeSafe answered, or err
// why it did not. In order:
//   - a failure, a model other than the template's, or answers that are not
//     the two questions' keep the default: jev_failure, with jev_error;
//   - information not sufficient (below the sufficiency threshold), an
//     unknown complexity and a low confidence (below the confidence
//     threshold) keep it, each with its reason;
//   - bounded keeps it, with no reason;
//   - demanding moves to the provider's strong binding at high, or at the
//     boss's effort, unless a filter drops it: escalation_ineligible.
func (p Policy) ApplyJev(d Decision, model string, answers map[string]json.RawMessage, err error) Decision {
	if !d.AsksJev() {
		return d
	}
	fail := func(why string) Decision {
		d.FallbackReason, d.JevError = ReasonJevFailure, clip(why, maxJevError)
		return d
	}
	if err != nil {
		return fail(err.Error())
	}
	d.JevModelReturned = clip(model, maxJevModel)
	if model != p.rules.jev.model {
		return fail("Jev answered as another model than " + p.rules.jev.model)
	}
	a, raw, err := parseAnswers(answers)
	if err != nil {
		return fail(err.Error())
	}
	d.Answers = raw
	switch {
	case *a.Sufficient.Noul < p.rules.jev.sufficiency:
		d.FallbackReason = ReasonJevInsufficient
	case a.Complexity.Choice == ComplexityUnknown:
		d.FallbackReason = ReasonJevUnknown
	case *a.Complexity.Confidence < p.rules.jev.confidence:
		d.FallbackReason = ReasonJevLowConfidence
	case a.Complexity.Choice == ComplexityBounded:
		d.FallbackReason = ReasonNone
	default:
		name, effort, ok := p.escalation(p.rules.roles[d.Role], d.Provider, d.Override)
		if !ok || ineligible(p.bindings[name], effort) != "" {
			d.FallbackReason = ReasonEscalationIneligible
			return d
		}
		d.SelectedBinding, d.ModelID, d.Effort, d.FallbackReason = name, p.bindings[name].ModelID, effort, ReasonNone
	}
	return d
}

// parseAnswers reads the two answers and checks them: exactly the two
// questions, each of its type, a choice the criteria name, numbers from 0
// to 1. raw is what a decision records of them.
func parseAnswers(answers map[string]json.RawMessage) (a jevAnswers, raw json.RawMessage, err error) {
	if keys := slices.Sorted(maps.Keys(answers)); !slices.Equal(keys, []string{questionComplexity, questionSufficient}) {
		return a, nil, fmt.Errorf("the answers are to %s, not %s and %s", strings.Join(keys, ", "), questionComplexity, questionSufficient)
	}
	if json.Unmarshal(answers[questionComplexity], &a.Complexity) != nil || json.Unmarshal(answers[questionSufficient], &a.Sufficient) != nil {
		return a, nil, errors.New("an answer is not a JSON object of its fields")
	}
	c, s := a.Complexity, a.Sufficient
	switch {
	case c.Type != "choice" || s.Type != "noul":
		return a, nil, fmt.Errorf("%s is not a choice, or %s not a noul", questionComplexity, questionSufficient)
	case c.Choice != ComplexityBounded && c.Choice != ComplexityDemanding && c.Choice != ComplexityUnknown:
		return a, nil, fmt.Errorf("%s's choice is not bounded, demanding or unknown", questionComplexity)
	case !unit(c.Confidence) || !unit(s.Noul):
		return a, nil, errors.New("a confidence or noul is missing or outside 0 to 1")
	}
	for k, v := range c.Probabilities {
		if (k != ComplexityBounded && k != ComplexityDemanding && k != ComplexityUnknown) || !unit(&v) {
			return a, nil, fmt.Errorf("%s's probabilities name another choice, or one outside 0 to 1", questionComplexity)
		}
	}
	// Strings and numbers always encode.
	raw, _ = json.Marshal(a)
	return a, raw, nil
}

// unit reports whether f is set and from 0 to 1.
func unit(f *float64) bool { return f != nil && *f >= 0 && *f <= 1 }

// clip cuts s to at most n bytes of valid UTF-8.
func clip(s string, n int) string {
	if len(s) > n {
		s = s[:n]
	}
	return strings.ToValidUTF8(s, "")
}
