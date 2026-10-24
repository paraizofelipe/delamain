package store

import (
	"context"
	"os"
	"testing"
)

func TestAcquireDaemonSlotShouldStopAtTheLimit(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if _, ok, err := s.AcquireDaemonSlot(ctx, 2); !ok || err != nil {
			t.Fatalf("slot %d: %v %v", i, ok, err)
		}
	}
	if _, ok, err := s.AcquireDaemonSlot(ctx, 2); ok || err != nil {
		t.Fatalf("a third slot is over the limit: %v %v", ok, err)
	}
	if n, err := s.SlotsInUse(ctx); n != 2 || err != nil {
		t.Fatalf("in use: %d %v", n, err)
	}
}

func TestAWaitingRunShouldGoAheadOfTheDaemon(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	held, ok, err := s.AcquireDaemonSlot(ctx, 1)
	if !ok || err != nil {
		t.Fatal(ok, err)
	}
	wait, err := s.WaitSlot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.TakeSlot(ctx, wait, 1); ok || err != nil {
		t.Fatalf("the run waits while the slots are held: %v %v", ok, err)
	}
	if err := s.ReleaseSlot(ctx, held); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.AcquireDaemonSlot(ctx, 1); ok || err != nil {
		t.Fatalf("the daemon does not take a slot a run waits for: %v %v", ok, err)
	}
	if ok, err := s.TakeSlot(ctx, wait, 1); !ok || err != nil {
		t.Fatalf("the run takes the freed slot: %v %v", ok, err)
	}
	if err := s.ReleaseSlot(ctx, wait); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.AcquireDaemonSlot(ctx, 1); !ok || err != nil {
		t.Fatalf("with no run waiting the daemon gets the slot back: %v %v", ok, err)
	}
}

func TestPruneSlotsShouldFreeTheSlotsOfDeadProcesses(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO slots (pid, holder, held, created_at) VALUES (999999, 'run', 1, CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.AcquireDaemonSlot(ctx, 2); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if err := s.PruneSlots(ctx, func(pid int) bool { return pid == os.Getpid() }); err != nil {
		t.Fatal(err)
	}
	if n, err := s.SlotsInUse(ctx); n != 1 || err != nil {
		t.Fatalf("only the live process keeps its slot: %d %v", n, err)
	}
}
