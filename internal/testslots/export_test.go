package testslots

import "io"

// ReadingOf is one reading made of the reads in contents, as Read makes it
// from the reads it makes, with the holders judged against proc.
func ReadingOf(proc string, contents ...[]byte) (*Reading, error) {
	return reading(proc, contents...)
}

// ReadStaggered and ReadWhole let the tests read a lock list served as the
// kernel serves /proc/locks, a page at a time.
var (
	ReadStaggered = readStaggered
	ReadWhole     = readWhole
)

// ReadHeld is the files the three staggered reads of what open opens find
// some process holding a lock on, as Read reads them.
func ReadHeld(name string, open func() (io.ReadCloser, error), page int) (map[File]bool, error) {
	r, err := readStaggered("", name, open, page)
	if err != nil {
		return nil, err
	}
	held := make(map[File]bool, len(r.locks.holders))
	for f := range r.locks.holders {
		held[f] = true
	}
	return held, nil
}
