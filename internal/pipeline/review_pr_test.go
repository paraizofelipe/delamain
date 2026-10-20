package pipeline

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/paraizo/delamain/internal/forge"
	"github.com/paraizo/delamain/internal/gitx"
	"github.com/paraizo/delamain/internal/store"
)

const (
	prHead = "aaaaaaaa" // fakeGit's default HEAD: the worktree is at the PR head
	prBase = "bbbbbbbb"
	// Messages of the fixture findings; inline Keys hash them.
	majorMsg = "parse ignora o erro: retorne-o ao chamador"
	minorMsg = "nome pouco descritivo"
)

// prReviewJSON is a pr-reviewer artifact: one major finding on a.go:3, one minor and one dropped.
func prReviewJSON(findings string) string {
	if findings == "" {
		findings = `[{"severity":"major","category":"erro","file":"a.go","line":3,"evidence":"v, _ := parse(s)","rule":"erros tratados","message":"` + majorMsg + `"},
{"severity":"minor","category":"nome","file":"a.go","line":4,"evidence":"x := v","rule":"","message":"` + minorMsg + `"}]`
	}
	return `{"verdict":"concerns","adherence":"partial","summary":"Adiciona o parse, mas engole o erro.",
"axes":[{"axis":"spec","executed":true,"findings":` + findings + `},
{"axis":"standards","executed":true,"findings":[]},{"axis":"security","executed":true,"findings":[]}],
"dropped":[{"file":"b.go","line":9,"message":"talvez nil","reason":"verificado via grep=nil: já tratado"}],
"learnings":["Os testes usam fakes em fakes_test.go"]}`
}

func reviewsPR(artifact string) behavior {
	return func(string, int, string) reply { return reply{write: artifact} }
}

type reviewHarness struct {
	*harness
	f    *fakeForge
	repo string
}

func newReviewHarness(t *testing.T, b behavior) *reviewHarness {
	t.Helper()
	hs := newHarness(t, b)
	f := &fakeForge{
		pr: forge.PRDetail{
			Number: 5, URL: "https://github.com/acme/api/pull/5", Title: "Add parse", Body: "Closes #3", Author: "alice",
			Open: true, Fork: true, BaseRef: "main", HeadRef: "feat/parse", BaseSHA: prBase, HeadSHA: prHead, StartSHA: prBase,
		},
		threads: []forge.Thread{{ID: "t1", Path: "a.go", Line: 1, Comments: []forge.Comment{{ID: "c1", Author: "bob", Body: "por que um novo pacote?"}}}},
	}
	hs.r.Forge = f
	hs.g.lines = map[string][]gitx.LineRange{"a.go": {{Start: 1, End: 10}}}
	return &reviewHarness{harness: hs, f: f, repo: hs.task.Repo}
}

func (rh *reviewHarness) review(t *testing.T) Result {
	t.Helper()
	res, err := rh.r.RunReview(context.Background(), rh.repo, 5)
	if err != nil {
		t.Fatalf("a review must record its outcome instead of failing: %v", err)
	}
	return res
}

func marker(t *testing.T, body string) forge.Marker {
	t.Helper()
	m, ok := forge.ParseMarker(body)
	if !ok {
		t.Fatalf("every posted body carries a delamain marker:\n%s", body)
	}
	return m
}

func TestRunReviewShouldPostCommentEventWithInlineAndSummary(t *testing.T) {
	rh := newReviewHarness(t, reviewsPR(prReviewJSON("")))
	res := rh.review(t)

	if res.Outcome != OutcomeReviewed {
		t.Fatalf("want reviewed, got %s (%s)", res.Outcome, res.Reason)
	}
	if len(rh.f.posted) != 1 {
		t.Fatalf("one review is posted, got %d", len(rh.f.posted))
	}
	o := rh.f.posted[0]
	if o.HeadSHA != prHead || o.BaseSHA != prBase || o.StartSHA != prBase {
		t.Fatalf("the review is pinned to the reviewed head and base: %+v", o)
	}
	if len(o.Comments) != 1 || o.Comments[0].Path != "a.go" || o.Comments[0].Line != 3 || !strings.Contains(o.Comments[0].Body, majorMsg) {
		t.Fatalf("only the major finding goes inline, on its line: %+v", o.Comments)
	}
	if m := marker(t, o.Comments[0].Body); m.RunID != res.RunID || m.Kind != "review" || m.Key != forge.Key("a.go", "3", majorMsg) {
		t.Fatalf("inline marker %+v, want run %d kind review key of path/line/message", m, res.RunID)
	}
	for _, want := range []string{"Adiciona o parse, mas engole o erro.", minorMsg, "talvez nil"} {
		if !strings.Contains(o.Summary, want) {
			t.Errorf("the summary must carry %q:\n%s", want, o.Summary)
		}
	}
	if m := marker(t, o.Summary); m.RunID != res.RunID || m.Kind != "summary" || m.Key != forge.Key("summary", prHead) {
		t.Fatalf("summary marker %+v, want kind summary keyed by the head", m)
	}
	if res.Review == nil || *res.Review != (ReviewSummary{PRURL: rh.f.pr.URL, Inline: 1, InSummary: 1, Dropped: 1}) {
		t.Fatalf("result counts: %+v", res.Review)
	}
	rec := rh.finalRun(t, res.RunID)
	if rec.Kind != store.KindReview || rec.IssueRef != "pr:5" || rec.State != store.StateRecorded || !strings.HasPrefix(rec.Reason, "reviewed") {
		t.Fatalf("the run is recorded as a review: %+v", rec)
	}
	c, err := rh.st.Cursor(context.Background(), rh.repo, 5)
	if err != nil || c.LastHeadSHA != prHead {
		t.Fatalf("the cursor must remember the reviewed head: %+v %v", c, err)
	}
	prompt := strings.Join(rh.h.prompts["pr-reviewer"], "\n")
	for _, want := range []string{"git diff " + prBase + "...HEAD", "Add parse", "por que um novo pacote?"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the pr-reviewer prompt must carry %q", want)
		}
	}
}

