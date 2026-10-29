package pipeline

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/paraizo/delamain/internal/forge"
	"github.com/paraizo/delamain/internal/gitx"
)

// withForge wires a fake forge and turns the task into issue #7.
func withForge(hs *harness) *fakeForge {
	f := &fakeForge{}
	hs.r.Forge = f
	hs.task.Issue = 7
	return f
}

func blockingReview(role string, n int, text string) reply {
	if role == "reviewer" {
		return reply{write: reviewJSON("major")}
	}
	return happy(role, n, text)
}

func TestDeliverShouldPushAndOpenReadyPRWhenApproved(t *testing.T) {
	hs := newHarness(t, happy)
	f := withForge(hs)
	res := hs.run(t)

	if res.Outcome != OutcomeApproved || res.PRURL != "https://github.com/acme/api/pull/42" || res.Draft {
		t.Fatalf("approved work must become a ready PR: %s %s url=%q draft=%v", res.Outcome, res.Reason, res.PRURL, res.Draft)
	}
	if !slices.Equal(hs.g.pushed, []string{"fix/7-fix-add"}) {
		t.Fatalf("only the work branch is pushed, pushed=%v", hs.g.pushed)
	}
	if len(f.created) != 1 {
		t.Fatalf("exactly one PR, got %d", len(f.created))
	}
	pr := f.created[0]
	if pr.Head != "fix/7-fix-add" || pr.Base != "main" || pr.Draft || pr.Title != "fix: correct the add operation" {
		t.Fatalf("pr options = %+v", pr)
	}
	for _, want := range []string{"Closes #7", "fix add", "Add(2,3)=5", "calc_test.go::TestAdd", "após 1 iteração."} {
		if !strings.Contains(pr.Body, want) {
			t.Errorf("PR body must mention %q:\n%s", want, pr.Body)
		}
	}
	if rec := hs.finalRun(t, res.RunID); rec.MRURL != res.PRURL {
		t.Fatalf("store must keep the PR URL, got %q", rec.MRURL)
	}
}

// implTitled is implJSON with another pr_title; "" leaves the field out.
func implTitled(title string) string {
	field := `,"pr_title":"fix: correct the add operation"`
	if title == "" {
		return strings.Replace(implJSON, field, "", 1)
	}
	return strings.Replace(implJSON, field, `,"pr_title":"`+title+`"`, 1)
}

func TestRunShouldOpenPRWithImplementerTitle(t *testing.T) {
	hs := newHarness(t, func(role string, n int, text string) reply {
		if role == "implementer" {
			return reply{write: implTitled("fix: make add return the sum"), mutate: fixAdd}
		}
		return happy(role, n, text)
	})
	f := withForge(hs)
	res := hs.run(t)

	if res.Outcome != OutcomeApproved || len(f.created) != 1 {
		t.Fatalf("got %s (%s) created=%+v", res.Outcome, res.Reason, f.created)
	}
	if got := f.created[0].Title; got != "fix: make add return the sum" {
		t.Fatalf("a valid pr_title must title the PR, got %q", got)
	}
}

func TestRunShouldKeepPreviousPRTitleWhenIterationLeavesItEmpty(t *testing.T) {
	hs := newHarness(t, func(role string, n int, text string) reply {
		switch {
		case role == "implementer" && n == 1:
			return reply{write: implTitled("fix: make add return the sum"), mutate: fixAdd}
		case role == "implementer":
			return reply{write: implTitled(""), mutate: func(g *fakeGit) { g.commit("test: cover add") }}
		case role == "reviewer" && n == 1:
			return reply{write: reviewJSON("major")}
		}
		return happy(role, n, text)
	})
	f := withForge(hs)
	res := hs.run(t)

	if res.Outcome != OutcomeApproved || res.Iterations != 2 || len(f.created) != 1 {
		t.Fatalf("got %s (%s) iterations=%d created=%+v", res.Outcome, res.Reason, res.Iterations, f.created)
	}
	if got := f.created[0].Title; got != "fix: make add return the sum" {
		t.Fatalf("an iteration without pr_title keeps the previous one, got %q", got)
	}
}

