package jev

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/abanna/herdr-agentisan/internal/trail"
)

// The key comes from the env file [routing.jev] env_file names (A57): read
// when a spawn asks Jev, never at start, so a key changed in the file, or a
// 1Password unlocked since, is read without a restart.

// KeyVar is the variable the env file holds the key in, a fixed name as the
// debugger's are.
const KeyVar = "TYPESAFE_API_KEY"

const (
	// KeyWait is the most an ask waits for the env file. 1Password answers
	// a file it mounts only while it is unlocked, so an ask with no key by
	// then goes without one.
	KeyWait = time.Second
	// KeyTTL is how long a key read from the file is kept.
	KeyTTL = 10 * time.Minute
)

// noBound is the timeout of a read Keys starts: none to speak of. The read
// ends when the file answers, so it is never abandoned mid-open, and a
// file that never answers holds one read, not one per ask.
const noBound = time.Duration(math.MaxInt64)

// ReadKey reads the key from the env file at path within timeout. Any
// failure, a key that cannot be a Bearer token included, is ErrNoKey; no
// error carries the key or another value of the file.
func ReadKey(ctx context.Context, path string, timeout time.Duration) (string, error) {
	vals, err := trail.ReadEnvFile(ctx, path, []string{KeyVar}, timeout)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrNoKey, err)
	}
	key := vals[KeyVar]
	if err := checkKey(key); err != nil {
		return "", fmt.Errorf("%w: %s's %s", err, path, KeyVar)
	}
	return key, nil
}

// CheckKeyFile reads the key from the env file at path as config resolve
// --check-secrets does, within trail.EnvFileTimeout, and keeps nothing.
func CheckKeyFile(ctx context.Context, path string) error {
	_, err := ReadKey(ctx, path, trail.EnvFileTimeout)
	return err
}

// Keys reads the key for a long-lived caller, the daemon. A key read is kept
// KeyTTL. At most one read of a file is in flight, and it is left to finish
// however long the file takes, while each ask waits at most Wait for it: a
// locked 1Password costs an ask a second, and the daemon one read (two
// goroutines with the default reader: the read and trail's open) for as
// long as it stays locked. Failures are not kept. The zero Keys is ready.
type Keys struct {
	// Read reads the key from the file at path; nil is ReadKey with no
	// bound. An error it returns is wrapped in ErrNoKey.
	Read func(ctx context.Context, path string) (string, error)
	// Now is the clock keys are kept by; nil is time.Now.
	Now func() time.Time
	// Wait bounds each ask's wait; 0 is KeyWait.
	Wait time.Duration

	mu      sync.Mutex
	flights map[string]*keyRead
	path    string
	key     string
	at      time.Time
}

// keyRead is one read of a file; done closes once key and err are set.
type keyRead struct {
	done chan struct{}
	key  string
	err  error
}

// Key is the key in the env file at path: the one kept, if read from path
// less than KeyTTL ago, else the answer of a read, joining one in flight.
// No answer within Wait, or ctx's end first, is ErrNoKey.
func (k *Keys) Key(ctx context.Context, path string) (string, error) {
	wait := cmp.Or(k.Wait, KeyWait)
	k.mu.Lock()
	if k.key != "" && k.path == path && k.now().Sub(k.at) < KeyTTL {
		key := k.key
		k.mu.Unlock()
		return key, nil
	}
	f := k.flights[path]
	if f == nil {
		f = &keyRead{done: make(chan struct{})}
		if k.flights == nil {
			k.flights = map[string]*keyRead{}
		}
		k.flights[path] = f
		// The read outlives the ask that started it: no ask's end ends it.
		go k.read(context.WithoutCancel(ctx), path, f)
	}
	k.mu.Unlock()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-f.done:
		return f.key, f.err
	case <-timer.C:
		return "", fmt.Errorf("%w: %s did not answer within %s; if 1Password mounts it, is 1Password unlocked?", ErrNoKey, path, wait)
	case <-ctx.Done():
		return "", fmt.Errorf("%w: %w", ErrNoKey, ctx.Err())
	}
}

// Forget drops the key kept for path, which TypeSafe refused: the next ask
// reads the file again.
func (k *Keys) Forget(path string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.path == path {
		k.key = ""
	}
}

// read reads path for f, keeps a good key, and ends f.
func (k *Keys) read(ctx context.Context, path string, f *keyRead) {
	read := k.Read
	if read == nil {
		read = func(ctx context.Context, path string) (string, error) { return ReadKey(ctx, path, noBound) }
	}
	key, err := read(ctx, path)
	if err == nil {
		err = checkKey(key)
	}
	if err != nil && !errors.Is(err, ErrNoKey) {
		err = fmt.Errorf("%w: %w", ErrNoKey, err)
	}
	k.mu.Lock()
	if err == nil {
		k.path, k.key, k.at = path, key, k.now()
	} else {
		key = ""
	}
	f.key, f.err = key, err
	delete(k.flights, path)
	k.mu.Unlock()
	close(f.done)
}

func (k *Keys) now() time.Time {
	if k.Now != nil {
		return k.Now()
	}
	return time.Now()
}

// Asker asks System One with the key from an env file: what a spawn asks
// Jev through. A 401 drops the key it sent, so a key rotated in the file is
// read at the next ask, not up to KeyTTL later.
type Asker struct {
	Keys   *Keys
	Client Client
}

// Ask posts body to System One within timeout, with the key from the env
// file at envFile. No key is ErrNoKey, and nothing is sent.
func (a Asker) Ask(ctx context.Context, envFile string, body []byte, timeout time.Duration) (Answer, error) {
	key, err := a.Keys.Key(ctx, envFile)
	if err != nil {
		return Answer{}, err
	}
	c := a.Client
	c.Key = key
	ans, err := c.Ask(ctx, body, timeout)
	if errors.Is(err, ErrUnauthorized) {
		a.Keys.Forget(envFile)
	}
	return ans, err
}
