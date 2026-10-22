package report

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"strconv"
	"strings"
	"time"
)

// ErrStartedAt means a process's start could not be put on the wall clock:
// the boot time a Linux start counts from could not be read or parsed, or
// the start is past what a time holds.
var ErrStartedAt = errors.New("process start not on the wall clock")

// UserHZ is the unit of /proc's clock ticks, USER_HZ: the /proc ABI fixes it
// at 100 on every architecture Linux builds for, and ps converts with it.
const UserHZ = 100

// tick is one UserHZ clock tick.
const tick = time.Second / UserHZ

// StartedAtFunc puts a Stat's Start on the wall clock: when the process
// started (ADR-001 A47).
type StartedAtFunc func(start uint64) (time.Time, error)

// ProcStartedAt puts Linux starts, clock ticks since boot, on the wall
// clock, as ps does: the boot time, btime in a Linux procfs's stat file at
// proc, plus the ticks at UserHZ. btime is whole seconds, so a start reads
// up to a second off, and it is read at every call.
func ProcStartedAt(proc fs.FS) StartedAtFunc {
	return func(start uint64) (time.Time, error) {
		raw, err := fs.ReadFile(proc, "stat")
		if err != nil {
			return time.Time{}, fmt.Errorf("%w: %w", ErrStartedAt, err)
		}
		boot, err := bootTime(raw)
		if err != nil {
			return time.Time{}, err
		}
		if start > uint64(math.MaxInt64/int64(tick)) {
			return time.Time{}, fmt.Errorf("%w: %d ticks since boot is past what a time holds", ErrStartedAt, start)
		}
		return boot.Add(time.Duration(start) * tick), nil // #nosec G115 -- checked in range above
	}
}

// bootTime is the btime line of a stat file: "btime <seconds since the
// epoch>", a positive whole number.
func bootTime(raw []byte) (time.Time, error) {
	for line := range bytes.Lines(raw) {
		fields := strings.Fields(string(line))
		if len(fields) == 0 || fields[0] != "btime" {
			continue
		}
		if len(fields) != 2 {
			return time.Time{}, fmt.Errorf("%w: btime line %q", ErrStartedAt, strings.TrimSpace(string(line)))
		}
		sec, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil || sec <= 0 {
			return time.Time{}, fmt.Errorf("%w: btime %q is not a boot time", ErrStartedAt, fields[1])
		}
		return time.Unix(sec, 0), nil
	}
	return time.Time{}, fmt.Errorf("%w: stat has no btime line", ErrStartedAt)
}

// KinfoStartedAt puts darwin starts, which KinfoStat reads as microseconds
// since the epoch, on the wall clock.
func KinfoStartedAt(start uint64) (time.Time, error) {
	if start > math.MaxInt64 {
		return time.Time{}, fmt.Errorf("%w: %d µs since the epoch is past what a time holds", ErrStartedAt, start)
	}
	return time.UnixMicro(int64(start)), nil // #nosec G115 -- checked in range above
}

// SystemStartedAt puts SystemStat's starts on the wall clock, nil on an OS
// with no process table reader.
func SystemStartedAt() StartedAtFunc {
	return systemStartedAt()
}