func TestRunShouldFallBackToIssueTitleWhenPRTitleIsInvalid(t *testing.T) {
	hs := newHarness(t, func(role string, n int, text string) reply {
		if role == "implementer" {
			return reply{write: implTitled("feat: adiciona exportação"), mutate: fixAdd}
		}
		return happy(role, n, text)
	})
	f := withForge(hs)
	hs.task.Title = "Corrigir a validação da exportação quando a configuração não tem o campo de destino"
	res := hs.run(t)

	if res.Outcome != OutcomeExhausted || len(f.created) != 1 || !f.created[0].Draft {
		t.Fatalf("an invalid pr_title on every iteration ends in a draft: %s (%s) created=%+v", res.Outcome, res.Reason, f.created)
	}
	// Cut at 72 runes, not bytes, and the trailing space after the cut is dropped.
	const want = "fix: corrigir a validação da exportação quando a configuração não tem o"
	if got := f.created[0].Title; got != want || utf8.RuneCountInString(got) > 72 {
		t.Fatalf("fallback title = %q (%d runes), want %q", got, utf8.RuneCountInString(got), want)
	}
}

func TestDeliverShouldOpenDraftWhenIterationsAreExhausted(t *testing.T) {
	hs := newHarness(t, blockingReview)
	f := withForge(hs)
	res := hs.run(t)

	if res.Outcome != OutcomeExhausted || !res.Draft || len(f.created) != 1 || !f.created[0].Draft {
		t.Fatalf("exhausted work must become a draft: %s draft=%v created=%+v", res.Outcome, res.Draft, f.created)
	}
	body := f.created[0].Body
	if !strings.Contains(body, "subtrai em vez de somar") || !strings.Contains(body, "3 iterações sem aprovação") {
		t.Fatalf("draft body must say why a human is needed:\n%s", body)
	}
}

func TestDeliverShouldOpenDraftWhenHaltedAfterCommits(t *testing.T) {
	hs := newHarness(t, func(role string, n int, text string) reply {
		if role == "reviewer" {
			return reply{provider: "529 overloaded"}
		}
		return happy(role, n, text)
	})
	f := withForge(hs)
	res := hs.run(t)

	if res.Outcome != OutcomeHalted || len(f.created) != 1 || !f.created[0].Draft {
		t.Fatalf("committed work must not be lost on a halt: %s (%s) created=%+v", res.Outcome, res.Reason, f.created)
	}
	if !strings.Contains(f.created[0].Body, "529 overloaded") {
		t.Fatalf("draft must carry the halt reason:\n%s", f.created[0].Body)
	}
}

func TestDeliverShouldOpenDraftWhenTheReviewerFailsToStartAfterCommits(t *testing.T) {
	hs := newHarness(t, happy)
	hs.h.startErr = failStart("reviewer")
	f := withForge(hs)
	res := hs.run(t)

	if res.Outcome != OutcomeHalted || len(f.created) != 1 || !f.created[0].Draft {
		t.Fatalf("a late agent start must not lose committed work: %s (%s) created=%+v", res.Outcome, res.Reason, f.created)
	}
	if !strings.Contains(f.created[0].Body, "o agente reviewer não subiu") {
		t.Fatalf("draft must carry the halt reason:\n%s", f.created[0].Body)
	}
}

func TestDeliverShouldDoNothingWithoutCommits(t *testing.T) {
	hs := newHarness(t, happy)
	hs.h.startErr = failStart("planner")
	f := withForge(hs)
	res := hs.run(t)

	if res.Outcome != OutcomeHalted || len(hs.g.pushed) != 0 || len(f.created) != 0 {
		t.Fatalf("nothing to deliver: pushed=%v created=%v", hs.g.pushed, f.created)
	}
}

