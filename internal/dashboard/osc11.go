package dashboard

import (
	"fmt"
	"io"
	"strconv"
	"strings"
)

const (
	osc11Prefix = "\x1b]11;"
	xRGBPrefix  = "rgb:"
	maxXRGB     = len(xRGBPrefix) + 4*3 + 2 // three channels and two slashes
)

// terminalInput retains Bubble Tea's raw-mode interface. A named file also
// retains the interface used for cancelable reads.
type terminalInput interface {
	io.ReadWriteCloser
	Fd() uintptr
}

type osc11File struct {
	terminalInput
	reader *osc11Reader
}

func (f *osc11File) Read(p []byte) (int, error) { return f.reader.Read(p) }

type osc11NamedFile struct {
	*osc11File
	name func() string
}

func (f *osc11NamedFile) Name() string { return f.name() }

func normalizeTerminalInput(in io.Reader) io.Reader {
	if in == nil {
		return nil
	}
	reader := &osc11Reader{source: in}
	if terminal, ok := in.(terminalInput); ok {
		file := &osc11File{terminalInput: terminal, reader: reader}
		if named, ok := in.(interface{ Name() string }); ok {
			return &osc11NamedFile{osc11File: file, name: named.Name}
		}
		return file
	}
	return reader
}

// osc11Reader fixes x/ansi's value-based conversion of RGB channels before
// the decoder discards the original channel widths. The OSC introducer is
// passed through as it arrives, including a lone Esc. Only candidate colour
// payload bytes are held for the terminator.
type osc11Reader struct {
	source  io.Reader
	matched int
	body    []byte
	pending []byte
	readErr error
}

func (r *osc11Reader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(r.pending) != 0 {
		n := copy(p, r.pending)
		r.pending = r.pending[n:]
		return n, nil
	}
	if r.readErr != nil {
		err := r.readErr
		r.readErr = nil
		return 0, err
	}

	// Leave room for the held bytes so the descriptor's poller never needs
	// to wake again just to drain buffered output. With short buffers, keep
	// the excess for the next Read.
	headroom := min(len(r.body), len(p)-1)
	out := p[:0]
	if headroom < len(r.body) {
		out = make([]byte, 0, len(r.body)+1)
	}
	n, err := r.source.Read(p[headroom:])
	for _, b := range p[headroom : headroom+n] {
		out = r.consume(out, b)
	}
	if err != nil {
		out = append(out, r.body...)
		r.body = r.body[:0]
		r.matched = 0
	}
	if len(out) == 0 {
		return 0, err //nolint:wrapcheck // Preserve the input reader's error, including io.EOF.
	}
	n = copy(p, out)
	if n < len(out) {
		r.pending = out[n:]
	}
	// The terminal decoder discards bytes returned alongside an error.
	r.readErr = err
	return n, nil
}

func (r *osc11Reader) consume(out []byte, b byte) []byte {
	if r.matched == len(osc11Prefix) {
		body := r.body
		if len(body) > 0 && body[len(body)-1] == '\x1b' {
			if b == '\\' {
				return r.finish(out, body[:len(body)-1], "\x1b\\")
			}
			r.matched = 1 // The held Esc can begin the next introducer.
		} else {
			switch {
			case b == '\a':
				return r.finish(out, body, "\a")
			case b == '\x1b' || xRGBByte(len(body), b):
				r.body = append(r.body, b)
				return out
			}
			r.matched = 0
		}
		out = append(out, body...)
		r.body = r.body[:0]
	}
	switch b {
	case osc11Prefix[r.matched]:
		r.matched++
	case '\x1b':
		r.matched = 1
	default:
		r.matched = 0
	}
	return append(out, b)
}

func xRGBByte(offset int, b byte) bool {
	if offset < len(xRGBPrefix) {
		return b == xRGBPrefix[offset]
	}
	return offset < maxXRGB && strings.ContainsRune("0123456789abcdefABCDEF/", rune(b))
}

func (r *osc11Reader) finish(out, body []byte, terminator string) []byte {
	r.body = r.body[:0]
	r.matched = 0
	if rgb, ok := parseXRGB(string(body)); ok {
		// The hex spelling is recognized by x/ansi and is shorter than an
		// rgb: reply even when all three input channels are one digit.
		out = fmt.Appendf(out, "#%02x%02x%02x", rgb[0], rgb[1], rgb[2])
	} else {
		out = append(out, body...)
	}
	return append(out, terminator...)
}

func parseXRGB(payload string) ([3]uint64, bool) {
	var rgb [3]uint64
	body, ok := strings.CutPrefix(payload, xRGBPrefix)
	channels := strings.Split(body, "/")
	if !ok || len(channels) != len(rgb) {
		return rgb, false
	}
	for i, channel := range channels {
		width := len(channel)
		if width == 0 || width > 4 {
			return rgb, false
		}
		value, err := strconv.ParseUint(channel, 16, 16)
		if err != nil {
			return rgb, false
		}
		limit := uint64(1)<<(4*width) - 1
		rgb[i] = (value*255 + limit/2) / limit
	}
	return rgb, true
}
