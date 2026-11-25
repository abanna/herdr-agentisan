package dashboard

import (
	"bytes"
	"fmt"
	"io"
	"math/rand/v2"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"
	"testing/synctest"
	"time"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
)

var qa4Candidate = regexp.MustCompile(`^(?i:r|rg|rgb|rgb:[0-9a-fA-F/]*)?\x1b?$`)

var qa4Reply = regexp.MustCompile(`\x1b\]11;(?i:rgb:)([0-9a-fA-F]{1,4})/([0-9a-fA-F]{1,4})/([0-9a-fA-F]{1,4})(\a|\x1b|\x{fffd})`)

// qa4Reference is the specification: only a complete candidate colour before
// BEL, ST or ESC is rewritten. The decoder handles ESC cancellation.
func qa4Reference(in []byte) []byte {
	return qa4Reply.ReplaceAllFunc(in, func(m []byte) []byte {
		s := qa4Reply.FindSubmatch(m)
		if term := s[4][0]; term != '\a' && term != '\x1b' && term != 0x9c {
			return m // The regexp also recognizes literal U+FFFD.
		}
		var c [3]uint64
		for i := range c {
			v, _ := strconv.ParseUint(string(s[i+1]), 16, 16)
			lim := uint64(1)<<(4*len(s[i+1])) - 1
			c[i] = (v*255 + lim/2) / lim
		}
		return []byte(fmt.Sprintf("\x1b]11;#%02x%02x%02x%s", c[0], c[1], c[2], s[4]))
	})
}

func qa4Reader(src io.Reader) *osc11Reader {
	var buf [terminalReadSize]byte
	return &osc11Reader{next: func(time.Time, bool) terminalChunk {
		n, err := src.Read(buf[:])
		return terminalChunk{data: buf[:n], at: time.Now(), err: err}
	}}
}

type qa4Chunks struct {
	data []byte
	rng  *rand.Rand
	max  int
}

func (c *qa4Chunks) Read(p []byte) (int, error) {
	if len(c.data) == 0 {
		return 0, io.EOF
	}
	n := 1 + c.rng.IntN(c.max)
	n = min(n, len(c.data), len(p))
	copy(p, c.data[:n])
	c.data = c.data[n:]
	return n, nil
}

func qa4Drain(t *testing.T, r io.Reader, rng *rand.Rand, bufMax int) []byte {
	t.Helper()
	var out []byte
	for range 1 << 16 {
		p := make([]byte, 1+rng.IntN(bufMax))
		n, err := r.Read(p)
		out = append(out, p[:n]...)
		if err == io.EOF {
			return out
		}
		require.NoError(t, err)
		if len(out) > 1<<16 { // streams are under 2 KiB: a reader that never ends must fail, not grow
			t.Fatalf("output grew to %d bytes without EOF", len(out))
		}
	}
	t.Fatal("no EOF")
	return nil
}

var qa4Tokens = []string{
	"\x1b", "]", "1", "1", ";", "\x1b]11;", "\x1b]11;", "\x1b]11;rgb:", "rgb:", "r", "g", "b", ":",
	"0", "1", "e", "f", "a", "F", "0", "7", "/", "/", "\a", "\x1b\\", "\\", "q", " ", "x", "\x9c", "#", "[", "A", "\x1bO",
	"00ee", "1e1e", "ffff", "f", "ff", "fff", "5", "\x1b]10;rgb:1/2/3\a", "\n", "é",
}

func qa4Stream(rng *rand.Rand) []byte {
	var b bytes.Buffer
	for range 1 + rng.IntN(40) {
		b.WriteString(qa4Tokens[rng.IntN(len(qa4Tokens))])
	}
	return b.Bytes()
}

// The differential property: whatever the chunking of the source and the size
// of the caller's buffer, the output equals the reference rewrite of the whole
// stream. That covers no loss, no reordering, no duplication, and a malformed
// reply never becoming valid.
func TestQA4Differential(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		rng := rand.New(rand.NewPCG(1, 2))
		for i := range 20_000 {
			in := qa4Stream(rng)
			want := qa4Reference(in)
			for _, cfg := range [][2]int{{1, 1}, {3, 2}, {8, 40}, {64, 64}, {2, 300}} {
				r := qa4Reader(&qa4Chunks{data: bytes.Clone(in), rng: rng, max: cfg[0]})
				got := qa4Drain(t, r, rng, cfg[1])
				if !bytes.Equal(got, want) {
					t.Fatalf("iter %d cfg %v\n in   %q\n want %q\n got  %q", i, cfg, in, want, got)
				}
			}
		}
	})
}

