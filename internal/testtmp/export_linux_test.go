package testtmp

import (
	"io"

	"golang.org/x/sys/unix"
)

// UseOn is Use on base, with the host's calls given.
func UseOn(base string, statfs func(string, *unix.Statfs_t) error, access func(string, uint32) error,
	getenv func(string) string, setenv func(string, string) error, stderr io.Writer,
) error {
	return use(base, sys{statfs: statfs, access: access, getenv: getenv, setenv: setenv, stderr: stderr})
}
