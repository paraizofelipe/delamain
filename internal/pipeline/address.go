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
	"github.com/paraizo/delamain/internal/memory"
	"github.com/paraizo/delamain/internal/prompts"
	"github.com/paraizo/delamain/internal/store"
)

const (
	// excerptContext is how many lines around a thread's line its excerpt shows.
	excerptContext = 5
	// pendingCommentRunes caps each comment of a pending thread in the responder's prompt.
	pendingCommentRunes = 2000
)

// RunAddress answers the pending review threads of delamain's PR/MR n of repo: one reply per
// thread, never a commit, a push or a resolved thread. Like RunReview, a non-nil error means
// the run could not even be recorded; guardrail stops are OutcomeHalted.
func (r *Runner) RunAddress(ctx context.Context, repo string, n int) (Result, error) {
	res := Result{}
	if r.Forge == nil {
		return res, errors.New("address requires a GitHub or GitLab origin")
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
	threads, err := r.Forge.Threads(ctx, n)
	if err != nil {
		return res, fmt.Errorf("discussões do PR/MR #%d: %w", n, err)
	}
	cur, err := r.Store.Cursor(ctx, repo, n)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return res, err
	}
	pending := PendingThreads(threads, r.ForgeUser, cur.LastSeenAt)
	if len(pending) == 0 {
		res.Outcome, res.Reason = OutcomeSkipped, fmt.Sprintf("nenhuma discussão pendente no PR/MR #%d", n)
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

	ref := "addr:" + strconv.Itoa(n)
	rec, err := r.Store.CreateRun(ctx, repo, ref, pr.Title, string(r.Cfg.Agent.Harness), r.Cfg.Pipeline.MaxIterations, r.Cfg.Review.PerReview, store.KindAddress)
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
	p := &prRun{pr: pr, threads: threads}
	ru.pr = p
	defer r.cleanup(context.WithoutCancel(ctx), ru) // safety net; finishReview stops agents first
	err = r.address(ctx, ru, p, pending)

	var h halt
	switch {
	case err == nil:
	case errors.As(err, &h):
		res.Outcome, res.Reason = OutcomeHalted, h.reason
	default:
		res.Outcome, res.Reason = OutcomeHalted, err.Error()
	}
	if err != nil {
		r.noteAddressHalt(ctx, ru, p)
	}
	return res, r.finishReview(ctx, ru, p)
}

// noteAddressHalt spends a round on a run that halted before answering. A halt is usually
// deterministic — a rejected artifact, a checkout the responder touched — and the comments stay
// pending, so without this the daemon would start the same run on every poll until
// review.max_rounds hands the PR to a human.
func (r *Runner) noteAddressHalt(ctx context.Context, ru *run, p *prRun) {
	if ctx.Err() != nil {
		// The operator stopped the daemon: that says nothing about this PR.
		ru.log.Info("address interrupted, no round spent", "run", ru.rec.ID, "pr", p.pr.Number)
		return
	}
	ctx = context.WithoutCancel(ctx)
	c, err := r.Store.Cursor(ctx, ru.rec.Repo, p.pr.Number)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		ru.log.Warn("could not read the review cursor to count the halt", "pr", p.pr.Number, "err", err)
		return
	}
	c.Repo, c.PR = ru.rec.Repo, p.pr.Number
	c.Rounds++
	if err := r.Store.SaveCursor(ctx, c); err != nil {
		ru.log.Warn("could not count the halted round", "pr", p.pr.Number, "err", err)
		return
	}
	ru.log.Info("halted address counted as a round", "run", ru.rec.ID, "pr", p.pr.Number, "rounds", c.Rounds)
}

// escalate hands a PR whose rounds are spent to a human: the needs-human label, a notice posted
// once per PR and the cursor past the comments seen, so the producer does not start the same
// run on every poll. No agent runs and no round is spent.
func (r *Runner) escalate(ctx context.Context, ru *run, p *prRun, prev store.ReviewCursor, pending []forge.Thread) error {
	pr := p.pr
	label := r.Cfg.Forge.Labels.NeedsHuman
	if err := r.Forge.RelabelPR(ctx, pr.Number, []string{label}, nil); err != nil {
		return haltf("falha ao marcar o PR/MR #%d com %s: %v", pr.Number, label, err)
	}
	// The marker's kind is a single word (see forge.ParseMarker), hence the underscore.
	m := forge.Marker{RunID: ru.rec.ID, Kind: "needs_human", Key: forge.Key("needs-human", strconv.Itoa(pr.Number))}
	if !postedKeys(p.threads, r.ForgeUser)[m.Key] {
		body := fmt.Sprintf("🤖 **delamain**: este PR/MR chegou ao limite de %d rodadas automáticas (`review.max_rounds`). As próximas discussões ficam com um humano.\n\n%s", r.Cfg.Review.MaxRounds, m.String())
		if err := r.Forge.PRComment(ctx, pr.Number, body); err != nil {
			return haltf("falha ao avisar no PR/MR #%d: %v", pr.Number, err)
		}
	}
	c := prev
	c.Repo, c.PR, c.LastSeenAt = ru.rec.Repo, pr.Number, newestComment(pending)
	if err := r.Store.SaveCursor(ctx, c); err != nil {
		return err
	}
	ru.res.Outcome = OutcomeHalted
	ru.res.Reason = fmt.Sprintf("review.max_rounds (%d) atingido: PR/MR #%d marcado %s", r.Cfg.Review.MaxRounds, pr.Number, label)
	return nil
}

