package daemon

// SetStatxForTest replaces the statx call socketIdentity makes, so a test can
// stand in for a kernel or a seccomp profile without statx. It returns a
// function that restores the real one.
func SetStatxForTest(f func(path string) error) (restore func()) {
	old := statxHook
	statxHook = f
	return func() { statxHook = old }
}
