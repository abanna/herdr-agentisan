package daemon

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// socketIdentity stats path with statx, which reports the birth time stat
// cannot.
func socketIdentity(path string) (Identity, bool, error) {
	var st unix.Statx_t
	err := unix.Statx(unix.AT_FDCWD, path, 0, unix.STATX_TYPE|unix.STATX_INO|unix.STATX_BTIME|unix.STATX_CTIME, &st)
	if err != nil {
		return Identity{}, false, fmt.Errorf("statx %s: %w", path, err)
	}
	born := st.Ctime
	if st.Mask&unix.STATX_BTIME != 0 {
		born = st.Btime
	}
	return Identity{
		Dev:  unix.Mkdev(st.Dev_major, st.Dev_minor),
		Ino:  st.Ino,
		Born: born.Sec*1e9 + int64(born.Nsec),
	}, st.Mode&unix.S_IFMT == unix.S_IFSOCK, nil
}
