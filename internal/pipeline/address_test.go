package pipeline

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/paraizo/delamain/internal/forge"
	"github.com/paraizo/delamain/internal/gitx"
	"github.com/paraizo/delamain/internal/store"
)

const (
	bot          = "delamain-bot"
	invalidReply = "O erro já é tratado em parse.go:12; o comentário não se aplica."
	validReply   = "Procede: o nome confunde e vale renomear numa próxima rodada."
	learning     = "Erros de parse são tratados em parse.go, não no chamador"
)

// t0 is when the fixture's discussions started.
var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// response is one entry of a responder artifact.
func response(thread, verdict, text string) string {
	return `{"thread_id":"` + thread + `","verdict":"` + verdict + `","reply":"` + text + `","fixed":false}`
}

// responsesJSON is a responder artifact answering the given responses.
func responsesJSON(responses ...string) string {
	return `{"responses":[` + strings.Join(responses, ",") + `],"learnings":["` + learning + `"]}`
}

// answers is the responder of the fixture: one answer per pending thread.
func answers(artifact string) behavior {
	return func(string, int, string) reply { return reply{write: artifact} }
}

// newAddressHarness is delamain's own PR #5 with two discussions waiting for an answer: an
// inline one on a.go:3 and a PR-level comment.
func newAddressHarness(t *testing.T, b behavior) *reviewHarness {
	t.Helper()
	rh := newReviewHarness(t, b)
	rh.f.pr.Author = bot
	rh.f.threads = []forge.Thread{
		{ID: "t1", Path: "a.go", Line: 3, Comments: []forge.Comment{{ID: "c1", Author: "bob", Body: "parse ignora o erro", CreatedAt: t0.Add(time.Hour)}}},
		{ID: "issue-comment:9", Comments: []forge.Comment{{ID: "9", Author: "carol", Body: "o nome x confunde", CreatedAt: t0.Add(2 * time.Hour)}}},
	}
	return rh
}

func happyAnswers() string {
	return responsesJSON(response("t1", "invalid", invalidReply), response("issue-comment:9", "valid", validReply))
}

func (rh *reviewHarness) address(t *testing.T) Result {
	t.Helper()
	res, err := rh.r.RunAddress(context.Background(), rh.repo, 5)
	if err != nil {
		t.Fatalf("addressing must record its outcome instead of failing: %v", err)
	}
	return res
}

func TestRunAddressShouldReplyToEachPendingThreadWithMarker(t *testing.T) {
	rh := newAddressHarness(t, answers(happyAnswers()))
	res := rh.address(t)

	if res.Outcome != OutcomeAnswered {
		t.Fatalf("want answered, got %s (%s)", res.Outcome, res.Reason)
	}
	if len(rh.f.replies) != 2 {
		t.Fatalf("one reply per pending thread, got %+v", rh.f.replies)
	}
	want := map[string]struct{ text, verdict, last string }{
		"t1":              {invalidReply, "invalid", "c1"},
		"issue-comment:9": {validReply, "valid", "9"},
	}
	for _, rp := range rh.f.replies {
		w, ok := want[rp.thread]
		if !ok {
			t.Fatalf("reply posted to a thread nobody asked about: %s", rp.thread)
		}
		if !strings.Contains(rp.body, w.text) || !strings.Contains(rp.body, w.verdict) {
			t.Errorf("the reply to %s carries the responder's words and verdict:\n%s", rp.thread, rp.body)
		}
		if m := marker(t, rp.body); m.RunID != res.RunID || m.Kind != "reply" || m.Key != forge.Key(rp.thread, w.last) {
			t.Errorf("reply marker %+v, want run %d kind reply keyed by thread and last comment", m, res.RunID)
		}
		delete(want, rp.thread)
	}
	rec := rh.finalRun(t, res.RunID)
	if rec.Kind != store.KindAddress || rec.IssueRef != "addr:5" || rec.State != store.StateRecorded || !strings.HasPrefix(rec.Reason, "answered") {
		t.Fatalf("the run is recorded as addressing the PR: %+v", rec)
	}
	if len(rh.f.posted)+len(rh.f.resolved) != 0 {
		t.Fatalf("addressing only replies: posted %+v, resolved %v", rh.f.posted, rh.f.resolved)
	}
}

