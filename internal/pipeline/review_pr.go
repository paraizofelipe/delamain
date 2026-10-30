package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/paraizo/delamain/internal/artifacts"
	"github.com/paraizo/delamain/internal/budget"
	"github.com/paraizo/delamain/internal/forge"
	"github.com/paraizo/delamain/internal/gates"
	"github.com/paraizo/delamain/internal/gitx"
	"github.com/paraizo/delamain/internal/memory"
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
	cmds    artifacts.Commands // from the base's .delamain.yaml, for the report
}

// memoryPromptBytes caps the repository memory sent to the pr-reviewer.
const memoryPromptBytes = 8192

// RunReview reviews PR/MR n of repo read-only and posts a comment-only review. Like Run, a
// non-nil error means the review could not even be recorded; guardrail stops are
// OutcomeHalted.
func (r *Runner) RunReview(ctx context.Context, repo string, n int) (Result, error) {
	res := Result{}
	if r.Forge == nil {
		return res, errors.New("review requires a GitHub or GitLab origin")
	}
	repo, err := r.Git.MainWorktree(ctx, repo)
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
	// A head in another repository is third-party code: its checkout brings the files that
	// steer the agent, and the review would speak as the operator about code nobody here
	// vouched for. delamain does not review it, on any harness. A PR whose branch lives in
	// this repository is what delamain implemented, or what someone with write access pushed:
	// the same code the issue flow already works on.
	if pr.Fork {
		return res, fmt.Errorf("o delamain não revisa PR/MR vindo de fork: o head do #%d está em outro repositório, fora do alcance de quem responde por este. Revise à mão, ou traga a branch para este repositório e revise o PR/MR dela", n)
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
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	defer r.watchStop(ctx, rec.ID, cancel)()
	ru := &run{
		rec:      rec,
		task:     prompts.Issue{Ref: ref, Title: pr.Title, URL: pr.URL},
		dir:      r.runDir(repo, rec.ID),
		agents:   map[string]Agent{},
		sessions: map[string]session{},
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
	case errors.Is(context.Cause(ctx), ErrStopped):
		ru.log.Info("run stopped by request", "run", rec.ID)
		res.Outcome, res.Reason = OutcomeHalted, ErrStopped.Error()
	case err == nil:
	case errors.As(err, &h):
		res.Outcome, res.Reason = OutcomeHalted, h.reason
	default:
		res.Outcome, res.Reason = OutcomeHalted, err.Error()
	}
	r.noteReviewHalt(ctx, ru, p)
	return res, r.finishReview(ctx, ru, p)
}

// noteReviewHalt counts a review that halted against the head it was reviewing. A halt is
// usually deterministic — a head that moved, a checkout the reviewer touched, a rejected
// artifact — so without this count the daemon's producer would start the same review on every
// poll, spending the daily budget the issues share. A new push changes the head and the count
// starts over; a review that posts clears it.
func (r *Runner) noteReviewHalt(ctx context.Context, ru *run, p *prRun) {
	if ru.res.Outcome != OutcomeHalted || p.pr.HeadSHA == "" {
		return
	}
	if ctx.Err() != nil {
		// The operator stopped the daemon: that says nothing about this head, and three
		// restarts during its review would otherwise retire it until the next push.
		ru.log.Info("review interrupted, not counted against the PR/MR head", "run", ru.rec.ID, "pr", p.pr.Number)
		return
	}
	// A halt the review itself decided is still recorded if the operator interrupts right now.
	ctx = context.WithoutCancel(ctx)
	c, err := r.Store.Cursor(ctx, ru.rec.Repo, p.pr.Number)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		ru.log.Warn("could not read the review cursor to count the halt", "pr", p.pr.Number, "err", err)
		return
	}
	c.Repo, c.PR, c.LastSeenAt = ru.rec.Repo, p.pr.Number, time.Now()
	if c.HaltedHeadSHA != p.pr.HeadSHA {
		c.HaltedHeadSHA, c.Halts = p.pr.HeadSHA, 0
	}
	c.Halts++
	if err := r.Store.SaveCursor(ctx, c); err != nil {
		ru.log.Warn("could not count the halted review", "pr", p.pr.Number, "err", err)
		return
	}
	ru.log.Info("halted review counted against the PR/MR head", "run", ru.rec.ID, "pr", p.pr.Number, "halts", c.Halts)
}

