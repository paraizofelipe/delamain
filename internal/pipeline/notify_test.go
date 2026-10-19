package pipeline

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paraizo/delamain/internal/notify"
	"github.com/paraizo/delamain/internal/store"
)

const issueURL = "https://github.com/acme/api/issues/7"

// withNotifier wires a fake notifier and gives the task its issue URL on the fake forge.
func withNotifier(hs *harness) (*fakeNotifier, *fakeForge) {
	f := withForge(hs)
	hs.task.URL = issueURL
	n := &fakeNotifier{forge: f}
	hs.r.Notify = n
	return n, f
}

// spends167K makes the implementer's turn cost 167K tokens.
func spends167K(b behavior) behavior {
	return func(role string, n int, text string) reply {
		rep := b(role, n, text)
		if role == "implementer" {
			rep.usage = 167_000
		}
		return rep
	}
}

func only(t *testing.T, n *fakeNotifier, e notify.Event) (notify.Message, int) {
	t.Helper()
	ms, seen := n.of(e)
	if len(ms) != 1 {
		t.Fatalf("want exactly one %s toast, got %d: %+v", e, len(ms), n.sent)
	}
	return ms[0], seen[0]
}

func TestFinishShouldNotifyPROpenedAfterAnnounce(t *testing.T) {
	hs := newHarness(t, spends167K(happy))
	n, _ := withNotifier(hs)
	res := hs.run(t)

	m, comments := only(t, n, notify.EventPROpened)
	if comments != 1 {
		t.Fatalf("the toast goes out after the issue comment, comments seen = %d", comments)
	}
	pr := "https://github.com/acme/api/pull/42"
	if m.RunID != res.RunID || m.Title != "✅ PR opened · repo #7" || m.Body != "Fix add\nPR #42 · 1 iterations · 167K tokens" || m.Urgency != notify.Normal {
		t.Fatalf("toast = %+v", m)
	}
	if len(m.Actions) != 3 || m.Actions[0].Target != pr || m.Actions[2].Target != issueURL {
		t.Fatalf("default opens the PR, and the issue is offered: %+v", m.Actions)
	}
}

func TestFinishShouldNotifyNeedsHumanForDraft(t *testing.T) {
	hs := newHarness(t, spends167K(blockingReview))
	n, _ := withNotifier(hs)
	res := hs.run(t)

	m, comments := only(t, n, notify.EventNeedsHuman)
	if comments != 1 {
		t.Fatalf("the toast goes out after the issue comment, comments seen = %d", comments)
	}
	want := "Fix add\nDraft PR #42 · 3/3 iterations · 501K tokens\n3 iterações sem aprovação"
	if m.Title != "⚠️ Needs review · repo #7" || m.Body != want {
		t.Fatalf("toast = %q / %q, want body %q", m.Title, m.Body, want)
	}
	report := filepath.Join(res.RunDir, "report.md")
	if len(m.Actions) != 3 || m.Actions[2].Target != report {
		t.Fatalf("the report of the run is offered (%s): %+v", report, m.Actions)
	}
	if got, _ := n.of(notify.EventPROpened); len(got) != 0 {
		t.Fatalf("a draft is not an opened PR: %+v", got)
	}
}

func TestFinishShouldNotifyHaltedWithReason(t *testing.T) {
	hs := newHarness(t, func(role string, n int, text string) reply {
		if role == "reviewer" {
			return reply{write: reviewJSON(""), mutate: func(g *fakeGit) { g.commit("refactor: reviewer sneaks a fix") }}
		}
		return happy(role, n, text)
	})
	n, _ := withNotifier(hs)
	res := hs.run(t)

	m, _ := only(t, n, notify.EventHalted)
	lines := strings.Split(m.Body, "\n")
	if m.Title != "⛔ Halted · repo #7" || m.Urgency != notify.Critical || m.Expire != 0 {
		t.Fatalf("toast = %+v", m)
	}
	if len(lines) != 3 || lines[1] != fmt.Sprintf("run %d · review · 1/3 iterations", res.RunID) || !strings.HasPrefix(lines[2], "o reviewer alterou a worktree") {
		t.Fatalf("body must show the state the run stopped in and its reason: %q", m.Body)
	}
}

func TestNeedsInfoShouldNotify(t *testing.T) {
	hs := newHarness(t, happy)
	n, _ := withNotifier(hs)
	hs.task.Body = "Só uma frase vaga."
	hs.run(t)

	m, _ := only(t, n, notify.EventNeedsInfo)
	if m.RunID != 0 || m.Title != "ℹ️ Needs info · repo #7" || m.Body != "Fix add\nIssue relabeled delamain:needs-info" {
		t.Fatalf("toast = %+v", m)
	}
	if len(m.Actions) == 0 || m.Actions[0].Target != issueURL {
		t.Fatalf("default opens the issue: %+v", m.Actions)
	}
}

func TestRunShouldNotifyStartedAfterCreatingTheRun(t *testing.T) {
	hs := newHarness(t, happy)
	n, _ := withNotifier(hs)
	res := hs.run(t)

	if len(n.sent) == 0 || n.sent[0].Event != notify.EventStarted {
		t.Fatalf("the first toast is started: %+v", n.sent)
	}
	m := n.sent[0]
	if m.RunID != res.RunID || m.Title != "▶️ Started · repo #7" || m.Body != fmt.Sprintf("Fix add\nrun %d · omp", res.RunID) {
		t.Fatalf("toast = %+v", m)
	}
}

func TestNilNotifierShouldBeANoOp(t *testing.T) {
	hs := newHarness(t, happy)
	withForge(hs)
	hs.r.Notify = nil
	if res := hs.run(t); res.Outcome != OutcomeApproved || res.PRURL == "" {
		t.Fatalf("a run without a notifier is delivered as usual: %s %s", res.Outcome, res.Reason)
	}
}

func TestFinishShouldKeepTheResultWhenNotifyFails(t *testing.T) {
	hs := newHarness(t, happy)
	n, _ := withNotifier(hs)
	n.err = errors.New("org.freedesktop.Notifications: timeout")
	var log bytes.Buffer
	hs.r.Log = slog.New(slog.NewTextHandler(&log, nil))
	res := hs.run(t)

	if len(n.sent) == 0 {
		t.Fatal("the notifier must have been asked")
	}
	if res.Outcome != OutcomeApproved || res.PRURL == "" || len(res.Warnings) != 0 {
		t.Fatalf("a failed toast changes nothing in the result: %s %q warnings=%v", res.Outcome, res.PRURL, res.Warnings)
	}
	if !strings.Contains(log.String(), "desktop notification failed") {
		t.Fatalf("the failure is logged as a warning:\n%s", log.String())
	}
	if rec := hs.finalRun(t, res.RunID); rec.State != store.StateRecorded {
		t.Fatalf("the run is still recorded: %+v", rec)
	}
}

func TestRecoveredRunsShouldNotNotifyIndividually(t *testing.T) {
	hs := newHarness(t, happy)
	n, _ := withNotifier(hs)
	crashed(t, hs, store.StateImplement)
	fixAdd(hs.g)
	if out := recoverAll(t, hs); len(out) != 1 {
		t.Fatalf("results = %+v", out)
	}
	if len(n.sent) != 0 {
		t.Fatalf("recovered runs are summed up by the recover toast, got %+v", n.sent)
	}
}