func (r *Runner) address(ctx context.Context, ru *run, p *prRun, pending []forge.Thread) error {
	pr, repo := p.pr, ru.rec.Repo
	if err := r.setState(ctx, ru, store.StateSetup); err != nil {
		return err
	}
	prev, err := r.Store.Cursor(ctx, repo, pr.Number)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if prev.Rounds >= r.Cfg.Review.MaxRounds {
		return r.escalate(ctx, ru, p, prev, pending)
	}
	for _, d := range []string{filepath.Join(ru.dir, "sessions")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	_, err = r.Git.FetchPRHead(ctx, repo, r.Forge.Repo().Kind, pr.Number)
	p.ref = gitx.PRRef(pr.Number) // removed even when the fetch half-failed
	if err != nil {
		return err
	}
	wt := filepath.Join(r.Cfg.Paths.WorktreesDir, filepath.Base(repo), fmt.Sprintf("address-pr-%d-%d", pr.Number, ru.rec.ID))
	if err := r.Git.AddUntrustedWorktree(ctx, repo, wt, p.ref); err != nil {
		return err
	}
	base := "origin/" + pr.BaseRef
	ru.repo = prompts.Repo{Worktree: wt, Base: base}
	ru.rec.Worktree, ru.rec.Base = wt, base
	ru.res.Worktree, ru.res.Base = wt, base
	ru.log.Info("address worktree", "run", ru.rec.ID, "path", wt, "pr", pr.Number, "head", pr.HeadSHA)
	if err := r.Store.UpdateRun(ctx, ru.rec); err != nil {
		return err
	}
	linked, err := r.Forge.LinkedIssues(ctx, pr.Number)
	if err != nil {
		ru.log.Warn("could not read linked issues", "pr", pr.Number, "err", err)
	}

	if err := r.setState(ctx, ru, store.StateReview); err != nil {
		return err
	}
	artifact := filepath.Join(ru.dir, "responses.json")
	data := r.stageData(ru, 0, artifact, "")
	data.PR = prompts.PR{
		Number: pr.Number, URL: pr.URL, Title: pr.Title, Body: pr.Body, Author: pr.Author,
		BaseRef: pr.BaseRef, HeadRef: pr.HeadRef, BaseSHA: pr.BaseSHA, HeadSHA: pr.HeadSHA,
	}
	for _, is := range linked {
		data.LinkedIssues = append(data.LinkedIssues, prompts.Issue{Ref: "#" + strconv.Itoa(is.Number), Title: is.Title, Body: is.Body, URL: is.URL})
	}
	data.Threads = renderAddressThreads(p.threads, pending, func(t forge.Thread) string { return r.excerpt(ctx, repo, p.ref, t) })
	mem, fr := memory.Store{Dir: r.Cfg.Paths.MemoryDir()}, r.Forge.Repo()
	if data.Memory, err = mem.Read(fr.Host, fr.Path, memoryPromptBytes); err != nil {
		ru.log.Warn("could not read the repository memory", "err", err)
	}
	resp, err := stepArtifact[artifacts.Responses](ctx, r, ru, "responder", "address", data)
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(pending))
	for _, t := range pending {
		ids = append(ids, t.ID)
	}
	if err := resp.ValidatePending(ids); err != nil {
		return haltf("artefato do responder rejeitado: %v; nada foi postado", err)
	}

	if err := r.setState(ctx, ru, store.StateDeliver); err != nil {
		return err
	}
	last := map[string]string{}
	for _, t := range pending {
		c, _ := lastPublic(t)
		last[t.ID] = c.ID
	}
	// On GitHub the answer to a PR-level comment is a comment of its own, so the keys are looked
	// up across every thread of the PR.
	posted := postedKeys(p.threads, r.ForgeUser)
	count := map[string]int{}
	for _, a := range resp.Responses {
		m := forge.Marker{RunID: ru.rec.ID, Kind: "reply", Key: forge.Key(a.ThreadID, last[a.ThreadID])}
		if posted[m.Key] {
			ru.log.Info("thread already answered", "run", ru.rec.ID, "pr", pr.Number, "thread", a.ThreadID)
			continue
		}
		if err := r.Forge.Reply(ctx, pr.Number, a.ThreadID, replyBody(a)+"\n\n"+m.String()); err != nil {
			return haltf("falha ao responder a discussão %s do PR/MR #%d: %v", a.ThreadID, pr.Number, err)
		}
		count[a.Verdict]++
	}
	c := prev
	c.Repo, c.PR, c.LastSeenAt, c.Rounds = repo, pr.Number, newestComment(pending), prev.Rounds+1
	if err := r.Store.SaveCursor(ctx, c); err != nil {
		return err
	}
	ru.res.Outcome = OutcomeAnswered
	ru.res.Reason = fmt.Sprintf("%d respostas postadas (valid %d · invalid %d · unclear %d)", count["valid"]+count["invalid"]+count["unclear"], count["valid"], count["invalid"], count["unclear"])
	if n, err := mem.Append(fr.Host, fr.Path, ru.rec.ID, pr.Number, resp.Learnings, time.Now()); err != nil {
		ru.log.Warn("could not update the repository memory", "err", err)
	} else {
		ru.log.Info("repository memory", "run", ru.rec.ID, "learnings", n)
	}
	return nil
}

