package settings

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
)

// OpTimeout bounds one `op read`.
const OpTimeout = 10 * time.Second

// opWaitDelay bounds how long OpRead waits for op's output pipes to close
// after op is killed, in case it left a child holding them.
const opWaitDelay = time.Second

// opScheme prefixes a 1Password secret reference.
const opScheme = "op://"

// opMinSegments is vault/item/field; a section between item and field is
// allowed.
const opMinSegments = 3

// maxReasonLen bounds how much of op's stderr an error carries.
const maxReasonLen = 200

var envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

const refHint = `want { env = "NAME" } or { op = "op://vault/item/field" }`

// SecretRef points at a secret without holding it: exactly one of Env (an
// environment variable name) or Op (a 1Password op:// reference) is set.
type SecretRef struct {
	Env string `json:"env,omitempty"`
	Op  string `json:"op,omitempty"`
}

// ParseSecretRef reads a decoded TOML value as a secret reference: an inline
// table with exactly one key, env (a variable name) or op (an
// op://vault/item/[section/]field reference). Anything else, the secret
// itself above all, is ErrSecretValue; the error never repeats the value.
func ParseSecretRef(v any) (SecretRef, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return SecretRef{}, fmt.Errorf("%w: got %s; %s", ErrSecretValue, tomlType(v), refHint)
	}
	if len(m) != 1 {
		got := "no keys"
		if len(m) > 0 {
			got = strings.Join(slices.Sorted(maps.Keys(m)), ", ")
		}
		return SecretRef{}, fmt.Errorf("%w: want exactly one of env or op, got %s; %s", ErrSecretValue, got, refHint)
	}
	var key string
	for k := range m {
		key = k
	}
	s, isString := m[key].(string)
	switch {
	case key != "env" && key != "op":
		return SecretRef{}, fmt.Errorf("%w: unknown key %q; %s", ErrSecretValue, key, refHint)
	case !isString:
		return SecretRef{}, fmt.Errorf("%w: %s must be a string, got %s", ErrSecretValue, key, tomlType(m[key]))
	case key == "env":
		if !envNamePattern.MatchString(s) {
			return SecretRef{}, fmt.Errorf("%w: env must name an environment variable matching %s", ErrSecretValue, envNamePattern)
		}
		return SecretRef{Env: s}, nil
	default:
		if err := checkOpRef(s); err != nil {
			return SecretRef{}, err
		}
		return SecretRef{Op: s}, nil
	}
}

// checkOpRef checks the shape of a 1Password secret reference. It does not
// repeat the reference: a malformed one may be a pasted secret.
func checkOpRef(ref string) error {
	rest, ok := strings.CutPrefix(ref, opScheme)
	segments := strings.Split(rest, "/")
	if !ok || len(segments) < opMinSegments || slices.Contains(segments, "") || strings.ContainsFunc(ref, unicode.IsControl) {
		return fmt.Errorf("%w: op must be op://vault/item/[section/]field with no empty part and no control character", ErrSecretValue)
	}
	return nil
}

// String renders the reference, env:NAME or the op:// URI, never the secret.
func (r SecretRef) String() string {
	if r.Env != "" {
		return "env:" + r.Env
	}
	return r.Op
}

// Resolve returns the secret: the variable read through lookupEnv
// (os.LookupEnv when nil), or the reference read through opRead. opRead is
// never defaulted, so only a caller that chose to run op does; nil makes an
// op reference unavailable. An unset or empty variable, and an opRead that
// fails or returns nothing, are ErrSecretUnavailable.
func (r SecretRef) Resolve(ctx context.Context, lookupEnv func(string) (string, bool), opRead func(context.Context, string) (string, error)) (string, error) {
	switch {
	case r.Env != "" && r.Op == "":
		if lookupEnv == nil {
			lookupEnv = os.LookupEnv
		}
		v, ok := lookupEnv(r.Env)
		if !ok {
			return "", fmt.Errorf("%w: %s is not set", ErrSecretUnavailable, r.Env)
		}
		if v == "" {
			return "", fmt.Errorf("%w: %s is empty", ErrSecretUnavailable, r.Env)
		}
		return v, nil
	case r.Op != "" && r.Env == "":
		if opRead == nil {
			return "", fmt.Errorf("%w: %s: no 1Password reader", ErrSecretUnavailable, r.Op)
		}
		v, err := opRead(ctx, r.Op)
		if err != nil {
			return "", fmt.Errorf("%w: %s: %w", ErrSecretUnavailable, r.Op, err)
		}
		if v == "" {
			return "", fmt.Errorf("%w: %s: 1Password returned an empty value", ErrSecretUnavailable, r.Op)
		}
		return v, nil
	default:
		return "", fmt.Errorf("%w: a reference needs exactly one of env or op", ErrSecretUnavailable)
	}
}

// OpRead reads a 1Password secret reference by running
// `op read --no-newline <ref>` under OpTimeout. It is the opRead the CLI
// wires in; nothing else runs op. ref must be an op:// reference, so it can
// never reach op as a flag. An error carries the first line of op's stderr,
// never its stdout.
func OpRead(ctx context.Context, ref string) (string, error) {
	if err := checkOpRef(ref); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, OpTimeout)
	defer cancel()

	// #nosec G204 -- a fixed program and verb; ref is one argv element, checked above to be an op:// reference, and no shell is involved.
	cmd := exec.CommandContext(ctx, "op", "read", "--no-newline", ref)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = opWaitDelay
	killGroup(cmd)
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("op read: %w", ctx.Err())
		}
		if reason := firstLine(stderr.String()); reason != "" {
			return "", fmt.Errorf("op read: %w: %s", err, reason)
		}
		return "", fmt.Errorf("op read: %w", err)
	}
	return stdout.String(), nil
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	if len(line) > maxReasonLen {
		line = line[:maxReasonLen]
	}
	// The cut can split a character; an error must stay valid UTF-8.
	return strings.TrimSpace(strings.ToValidUTF8(line, ""))
}

// SecretStatus is whether one secret reference resolves. Err is nil when it
// does; the value itself is never kept.
type SecretStatus struct {
	Path string
	Ref  SecretRef
	Err  error
}

// CheckSecrets resolves every secret reference in r, in path order, and
// discards each value as soon as it is read. The error is
// ErrSecretUnavailable when any is missing; each status says which and why.
func CheckSecrets(ctx context.Context, r Resolved, lookupEnv func(string) (string, bool), opRead func(context.Context, string) (string, error)) ([]SecretStatus, error) {
	var out []SecretStatus
	missing := 0
	for _, l := range r.Leaves() {
		ref, ok := l.Value.(SecretRef)
		if !ok {
			continue
		}
		_, err := ref.Resolve(ctx, lookupEnv, opRead)
		if err != nil {
			missing++
		}
		out = append(out, SecretStatus{Path: l.Path, Ref: ref, Err: err})
	}
	if missing > 0 {
		return out, fmt.Errorf("%w: %d of %d secrets missing", ErrSecretUnavailable, missing, len(out))
	}
	return out, nil
}
