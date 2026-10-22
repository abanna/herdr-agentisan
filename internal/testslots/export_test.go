package testslots

import "io"

// ReadingOf is one reading made of the reads in contents, as Read makes it
// from the reads it makes, with the holders judged against proc.
func ReadingOf(proc string, contents ...[]byte) (*Reading, error) {
	return reading(proc, contents...)
}

// Held reads the content of /proc/locks into the files some process holds a
// lock on, as parse reads it.
func Held(content []byte) (map[File]bool, error) {
	l, err := parse(content)
	if err != nil {
		return nil, err
	}
	held := make(map[File]bool, len(l.holders))
	for f := range l.holders {
		held[f] = true
	}
	return held, nil
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
