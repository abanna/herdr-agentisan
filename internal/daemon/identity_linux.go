package daemon

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// statxHook, when set, stands in for the statx call: a test makes statx fail
// the way an old kernel or a seccomp profile does.
var statxHook func(path string) error

// socketIdentity stats path with statx, which reports the birth time stat
// cannot. Where statx itself is unavailable (ENOSYS before Linux 4.11, EPERM
// under older container seccomp profiles) it falls back to stat, with the
// change time standing in for the birth time: herdr runs on such hosts, so
// the daemon must too.
func socketIdentity(path string) (Identity, bool, error) {
	var st unix.Statx_t
	err := statx(path, &st)
	switch {
	case err == nil:
		born := st.Ctime
		if st.Mask&unix.STATX_BTIME != 0 {
			born = st.Btime
		}
		return Identity{
			Dev:  unix.Mkdev(st.Dev_major, st.Dev_minor),
			Ino:  st.Ino,
			Born: born.Sec*1e9 + int64(born.Nsec),
		}, st.Mode&unix.S_IFMT == unix.S_IFSOCK, nil
	case errors.Is(err, unix.ENOENT):
		return Identity{}, false, fmt.Errorf("statx %s: %w", path, err)
	}
	return statIdentity(path)
}

func statx(path string, st *unix.Statx_t) error {
	if statxHook != nil {
		if err := statxHook(path); err != nil {
			return err
		}
	}
	return unix.Statx(unix.AT_FDCWD, path, 0, unix.STATX_TYPE|unix.STATX_INO|unix.STATX_BTIME|unix.STATX_CTIME, st) //nolint:wrapcheck // the caller wraps
}

// statIdentity is socketIdentity through stat(2), which every Linux has.
func statIdentity(path string) (Identity, bool, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return Identity{}, false, fmt.Errorf("stat %s: %w", path, err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return Identity{}, false, fmt.Errorf("stat %s: no system stat", path)
	}
	return Identity{Dev: st.Dev, Ino: st.Ino, Born: st.Ctim.Nano()}, fi.Mode()&os.ModeSocket != 0, nil
}
