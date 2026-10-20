package store

import (
	"encoding/binary"
	"fmt"
	"io"
	"time"
)

// crockford is the ULID alphabet: Crockford's base32, without I, L, O or U.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// newULID is a ULID: the millisecond at, 48 bits, then 80 bits from entropy,
// as 26 base32 characters, most significant first. Ids made in a later
// millisecond sort after earlier ones; the inbox's order is its seq, never
// the id. A time before 1970 or past the 48-bit millisecond is ErrInvalid.
func newULID(at time.Time, entropy io.Reader) (string, error) {
	ms := at.UnixMilli()
	if ms < 0 || ms >= 1<<48 {
		return "", fmt.Errorf("%w: time %v is outside what a ULID holds", ErrInvalid, at)
	}
	var b [16]byte
	binary.BigEndian.PutUint64(b[:8], uint64(ms)<<16)
	if _, err := io.ReadFull(entropy, b[6:]); err != nil {
		return "", fmt.Errorf("%w: ULID entropy: %w", ErrStore, err)
	}
	// 26 characters of 5 bits are 130 bits: the first carries the top 3.
	hi, lo := binary.BigEndian.Uint64(b[:8]), binary.BigEndian.Uint64(b[8:])
	var out [26]byte
	for i := len(out) - 1; i >= 0; i-- {
		out[i] = crockford[lo&31]
		lo = lo>>5 | hi<<59
		hi >>= 5
	}
	return string(out[:]), nil
}