func (r *Runner) reviewPR(ctx context.Context, ru *run, p *prRun) error {
	pr, repo := p.pr, ru.rec.Repo
	if err := r.setState(ctx, ru, store.StateSetup); err != nil {
		return err
	}
	prev, err := r.Store.Cursor(ctx, repo, pr.Number)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if prev.LastHeadSHA == pr.HeadSHA {
		ru.res.Outcome, ru.res.Reason = OutcomeSkipped, fmt.Sprintf("o head %s do PR/MR #%d já foi revisado", pr.HeadSHA, pr.Number)
		ru.res.Skip = SkipHeadReviewed
		return nil
	}
	// The agent never starts inside the PR checkout: its harness would load the settings, hooks
	// and instruction files (.claude/, .omp/, CLAUDE.md, AGENTS.md) the PR's author controls.
	ru.agentDir = filepath.Join(ru.dir, "agent")
	for _, d := range []string{filepath.Join(ru.dir, "sessions"), ru.agentDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	if err := r.Git.FetchRef(ctx, repo, pr.BaseRef); err != nil {
		return err
	}
	base := "origin/" + pr.BaseRef
	// The config comes from the base: the PR's author controls everything in its head.
	raw, err := r.Git.ShowFile(ctx, repo, base, gates.RepoConfigFile)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return haltf("não foi possível ler o %s da base %s: %v", gates.RepoConfigFile, base, err)
	}
	rc, err := gates.ParseRepoConfig(raw)
	if err != nil {
		return haltf("%v (lido de %s)", err, base)
	}
	ru.repoCfg, p.cmds = rc, rc.Commands
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
	if err := r.Git.AddUntrustedWorktree(ctx, repo, wt, p.ref); err != nil {
		return err
	}
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
	if r.ForgeUser == "" {
		ru.log.Warn("forge user unknown: no comment counts as already posted", "pr", pr.Number)
	}
	linked, err := r.Forge.LinkedIssues(ctx, pr.Number)
	if err != nil {
		ru.log.Warn("could not read linked issues", "pr", pr.Number, "err", err)
	}

	if err := r.setState(ctx, ru, store.StateReview); err != nil {
		return err
	}
	// The reviewer is not confined: RunReview refused the fork, so this head is a branch of
	// this repository. Answering threads is the confined one (see RunAddress), because the
	// comments it reads come from anyone who can comment on the PR.
	artifact := filepath.Join(ru.dir, "review.json")
	data := r.stageData(ru, 0, artifact, "")
	data.PR = prompts.PR{
		Number: pr.Number, URL: pr.URL, Title: pr.Title, Body: pr.Body, Author: pr.Author,
		BaseRef: pr.BaseRef, HeadRef: pr.HeadRef, BaseSHA: pr.BaseSHA, HeadSHA: pr.HeadSHA,
	}
	for _, is := range linked {
		// The review is posted where every reader of the PR sees it; a confidential issue the
		// PR's author linked could be quoted into it.
		if is.Confidential {
			ru.log.Info("confidential linked issue left out of the prompt", "pr", pr.Number, "issue", is.Number)
			continue
		}
		data.LinkedIssues = append(data.LinkedIssues, prompts.Issue{Ref: "#" + strconv.Itoa(is.Number), Title: is.Title, Body: is.Body, URL: is.URL})
	}
	data.Threads = renderThreads(threads)
	data.MaxComments = r.Cfg.Review.MaxComments
	mem, fr := memory.Store{Dir: r.Cfg.Paths.MemoryDir()}, r.Forge.Repo()
	if data.Memory, err = mem.Read(fr.Host, fr.Path, memoryPromptBytes); err != nil {
		ru.log.Warn("could not read the repository memory", "err", err)
	}
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
	if n := dropRepeated(&review, threads, r.ForgeUser); n > 0 {
		ru.log.Info("findings someone already discusses dropped", "run", ru.rec.ID, "pr", pr.Number, "repeated", n)
	}
	if err := review.ValidateLimit(r.Cfg.Review.MaxComments); err != nil {
		return haltf("artefato da review rejeitado: %v; nada foi postado", err)
	}
	p.review = &review

	lines, err := r.Git.ChangedLines(ctx, wt, pr.BaseSHA)
	if err != nil {
		return haltf("não foi possível ler as linhas do diff: %v; nada foi postado", err)
	}

	if err := r.setState(ctx, ru, store.StateDeliver); err != nil {
		return err
	}
	opts, sum := reviewOptions(ru.rec.ID, pr, review, lines, postedKeys(threads, r.ForgeUser),
		func(file string, line int) string { return r.excerpt(ctx, repo, p.ref, file, line) })
	if opts.Summary != "" || len(opts.Comments) > 0 {
		// The bodies are the agent's words, posted publicly as the operator: a prompt injection
		// could have it quote a local credential.
		if kind := leakedSecret(reviewBodies(opts), os.Environ()); kind != "" {
			return haltf("a review contém o que parece um segredo (%s); nada foi postado", kind)
		}
		if err := r.Forge.PostReview(ctx, pr.Number, opts); err != nil {
			return haltf("falha ao postar a review no PR/MR #%d: %v", pr.Number, err)
		}
	} else {
		ru.log.Info("review already posted", "run", ru.rec.ID, "pr", pr.Number)
	}
	ru.res.Review = &sum
	// The review is posted: this head is done, and the halt count of earlier attempts on it
	// goes away with the cursor it is written over (see noteReviewHalt).
	if err := r.Store.SaveCursor(ctx, store.ReviewCursor{Repo: repo, PR: pr.Number, LastSeenAt: time.Now(), Rounds: prev.Rounds + 1, LastHeadSHA: pr.HeadSHA}); err != nil {
		return err
	}
	ru.res.Outcome = OutcomeReviewed
	// The memory is read into every later review of this repository, so what goes in has to
	// come from a trusted head. This one is: RunReview refuses a fork, whose author would steer
	// the code the reviewer read and could plant a durable "fact" that silences findings in
	// other people's PRs.
	if n, err := mem.Append(fr.Host, fr.Path, ru.rec.ID, pr.Number, review.Learnings, time.Now()); err != nil {
		ru.log.Warn("could not update the repository memory", "err", err)
	} else {
		ru.log.Info("repository memory", "run", ru.rec.ID, "learnings", n)
	}
	return nil
}

