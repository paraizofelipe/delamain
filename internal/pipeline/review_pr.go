package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/paraizo/delamain/internal/artifacts"
	"github.com/paraizo/delamain/internal/budget"
	"github.com/paraizo/delamain/internal/forge"
	"github.com/paraizo/delamain/internal/gitx"
	"github.com/paraizo/delamain/internal/prompts"
	"github.com/paraizo/delamain/internal/store"
)

// ReviewSummary counts what one PR review posted.
type ReviewSummary struct {
	PRURL     string
	Inline    int // inline comments posted on diff lines
	InSummary int // findings listed in the PR-level summary
	Dropped   int // findings the verifier discarded
}

// prRun is the PR side of a review run.
type prRun struct {
	pr      forge.PRDetail
	ref     string // refs/delamain/pr/N once fetched
	threads []forge.Thread
	review  *artifacts.PRReview
}

// RunReview reviews PR/MR n of repo read-only and posts a comment-only review. Like Run, a
// non-nil error means the review could not even be recorded; guardrail stops are
// OutcomeHalted.
func (r *Runner) RunReview(ctx context.Context, repo string, n int) (Result, error) {
	res := Result{}
	if r.Forge == nil {
		return res, errors.New("review requires a GitHub or GitLab origin")
	}
	repo, err := r.Git.Toplevel(ctx, repo)
	if err != nil {
		return res, fmt.Errorf("repository: %w", err)
	}
	pr, err := r.Forge.PRView(ctx, n)
	if err != nil {
		return res, err
	}
	res.PRURL = pr.URL
	if !pr.Open {
		res.Outcome, res.Reason = OutcomeSkipped, fmt.Sprintf("o PR/MR #%d não está aberto", n)
		return res, nil
	}
	if err := r.ensurePrompts(); err != nil {
		return res, err
	}
	day, err := r.meter().Today(ctx)
	if err != nil {
		return res, err
	}
	if day.Level == budget.Hard {
		res.Outcome, res.Reason = OutcomeHalted, day.Reason+"; nenhuma run iniciada"
		return res, nil
	}

	ref := "pr:" + strconv.Itoa(n)
	rec, err := r.Store.CreateRun(ctx, repo, ref, pr.Title, string(r.Cfg.Agent.Harness), r.Cfg.Pipeline.MaxIterations, r.Cfg.Review.PerReview, store.KindReview)
	if err != nil {
		return res, err
	}
	res.RunID = rec.ID
	ru := &run{
		rec:      rec,
		task:     prompts.Issue{Ref: ref, Title: pr.Title, URL: pr.URL},
		dir:      r.runDir(repo, rec.ID),
		agents:   map[string]Agent{},
		res:      &res,
		maxIter:  r.Cfg.Pipeline.MaxIterations,
		perIssue: r.Cfg.Review.PerReview,
		budget:   day,
	}
	res.RunDir = ru.dir
	r.openLog(ru)
	defer ru.closeLog()
	p := &prRun{pr: pr}
	ru.pr = p
	defer r.cleanup(context.WithoutCancel(ctx), ru) // safety net; finishReview stops agents first
	err = r.reviewPR(ctx, ru, p)

	var h halt
	switch {
	case err == nil:
	case errors.As(err, &h):
		res.Outcome, res.Reason = OutcomeHalted, h.reason
	default:
		res.Outcome, res.Reason = OutcomeHalted, err.Error()
	}
	return res, r.finishReview(ctx, ru, p)
}