// After every read, the bytes withheld are the candidate payload of an OSC 11
// introducer that is the end of what was consumed so far, never more than 19.
func TestQA4WithheldIsBoundedAndIsACandidate(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(3, 4))
	for range 100_000 {
		in := qa4Stream(rng)
		var reader osc11Reader
		var out []byte
		for i, b := range in {
			out = reader.consume(out, b)
			if len(reader.body) > 0 {
				require.LessOrEqual(t, len(reader.body), 19)
				require.Regexp(t, qa4Candidate, string(reader.body), "only a possible x11 payload may be withheld")
				require.True(t, bytes.HasSuffix(in[:i+1], append([]byte(osc11Prefix), reader.body...)),
					"withheld %q is not the tail of a candidate reply in %q", reader.body, in[:i+1])
			}
		}
	}
}

// Bytes after a withheld candidate are never delayed past the next read.
func TestQA4TypedInputAfterIntroducerSplit(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ reads []string }{
		{[]string{"\x1b]", "11;", "rgb:", "1e1e/", "1e1e/2e2e\a", "qq"}},
		{[]string{"\x1b]11;rgb:1e1e/1e1e/2e2e\x1b", "\\", "x"}},
		{[]string{"\x1b]11;rgb:1e", "q", "\x1b", "[A"}},
		{[]string{"\x1b]11;", "\x1b", "x"}},
	} {
		src := &qa4Script{reads: tc.reads}
		r := qa4Reader(src)
		out := qa4Drain(t, r, rand.New(rand.NewPCG(5, 6)), 64)
		require.Equal(t, string(qa4Reference([]byte(strings.Join(tc.reads, "")))), string(out))
	}
}

type qa4Script struct{ reads []string }

func (s *qa4Script) Read(p []byte) (int, error) {
	if len(s.reads) == 0 {
		return 0, io.EOF
	}
	n := copy(p, s.reads[0])
	if n < len(s.reads[0]) {
		s.reads[0] = s.reads[0][n:]
	} else {
		s.reads = s.reads[1:]
	}
	return n, nil
}

// Every width 1..4 decodes, through the real decoder, as the X specification
// says (value / (16^w-1) of full scale), to within one 8-bit step, and the
// 4-digit duplicated-byte form every xterm-family terminal sends is exact.
func TestQA4WidthsThroughTheRealDecoder(t *testing.T) {
	t.Parallel()
	decode := func(payload string) (r, g, b uint8, ok bool) {
		in := []byte("\x1b]11;" + payload + "\a")
		rd := qa4Reader(bytes.NewReader(in))
		out, err := io.ReadAll(rd)
		require.NoError(t, err)
		var dec uv.EventDecoder
		n, ev := dec.Decode(out)
		require.Equal(t, len(out), n, "%q", out)
		bg, isBg := ev.(uv.BackgroundColorEvent)
		if !isBg || bg.Color == nil {
			return 0, 0, 0, false
		}
		R, G, B, _ := bg.RGBA()
		return uint8(R >> 8), uint8(G >> 8), uint8(B >> 8), true
	}
	maxDev := 0
	for w := 1; w <= 4; w++ {
		lim := uint64(1)<<(4*w) - 1
		for v := uint64(0); v <= lim; v++ {
			hex := fmt.Sprintf("%0*x", w, v)
			// vary which channel carries the value and use other widths for the rest
			for pos := range 3 {
				ch := []string{"7", "a5", "fff"}
				ch[pos] = hex
				r, g, b, ok := decode("rgb:" + strings.Join(ch, "/"))
				require.True(t, ok, "%v", ch)
				got := [3]uint8{r, g, b}
				for i := range 3 {
					var wv, wl uint64
					switch {
					case i == pos:
						wv, wl = v, lim
					default:
						c := ch[i]
						wv, _ = strconv.ParseUint(c, 16, 16)
						wl = uint64(1)<<(4*len(c)) - 1
					}
					exact := float64(wv) * 255 / float64(wl)
					dev := int(float64(got[i]) - exact)
					if dev < 0 {
						dev = -dev
					}
					if d := float64(got[i]) - exact; d > 0.5000001 || d < -0.5000001 {
						t.Fatalf("w=%d v=%x ch=%v i=%d got %d exact %.3f", w, v, ch, i, got[i], exact)
					}
					maxDev = max(maxDev, dev)
				}
				if w == 4 && pos == 0 && v%257 == 0 {
					require.Equal(t, uint8(v/257), got[0], "duplicated-byte reply must be exact: %s", hex)
				}
			}
		}
	}
	t.Logf("max |8-bit - exact| = within 0.5 step; widest integer deviation %d", maxDev)
}

