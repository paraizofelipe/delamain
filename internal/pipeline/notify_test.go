package pipeline

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/paraizo/delamain/internal/notify"
)

// fakeNotifier records the toasts; before, when set, runs as each one is sent.
type fakeNotifier struct {
	mu     sync.Mutex
	got    []notify.Message
	before func(notify.Message)
}

func (n *fakeNotifier) Notify(_ context.Context, m notify.Message) error {
	if n.before != nil {
		n.before(m)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.got = append(n.got, m)
	return nil
}

func (n *fakeNotifier) Close() error { return nil }

func (n *fakeNotifier) messages() []notify.Message {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone(n.got)
}

// withNotifier wires a fake notifier and gives the task an issue URL.
func withNotifier(hs *harness) *fakeNotifier {
	n := &fakeNotifier{}
	hs.r.Notify = n
	hs.task.URL = "https://github.com/acme/api/issues/7"
	return n
}

// costly makes every agent turn consume 1500 tokens.
func costly(b behavior) behavior {
	return func(role string, n int, text string) reply {
		rep := b(role, n, text)
		rep.usage = 1500
		return rep
	}
}

func onlyMessage(t *testing.T, n *fakeNotifier) notify.Message {
	t.Helper()
	got := n.messages()
	if len(got) != 1 {
		t.Fatalf("want exactly one toast, got %d: %+v", len(got), got)
	}
	return got[0]
}

func TestFinishShouldNotifyPROpenedAfterAnnounce(t *testing.T) {
	hs := newHarness(t, costly(happy))
	f := withForge(hs)
	n := withNotifier(hs)
	announced := false
	n.before = func(notify.Message) {
		last := f.relabels[len(f.relabels)-1]
		announced = len(f.comments) == 1 && slices.Equal(last[0], []string{hs.r.Cfg.Forge.Labels.MROpened})
	}
	res := hs.run(t)

	m := onlyMessage(t, n)
	if !announced {
		t.Fatal("the toast goes out after the issue is labeled and commented")
	}
	pr := "https://github.com/acme/api/pull/42"
	want := notify.Message{
		Event: notify.EventPROpened, RunID: res.RunID,
		Title:   "✅ PR opened · repo #7",
		Body:    "Fix add\nPR #42 · 1 iterations · 4.5K tokens",
		Urgency: notify.Normal, Expire: 15000, Category: "x-delamain.pr",
		Actions: []notify.Action{
			{Key: "default", Label: "Open PR", Target: pr},
			{Key: "open-pr", Label: "Open PR", Target: pr},
			{Key: "open-issue", Label: "Open issue", Target: hs.task.URL},
		},
		ForgeHost: "github.com",
	}
	if !equalMessage(m, want) {
		t.Fatalf("toast\n got: %#v\nwant: %#v", m, want)
	}
}

func equalMessage(a, b notify.Message) bool {
	return slices.Equal(a.Actions, b.Actions) && a.Event == b.Event && a.RunID == b.RunID && a.Title == b.Title &&
		a.Body == b.Body && a.Urgency == b.Urgency && a.Expire == b.Expire && a.Category == b.Category && a.ForgeHost == b.ForgeHost
}

func TestFinishShouldNotifyNeedsHumanForDraft(t *testing.T) {
	hs := newHarness(t, costly(blockingReview))
	withForge(hs)
	n := withNotifier(hs)
	res := hs.run(t)

	m := onlyMessage(t, n)
	pr, report := "https://github.com/acme/api/pull/42", filepath.Join(res.RunDir, "report.md")
	want := notify.Message{
		Event: notify.EventNeedsHuman, RunID: res.RunID,
		Title:   "⚠️ Needs review · repo #7",
		Body:    "Fix add\nDraft PR #42 · 3/3 iterations · 10.5K tokens\n3 iterações sem aprovação",
		Urgency: notify.Normal, Expire: 15000, Category: "x-delamain.needs-human",
		Actions: []notify.Action{
			{Key: "default", Label: "Open PR", Target: pr},
			{Key: "open-pr", Label: "Open PR", Target: pr},
			{Key: "open-report", Label: "Open report", Target: report},
		},
		ForgeHost: "github.com",
	}
	if !equalMessage(m, want) {
		t.Fatalf("toast\n got: %#v\nwant: %#v", m, want)
	}
	if res.ReportPath != report {
		t.Fatalf("the report action must open the report the run wrote, %q != %q", report, res.ReportPath)
	}
}

func TestFinishShouldNotifyHaltedWithReason(t *testing.T) {
	hs := newHarness(t, func(role string, n int, text string) reply {
		if role == "reviewer" {
			return reply{write: reviewJSON(""), mutate: func(g *fakeGit) { g.commit("refactor: reviewer sneaks a fix") }}
		}
		return happy(role, n, text)
	})
	withForge(hs)
	n := withNotifier(hs)
	res := hs.run(t)

	m := onlyMessage(t, n)
	if res.Outcome != OutcomeHalted || res.PRURL == "" {
		t.Fatalf("setup: want a halted run delivered as draft, got %s %q", res.Outcome, res.PRURL)
	}
	title, line2, reason := splitBody(t, m.Body)
	if m.Event != notify.EventHalted || m.Title != "⛔ Halted · repo #7" || m.Urgency != notify.Critical || m.Expire != 0 || m.Category != "x-delamain.halted" {
		t.Fatalf("halted beats the draft PR and never expires: %#v", m)
	}
	if title != "Fix add" || line2 != fmt.Sprintf("run %d · review · 1/3 iterations", res.RunID) || !strings.HasPrefix(reason, "o reviewer alterou a worktree") {
		t.Fatalf("body = %q", m.Body)
	}
	report := filepath.Join(res.RunDir, "report.md")
	if want := []notify.Action{
		{Key: "default", Label: "Open report", Target: report},
		{Key: "open-report", Label: "Open report", Target: report},
		{Key: "open-issue", Label: "Open issue", Target: hs.task.URL},
	}; !slices.Equal(m.Actions, want) {
		t.Fatalf("actions = %+v", m.Actions)
	}
}

func splitBody(t *testing.T, body string) (string, string, string) {
	t.Helper()
	l := strings.Split(body, "\n")
	if len(l) != 3 {
		t.Fatalf("want 3 body lines, got %q", body)
	}
	return l[0], l[1], l[2]
}

func TestNilNotifierShouldBeANoOp(t *testing.T) {
	hs := newHarness(t, happy)
	withForge(hs)
	hs.r.Notify = nil
	hs.r.Cfg.Notify.Events = append(hs.r.Cfg.Notify.Events, "started")
	if res := hs.run(t); res.Outcome != OutcomeApproved {
		t.Fatalf("a run without notifier works as before, got %s: %s", res.Outcome, res.Reason)
	}
}

func TestDisabledEventsShouldNotNotify(t *testing.T) {
	hs := newHarness(t, happy)
	withForge(hs)
	n := withNotifier(hs)
	hs.r.Cfg.Notify.Events = []string{"halted"}
	hs.run(t)
	if got := n.messages(); len(got) != 0 {
		t.Fatalf("pr_opened is filtered out, got %+v", got)
	}

	hs = newHarness(t, happy)
	withForge(hs)
	n = withNotifier(hs)
	hs.r.Cfg.Notify.Enabled = false
	hs.run(t)
	if got := n.messages(); len(got) != 0 {
		t.Fatalf("notify.enabled=false silences every event, got %+v", got)
	}
}
