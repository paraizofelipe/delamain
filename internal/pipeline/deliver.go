package pipeline

import (
	"cmp"
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/paraizo/delamain/internal/forge"
	"github.com/paraizo/delamain/internal/gates"
	"github.com/paraizo/delamain/internal/notify"
	"github.com/paraizo/delamain/internal/store"
)

// blockers resolves the issues that block a task through OpenBlockers. It fails closed: an
// unverifiable blocker or a malformed "depends_on:" line refuses the task.
func (r *Runner) blockers(ctx context.Context, t Task) []string {
	if r.Forge == nil {
		return nil
	}
	open, err := OpenBlockers(ctx, r.Forge, t)
	if err != nil {
		return []string{err.Error()}
	}
	problems := make([]string, 0, len(open))
	for _, b := range open {
		problems = append(problems, fmt.Sprintf("depende da issue %s, que ainda está aberta", b))
	}
	return problems
}

// OpenBlockers lists the open issues that block t: the dependencies its body declares ("depends_on:
// #N" lines or, when there are none, the legacy mentions) and the forge's native "blocked by"
// relations. Each one reads "<ref> (<sources>)", e.g. "#82 (corpo)" or "acme/other#90 (link)".
// A malformed "depends_on:" line or a blocker that cannot be read is an error.
func OpenBlockers(ctx context.Context, fg forge.Forge, t Task) ([]string, error) {
	deps, err := gates.Dependencies(t.Body)
	if err != nil {
		return nil, fmt.Errorf("depends_on inválido: %w", err)
	}
	type blocker struct {
		number  int
		ref     string
		sources []string
	}
	var open []*blocker
	byRef := map[string]*blocker{}
	add := func(number int, ref, source string) {
		if b, ok := byRef[ref]; ok {
			b.sources = append(b.sources, source)
			return
		}
		b := &blocker{number: number, ref: ref, sources: []string{source}}
		byRef[ref] = b
		open = append(open, b)
	}
	for _, n := range deps {
		is, err := fg.Issue(ctx, n)
		if err != nil {
			return nil, fmt.Errorf("não foi possível verificar a dependência #%d: %w", n, err)
		}
		if is.Open {
			add(n, fmt.Sprintf("#%d", n), "corpo")
		}
	}
	native, err := fg.Blockers(ctx, t.Issue)
	if err != nil {
		return nil, err
	}
	for _, b := range native {
		ref := b.Ref
		if ref == "" {
			ref = fmt.Sprintf("#%d", b.Number)
		}
		add(b.Number, ref, "link")
	}
	slices.SortFunc(open, func(a, b *blocker) int {
		return cmp.Or(cmp.Compare(a.number, b.number), strings.Compare(a.ref, b.ref))
	})
	out := make([]string, 0, len(open))
	for _, b := range open {
		out = append(out, fmt.Sprintf("%s (%s)", b.ref, strings.Join(b.sources, ", ")))
	}
	return out, nil
}

// checkForge stops before any worktree exists when the branch was already published or has an
// open PR/MR: delamain never piles a second PR on top of earlier work.
func (r *Runner) checkForge(ctx context.Context, repo, branch string) error {
	if r.Forge == nil {
		return nil
	}
	onOrigin, err := r.Git.RemoteBranchExists(ctx, repo, branch)
	if err != nil {
		return err
	}
	if onOrigin {
		return haltf("a branch %s já existe no origin; finalize ou remova o trabalho anterior", branch)
	}
	pr, err := r.Forge.OpenPR(ctx, branch)
	if err != nil {
		return err
	}
	if pr != nil {
		return haltf("já existe PR/MR aberto para %s: %s", branch, pr.URL)
	}
	return nil
}