// worktreeState is the HEAD and `git status --porcelain` of a PR checkout.
func (r *Runner) worktreeState(ctx context.Context, dir string) (head, status string, err error) {
	if head, err = r.Git.Head(ctx, dir); err != nil {
		return "", "", err
	}
	status, err = r.Git.UntrustedStatus(ctx, dir)
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

// reviewOptions routes the findings: every one on a line the head added or changed goes inline,
// whatever its severity, so the discussion sits on the code it is about. One the forge cannot
// anchor — no file of its own, or a line outside the diff — goes to the summary carrying the
// excerpt of the code it points at, so no comment is posted without the snippet it discusses.
// excerpt reads the head and may return "". Comments whose marker key is already on the PR are
// left out; with nothing new at all, both Summary and Comments are empty.
func reviewOptions(runID int64, pr forge.PRDetail, rv artifacts.PRReview, lines map[string][]gitx.LineRange, posted map[string]bool, excerpt func(file string, line int) string) (forge.ReviewOptions, ReviewSummary) {
	o := forge.ReviewOptions{HeadSHA: pr.HeadSHA, BaseSHA: pr.BaseSHA, StartSHA: pr.StartSHA}
	sum := ReviewSummary{PRURL: pr.URL, Dropped: len(rv.Dropped)}
	var others, outside []string
	for _, a := range rv.Axes {
		for _, f := range a.Findings {
			switch {
			case f.File == "":
				others = append(others, findingLine(a.Axis, f, ""))
			case !inDiff(lines[f.File], f.Line):
				outside = append(outside, findingLine(a.Axis, f, excerpt(f.File, f.Line)))
			default:
				m := forge.Marker{RunID: runID, Kind: "review", Key: forge.Key(f.File, strconv.Itoa(f.Line), f.Message)}
				if posted[m.Key] {
					continue
				}
				o.Comments = append(o.Comments, forge.InlineComment{Path: f.File, Line: f.Line, Body: inlineBody(a.Axis, f) + "\n\n" + m.String()})
			}
		}
	}
	sum.Inline, sum.InSummary = len(o.Comments), len(others)+len(outside)
	m := forge.Marker{RunID: runID, Kind: "summary", Key: forge.Key("summary", pr.HeadSHA)}
	switch {
	case !posted[m.Key]:
		o.Summary = summaryBody(rv, others, outside) + "\n\n" + m.String()
	case len(o.Comments) > 0:
		// The summary is already on the PR; the forge still wants a review body.
		o.Summary = "🤖 **Review automática**: comentários que faltavam desta review.\n\n" + m.String()
	}
	return o, sum
}

// inDiff reports whether line is one the head added or changed, so the forge can anchor it.
func inDiff(ranges []gitx.LineRange, line int) bool {
	for _, lr := range ranges {
		if line >= lr.Start && line <= lr.End {
			return true
		}
	}
	return false
}

// secretPatterns are credential shapes that must never reach a public comment.
var secretPatterns = []struct {
	name string
	re   *regexp.Regexp
}{
	{"chave privada", regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`)},
	{"token do GitHub", regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{36,}|github_pat_[A-Za-z0-9_]{22,})`)},
	{"token do GitLab", regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{20,}`)},
	{"chave da AWS", regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{"token do Slack", regexp.MustCompile(`\bxox[abposr]-[A-Za-z0-9-]{10,}`)},
	{"chave de API de LLM", regexp.MustCompile(`\bsk-(?:ant|proj)-[A-Za-z0-9_-]{20,}`)},
	{"chave do Google", regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}`)},
}

