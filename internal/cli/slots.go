package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/paraizo/delamain/internal/pipeline"
	"github.com/paraizo/delamain/internal/store"
)

// errNoSlot is a run refused because every slot is held, or the only free one is the queue's.
var errNoSlot = errors.New("maximum parallelism reached")

// exitNoSlot is the exit code of a run refused for want of a slot.
const exitNoSlot = 5

// acquireRunSlot takes one of the maxSlots every delamain process shares for a run and returns its
// release. A run never waits nor goes ahead of the queue: with no slot free for it, it is refused
// with errNoSlot, wrapped with how the slots stand.
func acquireRunSlot(ctx context.Context, st *store.Store, maxSlots int) (func(), error) {
	if err := st.PruneSlots(ctx, pipeline.ProcessAlive); err != nil {
		return nil, err
	}
	id, ok, err := st.AcquireSlot(ctx, store.HolderRun, maxSlots)
	if err != nil {
		return nil, err
	}
	if !ok {
		u, err := st.SlotUsage(ctx, nil)
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %s", errNoSlot, slotsText(u, maxSlots))
	}
	// The slot is freed even when ctx is what ended the run.
	return func() { _ = st.ReleaseSlot(context.WithoutCancel(ctx), id) }, nil
}

// slotsText says how the slots stand, e.g. "3/3 slots in use (1 reserved for the queue)".
func slotsText(u store.SlotUsage, maxSlots int) string {
	s := fmt.Sprintf("%d/%d slots in use", u.InUse, maxSlots)
	if u.Reserved {
		s += " (1 reserved for the queue)"
	}
	return s
}