func TestDeliverShouldKeepWorkLocalWhenThereIsNoForge(t *testing.T) {
	hs := newHarness(t, happy)
	res := hs.run(t)

	if res.Outcome != OutcomeApproved || len(hs.g.pushed) != 0 || res.PRURL != "" {
		t.Fatalf("no forge: no push, got pushed=%v url=%q", hs.g.pushed, res.PRURL)
	}
	if !strings.Contains(res.Delivery, "local") {
		t.Fatalf("the skipped delivery must be explicit (fail loud), got %q", res.Delivery)
	}
}

func TestDeliverShouldHaltLoudlyWhenPushFails(t *testing.T) {
	hs := newHarness(t, happy)
	f := withForge(hs)
	hs.g.pushErr = errors.New("remote: permission denied")
	res := hs.run(t)

	if res.Outcome != OutcomeHalted || !strings.Contains(res.Reason, "entrega falhou") || !strings.Contains(res.Reason, "permission denied") {
		t.Fatalf("got %s: %s", res.Outcome, res.Reason)
	}
	if len(f.created) != 0 {
		t.Fatal("no PR without a pushed branch")
	}
}

func TestDeliverShouldHaltWhenPRCreationFails(t *testing.T) {
	hs := newHarness(t, happy)
	f := withForge(hs)
	f.createErr = errors.New("gh pr create: HTTP 422")
	res := hs.run(t)
	if res.Outcome != OutcomeHalted || !strings.Contains(res.Reason, "HTTP 422") {
		t.Fatalf("got %s: %s", res.Outcome, res.Reason)
	}
}

func TestDeliverShouldRefuseWhenBranchAppearedOnOriginDuringTheRun(t *testing.T) {
	hs := newHarness(t, func(role string, n int, text string) reply {
		rep := happy(role, n, text)
		if role == "implementer" {
			commit := rep.mutate
			rep.mutate = func(g *fakeGit) { commit(g); g.remote["fix/7-fix-add"] = true } // an agent pushed
		}
		return rep
	})
	f := withForge(hs)
	res := hs.run(t)

	if res.Outcome != OutcomeHalted || !strings.Contains(res.Reason, "push") || len(hs.g.pushed) != 0 || len(f.created) != 0 {
		t.Fatalf("an agent push must block delivery: %s (%s) pushed=%v created=%v", res.Outcome, res.Reason, hs.g.pushed, f.created)
	}
}

func TestSetupShouldHaltWhenAnOpenPRAlreadyExistsForTheBranch(t *testing.T) {
	hs := newHarness(t, happy)
	f := withForge(hs)
	f.open = &forge.PR{Number: 3, URL: "https://github.com/acme/api/pull/3"}
	res := hs.run(t)

	if res.Outcome != OutcomeHalted || !strings.Contains(res.Reason, "pull/3") || hs.g.worktreeOps != 0 {
		t.Fatalf("existing PR must stop before any worktree: %s (%s) worktrees=%d", res.Outcome, res.Reason, hs.g.worktreeOps)
	}
}

func TestSetupShouldHaltWhenBranchAlreadyExistsOnOrigin(t *testing.T) {
	hs := newHarness(t, happy)
	withForge(hs)
	hs.g.remote["fix/7-fix-add"] = true
	if res := hs.run(t); res.Outcome != OutcomeHalted || !strings.Contains(res.Reason, "origin") || hs.g.worktreeOps != 0 {
		t.Fatalf("got %s (%s) worktrees=%d", res.Outcome, res.Reason, hs.g.worktreeOps)
	}
}

func TestSetupShouldStartEveryAgentWithPushBlocked(t *testing.T) {
	hs := newHarness(t, happy)
	hs.run(t)
	for _, role := range []string{"planner", "implementer", "reviewer"} {
		if env := hs.h.starts[role+"-r1"].Env; !slices.Equal(env, gitx.AgentEnv()) {
			t.Errorf("%s must start with the push-blocking env, got %v", role, env)
		}
	}
	if log := hs.h.tails["w9:p1"]; !strings.HasSuffix(log, "/logs/checks.log'") {
		t.Errorf("the checks tab (root) follows the gates' output, not an agent log: %q", log)
	}
}