func TestRunReviewShouldFetchMergeRequestRefOnGitLab(t *testing.T) {
	rh := newReviewHarness(t, reviewsPR(prReviewJSON("")))
	rh.f.kind = forge.GitLab
	rh.f.pr.URL = "https://gitlab.com/acme/api/-/merge_requests/5"
	res := rh.review(t)

	if res.Outcome != OutcomeReviewed || len(rh.f.posted) != 1 {
		t.Fatalf("an MR is reviewed like a PR: %s (%s), posted %d", res.Outcome, res.Reason, len(rh.f.posted))
	}
	if len(rh.g.fetchedPR) != 1 || rh.g.fetchedPR[0] != "gitlab 5" {
		t.Fatalf("the MR head comes from GitLab's ref: %v", rh.g.fetchedPR)
	}
	if len(rh.g.fetchedRef) != 1 || rh.g.fetchedRef[0] != "main" {
		t.Fatalf("the base branch is fetched too: %v", rh.g.fetchedRef)
	}
	if len(rh.g.detached) != 1 || rh.g.detached[0] != gitx.PRRef(5) {
		t.Fatalf("the worktree checks out the fetched PR ref: %v", rh.g.detached)
	}
}

func TestRunReviewShouldNotRunAnyCheck(t *testing.T) {
	rh := newReviewHarness(t, reviewsPR(prReviewJSON("")))
	rh.g.onDetached = func(path string) {
		_ = os.WriteFile(filepath.Join(path, "go.mod"), []byte("module evil\n"), 0o600)
	}
	res := rh.review(t)

	if res.Outcome != OutcomeReviewed {
		t.Fatalf("want reviewed, got %s (%s)", res.Outcome, res.Reason)
	}
	if len(rh.h.checkCalls) != 0 {
		t.Fatalf("a review never runs the PR's code: %+v", rh.h.checkCalls)
	}
	for role := range rh.h.prompts {
		if role != "pr-reviewer" {
			t.Errorf("only the pr-reviewer works on a review, got %s", role)
		}
	}
}

func TestRunReviewShouldRemoveWorktreeAndPRRef(t *testing.T) {
	for name, b := range map[string]behavior{
		"reviewed":     reviewsPR(prReviewJSON("")),
		"agent failed": func(string, int, string) reply { return reply{provider: "overloaded"} },
	} {
		t.Run(name, func(t *testing.T) {
			rh := newReviewHarness(t, b)
			res := rh.review(t)

			want := filepath.Join(rh.r.Cfg.Paths.WorktreesDir, filepath.Base(rh.repo), "review-pr-5-"+strconv.FormatInt(res.RunID, 10))
			if res.Worktree != want {
				t.Fatalf("worktree %q, want %q", res.Worktree, want)
			}
			if _, err := os.Stat(res.Worktree); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the review worktree must be removed (%s): %v", res.Outcome, err)
			}
			if len(rh.g.deleted) != 1 || rh.g.deleted[0] != "refs/delamain/pr/5" {
				t.Fatalf("the PR ref must be deleted (%s): %v", res.Outcome, rh.g.deleted)
			}
		})
	}
}

func TestRunReviewShouldWriteReportWithFindingsDroppedAndPRLink(t *testing.T) {
	rh := newReviewHarness(t, reviewsPR(prReviewJSON("")))
	res := rh.review(t)

	report, err := os.ReadFile(res.ReportPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"**reviewed**", rh.f.pr.URL, majorMsg, minorMsg, "talvez nil", "verificado via grep=nil: já tratado"} {
		if !strings.Contains(string(report), want) {
			t.Errorf("report must mention %q:\n%s", want, report)
		}
	}
	if filepath.Dir(res.ReportPath) != res.RunDir {
		t.Fatalf("the report lives in the run dir: %s", res.ReportPath)
	}
}

func TestRunReviewShouldFailWithoutForge(t *testing.T) {
	rh := newReviewHarness(t, reviewsPR(prReviewJSON("")))
	rh.r.Forge = nil
	_, err := rh.r.RunReview(context.Background(), rh.repo, 5)
	if err == nil || !strings.Contains(err.Error(), "review requires a GitHub or GitLab origin") {
		t.Fatalf("a repo without a forge has no PR to review: %v", err)
	}
	if runs, _ := rh.st.RecentRuns(context.Background(), 10); len(runs) != 0 {
		t.Fatalf("no run is recorded: %+v", runs)
	}
}

func TestRunReviewShouldSkipClosedPRWithoutCreatingARun(t *testing.T) {
	rh := newReviewHarness(t, reviewsPR(prReviewJSON("")))
	rh.f.pr.Open = false
	res := rh.review(t)

	if res.Outcome != OutcomeSkipped || res.RunID != 0 {
		t.Fatalf("a closed PR is skipped without a run: %+v", res)
	}
	if runs, _ := rh.st.RecentRuns(context.Background(), 10); len(runs) != 0 {
		t.Fatalf("no run is recorded: %+v", runs)
	}
	if len(rh.g.fetchedPR)+len(rh.f.posted)+len(rh.h.prompts) != 0 {
		t.Fatal("nothing is fetched, prompted or posted for a closed PR")
	}
}