// PendingThreads are the open threads of a PR waiting for delamain (user): their last public
// comment is newer than since and is not delamain's own reply. Markers are public, so only a
// comment by user that carries one counts as delamain's; without a known user nothing is
// pending, or delamain could end up answering itself. Internal notes are never read: the
// answer would be public.
func PendingThreads(ts []forge.Thread, user string, since time.Time) []forge.Thread {
	if user == "" {
		return nil
	}
	var out []forge.Thread
	for _, t := range ts {
		last, ok := lastPublic(t)
		if t.Resolved || !ok || ownComment(last, user) || !last.CreatedAt.After(since) {
			continue
		}
		out = append(out, t)
	}
	return out
}

// lastPublic is the newest comment of t that is not an internal note.
func lastPublic(t forge.Thread) (forge.Comment, bool) {
	for i := len(t.Comments) - 1; i >= 0; i-- {
		if !t.Comments[i].Internal {
			return t.Comments[i], true
		}
	}
	return forge.Comment{}, false
}

// ownComment reports whether c is one delamain (user) posted: its login and a valid marker.
func ownComment(c forge.Comment, user string) bool {
	if user == "" || !strings.EqualFold(c.Author, user) {
		return false
	}
	_, ok := forge.ParseMarker(c.Body)
	return ok
}

// newestComment is when the newest last comment of ts was made.
func newestComment(ts []forge.Thread) time.Time {
	var newest time.Time
	for _, t := range ts {
		if c, _ := lastPublic(t); c.CreatedAt.After(newest) {
			newest = c.CreatedAt
		}
	}
	return newest
}

func replyBody(a artifacts.Response) string {
	return fmt.Sprintf("🤖 **delamain** · parecer: `%s`\n\n%s", a.Verdict, a.Reply)
}

// excerpt is the head's code around the line a thread comments on, read from the PR ref in the
// object store: "" for PR-level threads or when the file cannot be read.
func (r *Runner) excerpt(ctx context.Context, repo, ref string, t forge.Thread) string {
	if t.Path == "" || t.Line <= 0 {
		return ""
	}
	raw, err := r.Git.ShowFile(ctx, repo, ref, t.Path)
	if err != nil {
		return ""
	}
	lines := strings.Split(string(raw), "\n")
	from, to := max(t.Line-excerptContext, 1), min(t.Line+excerptContext, len(lines))
	var b strings.Builder
	for i := from; i <= to; i++ {
		fmt.Fprintf(&b, "%d: %s\n", i, lines[i-1])
	}
	return b.String()
}

// renderAddressThreads is what the responder reads: the pending threads whole, with their
// excerpt, and the other discussions in short, internal notes left out.
func renderAddressThreads(ts, pending []forge.Thread, excerpt func(forge.Thread) string) string {
	isPending := map[string]bool{}
	var b strings.Builder
	b.WriteString("Discussões pendentes (responda cada uma):\n")
	for _, t := range pending {
		isPending[t.ID] = true
		where := "PR"
		if t.Path != "" {
			where = fmt.Sprintf("%s:%d", t.Path, t.Line)
		}
		fmt.Fprintf(&b, "\n### thread_id: %s · %s\n", t.ID, where)
		if ex := excerpt(t); ex != "" {
			b.WriteString("Trecho do head:\n```\n" + ex + "```\n")
		}
		for _, c := range t.Comments {
			if c.Internal {
				continue
			}
			fmt.Fprintf(&b, "- %s (%s): %s\n", c.Author, c.CreatedAt.UTC().Format(time.RFC3339), truncateRunes(c.Body, pendingCommentRunes))
		}
	}
	var others []forge.Thread
	for _, t := range ts {
		if !isPending[t.ID] {
			others = append(others, t)
		}
	}
	b.WriteString("\nOutras discussões (contexto, não responda):\n")
	b.WriteString(renderThreads(others))
	return b.String()
}

// truncateRunes cuts s to n runes without splitting one.
func truncateRunes(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n]) + "…"
}
