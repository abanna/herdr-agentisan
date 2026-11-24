package dashboard

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

const (
	osc11Prefix = "\x1b]11;"
	xRGBPrefix  = "rgb:"
	maxXRGB     = len(xRGBPrefix) + 4*3 + 2 // three channels and two slashes
)

// terminalInput retains Bubble Tea's raw-mode interface. The read pump
// owns cancellation on the original input descriptor.
type terminalInput interface {
	io.ReadWriteCloser
	Fd() uintptr
}

type osc11File struct {
	terminalInput
	reader *osc11Reader
}

func (f *osc11File) Read(p []byte) (int, error) { return f.reader.Read(p) }

func normalizeTerminalInput(ctx context.Context, in io.Reader) (io.Reader, func()) {
	if in == nil {
		return nil, func() {}
	}
	source := newTerminalReads(ctx, in)
	reader := &osc11Reader{next: source.next}
	if terminal, ok := in.(terminalInput); ok {
		return &osc11File{terminalInput: terminal, reader: reader}, source.stop
	}
	return reader, source.stop
}

// osc11Reader fixes x/ansi's value-based conversion of RGB channels before
// the decoder discards the original channel widths. The OSC introducer is
// passed through as it arrives, including a lone Esc. Only candidate colour
// payload bytes are held until the terminator, the byte limit, or the
// decoder's Escape timeout measured from the complete introducer.
type osc11Reader struct {
	next    func(time.Time) terminalChunk
	matched int
	body    []byte
	expires time.Time
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

	for {
		var deadline time.Time
		if r.matched == len(osc11Prefix) {
			deadline = r.expires
		}
		chunk := r.next(deadline)
		out := p[:0]
		if errors.Is(chunk.err, errTerminalReadTimeout) {
			out = r.expire(out)
			chunk.err = nil
		} else {
			out = r.appendChunk(out, chunk)
		}
		if len(out) == 0 {
			if chunk.err == nil {
				continue
			}
			return 0, chunk.err
		}
		n := copy(p, out)
		if n < len(out) {
			r.pending = out[n:]
		}
		// The terminal decoder discards bytes returned alongside an error.
		r.readErr = chunk.err
		return n, nil
	}
}

func (r *osc11Reader) appendChunk(out []byte, chunk terminalChunk) []byte {
	if !r.expires.IsZero() && !chunk.at.Before(r.expires) {
		out = r.expire(out)
	}
	for _, b := range chunk.data {
		out = r.consume(out, b)
	}
	if chunk.err != nil {
		out = r.release(out)
	}
	return out
}

// expire preserves the held bytes in a canceled OSC so a decoder that
// already expired the introducer cannot interpret the payload as keys.
// The single-byte OSC also works when the old introducer is still pending:
// in that case it is payload, and CAN cancels the whole sequence.
func (r *osc11Reader) expire(out []byte) []byte {
	if r.matched < len(osc11Prefix) {
		// A partial introducer has already passed through.
		r.resetReply()
		return out
	}
	body := r.body
	escape := len(body) > 0 && body[len(body)-1] == ansi.ESC
	if escape {
		body = body[:len(body)-1]
	}
	out = append(out, ansi.OSC, '1', '1', ';')
	out = append(out, body...)
	out = append(out, ansi.CAN)
	if escape {
		out = append(out, ansi.ESC)
	}
	r.resetReply()
	return out
}

func (r *osc11Reader) release(out []byte) []byte {
	out = append(out, r.body...)
	r.resetReply()
	return out
}

func (r *osc11Reader) resetReply() {
	r.body = r.body[:0]
	r.matched = 0
	r.expires = time.Time{}
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
			case b == ansi.BEL || b == ansi.ST:
				return r.finish(out, body, string([]byte{b}))
			case b == '\x1b' || xRGBByte(len(body), b):
				r.body = append(r.body, b)
				return out
			}
			r.matched = 0
		}
		out = append(out, body...)
		r.body = r.body[:0]
		r.expires = time.Time{}
	}
	switch b {
	case osc11Prefix[r.matched]:
		r.matched++
	case '\x1b':
		r.matched = 1
	default:
		r.matched = 0
	}
	r.expires = time.Time{}
	if r.matched > 0 {
		r.expires = time.Now().Add(uv.DefaultEscTimeout)
	}
	return append(out, b)
}

func xRGBByte(offset int, b byte) bool {
	if offset < len(xRGBPrefix) {
		return b|0x20 == xRGBPrefix[offset]
	}
	return offset < maxXRGB && strings.ContainsRune("0123456789abcdefABCDEF/", rune(b))
}

func (r *osc11Reader) finish(out, body []byte, terminator string) []byte {
	r.resetReply()
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
	body, ok := strings.CutPrefix(strings.ToLower(payload), xRGBPrefix)
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
