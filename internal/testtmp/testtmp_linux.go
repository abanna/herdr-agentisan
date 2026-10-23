package testtmp

import (
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// Use puts this test binary's temporary directories on Base: it sets TMPDIR,
// which os.TempDir, t.TempDir and every process the tests start read, when
// Base is a tmpfs this process can write, with MinFree room, and Env is not
// "keep". Otherwise it leaves TMPDIR alone and writes one line to stderr
// saying where the tests' state goes and why. Call it from TestMain, before
// m.Run. Disk is then where they were.
//
// It panics on a Base past the tests' socket room (ErrBase): only an edit of
// Base can cause it, and it must fail every test binary at once, not as a
// socket error in a few tests.
func Use() {
	before := os.TempDir()
	if err := use(Base, host()); err != nil {
		panic(err)
	}
	if os.TempDir() != before {
		disk = before
	}
}

// sys is what use reads and changes of the process and the host.
type sys struct {
	statfs func(path string, st *unix.Statfs_t) error
	access func(path string, mode uint32) error
	getenv func(key string) string
	setenv func(key, value string) error
	stderr io.Writer
}

func host() sys {
	return sys{statfs: unix.Statfs, access: unix.Access, getenv: os.Getenv, setenv: os.Setenv, stderr: os.Stderr}
}

// use is Use on base, through s. Only a base past maxBase is an error; any
// other reason to stay off base is said on s.stderr.
func use(base string, s sys) error {
	if len(base) > maxBase {
		return fmt.Errorf("%w: %s is %d bytes, over %d", ErrBase, base, len(base), maxBase)
	}
	why := onto(base, s)
	if why == "" {
		return nil
	}
	dir := s.getenv("TMPDIR")
	if dir == "" {
		dir = "/tmp"
	}
	_, _ = fmt.Fprintf(s.stderr, "testtmp: the tests' state stays in %s: %s\n", dir, why)
	return nil
}

// onto points TMPDIR at base and returns "", or returns why it did not.
func onto(base string, s sys) string {
	if s.getenv(Env) == "keep" {
		return Env + "=keep"
	}
	var st unix.Statfs_t
	if err := s.statfs(base, &st); err != nil {
		return fmt.Sprintf("%s cannot be read: %v", base, err)
	}
	if st.Type != unix.TMPFS_MAGIC {
		return base + " is not a tmpfs"
	}
	if err := s.access(base, unix.W_OK); err != nil {
		return fmt.Sprintf("%s is not writable: %v", base, err)
	}
	if free := st.Bavail * uint64(max(st.Bsize, 0)); free < MinFree {
		return fmt.Sprintf("%s has %d MiB free, under %d MiB", base, free>>20, MinFree>>20)
	}
	if err := s.setenv("TMPDIR", base); err != nil {
		return fmt.Sprintf("TMPDIR cannot be set: %v", err)
	}
	return ""
}

// CheckUsed fails t when the tests are not on Base though Use could have put
// them there: a package whose TestMain stopped calling Use would otherwise run
// on the disk again, with every test green. It skips when Use would have
// stayed off Base for any of its reasons. Each package that calls Use has one
// test calling it.
func CheckUsed(t testing.TB) {
	t.Helper()
	skip, fail := checkUsed(os.TempDir(), host())
	if fail != "" {
		t.Fatal(fail)
	}
	if skip != "" {
		t.Skip(skip)
	}
}

// checkUsed is CheckUsed's verdict on the tests' temporary directory, read
// before it runs: why to skip, or why to fail. It asks onto, so its reasons
// to skip are Use's own, but it never sets TMPDIR.
func checkUsed(tempDir string, s sys) (skip, fail string) {
	s.setenv = func(string, string) error { return nil }
	if why := onto(Base, s); why != "" {
		return "testtmp stays off " + Base + ": " + why, ""
	}
	if tempDir != Base && !strings.HasPrefix(tempDir, Base+"/") {
		return "", fmt.Sprintf("the tests' temporary directory is %s, not on %s: TestMain does not call testtmp.Use", tempDir, Base)
	}
	return "", ""
}
