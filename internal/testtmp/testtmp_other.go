//go:build !linux

package testtmp

// Use does nothing off Linux: there is no /dev/shm tmpfs to put the tests'
// state on, so it stays in TMPDIR.
func Use() {}
