package cli

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/paraizo/delamain/internal/store"
)

// shortStopWait makes `stop` check on the run every millisecond.
func shortStopWait(t *testing.T) {
	t.Helper()
	prev := stopWaitPoll
	stopWaitPoll = time.Millisecond
	t.Cleanup(func() { stopWaitPoll = prev })
}

func (f *fixture) stopRequested(t *testing.T, id int64) bool {
	t.Helper()
	stop, err := f.st.StopRequested(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return stop
}

func TestStopShouldFailForAnUnknownRun(t *testing.T) {
	f := newFixture(t)
	out, err := execute(t, "--config", f.cfgFile, "stop", "999")
	if err == nil || !strings.Contains(out, "run 999 not found") {
		t.Fatalf("an unknown run is an error: %v\n%s", err, out)
	}
}

func TestStopShouldReportARunThatAlreadyFinished(t *testing.T) {
	f := newFixture(t)
	rec := f.run(t, "#1", "", store.StateRecorded)
	out := f.exec(t, "stop", "#1")
	if !strings.Contains(out, "run 1 already finished (recorded)") {
		t.Fatalf("a finished run has nothing to stop:\n%s", out)
	}
	if f.stopRequested(t, rec.ID) {
		t.Fatal("a finished run gets no stop request")
	}
}

func TestStopNoWaitShouldRecordTheRequestAndReturnAtOnce(t *testing.T) {
	f := newFixture(t)
	rec := f.run(t, "#1", "", store.StateImplement) // owned by this test process, which is alive
	start := time.Now()
	out := f.exec(t, "stop", "1", "--no-wait")
	if want := "stop requested for run 1 (owner pid " + strconv.Itoa(os.Getpid()) + ")"; !strings.Contains(out, want) {
		t.Fatalf("want %q, got:\n%s", want, out)
	}
	if time.Since(start) > 30*time.Second {
		t.Fatal("--no-wait must not wait for the run")
	}
	if !f.stopRequested(t, rec.ID) {
		t.Fatal("the request must be recorded for the owner to see")
	}
	if got, _ := f.st.GetRun(context.Background(), rec.ID); got.State != store.StateImplement {
		t.Fatalf("stop never finishes a live owner's run itself: %+v", got)
	}
}

func TestStopShouldFailWhenTheRunDoesNotStopWithinTheTimeout(t *testing.T) {
	shortStopWait(t)
	f := newFixture(t)
	rec := f.run(t, "#1", "", store.StateImplement)
	out, err := execute(t, "--config", f.cfgFile, "stop", "1", "--timeout", "50ms")
	if err == nil || !strings.Contains(out, "run 1 did not stop within 50ms; the request stays recorded") {
		t.Fatalf("a run that does not stop in time is an error: %v\n%s", err, out)
	}
	if !f.stopRequested(t, rec.ID) {
		t.Fatal("the request stays recorded after the timeout")
	}
}

func TestStopShouldWaitForTheRunAndPrintHowItEnded(t *testing.T) {
	shortStopWait(t)
	f := newFixture(t)
	rec := f.run(t, "#1", "", store.StateImplement)
	ctx := context.Background()
	go func() { // the owner sees the request and records the run as stopped
		for {
			stop, err := f.st.StopRequested(ctx, rec.ID)
			if err != nil {
				return
			}
			if stop {
				break
			}
			time.Sleep(time.Millisecond)
		}
		rec.State, rec.Reason = store.StateRecorded, "halted: parada pelo usuário (delamain stop)"
		_ = f.st.UpdateRun(ctx, rec)
	}()
	out := f.exec(t, "stop", "1", "--timeout", "30s")
	for _, want := range []string{"STATE", "recorded", "RESULT", "halted: parada pelo usuário (delamain stop)"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stop prints how the run ended, missing %q:\n%s", want, out)
		}
	}
}