// secretEnvSuffixes mark the variables whose values are credentials of this machine.
var secretEnvSuffixes = []string{"_TOKEN", "_KEY", "_SECRET", "_PAT", "_PASSWORD"}

// minSecretValue skips short values (flags, "true") that would match ordinary text.
const minSecretValue = 12

// leakedSecret names the first secret in bodies, a known credential shape or the value of a
// secret-looking variable of environ, without repeating it; "" when there is none.
func leakedSecret(bodies, environ []string) string {
	for _, b := range bodies {
		for _, p := range secretPatterns {
			if p.re.MatchString(b) {
				return p.name
			}
		}
	}
	for _, kv := range environ {
		key, val, _ := strings.Cut(kv, "=")
		if len(val) < minSecretValue || !slices.ContainsFunc(secretEnvSuffixes, func(s string) bool { return strings.HasSuffix(strings.ToUpper(key), s) }) {
			continue
		}
		for _, b := range bodies {
			if strings.Contains(b, val) {
				return "valor de " + key
			}
		}
	}
	return ""
}

func reviewBodies(o forge.ReviewOptions) []string {
	bodies := []string{o.Summary}
	for _, c := range o.Comments {
		bodies = append(bodies, c.Body)
	}
	return bodies
}

// repeatWindow is how many lines apart a finding and an existing thread still discuss the same
// spot.
const repeatWindow = 2

// dropRepeated moves every finding that sits where an unresolved thread someone else opened
// already discusses — same file, at most repeatWindow lines apart — into rv.Dropped, so the
// review never repeats a critique a human already made. The reason names only the thread's id
// and position, never its body: that text is third-party input and the summary is public. A
// thread delamain itself opened stays out of the filter — its findings are deduplicated by
// marker key, and dropping on position would silence new findings on busy lines.
func dropRepeated(rv *artifacts.PRReview, threads []forge.Thread, user string) int {
	var dropped int
	for ai := range rv.Axes {
		kept := rv.Axes[ai].Findings[:0]
		for _, f := range rv.Axes[ai].Findings {
			t, ok := discussedBy(f, threads, user)
			if !ok {
				kept = append(kept, f)
				continue
			}
			rv.Dropped = append(rv.Dropped, artifacts.Dropped{File: f.File, Line: f.Line, Message: f.Message,
				Reason: fmt.Sprintf("já discutido na thread %s (%s:%d)", t.ID, t.Path, t.Line)})
			dropped++
		}
		rv.Axes[ai].Findings = kept
	}
	return dropped
}

// discussedBy finds the unresolved inline thread someone other than delamain (user) opened that
// already discusses finding f.
func discussedBy(f artifacts.Finding, threads []forge.Thread, user string) (forge.Thread, bool) {
	if f.File == "" {
		return forge.Thread{}, false
	}
	for _, t := range threads {
		if t.Path != f.File || t.Resolved {
			continue
		}
		if root, ok := firstPublic(t); !ok || ownComment(root, user) {
			continue
		}
		if d := f.Line - t.Line; -repeatWindow <= d && d <= repeatWindow {
			return t, true
		}
	}
	return forge.Thread{}, false
}

