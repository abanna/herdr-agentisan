package daemon

import (
	"fmt"
	"os"
	"syscall"
)

// socketIdentity stats path; darwin's stat carries the birth time.
func socketIdentity(path string) (Identity, bool, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return Identity{}, false, fmt.Errorf("stat %s: %w", path, err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return Identity{}, false, fmt.Errorf("stat %s: no system stat", path)
	}
	return Identity{
		Dev:  uint64(st.Dev), // #nosec G115 -- darwin's dev_t is an int32 and can be negative; it stays sign-extended because daemon.lock and health record it and a later run compares it
		Ino:  st.Ino,
		Born: st.Birthtimespec.Nano(),
	}, fi.Mode()&os.ModeSocket != 0, nil
}
