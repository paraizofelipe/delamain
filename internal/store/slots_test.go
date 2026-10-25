package store

import (
	"context"
	"os"
	"testing"
)

func acquire(t *testing.T, s *Store, holder string, maxSlots int) (int64, bool) {
	t.Helper()
	id, ok, err := s.AcquireSlot(context.Background(), holder, maxSlots)
	if err != nil {
		t.Fatal(err)
	}
	return id, ok
}

func usage(t *testing.T, s *Store) SlotUsage {
	t.Helper()
	u, err := s.SlotUsage(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestAcquireSlotShouldShareEverySlotWhileTheQueueIsEmpty(t *testing.T) {
	s := openTest(t)
	for i, h := range []string{HolderRun, HolderDaemon, HolderRun} {
		if _, ok := acquire(t, s, h, 3); !ok {
			t.Fatalf("slot %d: with no queue every slot is free", i)
		}
	}
	if _, ok := acquire(t, s, HolderRun, 3); ok {
		t.Fatal("a fourth slot is over the limit")
	}
	if u := usage(t, s); u != (SlotUsage{InUse: 3}) {
		t.Fatalf("usage: %+v", u)
	}
}

func TestAcquireSlotShouldReserveOneSlotForAQueueWithIssues(t *testing.T) {
	s := openTest(t)
	enqueue(t, s, 12, 21)
	for i := 0; i < 2; i++ {
		if _, ok := acquire(t, s, HolderRun, 3); !ok {
			t.Fatalf("run %d takes one of the two unreserved slots", i)
		}
	}
	if _, ok := acquire(t, s, HolderDaemon, 3); ok {
		t.Fatal("the third slot is the queue's")
	}
	if u := usage(t, s); u != (SlotUsage{InUse: 2, Reserved: true}) {
		t.Fatalf("usage: %+v", u)
	}
	if _, ok := acquire(t, s, HolderQueue, 3); !ok {
		t.Fatal("the queue takes its reserved slot")
	}
	if _, ok := acquire(t, s, HolderQueue, 3); ok {
		t.Fatal("the queue runs one issue at a time")
	}
	if u := usage(t, s); u != (SlotUsage{InUse: 3, QueueRunning: true, Reserved: true}) {
		t.Fatalf("usage: %+v", u)
	}
}

func TestAcquireSlotShouldKeepTheReservationWhileTheLastIssueRuns(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	enqueue(t, s, 12)
	q, ok := acquire(t, s, HolderQueue, 2)
	if !ok {
		t.Fatal("the queue takes a slot")
	}
	if _, err := s.RemoveQueueAt(ctx, 1); err != nil { // the issue left the queue as it started
		t.Fatal(err)
	}
	if _, ok := acquire(t, s, HolderRun, 2); !ok {
		t.Fatal("the other slot is free")
	}
	if err := s.ReleaseSlot(ctx, q); err != nil {
		t.Fatal(err)
	}
	if _, ok := acquire(t, s, HolderRun, 2); !ok {
		t.Fatal("with the queue done, its slot is free for a run")
	}
}

func TestAcquireSlotShouldLeaveASingleSlotToTheQueue(t *testing.T) {
	s := openTest(t)
	enqueue(t, s, 12)
	if _, ok := acquire(t, s, HolderRun, 1); ok {
		t.Fatal("with one slot and a queue, a run waits for the queue to end")
	}
	if _, ok := acquire(t, s, HolderQueue, 1); !ok {
		t.Fatal("the queue takes the only slot")
	}
}

func TestAcquireSlotShouldLetTheQueueWaitForASlotRunsHold(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	held, _ := acquire(t, s, HolderRun, 2)
	acquire(t, s, HolderRun, 2)
	enqueue(t, s, 12) // queued once both slots were taken
	if _, ok := acquire(t, s, HolderQueue, 2); ok {
		t.Fatal("no slot is free for the queue")
	}
	if err := s.ReleaseSlot(ctx, held); err != nil {
		t.Fatal(err)
	}
	if _, ok := acquire(t, s, HolderRun, 2); ok {
		t.Fatal("the freed slot is the queue's now")
	}
	if _, ok := acquire(t, s, HolderQueue, 2); !ok {
		t.Fatal("the queue takes the freed slot")
	}
}

func TestPruneSlotsShouldFreeTheSlotsOfDeadProcesses(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO slots (pid, holder, held, created_at) VALUES (999999, 'run', 1, CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	acquire(t, s, HolderDaemon, 2)
	alive := func(pid int) bool { return pid == os.Getpid() }
	if u, err := s.SlotUsage(ctx, alive); err != nil || u.InUse != 1 {
		t.Fatalf("usage leaves out the slots of dead processes: %+v %v", u, err)
	}
	if err := s.PruneSlots(ctx, alive); err != nil {
		t.Fatal(err)
	}
	if u := usage(t, s); u.InUse != 1 {
		t.Fatalf("only the live process keeps its slot: %+v", u)
	}
}