// deliver pushes the work branch and opens the PR/MR (step 10): ready when approved, draft
// otherwise. It never merges. Failures turn the run into a loud halt; the local branch stays.
func (r *Runner) deliver(ctx context.Context, ru *run) {
	res := ru.res
	switch {
	case ru.repo.Worktree == "":
		return // setup never produced a branch
	case res.PRURL != "":
		return // delivered before the process died
	case len(res.Commits) == 0:
		res.Delivery = "nenhum commit na branch: nada a entregar"
		return
	case r.Forge == nil:
		res.Delivery = "repositório sem forge (origin ausente ou forge.kind=none): branch mantida apenas localmente"
		return
	}
	draft := res.Outcome != OutcomeApproved
	fail := func(err error) {
		prev := ""
		if res.Reason != "" {
			prev = res.Reason + "; "
		}
		res.Outcome, res.Reason = OutcomeHalted, fmt.Sprintf("%sentrega falhou: %v (branch local %s preservada)", prev, err, ru.repo.Branch)
	}

	// The branch did not exist on origin at setup and agents run with push blocked:
	// if it is there now, something bypassed the guard and the work is not trustworthy.
	onOrigin, err := r.Git.RemoteBranchExists(ctx, ru.repo.Worktree, ru.repo.Branch)
	if err != nil {
		fail(err)
		return
	}
	pushedBeforeCrash := onOrigin && ru.recovered && (ru.rec.State == store.StateDeliver || ru.rec.State == store.StateDeliverDraft)
	switch {
	case pushedBeforeCrash:
		// The dead process pushed but may not have opened the PR/MR: reuse an open one.
		pr, err := r.Forge.OpenPR(ctx, ru.repo.Branch)
		if err != nil {
			fail(err)
			return
		}
		if pr != nil {
			res.PRURL, res.Draft, ru.rec.MRURL = pr.URL, pr.Draft, pr.URL
			return
		}
	case onOrigin:
		fail(fmt.Errorf("a branch %s apareceu no origin durante a run: um agente fez push por fora do delamain", ru.repo.Branch))
		return
	default:
		if err := r.Git.Push(ctx, ru.repo.Worktree, ru.repo.Branch, ru.repo.Base); err != nil {
			fail(err)
			return
		}
	}
	pr, err := r.Forge.CreatePR(ctx, forgePROptions(ru, draft))
	if err != nil {
		fail(err)
		return
	}
	res.PRURL, res.Draft = pr.URL, pr.Draft
	ru.rec.MRURL = pr.URL
	ru.log.Info("delivered", "run", ru.rec.ID, "url", pr.URL, "draft", pr.Draft)
}

// announce records the result on the issue (step 11): final label and a status comment.
func (r *Runner) announce(ctx context.Context, ru *run, t Task) {
	l := r.Cfg.Forge.Labels
	final := l.NeedsHuman
	if ru.res.PRURL != "" && !ru.res.Draft {
		final = l.MROpened
	}
	r.issueUpdate(ctx, t, ru.res, []string{final}, []string{l.InProgress}, issueComment(*ru.res))
}

func (r *Runner) needsInfo(ctx context.Context, t Task, problems []string, res *Result) {
	l := r.Cfg.Forge.Labels
	relabeled := r.issueUpdate(ctx, t, res, []string{l.NeedsInfo}, []string{l.Ready, l.InProgress}, needsInfoComment(problems, l.Ready))
	n := notify.Run{Repo: t.Repo, Ref: t.Ref, Title: t.Title}
	if r.Forge != nil && t.Issue != 0 {
		n.IssueURL = t.URL
	}
	if relabeled {
		n.NeedsInfoLabel = l.NeedsInfo
	}
	r.notify(ctx, r.Log, notify.FormatNeedsInfo(n))
}

// needsInfoComment lists what kept the run from starting. problems mixes gaps in the issue with
// open dependencies, so the wording must not blame the issue for either.
func needsInfoComment(problems []string, readyLabel string) string {
	var b strings.Builder
	b.WriteString("🤖 **Execução automática não iniciada**: há pendências nesta issue.\n\n")
	for _, p := range problems {
		fmt.Fprintf(&b, "- %s\n", p)
	}
	fmt.Fprintf(&b, "\nResolva as pendências e recoloque a label `%s`.", readyLabel)
	return b.String()
}

// issueUpdate relabels and comments on the task's issue. Forge hiccups here never fail the run:
// they are logged and reported as warnings. It reports whether the labels were applied.
func (r *Runner) issueUpdate(ctx context.Context, t Task, res *Result, add, remove []string, comment string) bool {
	if r.Forge == nil || t.Issue == 0 {
		return false
	}
	relabeled := true
	if err := r.Forge.Relabel(ctx, t.Issue, add, remove); err != nil {
		r.Log.Warn("could not relabel issue", "issue", t.Issue, "err", err)
		res.Warnings = append(res.Warnings, "labels da issue: "+err.Error())
		relabeled = false
	}
	if comment == "" {
		return relabeled
	}
	if err := r.Forge.Comment(ctx, t.Issue, comment); err != nil {
		r.Log.Warn("could not comment on issue", "issue", t.Issue, "err", err)
		res.Warnings = append(res.Warnings, "comentário na issue: "+err.Error())
	}
	return relabeled
}

var conventional = regexp.MustCompile(`^(feat|fix|chore|docs|refactor|test|ci|perf|build|style)(\([^)]+\))?!?: `)

// prTitle makes a Conventional Commits title, keeping one the issue already has.
func prTitle(kind, title string) string {
	title = strings.TrimSpace(title)
	if conventional.MatchString(title) {
		return title
	}
	prefix := "feat: "
	if kind == "bug" {
		prefix = "fix: "
	}
	r, n := utf8.DecodeRuneInString(title)
	return prefix + string(unicode.ToLower(r)) + title[n:]
}
