package dashboard

import (
	"io"
	"math/rand/v2"
	"testing"
	"testing/synctest"
	"time"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/stretchr/testify/require"
)

type timedFragment struct {
	delay time.Duration
	data  string
}

type timedInput struct {
	at        time.Time
	fragments []timedFragment
}

func (in *timedInput) Read(p []byte) (int, error) {
	if len(in.fragments) == 0 {
		time.Sleep(time.Until(in.at.Add(3 * uv.DefaultEscTimeout)))
		return 0, io.EOF
	}
	fragment := in.fragments[0]
	in.fragments = in.fragments[1:]
	in.at = in.at.Add(fragment.delay)
	time.Sleep(time.Until(in.at))
	return copy(p, fragment.data), nil
}

type timedDecoderLogger struct{}

func (timedDecoderLogger) Printf(format string, _ ...any) {
	// Ultraviolet checks After rather than >=. Fake time otherwise fires
	// exactly at the deadline and can defer expiry for a second interval.
	if format == "timeout reached" {
		time.Sleep(time.Nanosecond)
	}
}

func decodeTimedInput(t *testing.T, fragments []timedFragment, wrap bool) []uv.Event {
	t.Helper()
	raw := timedInput{time.Now(), fragments}
	var input io.Reader = &raw
	if wrap {
		var stop func()
		input, stop = normalizeTerminalInput(t.Context(), input)
		defer stop()
	}
	events := make(chan uv.Event, 256)
	decoder := uv.NewTerminalReader(input, "xterm-256color")
	decoder.SetLogger(timedDecoderLogger{})
	require.NoError(t, decoder.StreamEvents(t.Context(), events))
	close(events)

	var decoded []uv.Event
	for event := range events {
		if color, ok := event.(uv.BackgroundColorEvent); ok {
			if wrap {
				require.Equal(t, "#eeee11", color.String())
			}
			event = uv.BackgroundColorEvent{}
		}
		decoded = append(decoded, event)
	}
	return decoded
}

func TestOSC11TimedDifferential(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []timedFragment
	}{
		{"escape cancels", []timedFragment{{0, osc11Prefix + "rgb:00ee/"}, {25 * time.Millisecond, "\x1b"}, {60 * time.Millisecond, "q"}}},
		{"timeout preserves the unknown event", []timedFragment{{0, osc11Prefix}, {20 * time.Millisecond, "rgb:00ee/"}, {65 * time.Millisecond, "q"}}},
		{"escape after expiry stays separate", []timedFragment{{0, osc11Prefix + "rgb:00ee/"}, {67 * time.Millisecond, "\x1b"}, {67 * time.Millisecond, "q"}}},
		{"split ST cancels", []timedFragment{{0, osc11Prefix + "rgb:eeee/eeee/1111"}, {10 * time.Millisecond, "\x1b"}, {10 * time.Millisecond, "\\"}}},
		{"joined ST terminates", []timedFragment{{0, osc11Prefix + "rgb:eeee/eeee/1111"}, {10 * time.Millisecond, "\x1b\\"}}},
		{"fragments refresh idle deadline", []timedFragment{{0, osc11Prefix + "rgb:eeee/"}, {30 * time.Millisecond, "eeee/"}, {30 * time.Millisecond, "1111\a"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				base := decodeTimedInput(t, tc.in, false)
				require.Equal(t, base, decodeTimedInput(t, tc.in, true))
			})
		})
	}
}

func TestOSC11RandomTimedDifferential(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rng := rand.New(rand.NewPCG(5719, 11))
		gaps := []time.Duration{7, 17, 31, 49, 51, 67, 93, 125}
		for trial := range 300 {
			reply := osc11Prefix + "rgb:eeee/eeee/1111"
			terminators := []string{"\a", "\x9c", "\x1b\\", "\x1b"}
			term := terminators[rng.IntN(len(terminators))]
			if term == "\x1b" {
				// Inject a key cancellation while the reply is incomplete.
				reply = reply[:len(osc11Prefix)+1+rng.IntN(len(reply)-len(osc11Prefix)-1)]
			}
			var in []timedFragment
			for _, part := range []string{"j", reply, term, "q", "\x1b", "q"} {
				for offset := 0; offset < len(part); {
					end := min(len(part), offset+1+rng.IntN(8))
					delay := gaps[rng.IntN(len(gaps))] * time.Millisecond
					in = append(in, timedFragment{delay, part[offset:end]})
					offset = end
				}
			}
			base := decodeTimedInput(t, in, false)
			require.Equal(t, base, decodeTimedInput(t, in, true), "trial %d: %v", trial, in)
		}
	})
}
