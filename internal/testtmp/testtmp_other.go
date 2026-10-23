//go:build !linux

package testtmp

import "testing"

// Use does nothing off Linux: there is no /dev/shm tmpfs to put the tests'
// state on, so it stays in TMPDIR.
func Use() {}

// CheckUsed skips off Linux: nothing moved the tests.
func CheckUsed(t testing.TB) { t.Skip("testtmp moves nothing off Linux") }