// The x/ansi baseline is wrong for exactly the replies the change targets,
// so this pins that the probe is meaningful: without the reader, 00ee is 0xee.
func TestQA4BaselineIsWrongWithoutTheReader(t *testing.T) {
	t.Parallel()
	c := ansi.XParseColor("rgb:00ee/00ee/00ee")
	r, _, _, _ := c.RGBA()
	require.Equal(t, uint32(0xee), r>>8)
}

// Buffering is bounded however long a hostile reply runs.
func TestQA4NeverEndingReplyIsBounded(t *testing.T) {
	t.Parallel()
	in := append([]byte("\x1b]11;rgb:"), bytes.Repeat([]byte("1e/"), 1<<20)...)
	r := qa4Reader(bytes.NewReader(in))
	out := make([]byte, 0, len(in))
	for reads := 0; ; reads++ {
		require.Less(t, reads, len(in)+64, "a reader that never ends must fail, not loop")
		require.LessOrEqual(t, len(out), len(in), "output may never exceed input")
		p := make([]byte, 7)
		n, err := r.Read(p)
		out = append(out, p[:n]...)
		require.LessOrEqual(t, len(r.body), 19)
		require.LessOrEqual(t, len(r.pending), terminalReadSize+19)
		if err != nil {
			break
		}
	}
	require.Equal(t, in, out)
}

// The adapter exposes the descriptor for raw mode. It keeps polling and
// cancellation inside the pump so Bubble Tea can receive timed reads.
func TestQA4WrapperKeepsTheInterfaces(t *testing.T) {
	t.Parallel()
	f := &qa4File{}
	got, stop := normalizeTerminalInput(t.Context(), f)
	t.Cleanup(stop)
	require.Equal(t, uintptr(7), got.(terminalInput).Fd())
	_, externallyPolled := got.(interface{ Name() string })
	require.False(t, externallyPolled)
	input, stop := normalizeTerminalInput(t.Context(), nil)
	t.Cleanup(stop)
	require.Nil(t, input)
}

type qa4File struct{ bytes.Buffer }

func (*qa4File) Fd() uintptr  { return 7 }
func (*qa4File) Close() error { return nil }
func (*qa4File) Name() string { return "tty" }

func TestQA4EightBitTerminatorAndUppercaseRGB(t *testing.T) {
	t.Parallel()
	in := "\x1b]11;RGB:00ee/00ee/00ee\x9c"
	out, err := io.ReadAll(qa4Reader(iotest.OneByteReader(strings.NewReader(in))))
	require.NoError(t, err)
	require.Equal(t, "\x1b]11;#010101\x9c", string(out))
}

func TestQA4RGBAPassesUnchanged(t *testing.T) {
	t.Parallel()
	in := "\x1b]11;rgba:00ee/00ee/00ee/ffff\a"
	out, err := io.ReadAll(qa4Reader(strings.NewReader(in)))
	require.NoError(t, err)
	require.Equal(t, in, string(out))
}

func TestQA4Throughput(t *testing.T) {
	t.Parallel()
	mouse := bytes.Repeat([]byte("\x1b[<35;120;40M"), 1<<20)
	r := qa4Reader(bytes.NewReader(mouse))
	buf := make([]byte, 4096)
	start := time.Now()
	total := 0
	for reads := 0; reads < len(mouse) && total <= len(mouse); reads++ {
		n, err := r.Read(buf)
		total += n
		if err != nil {
			break
		}
	}
	require.Equal(t, len(mouse), total)
	t.Logf("THROUGHPUT %d MB of mouse reports in %v", len(mouse)>>20, time.Since(start))
}

