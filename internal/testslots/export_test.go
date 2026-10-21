package testslots

// ReadHeld and ReadWhole let the tests read a lock list served as the kernel
// serves /proc/locks, a page at a time.
var (
	ReadHeld  = readHeld
	ReadWhole = readWhole
)
