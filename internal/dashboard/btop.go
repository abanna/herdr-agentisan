package dashboard

import (
	"context"
	"errors"
	"fmt"

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/plugin"
)

// ErrBtop means btop could not be opened. The dashboard shows it in the
// footer; it is never fatal.
var ErrBtop = errors.New("btop failed")

// btopSize is the popup's width and height: most of the terminal, as v12's
// full-screen btop and the 92% prefix+m popup in herdr's own config.
const btopSize = "92%"

// BtopOpener opens btop for the reader. A click on [ btop ] calls it.
type BtopOpener interface {
	OpenBtop(ctx context.Context) error
}

// HerdrBtop opens btop through herdr's socket: plugin.pane.open on this
// plugin's btop pane (herdr-plugin.toml's [[panes]] plugin.BtopPane), as a
// popup. herdr spawns btop; the plugin starts no process, and the dashboard
// keeps running under the popup instead of giving up its terminal as v12's
// did.
type HerdrBtop struct {
	Client herdr.Client
}

// OpenBtop asks herdr for the popup. It returns once herdr has opened it,
// not when btop exits: the popup is herdr's from then on.
func (b HerdrBtop) OpenBtop(ctx context.Context) error {
	err := b.Client.OpenPluginPopup(ctx, herdr.PluginPopup{
		PluginID: plugin.ID, Entrypoint: plugin.BtopPane, Width: btopSize, Height: btopSize,
	})
	if err != nil {
		return fmt.Errorf("%w: %w", ErrBtop, err)
	}
	return nil
}
