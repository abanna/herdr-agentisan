package testslots

// ReadingOf is one reading made of the passes in contents, as Read makes it
// from the passes it reads, with the holders judged against proc.
func ReadingOf(proc string, contents ...[]byte) (*Reading, error) {
	return reading(proc, contents...)
}

// ReadPasses is one reading made as Read makes it, with read standing in
// for each pass's read of the lock list.
func ReadPasses(proc string, read func() ([]byte, error)) (*Reading, error) {
	return readPasses(proc, read)
}