func (r *Runner) reviewPR(ctx context.Context, ru *run, p *prRun) error {
	pr, repo := p.pr, ru.rec.Repo
	if err := r.setState(ctx, ru, store.StateSetup); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(ru.dir, "sessions"), 0o700); err != nil {
		return err
	}
	if err := r.Git.FetchRef(ctx, repo, pr.BaseRef); err != nil {
		return err
	}
	fetched, err := r.Git.FetchPRHead(ctx, repo, r.Forge.Repo().Kind, pr.Number)
	p.ref = gitx.PRRef(pr.Number) // removed even when the fetch half-failed
	if err != nil {
		return err
	}
	// Fail closed: a review of another head would comment on code the PR no longer has.
	if fetched != pr.HeadSHA {
		return haltf("o head buscado (%s) difere do head do PR/MR #%d (%s); nada foi postado", short(fetched), pr.Number, short(pr.HeadSHA))
	}
	wt := filepath.Join(r.Cfg.Paths.WorktreesDir, filepath.Base(repo), fmt.Sprintf("review-pr-%d-%d", pr.Number, ru.rec.ID))
	if err := r.Git.AddDetachedWorktree(ctx, repo, wt, p.ref); err != nil {
		return err
	}
	base := "origin/" + pr.BaseRef
	ru.repo = prompts.Repo{Worktree: wt, Base: base}
	ru.rec.Worktree, ru.rec.Base = wt, base
	ru.res.Worktree, ru.res.Base = wt, base
	ru.log.Info("review worktree", "run", ru.rec.ID, "path", wt, "pr", pr.Number, "head", pr.HeadSHA)
	if err := r.Store.UpdateRun(ctx, ru.rec); err != nil {
		return err
	}
	head, status, err := r.worktreeState(ctx, wt)
	if err != nil {
		return err
	}
	if head != pr.HeadSHA {
		return haltf("a worktree está em %s, não no head do PR/MR #%d (%s); nada foi postado", short(head), pr.Number, short(pr.HeadSHA))
	}

	threads, err := r.Forge.Threads(ctx, pr.Number)
	if err != nil {
		return haltf("não foi possível ler as discussões do PR/MR #%d: %v", pr.Number, err)
	}
	p.threads = threads
	linked, err := r.Forge.LinkedIssues(ctx, pr.Number)
	if err != nil {
		ru.log.Warn("could not read linked issues", "pr", pr.Number, "err", err)
	}

	if err := r.setState(ctx, ru, store.StateReview); err != nil {
		return err
	}
	data := r.stageData(ru, 0, filepath.Join(ru.dir, "review.json"), "")
	data.PR = prompts.PR{
		Number: pr.Number, URL: pr.URL, Title: pr.Title, Body: pr.Body, Author: pr.Author,
		BaseRef: pr.BaseRef, HeadRef: pr.HeadRef, BaseSHA: pr.BaseSHA, HeadSHA: pr.HeadSHA,
	}
	for _, is := range linked {
		data.LinkedIssues = append(data.LinkedIssues, prompts.Issue{Ref: "#" + strconv.Itoa(is.Number), Title: is.Title, Body: is.Body, URL: is.URL})
	}
	data.Threads = renderThreads(threads)
	data.MaxComments = r.Cfg.Review.MaxComments
	review, err := stepArtifact[artifacts.PRReview](ctx, r, ru, "pr-reviewer", "review-pr", data)
	if err != nil {
		return err
	}
	// The pr-reviewer is read-only by contract; prove it.
	head2, status2, err := r.worktreeState(ctx, wt)
	if err != nil {
		return err
	}
	if head2 != head || status2 != status {
		return haltf("o pr-reviewer alterou a worktree (HEAD %s→%s); review descartada, nada foi postado", short(head), short(head2))
	}
	if err := review.ValidateLimit(r.Cfg.Review.MaxComments); err != nil {
		return haltf("artefato da review rejeitado: %v; nada foi postado", err)
	}
	p.review = &review

	if err := r.setState(ctx, ru, store.StateDeliver); err != nil {
		return err
	}
	opts, sum := reviewOptions(ru.rec.ID, pr, review)
	if err := r.Forge.PostReview(ctx, pr.Number, opts); err != nil {
		return haltf("falha ao postar a review no PR/MR #%d: %v", pr.Number, err)
	}
	ru.res.Review = &sum
	prev, err := r.Store.Cursor(ctx, repo, pr.Number)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if err := r.Store.SaveCursor(ctx, store.ReviewCursor{Repo: repo, PR: pr.Number, LastSeenAt: time.Now(), Rounds: prev.Rounds + 1, LastHeadSHA: pr.HeadSHA}); err != nil {
		return err
	}
	ru.res.Outcome = OutcomeReviewed
	return nil
}

// worktreeState is the HEAD and `git status --porcelain` of a worktree.
func (r *Runner) worktreeState(ctx context.Context, dir string) (head, status string, err error) {
	if head, err = r.Git.Head(ctx, dir); err != nil {
		return "", "", err
	}
	status, err = r.Git.Status(ctx, dir)
	return head, status, err
}