func TestRunAddressShouldSendOnlyPendingThreadsWithHistoryAndExcerptInsideThreadsDelimiter(t *testing.T) {
	rh := newAddressHarness(t, answers(responsesJSON(response("t1", "invalid", invalidReply))))
	long := "Começo do comentário longo " + strings.Repeat("detalhe ", 150) + "fim do comentário longo"
	rh.f.threads = []forge.Thread{
		{ID: "t1", Path: "a.go", Line: 3, Comments: []forge.Comment{
			{ID: "c1", Author: "bob", Body: "parse ignora o erro", CreatedAt: t0.Add(time.Hour)},
			{ID: "c2", Author: bot, Body: "Já tratado.\n\n" + forge.Marker{RunID: 1, Kind: "reply", Key: forge.Key("t1", "c1")}.String(), CreatedAt: t0.Add(2 * time.Hour)},
			{ID: "c3", Author: "bob", Body: long, CreatedAt: t0.Add(3 * time.Hour)},
		}},
		{ID: "t2", Path: "b.go", Line: 1, Comments: []forge.Comment{
			{ID: "d1", Author: "alice", Body: "discussao ja respondida", CreatedAt: t0.Add(time.Hour)},
			{ID: "d2", Author: bot, Body: "Respondido.\n\n" + forge.Marker{RunID: 1, Kind: "reply", Key: forge.Key("t2", "d1")}.String(), CreatedAt: t0.Add(2 * time.Hour)},
		}},
	}
	lines := make([]string, 20)
	for i := range lines {
		lines[i] = "linha" + strconv.Itoa(i+1) + " do head"
	}
	rh.g.files[gitx.PRRef(5)+":a.go"] = strings.Join(lines, "\n")
	if res := rh.address(t); res.Outcome != OutcomeAnswered {
		t.Fatalf("got %s (%s)", res.Outcome, res.Reason)
	}

	prompt := strings.Join(rh.h.prompts["responder"], "\n")
	open, closing := strings.Index(prompt, "<threads>"), strings.Index(prompt, "</threads>")
	if open < 0 || closing < open {
		t.Fatalf("the discussions go inside <threads>…</threads>:\n%s", prompt)
	}
	threads := prompt[open:closing]
	pending, others, ok := strings.Cut(threads, "Outras discussões")
	if !ok {
		t.Fatalf("the threads waiting for an answer are set apart from the rest:\n%s", threads)
	}
	for _, want := range []string{"t1", "parse ignora o erro", "Já tratado.", long, "linha8 do head", "linha3 do head", "linha1 do head"} {
		if !strings.Contains(pending, want) {
			t.Errorf("the pending thread goes whole, with its history and excerpt: missing %q", want)
		}
	}
	if strings.Contains(pending, "linha9 do head") {
		t.Error("the excerpt is the commented line ±5, not the whole file")
	}
	if strings.Contains(pending, "t2") || !strings.Contains(others, "t2") {
		t.Errorf("an answered thread is context only, never pending:\n%s", threads)
	}
}

func TestRunAddressShouldAdvanceTheCursorToTheNewestHandledComment(t *testing.T) {
	rh := newAddressHarness(t, answers(happyAnswers()))
	// A review of this PR already recorded its head: addressing shares that row.
	if err := rh.st.SaveCursor(context.Background(), store.ReviewCursor{Repo: rh.repo, PR: 5, LastSeenAt: t0, Rounds: 1, LastHeadSHA: prHead}); err != nil {
		t.Fatal(err)
	}
	if res := rh.address(t); res.Outcome != OutcomeAnswered {
		t.Fatalf("got %s (%s)", res.Outcome, res.Reason)
	}
	cur := rh.cursor(t)
	if !cur.LastSeenAt.Equal(t0.Add(2 * time.Hour)) {
		t.Fatalf("the cursor moves to the newest comment answered (%s), got %s", t0.Add(2*time.Hour), cur.LastSeenAt)
	}
	if cur.Rounds != 2 || cur.LastHeadSHA != prHead {
		t.Fatalf("one more round, the reviewed head kept: %+v", cur)
	}
}

func TestRunAddressShouldAppendFalsePositiveLearningsToMemory(t *testing.T) {
	rh := newAddressHarness(t, answers(happyAnswers()))
	if res := rh.address(t); res.Outcome != OutcomeAnswered {
		t.Fatalf("got %s (%s)", res.Outcome, res.Reason)
	}
	b, err := os.ReadFile(filepath.Join(rh.r.Cfg.Paths.MemoryDir(), "github.com", "acme", "api.md"))
	if err != nil {
		t.Fatalf("the learnings go to the repository memory: %v", err)
	}
	if !strings.Contains(string(b), learning) || !strings.Contains(string(b), "pr #5") {
		t.Fatalf("the memory records the learning and its PR:\n%s", b)
	}
}

func TestRunAddressShouldRemoveWorktreeAndPRRefAndWriteReport(t *testing.T) {
	for name, b := range map[string]behavior{
		"answered":     answers(happyAnswers()),
		"agent failed": func(string, int, string) reply { return reply{provider: "overloaded"} },
	} {
		t.Run(name, func(t *testing.T) {
			rh := newAddressHarness(t, b)
			res := rh.address(t)

			if res.Worktree == "" {
				t.Fatalf("the PR head is checked out (%s: %s)", res.Outcome, res.Reason)
			}
			if _, err := os.Stat(res.Worktree); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the worktree must be removed (%s): %v", res.Outcome, err)
			}
			if len(rh.g.deleted) != 1 || rh.g.deleted[0] != gitx.PRRef(5) {
				t.Fatalf("the PR ref must be deleted (%s): %v", res.Outcome, rh.g.deleted)
			}
			report, err := os.ReadFile(res.ReportPath)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"**" + string(res.Outcome) + "**", rh.f.pr.URL} {
				if !strings.Contains(string(report), want) {
					t.Errorf("report must mention %q:\n%s", want, report)
				}
			}
		})
	}
}

func TestRunAddressShouldSkipClosedPRWithoutCreatingARun(t *testing.T) {
	rh := newAddressHarness(t, answers(happyAnswers()))
	rh.f.pr.Open = false
	res := rh.address(t)

	if res.Outcome != OutcomeSkipped || res.RunID != 0 {
		t.Fatalf("a closed PR is skipped without a run: %+v", res)
	}
	if runs, _ := rh.st.RecentRuns(context.Background(), 10); len(runs) != 0 {
		t.Fatalf("no run is recorded: %+v", runs)
	}
	if len(rh.g.fetchedPR)+len(rh.f.replies)+len(rh.h.prompts) != 0 {
		t.Fatal("nothing is fetched, prompted or replied for a closed PR")
	}
}