// postedKeys are the marker keys of the comments delamain (user) already left on the PR. Markers
// are public and keys predictable, so a marker in anyone else's comment is ignored: it could
// otherwise suppress the review. Without a known user nothing counts as posted.
func postedKeys(ts []forge.Thread, user string) map[string]bool {
	keys := map[string]bool{}
	if user == "" {
		return keys
	}
	for _, t := range ts {
		for _, c := range t.Comments {
			if !strings.EqualFold(c.Author, user) {
				continue
			}
			if m, ok := forge.ParseMarker(c.Body); ok {
				keys[m.Key] = true
			}
		}
	}
	return keys
}

// inlineBody is the comment posted on the line a finding is about. The forge shows the line
// itself, and the evidence the reviewer read goes with it, so the comment says which part of
// the change it is about.
func inlineBody(axis string, f artifacts.Finding) string {
	body := fmt.Sprintf("🤖 **%s** · %s · %s\n\n%s", f.Severity, axis, f.Category, f.Message)
	if f.Evidence != "" {
		body += "\n\n" + codeBlock(f.Evidence)
	}
	return body
}

// findingLine is one finding in the summary, with the code it is about underneath: the head's
// excerpt around its line when there is one, the evidence the reviewer read otherwise. Both can
// be empty, and then the line stands alone.
func findingLine(axis string, f artifacts.Finding, excerpt string) string {
	where := "PR"
	if f.File != "" {
		where = fmt.Sprintf("`%s:%d`", f.File, f.Line)
	}
	line := fmt.Sprintf("- **%s** · %s · %s — %s", f.Severity, axis, where, flat(f.Message))
	snippet := excerpt
	if snippet == "" {
		snippet = f.Evidence
	}
	if snippet == "" {
		return line
	}
	// Indented, or the fence would end the list item instead of belonging to it.
	return line + "\n\n" + indent(codeBlock(snippet), "  ")
}

// codeBlock fences s, with a fence long enough that a fence inside s cannot close it early —
// the excerpt of a Markdown file carries its own.
func codeBlock(s string) string {
	longest, run := 0, 0
	for _, c := range s {
		if c != '`' {
			run = 0
			continue
		}
		run++
		longest = max(longest, run)
	}
	fence := strings.Repeat("`", max(3, longest+1))
	return fence + "\n" + strings.Trim(s, "\n") + "\n" + fence
}

// indent puts with before every non-empty line of s.
func indent(s, with string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = with + l
		}
	}
	return strings.Join(lines, "\n")
}

func summaryBody(rv artifacts.PRReview, others, outside []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "🤖 **Review automática** · aderência: `%s`\n\n%s\n", rv.Adherence, rv.Summary)
	if len(outside) > 0 {
		b.WriteString("\n### Fora das linhas alteradas\n\n" + strings.Join(outside, "\n") + "\n")
	}
	if len(others) > 0 {
		b.WriteString("\n### Outros achados\n\n" + strings.Join(others, "\n") + "\n")
	}
	if len(rv.Dropped) > 0 {
		b.WriteString("\n### Descartados pela verificação\n\n")
		for _, d := range rv.Dropped {
			fmt.Fprintf(&b, "- `%s:%d` — %s (motivo: %s)\n", d.File, d.Line, flat(d.Message), flat(d.Reason))
		}
	}
	b.WriteString("\n---\nFeita por agentes de IA, só comentários.")
	return b.String()
}

func flat(s string) string { return strings.Join(strings.Fields(s), " ") }

// renderThreads is the compact view of a PR's discussions the pr-reviewer reads, internal notes
// left out.
func renderThreads(ts []forge.Thread) string {
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
		var lines []string
		for _, c := range t.Comments {
			if c.Internal { // not visible to who reads the review
				continue
			}
			lines = append(lines, fmt.Sprintf("  - %s: %s\n", c.Author, truncate(flat(c.Body), 500)))
		}
		if len(lines) == 0 {
			continue
		}
		fmt.Fprintf(&b, "- %s · %s · %s\n%s", t.ID, where, state, strings.Join(lines, ""))
	}
	if b.Len() == 0 {
		return "Nenhuma discussão."
	}
	return strings.TrimRight(b.String(), "\n")
}