// finishReview runs for every review that got a record: final budget tally, agents stopped,
// worktree and PR ref removed (also after a halt) and the report.
func (r *Runner) finishReview(ctx context.Context, ru *run, p *prRun) error {
	ctx = context.WithoutCancel(ctx)
	_ = r.checkBudget(ctx, ru)
	r.cleanup(ctx, ru) // agents stop before their worktree goes away
	if ru.repo.Worktree != "" {
		if err := r.Git.RemoveWorktree(ctx, ru.rec.Repo, ru.repo.Worktree); err != nil {
			ru.res.Warnings = append(ru.res.Warnings, fmt.Sprintf("worktree %s não removida: %v", ru.repo.Worktree, err))
		}
	}
	if p.ref != "" {
		if err := r.Git.DeleteRef(ctx, ru.rec.Repo, p.ref); err != nil {
			ru.res.Warnings = append(ru.res.Warnings, fmt.Sprintf("ref %s não removida: %v", p.ref, err))
		}
	}
	return r.record(ctx, ru)
}

// reviewOptions splits the findings: critical/major go inline, everything else to the summary.
func reviewOptions(runID int64, pr forge.PRDetail, rv artifacts.PRReview) (forge.ReviewOptions, ReviewSummary) {
	o := forge.ReviewOptions{HeadSHA: pr.HeadSHA, BaseSHA: pr.BaseSHA, StartSHA: pr.StartSHA}
	sum := ReviewSummary{PRURL: pr.URL, Dropped: len(rv.Dropped)}
	var others []string
	for _, a := range rv.Axes {
		for _, f := range a.Findings {
			if f.Blocking() && f.File != "" {
				m := forge.Marker{RunID: runID, Kind: "review", Key: forge.Key(f.File, strconv.Itoa(f.Line), f.Message)}
				o.Comments = append(o.Comments, forge.InlineComment{Path: f.File, Line: f.Line, Body: inlineBody(a.Axis, f) + "\n\n" + m.String()})
				continue
			}
			others = append(others, findingLine(a.Axis, f))
		}
	}
	sum.Inline, sum.InSummary = len(o.Comments), len(others)
	m := forge.Marker{RunID: runID, Kind: "summary", Key: forge.Key("summary", pr.HeadSHA)}
	o.Summary = summaryBody(rv, others) + "\n\n" + m.String()
	return o, sum
}

func inlineBody(axis string, f artifacts.Finding) string {
	return fmt.Sprintf("**%s** · %s · %s\n\n%s", f.Severity, axis, f.Category, f.Message)
}

func findingLine(axis string, f artifacts.Finding) string {
	where := "PR"
	if f.File != "" {
		where = fmt.Sprintf("`%s:%d`", f.File, f.Line)
	}
	return fmt.Sprintf("- **%s** · %s · %s — %s", f.Severity, axis, where, flat(f.Message))
}

func summaryBody(rv artifacts.PRReview, others []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "🤖 **delamain review** · aderência: `%s`\n\n%s\n", rv.Adherence, rv.Summary)
	if len(others) > 0 {
		b.WriteString("\n### Outros achados\n\n" + strings.Join(others, "\n") + "\n")
	}
	if len(rv.Dropped) > 0 {
		b.WriteString("\n### Descartados pela verificação\n\n")
		for _, d := range rv.Dropped {
			fmt.Fprintf(&b, "- `%s:%d` — %s (motivo: %s)\n", d.File, d.Line, flat(d.Message), flat(d.Reason))
		}
	}
	b.WriteString("\n---\nReview automática, só comentários: o delamain nunca aprova nem faz merge.")
	return b.String()
}

func flat(s string) string { return strings.Join(strings.Fields(s), " ") }

// renderThreads is the compact view of a PR's discussions the pr-reviewer reads.
func renderThreads(ts []forge.Thread) string {
	if len(ts) == 0 {
		return "Nenhuma discussão."
	}
	var b strings.Builder
	for _, t := range ts {
		where := "PR"
		if t.Path != "" {
			where = fmt.Sprintf("%s:%d", t.Path, t.Line)
		}
		state := "aberta"
		if t.Resolved {
			state = "resolvida"
		}
		fmt.Fprintf(&b, "- %s · %s · %s\n", t.ID, where, state)
		for _, c := range t.Comments {
			fmt.Fprintf(&b, "  - %s: %s\n", c.Author, truncate(flat(c.Body), 500))
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