// A malformed reply passes untouched; it is never turned into a valid one.
// The differential's random streams rarely build these, so they are named:
// a 5-digit channel with leading zeros parses as a number but is not X11.
func TestQA4MalformedIsNotTurnedValid(t *testing.T) {
	t.Parallel()
	for _, hazard := range []string{"rgb:00001/1/1", "rgb:1/1/1/1"} {
		_, ok := parseXRGB(hazard)
		require.False(t, ok, "%q must be refused by the normalizer", hazard)
	}
	for _, payload := range []string{
		"rgb:00001/1/1", "rgb:1/00001/1", "rgb:1/1/00001", "rgb:0000f/0000f/0000f",
		"rgb:1/1/1/1", "rgb:1/1", "rgb:1", "rgb:", "rgb:1//1", "rgb:/1/1", "rgb:1/1/", "rgb:1/1/1/",
		"rgb:ffff/ffff/fffff", "rgb:0x1/1/1", "rgb:+1/1/1", "rgb:-1/1/1", "rgb:1_0/1/1", "rgb:g/1/1",
		" rgb:1/1/1", "rgb:1/1/1 ", "rgb: 1/1/1", "#fff", "#ffffff", "rgb:1/1/1;", "rgb:1/1/1:",
	} {
		for _, term := range []string{"\a", "\x1b\\"} {
			in := "\x1b]11;" + payload + term
			for _, src := range []io.Reader{strings.NewReader(in), iotest.OneByteReader(strings.NewReader(in))} {
				out, err := io.ReadAll(qa4Reader(src))
				require.NoError(t, err)
				require.Equal(t, in, string(out), "malformed %q must pass through unchanged", payload)
			}
		}
	}
}

// With a candidate withheld and the caller's buffer filled by the next read,
// the held bytes come first, and the remainder drains without a new
// underlying read or wakeup.
func TestQA4FullBufferStrandsNothing(t *testing.T) {
	t.Parallel()
	src := &qa4Script{reads: []string{"\x1b]11;rgb:1e", strings.Repeat("q", 64)}}
	r := qa4Reader(src)
	p := make([]byte, 64)
	n, err := r.Read(p)
	require.NoError(t, err)
	require.Equal(t, "\x1b]11;", string(p[:n]))
	n, err = r.Read(p)
	require.NoError(t, err)
	require.Equal(t, "rgb:1e"+strings.Repeat("q", 58), string(p[:n]), "withheld bytes come first, in order")
	require.Equal(t, 64, n)
	n, err = r.Read(p)
	require.NoError(t, err)
	require.Equal(t, strings.Repeat("q", 6), string(p[:n]))
	require.Empty(t, r.pending)
}

type qa4Boom struct{ sent bool }

func (b *qa4Boom) Read(p []byte) (int, error) {
	if !b.sent {
		b.sent = true
		return copy(p, "qq"), io.ErrUnexpectedEOF
	}
	return copy(p, "z"), nil
}

// An error that comes with data is delivered after the data, once, and the
// stream goes on if the source does.
func TestQA4ErrorWithDataIsDeliveredAfterTheData(t *testing.T) {
	t.Parallel()
	r := qa4Reader(&qa4Boom{})
	p := make([]byte, 16)
	n, err := r.Read(p)
	require.NoError(t, err)
	require.Equal(t, "qq", string(p[:n]))
	n, err = r.Read(p)
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	require.Zero(t, n)
	n, err = r.Read(p)
	require.NoError(t, err)
	require.Equal(t, "z", string(p[:n]))
}

// A reply is still found after stray or aborted introducers and Escs.
func TestQA4ReplyAfterStrayEscapes(t *testing.T) {
	t.Parallel()
	const reply = "\x1b]11;rgb:00ee/00ee/00ee\a"
	for _, lead := range []string{"\x1b", "\x1b\x1b", "\x1b]", "\x1b]1", "\x1b]11", "\x1b]11;\x1b", "\x1b]11;rgb:1/\x1b", "\x1b[", "q\x1b"} {
		in := lead + reply
		out, err := io.ReadAll(qa4Reader(iotest.OneByteReader(strings.NewReader(in))))
		require.NoError(t, err)
		require.Equal(t, string(qa4Reference([]byte(in))), string(out), "%q", in)
		require.Contains(t, string(out), "#010101", "%q: the reply must be rewritten", in)
	}
}

// What follows a finished reply starts from nothing: payload-looking bytes
// after it are not a payload.
func TestQA4NothingCarriesOverAReply(t *testing.T) {
	t.Parallel()
	in := "\x1b]11;rgb:f/f/f\argb:1/1/1\a"
	out, err := io.ReadAll(qa4Reader(strings.NewReader(in)))
	require.NoError(t, err)
	require.Equal(t, "\x1b]11;#ffffff\argb:1/1/1\a", string(out))
}
