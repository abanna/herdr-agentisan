package dashboard

import (
	"strings"
	"testing"
	"testing/iotest"

	"github.com/stretchr/testify/require"
)

func TestOSC11InputNormalization(t *testing.T) {
	t.Parallel()

	const mixed = "j\x1b]11;rgb:A/7f/800\a\x1b]11;rgb:00ee/00ff/8000\x1b\\q"
	const normalized = "j\x1b]11;#aa7f80\a\x1b]11;#010180\x1b\\q"
	const unchanged = "é\x1b[A\x1b]10;rgb:00ee/00ee/00ee\a" +
		"\x1b]11;#112233\x1b\\\x1b]11;rgba:1/2/3/4\a\x1b]11;rgb:1//f\a" +
		"\x1b]11;rgb:123456789012345/ff/ff\a\x1b]11;rgb:00ee/\x1b"
	for _, tc := range []struct {
		name, input, want string
	}{
		{"mixed widths and terminators", mixed, normalized},
		{"unrelated, invalid and truncated sequences", unchanged, unchanged},
		{"aborted reply followed by a fresh reply", "\x1b]11;rgb:1/\x1b]11;rgb:abc/ff/0\a", "\x1b]11;rgb:1/\x1b]11;#abff00\a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, reader := range []*osc11Reader{
				{source: strings.NewReader(tc.input)},
				{source: iotest.OneByteReader(strings.NewReader(tc.input))},
				{source: iotest.DataErrReader(strings.NewReader(tc.input))},
			} {
				require.NoError(t, iotest.TestReader(reader, []byte(tc.want)))
			}
		})
	}
}

func TestOSC11InputPassesEscapeImmediately(t *testing.T) {
	t.Parallel()

	for _, key := range []string{"\x1b", "\x1b]"} {
		reader := normalizeTerminalInput(iotest.TimeoutReader(strings.NewReader(key)))
		var buf [64]byte
		n, err := reader.Read(buf[:])
		require.NoError(t, err)
		require.Equal(t, key, string(buf[:n]), "return before requesting more input")
	}
}
