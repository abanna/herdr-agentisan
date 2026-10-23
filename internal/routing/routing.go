// Package routing chooses the model and reasoning effort each spawned worker
// starts with, by the user's routing rules (ADR-001 A50, NERD-5289). The rules
// name the bindings, each a model and the efforts it takes, and give every
// role its allowed bindings, its default and how it selects. The user's
// [routing] supplies the deployment's facts: the real model ids, the efforts
// each client takes, which bindings are usable, and the user's own overrides.
//
// It decides and launches nothing: worker spawn asks it, and records the
// decision. Jev's complexity answer for coders arrives in a later part; until
// then a coder resolves to its default.
package routing

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// The providers: the agent a binding runs under, and the agent a worker
// type's command starts.
const (
	Claude = "claude"
	Codex  = "codex"
)

// Sentinel errors. Callers branch on them with errors.Is.
var (
	// ErrUnknownRole means a worker type or role the rules do not have.
	ErrUnknownRole = errors.New("unknown role")
	// ErrProviderMismatch means an ask for a provider other than the one
	// the worker type's command runs. Routing never switches provider.
	ErrProviderMismatch = errors.New("provider mismatch")
	// ErrOverrideRefused means an override the rules or the deployment do
	// not allow, such as a binding outside the role's allowed list, a
	// premium binding not chosen by the user, or one a filter drops.
	ErrOverrideRefused = errors.New("override refused")
	// ErrBlocked means the role has no eligible binding to fall back on.
	ErrBlocked = errors.New("no eligible binding")
	// ErrInvalid means rules, deployment facts or a task packet that break
	// a rule of their own.
	ErrInvalid = errors.New("invalid routing")
	// ErrJevKey means Jev was not asked for want of the TypeSafe key, such
	// as an env file 1Password has not unlocked: a caller wraps its key's
	// failure in it, and ApplyJev keeps the default as jev_key_unavailable.
	ErrJevKey = errors.New("no TypeSafe key")
)

// groupRoles maps the team model's worker types (A19) onto the rules' roles.
// qa has no role of its own: it verifies a handoff as pre_check does, and
// takes its binding (the user, 2026-10-20). boss is the boss's own seat.
var groupRoles = map[string]string{
	"boss":      "boss",
	"coder":     "coder",
	"precheck":  "pre_check",
	"qa":        "pre_check",
	"review":    "code_review",
	"research":  "researcher",
	"librarian": "clerk",
	"debugger":  "alex_debugger",
}

// RoleFor is the rules' role for a worker type. A type outside the team
// model, such as a group the configuration adds, has none: ErrUnknownRole.
func RoleFor(group string) (string, error) {
	if role, ok := groupRoles[group]; ok {
		return role, nil
	}
	return "", fmt.Errorf("%w: the worker type %q has no role in the routing rules; the types that do are %s", ErrUnknownRole, group, strings.Join(slices.Sorted(maps.Keys(groupRoles)), ", "))
}
