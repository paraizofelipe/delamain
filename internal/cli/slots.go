package cli

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/paraizo/delamain/internal/pipeline"
	"github.com/paraizo/delamain/internal/store"
)

// slotPoll paces a run waiting for a slot; tests shorten it.
var slotPoll = 2 * time.Second

// acquireRunSlot waits for one of the maxSlots every delamain process shares and returns its
// release. A waiting run goes ahead of the daemon's queue: the daemon takes no slot while one
// waits. Cancelling ctx gives the place up.
func acquireRunSlot(ctx context.Context, st *store.Store, maxSlots int, w io.Writer) (func(), error) {
	if err := st.PruneSlots(ctx, pipeline.ProcessAlive); err != nil {
		return nil, err
	}
	id, err := st.WaitSlot(ctx)
	if err != nil {
		return nil, err
	}
	// The slot is freed even when ctx is what ended the run.
	release := func() { _ = st.ReleaseSlot(context.WithoutCancel(ctx), id) }
	told := false
	for {
		ok, err := st.TakeSlot(ctx, id, maxSlots)
		if err != nil {
			release()
			return nil, err
		}
		if ok {
			return release, nil
		}
		if !told {
			told = true
			n, _ := st.SlotsInUse(ctx)
			fmt.Fprintf(w, "waiting for a free slot (%d/%d in use)…\n", n, maxSlots)
		}
		select {
		case <-ctx.Done():
			release()
			return nil, ctx.Err()
		case <-time.After(slotPoll):
		}
		// A holder that died keeps its slot until someone prunes it.
		if err := st.PruneSlots(ctx, pipeline.ProcessAlive); err != nil {
			release()
			return nil, err
		}
	}
}
