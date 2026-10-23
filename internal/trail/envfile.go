package trail

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"
)

// EnvFileTimeout bounds reading the env file. A file 1Password mounts is a
// named pipe it answers only while it is unlocked: until then a read waits,
// so an unanswered read fails the query instead of hanging the debugger.
const EnvFileTimeout = 10 * time.Second

// maxEnvFile is the most of the env file read: far more than any dotenv
// file holds, small enough that a wrong path cannot fill memory.
const maxEnvFile = 64 << 10

// ReadEnvFile reads the dotenv file at path, within timeout, and returns
// the values of names, and only those: every other line is passed over
// once its name is read, so no other secret in the file is kept. Lines are
// NAME=value, optionally after `export `, with the value optionally in
// single or double quotes; blank lines and # comments are skipped, as is any
// line that is not NAME=value. A name missing from the file, or empty in
// it, is ErrNotConfigured; no error ever carries a value.
func ReadEnvFile(ctx context.Context, path string, names []string, timeout time.Duration) (map[string]string, error) {
	data, err := readBounded(ctx, path, timeout)
	if err != nil {
		return nil, err
	}
	vals, err := parseEnv(data, names)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for _, n := range names {
		if vals[n] == "" {
			return nil, fmt.Errorf("%w: %s has no %s", ErrNotConfigured, path, n)
		}
	}
	return vals, nil
}

// readBounded reads at most maxEnvFile bytes of path within timeout. The
// read runs on its own goroutine: a pipe with no writer blocks in open, which
// no deadline reaches, so on timeout the goroutine is left to the process's
// exit, and its buffered channel lets it finish if the writer comes late.
func readBounded(ctx context.Context, path string, timeout time.Duration) ([]byte, error) {
	type result struct {
		data []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		// #nosec G304 -- path is debugger.env_file or routing.jev.env_file, the user's own file in the shared config, opened read-only.
		f, err := os.Open(path)
		if err != nil {
			ch <- result{err: err}
			return
		}
		defer func() { _ = f.Close() }()
		data, err := io.ReadAll(io.LimitReader(f, maxEnvFile+1))
		ch <- result{data: data, err: err}
	}()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	select {
	case r := <-ch:
		switch {
		case errors.Is(r.err, os.ErrNotExist):
			return nil, fmt.Errorf("%w: %s does not exist", ErrEnvFile, path)
		case r.err != nil:
			return nil, fmt.Errorf("%w: %w", ErrEnvFile, r.err)
		case len(r.data) > maxEnvFile:
			return nil, fmt.Errorf("%w: %s is over %d KiB", ErrEnvFile, path, maxEnvFile>>10)
		}
		return r.data, nil
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("%w: %s did not answer within %s; if 1Password mounts it, is 1Password unlocked?", ErrEnvFile, path, timeout)
		}
		return nil, fmt.Errorf("%w: %w", ErrEnvFile, ctx.Err())
	}
}

// parseEnv returns the values of names from a dotenv file's bytes.
func parseEnv(data []byte, names []string) (map[string]string, error) {
	text := strings.TrimPrefix(string(data), "\ufeff")
	out := map[string]string{}
	for line := range strings.SplitSeq(text, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		name, value, ok := strings.Cut(line, "=")
		name = strings.TrimSpace(name)
		if !ok || !slices.Contains(names, name) {
			continue
		}
		v, err := unquote(strings.TrimSpace(value))
		if err != nil {
			return nil, fmt.Errorf("%w: %s's value %w", ErrEnvFile, name, err)
		}
		out[name] = v
	}
	return out, nil
}

// errQuote is a value that opens a quote it does not close.
var errQuote = errors.New("opens a quote it does not close")

// unquote strips one pair of matching quotes. In double quotes, \" and \\
// stand for " and \; single quotes take the value as written.
func unquote(v string) (string, error) {
	if v == "" || (v[0] != '"' && v[0] != '\'') {
		return v, nil
	}
	q := v[0]
	if len(v) < 2 || v[len(v)-1] != q {
		return "", errQuote
	}
	v = v[1 : len(v)-1]
	if q == '"' {
		v = strings.NewReplacer(`\"`, `"`, `\\`, `\`).Replace(v)
	}
	return v, nil
}
