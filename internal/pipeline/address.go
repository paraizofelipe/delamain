package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/paraizo/delamain/internal/artifacts"
	"github.com/paraizo/delamain/internal/budget"
	"github.com/paraizo/delamain/internal/config"
	"github.com/paraizo/delamain/internal/forge"
	"github.com/paraizo/delamain/internal/gates"
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
// thread. With review.address.push on, the responder may also commit fixes for the valid threads
// of a PR an issue run opened; the orchestrator checks and pushes them as a fast-forward, then
// resolves those threads (see fixRun). Like RunReview, a non-nil error means the run could not
// even be recorded; guardrail stops are OutcomeHalted.
func (r *Runner) RunAddress(ctx context.Context, repo string, n int) (Result, error) {
	res := Result{}
	if r.Forge == nil {
		return res, errors.New("address requires a GitHub or GitLab origin")
	}
	// Anyone who can comment on the PR writes the responder's input, so the responder runs
	// confined to reading the checkout, which only Claude Code can do (see confined).
	if r.Cfg.Agent.Harness != config.HarnessClaude {
		return res, fmt.Errorf("responder discussões de PR/MR pede agent.harness: claude, o único que confina o responder à leitura do PR (got %q)", r.Cfg.Agent.Harness)
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
	// The agent never starts inside the PR checkout: its harness would load the settings, hooks
	// and instruction files anyone with push access to the branch controls.
	ru.agentDir = filepath.Join(ru.dir, "agent")
	for _, d := range []string{filepath.Join(ru.dir, "sessions"), ru.agentDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	fetched, err := r.Git.FetchPRHead(ctx, repo, r.Forge.Repo().Kind, pr.Number)
	p.ref = gitx.PRRef(pr.Number) // removed even when the fetch half-failed
	if err != nil {
		return err
	}
	// Fail closed: the excerpts would show code the PR no longer has.
	if fetched != pr.HeadSHA {
		return haltf("o head buscado (%s) difere do head do PR/MR #%d (%s); nada foi postado", short(fetched), pr.Number, short(pr.HeadSHA))
	}
	base := "origin/" + pr.BaseRef
	fix, err := r.fixTarget(ctx, ru, pr)
	if err != nil {
		return err
	}
	var wt string
	if fix != nil {
		wt, err = r.fixWorktree(ctx, ru, pr, fix, base)
	} else {
		wt = filepath.Join(r.Cfg.Paths.WorktreesDir, filepath.Base(repo), fmt.Sprintf("address-pr-%d-%d", pr.Number, ru.rec.ID))
		err = r.Git.AddUntrustedWorktree(ctx, repo, wt, p.ref)
	}
	if err != nil {
		return err
	}
	ru.repo = prompts.Repo{Worktree: wt, Base: base}
	ru.rec.Worktree, ru.rec.Base = wt, base
	ru.res.Worktree, ru.res.Base = wt, base
	if fix != nil {
		ru.repo.Branch, ru.rec.Branch, ru.res.Branch = fix.branch, fix.branch, fix.branch
	}
	ru.log.Info("address worktree", "run", ru.rec.ID, "path", wt, "pr", pr.Number, "head", pr.HeadSHA)
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
	if fix != nil {
		// The gates come from the branch as it is now: nothing the responder edits can change them.
		if err := r.loadGates(ru); err != nil {
			return err
		}
		ru.cmds, ru.res.CommandSources = gates.Effective(ru.repoCfg.Commands, ru.stack.Commands, artifacts.Commands{})
	}
	linked, err := r.Forge.LinkedIssues(ctx, pr.Number)
	if err != nil {
		ru.log.Warn("could not read linked issues", "pr", pr.Number, "err", err)
	}

	if err := r.setState(ctx, ru, store.StateReview); err != nil {
		return err
	}
	artifact := filepath.Join(ru.dir, "responses.json")
	ru.confine = &Confinement{Checkout: wt, Base: pr.BaseSHA, Artifact: artifact, Fix: fix != nil}
	data := r.stageData(ru, 0, artifact, "")
	if fix != nil {
		// Outside the checkout, so add -A never commits it.
		ru.confine.CommitMessage = filepath.Join(ru.dir, "commit-msg.txt")
		data.CanFix, data.CommitMessagePath = true, ru.confine.CommitMessage
	}
	data.PR = prompts.PR{
		Number: pr.Number, URL: pr.URL, Title: pr.Title, Body: pr.Body, Author: pr.Author,
		BaseRef: pr.BaseRef, HeadRef: pr.HeadRef, BaseSHA: pr.BaseSHA, HeadSHA: pr.HeadSHA,
	}
	for _, is := range linked {
		// The replies are public; a confidential issue linked to the PR could be quoted in them.
		if is.Confidential {
			ru.log.Info("confidential linked issue left out of the prompt", "pr", pr.Number, "issue", is.Number)
			continue
		}
		data.LinkedIssues = append(data.LinkedIssues, prompts.Issue{Ref: "#" + strconv.Itoa(is.Number), Title: is.Title, Body: is.Body, URL: is.URL})
	}
	data.Threads = renderAddressThreads(p.threads, pending, func(t forge.Thread) string { return r.excerpt(ctx, repo, p.ref, t) })
	mem, fr := memory.Store{Dir: r.Cfg.Paths.MemoryDir()}, r.Forge.Repo()
	if data.Memory, err = mem.Read(fr.Host, fr.Path, memoryPromptBytes); err != nil {
		ru.log.Warn("could not read the repository memory", "err", err)
	}
	var resp artifacts.Responses
	if fix != nil {
		fr, err := stepArtifact[artifacts.FixResponses](ctx, r, ru, "responder", "address", data)
		if err != nil {
			return err
		}
		resp = artifacts.Responses(fr)
		if err := r.checkFix(ctx, ru, fix, resp); err != nil {
			return err
		}
	} else {
		if resp, err = stepArtifact[artifacts.Responses](ctx, r, ru, "responder", "address", data); err != nil {
			return err
		}
		// The responder only replies by contract; prove it.
		head2, status2, err := r.worktreeState(ctx, wt)
		if err != nil {
			return err
		}
		if head2 != head || status2 != status {
			return haltf("o responder alterou a worktree (HEAD %s→%s); respostas descartadas, nada foi postado", short(head), short(head2))
		}
	}
	ids := make([]string, 0, len(pending))
	for _, t := range pending {
		ids = append(ids, t.ID)
	}
	if err := resp.ValidatePending(ids); err != nil {
		return haltf("artefato do responder rejeitado: %v; nada foi postado", err)
	}

	// The replies are posted publicly as the operator and the learnings feed later prompts: a
	// prompt injection could have the responder quote a local credential in either.
	bodies := slices.Clone(resp.Learnings)
	for _, a := range resp.Responses {
		bodies = append(bodies, a.Reply)
	}
	if kind := leakedSecret(bodies, os.Environ()); kind != "" {
		return haltf("as respostas contêm o que parece um segredo (%s); nada foi postado", kind)
	}
	if err := r.setState(ctx, ru, store.StateDeliver); err != nil {
		return err
	}
	if fix != nil && len(fix.commits) > 0 {
		if err := r.publishFix(ctx, ru, fix); err != nil {
			return err
		}
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
		if err := r.Forge.Reply(ctx, pr.Number, a.ThreadID, fix.replyBody(a)+"\n\n"+m.String()); err != nil {
			return haltf("falha ao responder a discussão %s do PR/MR #%d: %v", a.ThreadID, pr.Number, err)
		}
		count[a.Verdict]++
	}
	resolved, err := r.resolveFixed(ctx, ru, fix, resp)
	if err != nil {
		return err
	}
	c := prev
	c.Repo, c.PR, c.LastSeenAt, c.Rounds = repo, pr.Number, newestComment(pending), prev.Rounds+1
	if err := r.Store.SaveCursor(ctx, c); err != nil {
		return err
	}
	ru.res.Outcome = OutcomeAnswered
	ru.res.Reason = fmt.Sprintf("%d respostas postadas (valid %d · invalid %d · unclear %d)", count["valid"]+count["invalid"]+count["unclear"], count["valid"], count["invalid"], count["unclear"]) + fix.summary(resolved)
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

// fixState is what became of the responder's commits.
type fixState int

const (
	fixNone         fixState = iota // no commit to publish
	fixPushed                       // checked and pushed as a fast-forward
	fixChecksFailed                 // a check failed: nothing pushed
	fixRemoteMoved                  // the branch moved on origin during the run: nothing pushed
)

// fixRun is the fixing side of an address run on a PR an issue run opened from branch.
type fixRun struct {
	branch   string
	expected string   // origin head of branch, recorded before the responder starts
	commits  []string // the responder's commits, expected..HEAD
	state    fixState
	sha      string   // HEAD pushed
	failed   []string // checks that failed
}

// fixTarget is the PR's fixing side when the responder may fix it: review.address.push on and a
// PR that an issue run of delamain opened from its own branch, not a fork. Anything else only
// gets replies (nil): fixing a branch delamain does not own is out of scope.
func (r *Runner) fixTarget(ctx context.Context, ru *run, pr forge.PRDetail) (*fixRun, error) {
	if !r.Cfg.Review.Address.Push {
		return nil, nil
	}
	runs, err := r.Store.RunsWithMR(ctx, ru.rec.Repo)
	if err != nil {
		return nil, err
	}
	for _, rec := range runs {
		if rec.MRURL == pr.URL && rec.Branch != "" && rec.Branch == pr.HeadRef && !pr.Fork {
			return &fixRun{branch: rec.Branch}, nil
		}
	}
	ru.log.Info("PR/MR not opened from a branch of delamain: only replying", "pr", pr.Number, "head", pr.HeadRef, "fork", pr.Fork)
	return nil, nil
}

// fixWorktree records the branch's origin head and checks the branch out at it. The issue run's
// worktree at the same path is reused when clean, on the branch and at that head, and recreated
// from origin only when clean and holding no commit origin lacks; anything else is someone's work,
// so the run halts without touching it.
func (r *Runner) fixWorktree(ctx context.Context, ru *run, pr forge.PRDetail, fix *fixRun, base string) (string, error) {
	repo := ru.rec.Repo
	expected, err := r.Git.RemoteSHA(ctx, repo, fix.branch)
	if err != nil {
		return "", err
	}
	if expected != pr.HeadSHA {
		return "", haltf("a branch %s está em %s no remoto, não no head do PR/MR #%d (%s); nada foi postado", fix.branch, short(expected), pr.Number, short(pr.HeadSHA))
	}
	fix.expected = expected
	wt := filepath.Join(r.Cfg.Paths.WorktreesDir, filepath.Base(repo), fix.branch)
	if _, err := os.Stat(wt); err == nil {
		branch, err := r.Git.CurrentBranch(ctx, wt)
		if err != nil {
			return "", err
		}
		head, status, err := r.worktreeState(ctx, wt)
		if err != nil {
			return "", err
		}
		if branch == fix.branch && head == expected && status == "" {
			ru.log.Info("reusing the branch worktree", "path", wt, "branch", fix.branch, "head", expected)
			return wt, nil
		}
		if status != "" {
			return "", haltf("a worktree %s tem mudanças fora de commit; nada foi tocado nem postado", wt)
		}
		if err := r.Git.FetchRef(ctx, repo, fix.branch); err != nil {
			return "", err
		}
		if ok, err := r.Git.IsAncestor(ctx, wt, head, expected); err != nil {
			return "", err
		} else if !ok {
			return "", haltf("a worktree %s tem commits que não estão em origin/%s; nada foi tocado nem postado", wt, fix.branch)
		}
		ru.log.Info("recreating the branch worktree", "path", wt, "branch", fix.branch)
		if err := r.Git.RemoveWorktree(ctx, repo, wt); err != nil {
			return "", err
		}
	}
	return wt, r.Git.AddWorktreeOnBranch(ctx, repo, wt, fix.branch, base)
}

// checkFix holds the responder to its contract before anything is published: everything
// committed, on the branch, on top of the recorded head, and commits if and only if a thread
// says fixed.
func (r *Runner) checkFix(ctx context.Context, ru *run, fix *fixRun, resp artifacts.Responses) error {
	wt := ru.repo.Worktree
	branch, err := r.Git.CurrentBranch(ctx, wt)
	if err != nil {
		return err
	}
	if branch != fix.branch {
		return haltf("o responder saiu da branch %s (%s); nada foi enviado nem postado", fix.branch, branch)
	}
	_, status, err := r.worktreeState(ctx, wt)
	if err != nil {
		return err
	}
	if status != "" {
		return haltf("o responder deixou mudanças fora de commit; nada foi enviado nem postado")
	}
	ok, err := r.Git.IsAncestor(ctx, wt, fix.expected, "HEAD")
	if err != nil {
		return err
	}
	if !ok {
		return haltf("os commits do responder não descendem de %s, o head do remoto; nada foi enviado nem postado", short(fix.expected))
	}
	if fix.commits, err = r.Git.CommitsSince(ctx, wt, fix.expected); err != nil {
		return err
	}
	fixed := 0
	for _, a := range resp.Responses {
		if a.Fixed {
			fixed++
		}
	}
	switch {
	case fixed > 0 && len(fix.commits) == 0:
		return haltf("o responder marcou %d discussão(ões) como corrigida(s) sem nenhum commit; nada foi postado", fixed)
	case fixed == 0 && len(fix.commits) > 0:
		return haltf("o responder fez %d commit(s) sem marcar nenhuma discussão como corrigida; nada foi enviado nem postado", len(fix.commits))
	}
	return nil
}

// publishFix runs the checks on the responder's commits and pushes them only when all pass,
// as a fast-forward of the recorded head. A failed check or a moved origin is not a halt: the
// replies still go out, saying the fix was not sent. Any other push failure is.
func (r *Runner) publishFix(ctx context.Context, ru *run, fix *fixRun) error {
	dir := filepath.Join(ru.dir, "fix")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	ok, detail := gates.Commits(fix.commits, ru.repoCfg.Gates.SubjectLimit)
	checks := []CheckResult{gitCheck("commits", ok, detail)}
	// The commit messages, bodies included, are published with the push; gitleaks scans only
	// the patches.
	msgs, err := r.Git.CommitMessages(ctx, ru.repo.Worktree, fix.expected)
	if err != nil {
		return err
	}
	leak := leakedSecret(msgs, os.Environ())
	checks = append(checks, gitCheck("segredos-commits", leak == "", "uma mensagem de commit parece conter um segredo ("+leak+")"))
	cmds, err := r.commandChecks(ctx, ru, dir)
	if err != nil {
		return err
	}
	sec, err := r.secrets(ctx, ru, dir, fix.expected)
	if err != nil {
		return err
	}
	for _, c := range append(append(checks, cmds...), sec) {
		if !c.Passed() {
			fix.failed = append(fix.failed, c.Name)
		}
	}
	if len(fix.failed) > 0 {
		fix.state = fixChecksFailed
		ru.log.Warn("fix not pushed: checks failed", "run", ru.rec.ID, "failed", fix.failed)
		return nil
	}
	head, err := r.Git.Head(ctx, ru.repo.Worktree)
	if err != nil {
		return err
	}
	err = r.Git.PushFastForward(ctx, ru.repo.Worktree, fix.branch, ru.repo.Base, fix.expected)
	switch {
	case errors.Is(err, gitx.ErrRemoteMoved):
		fix.state = fixRemoteMoved
		ru.log.Warn("fix not pushed: the branch moved on origin", "run", ru.rec.ID, "branch", fix.branch, "err", err)
		return nil
	case err != nil:
		return haltf("push da correção para %s recusado: %v; nada foi postado", fix.branch, err)
	}
	fix.state, fix.sha = fixPushed, head
	ru.log.Info("fix pushed", "run", ru.rec.ID, "branch", fix.branch, "from", fix.expected, "to", head)
	return nil
}

// resolveFixed resolves the threads whose fix was pushed, and only those.
func (r *Runner) resolveFixed(ctx context.Context, ru *run, fix *fixRun, resp artifacts.Responses) (int, error) {
	if fix == nil || fix.state != fixPushed {
		return 0, nil
	}
	n := 0
	for _, a := range resp.Responses {
		if !a.Fixed || a.Verdict != "valid" {
			continue
		}
		if err := r.Forge.ResolveThread(ctx, ru.pr.pr.Number, a.ThreadID); err != nil {
			return n, haltf("correção enviada em %s, mas falha ao resolver a discussão %s do PR/MR #%d: %v", short(fix.sha), a.ThreadID, ru.pr.pr.Number, err)
		}
		n++
	}
	return n, nil
}

// replyBody is the reply to a, telling what became of its fix when it has one.
func (fix *fixRun) replyBody(a artifacts.Response) string {
	if fix == nil || !a.Fixed {
		return replyBody(a)
	}
	switch fix.state {
	case fixPushed:
		return replyBody(a) + fmt.Sprintf("\n\nCorreção enviada em `%s`.", short(fix.sha))
	case fixChecksFailed:
		a.Reply = fmt.Sprintf("A correção desta discussão não passou nos checks (%s) e não foi enviada; fica para revisão humana.", strings.Join(fix.failed, ", "))
	case fixRemoteMoved:
		a.Reply = "A correção desta discussão não foi enviada: a branch mudou no remoto durante a run. Fica para revisão humana."
	}
	return replyBody(a)
}

// summary is what the outcome says about the fix; "" without one.
func (fix *fixRun) summary(resolved int) string {
	if fix == nil {
		return ""
	}
	switch fix.state {
	case fixPushed:
		return fmt.Sprintf("; correção enviada em %s, %d discussão(ões) resolvida(s)", short(fix.sha), resolved)
	case fixChecksFailed:
		return fmt.Sprintf("; correções não enviadas: checks reprovados (%s)", strings.Join(fix.failed, ", "))
	case fixRemoteMoved:
		return fmt.Sprintf("; correções não enviadas: a branch %s mudou no remoto durante a run", fix.branch)
	}
	return ""
}

func replyBody(a artifacts.Response) string {
	return fmt.Sprintf("🤖 **delamain** · parecer: `%s`\n\n%s", a.Verdict, a.Reply)
}

// excerpt is the head's code around the line a thread comments on, read from the PR ref in the
// object store: "" for PR-level threads or when the file cannot be read.
func (r *Runner) excerpt(ctx context.Context, repo, ref string, t forge.Thread) string {
	// Read from the ref's objects, never the checkout: no symlink is followed and a path that
	// leaves the repository is not read at all.
	if t.Path == "" || t.Line <= 0 || !filepath.IsLocal(t.Path) {
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