func TestIssueShouldMoveFromInProgressToMROpenedWithAComment(t *testing.T) {
	hs := newHarness(t, happy)
	f := withForge(hs)
	res := hs.run(t)

	l := hs.r.Cfg.Forge.Labels
	want := [][2][]string{
		{{l.InProgress}, {l.Ready}},
		{{l.MROpened}, {l.InProgress}},
	}
	if len(f.relabels) != 2 || !slices.Equal(f.relabels[0][0], want[0][0]) || !slices.Equal(f.relabels[0][1], want[0][1]) ||
		!slices.Equal(f.relabels[1][0], want[1][0]) || !slices.Equal(f.relabels[1][1], want[1][1]) {
		t.Fatalf("label transitions = %v, want %v", f.relabels, want)
	}
	if len(f.comments) != 1 || !strings.Contains(f.comments[0], res.PRURL) {
		t.Fatalf("issue must get a status comment with the PR link: %v", f.comments)
	}
}

func TestIssueShouldBeFlaggedForHumansWhenWorkIsDraft(t *testing.T) {
	hs := newHarness(t, blockingReview)
	f := withForge(hs)
	hs.run(t)
	last := f.relabels[len(f.relabels)-1]
	if !slices.Equal(last[0], []string{hs.r.Cfg.Forge.Labels.NeedsHuman}) {
		t.Fatalf("draft work needs a human, final labels = %v", last)
	}
}

func TestIssueShouldGetNeedsInfoWhenPreflightFails(t *testing.T) {
	hs := newHarness(t, happy)
	f := withForge(hs)
	hs.task.Body = "sem critérios"
	res := hs.run(t)

	if res.Outcome != OutcomeNeedsInfo || len(f.comments) != 1 || !strings.Contains(f.comments[0], "critérios de aceite") {
		t.Fatalf("issue must explain what is missing: %s %v", res.Outcome, f.comments)
	}
	if len(f.relabels) != 1 || !slices.Contains(f.relabels[0][0], hs.r.Cfg.Forge.Labels.NeedsInfo) {
		t.Fatalf("labels = %v", f.relabels)
	}
	if runs, _ := hs.st.UnfinishedRuns(context.Background()); len(runs) != 0 || hs.g.worktreeOps != 0 {
		t.Fatal("needs_info must not create a run or a worktree")
	}
}

func TestLocalTasksShouldNeverTouchIssues(t *testing.T) {
	hs := newHarness(t, happy)
	f := withForge(hs)
	hs.task.Issue = 0 // `run --task`: there is no issue to label
	res := hs.run(t)
	if len(f.relabels) != 0 || len(f.comments) != 0 || res.PRURL == "" {
		t.Fatalf("tasks still get a PR but no issue updates: relabels=%v comments=%v url=%q", f.relabels, f.comments, res.PRURL)
	}
	if strings.Contains(f.created[0].Body, "Closes #") {
		t.Fatal("a local task closes no issue")
	}
}

func TestPRTitleShouldBeConventional(t *testing.T) {
	cases := []struct{ kind, title, want string }{
		{"bug", "Corrige soma", "fix: corrige soma"},
		{"feature", "Add CSV export", "feat: add CSV export"},
		{"feature", "feat(api): add login", "feat(api): add login"},
		{"bug", "fix: Login breaks", "fix: Login breaks"},
		{"bug", "revert: restore the old parser", "revert: restore the old parser"},
	}
	for _, c := range cases {
		if got := prTitle(c.kind, c.title); got != c.want {
			t.Errorf("prTitle(%q, %q) = %q, want %q", c.kind, c.title, got, c.want)
		}
	}
}
