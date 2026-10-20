package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/paraizo/delamain/internal/gates"
)

// CheckResult is one deterministic gate run by the orchestrator, never by an agent.
type CheckResult struct {
	Name     string
	Command  string
	ExitCode int
	LogPath  string
	Tail     string
	// Skipped marks a gate that could not run (missing tool); it does not block but is reported.
	Skipped bool
}

func (c CheckResult) Passed() bool { return c.ExitCode == 0 }

func allPassed(cs []CheckResult) bool {
	for _, c := range cs {
		if !c.Passed() {
			return false
		}
	}
	return len(cs) > 0
}

// verify proves the implementer's claims: git state first, then the project's own commands.
func (r *Runner) verify(ctx context.Context, ru *run, iterDir string) ([]CheckResult, error) {
	wt := ru.repo.Worktree
	branch, err := r.Git.CurrentBranch(ctx, wt)
	if err != nil {
		return nil, err
	}
	if branch != ru.repo.Branch {
		return nil, haltf("a worktree saiu da branch de trabalho (%s → %s)", ru.repo.Branch, branch)
	}

	var results []CheckResult
	status, err := r.Git.Status(ctx, wt)
	if err != nil {
		return nil, err
	}
	results = append(results, gitCheck("worktree-limpa", status == "", "mudanças não commitadas:\n"+status))
	commits, err := r.Git.CommitsSince(ctx, wt, ru.repo.Base)
	if err != nil {
		return nil, err
	}
	results = append(results, gitCheck("tem-commits", len(commits) > 0, "nenhum commit em "+ru.repo.Base+"..HEAD"))
	subjectsOK, subjectDetail := gates.Commits(commits, ru.repoCfg.Gates.SubjectLimit)
	results = append(results, gitCheck("commits", subjectsOK, subjectDetail))
	r.detectNewStack(ru)

	changed, err := r.Git.ChangedFiles(ctx, wt, ru.repo.Base)
	if err != nil {
		return nil, err
	}
	if r.requireTests(ru) {
		ok, detail := gates.TestsTouched(changed)
		results = append(results, gitCheck("testes-tocados", ok, detail))
	}
	problems := gates.Matrix(wt, ru.plan, changed)
	matrix := gitCheck("matriz-criterios", len(problems) == 0, strings.Join(problems, "\n"))
	matrix.Command = "plan.json"
	results = append(results, matrix)

	c := ru.cmds
	for _, cmd := range []struct{ name, line string }{
		{"format", c.FormatCheck}, {"lint", c.Lint}, {"typecheck", c.Typecheck}, {"test", c.Test},
	} {
		if strings.TrimSpace(cmd.line) == "" {
			continue
		}
		res, err := r.runCheck(ctx, ru, iterDir, cmd.name, cmd.line)
		if err != nil {
			return results, err
		}
		results = append(results, res)
	}
	// Proving red only makes sense once the branch itself is green.
	if allPassed(results) && r.wantsRedProof(ru) {
		res, err := r.redProof(ctx, ru, iterDir, changed)
		if err != nil {
			return results, err
		}
		results = append(results, res)
	}
	sec, err := r.secrets(ctx, ru, iterDir)
	if err != nil {
		return results, err
	}
	return append(results, sec), nil
}

// loadGates reads .delamain.yaml and detects the stack from the fresh worktree, which still
// equals the base branch: nothing an agent writes later can change the gates (see detectNewStack).
func (r *Runner) loadGates(ru *run) error {
	rc, err := gates.LoadRepoConfig(ru.repo.Worktree)
	if err != nil {
		return halt{err.Error()}
	}
	ru.repoCfg, ru.stack = rc, gates.Detect(ru.repo.Worktree)
	return nil
}

// detectNewStack covers a base with no stack (a new project): the stack the implementer created
// then supplies its fixed gates instead of leaving only the planner's commands. A base that
// already has a stack keeps it, so an agent can never swap the gates for its own.
func (r *Runner) detectNewStack(ru *run) {
	if ru.stack.Name != "" {
		return
	}
	if ru.stack = gates.Detect(ru.repo.Worktree); ru.stack.Name == "" {
		return
	}
	ru.cmds, ru.res.CommandSources = gates.Effective(ru.repoCfg.Commands, ru.stack.Commands, ru.plan.Commands)
	ru.log.Info("gate commands", "run", ru.rec.ID, "stack", ru.stack.Name, "sources", ru.res.CommandSources, "detected_after", "implement")
}

func (r *Runner) requireTests(ru *run) bool {
	if p := ru.repoCfg.Gates.RequireTests; p != nil {
		return *p
	}
	return r.Cfg.Gates.RequireTests
}

// wantsRedProof applies the policy to the issue kind as either side saw it, so an agent cannot
// skip the proof by reclassifying a bug as a feature.
func (r *Runner) wantsRedProof(ru *run) bool {
	policy := r.Cfg.Gates.RedProof
	if ru.repoCfg.Gates.RedProof != "" {
		policy = ru.repoCfg.Gates.RedProof
	}
	switch policy {
	case gates.RedProofAlways:
		return true
	case gates.RedProofBug:
		return ru.task.Kind == "bug" || ru.plan.Kind == "bug"
	}
	return false
}

// redProof checks the base branch out in a temporary worktree, applies only the branch's test
// files and runs the suite: it must fail, otherwise the tests do not catch what was fixed.
func (r *Runner) redProof(ctx context.Context, ru *run, iterDir string, changed []string) (CheckResult, error) {
	const name = "prova-vermelho"
	var tests []string
	for _, f := range changed {
		if gates.IsTest(f) {
			tests = append(tests, f)
		}
	}
	if len(tests) == 0 {
		return CheckResult{Name: name, Command: "git", ExitCode: 1, Tail: "nenhum arquivo de teste novo ou alterado para provar o vermelho no base"}, nil
	}
	dir := filepath.Join(r.Cfg.Paths.WorktreesDir, ".redproof", fmt.Sprintf("run%d-%s", ru.rec.ID, filepath.Base(iterDir)))
	if err := r.Git.AddDetachedWorktree(ctx, ru.repo.Worktree, dir, ru.repo.Base); err != nil {
		return CheckResult{}, err
	}
	defer func() {
		if err := r.Git.RemoveWorktree(context.WithoutCancel(ctx), ru.repo.Worktree, dir); err != nil {
			ru.log.Warn("could not remove red-proof worktree", "path", dir, "err", err)
		}
	}()
	if err := r.Git.CheckoutFiles(ctx, dir, ru.repo.Branch, tests); err != nil {
		return CheckResult{}, err
	}
	res, err := r.runCheckIn(ctx, ru, iterDir, "redproof", ru.cmds.Test, dir)
	if err != nil {
		return res, err
	}
	res.Name = name
	res.Command = fmt.Sprintf("%s (base %s + %d teste(s) da branch)", ru.cmds.Test, ru.repo.Base, len(tests))
	if res.ExitCode == 0 {
		res.ExitCode = 1
		res.Tail = fmt.Sprintf("os testes novos/alterados (%s) passam no base %s sem a correção: não provam o comportamento pedido. Escreva testes que falhem sem a mudança de produção.\n%s",
			strings.Join(tests, ", "), ru.repo.Base, res.Tail)
	} else {
		res.ExitCode = 0 // red on base is the expected evidence; the log keeps the failure output
	}
	return res, nil
}

// secrets scans the branch commits with gitleaks; without it the gate is reported as skipped.
func (r *Runner) secrets(ctx context.Context, ru *run, iterDir string) (CheckResult, error) {
	look := r.LookPath
	if look == nil {
		look = exec.LookPath
	}
	const name = "segredos"
	if _, err := look("gitleaks"); err != nil {
		return CheckResult{Name: name, Command: "gitleaks", Skipped: true, Tail: "gitleaks não instalado: varredura de segredos não executada"}, nil
	}
	line := fmt.Sprintf("gitleaks git --no-banner --redact --log-opts=%s..HEAD .", ru.repo.Base)
	res, err := r.runCheck(ctx, ru, iterDir, "secrets", line)
	res.Name = name
	return res, err
}

func gitCheck(name string, ok bool, detail string) CheckResult {
	if ok {
		return CheckResult{Name: name, Command: "git"}
	}
	return CheckResult{Name: name, Command: "git", ExitCode: 1, Tail: detail}
}

// runCheck runs a gate in the worktree. Its output goes to its own log and to the run's
// checks.log, which `delamain logs` (and a herdr tab) follow.
func (r *Runner) runCheck(ctx context.Context, ru *run, iterDir, name, line string) (CheckResult, error) {
	return r.runCheckIn(ctx, ru, iterDir, name, line, ru.repo.Worktree)
}

func (r *Runner) runCheckIn(ctx context.Context, ru *run, iterDir, name, line, dir string) (CheckResult, error) {
	checks := r.Checks
	if checks == nil {
		checks = ExecChecks{}
	}
	logPath := filepath.Join(iterDir, "verify-"+name+".log")
	code, err := checks.Run(ctx, Check{
		Name: name, Command: line, Dir: dir, LogPath: logPath, Echo: r.checksLog(ru), Timeout: r.Cfg.Pipeline.CheckTimeout,
	})
	if errors.Is(err, ErrCheckTimeout) {
		return CheckResult{}, haltf("check %s não terminou em %s", name, r.Cfg.Pipeline.CheckTimeout)
	}
	if err != nil {
		return CheckResult{}, err
	}
	ru.log.Info("gate", "run", ru.rec.ID, "name", name, "exit", code)
	return CheckResult{Name: name, Command: line, ExitCode: code, LogPath: logPath, Tail: tailFile(logPath, 60, 4000)}, nil
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func tailFile(path string, lines, maxBytes int) string {
	b, err := os.ReadFile(path) //nolint:gosec // inside the run dir
	if err != nil {
		return ""
	}
	all := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(all) > lines {
		all = all[len(all)-lines:]
	}
	out := strings.Join(all, "\n")
	if len(out) > maxBytes {
		out = "…" + out[len(out)-maxBytes:]
	}
	return out
}

func verifyFeedback(cs []CheckResult) string {
	var b strings.Builder
	for _, c := range cs {
		if c.Passed() {
			continue
		}
		fmt.Fprintf(&b, "- Gate `%s` (`%s`) falhou com código %d. Últimas linhas:\n```\n%s\n```\n", c.Name, c.Command, c.ExitCode, c.Tail)
	}
	return strings.TrimSpace(b.String())
}

func verifySummary(cs []CheckResult) string {
	parts := make([]string, 0, len(cs))
	for _, c := range cs {
		state := "ok"
		if !c.Passed() {
			state = fmt.Sprintf("falhou (%d)", c.ExitCode)
		}
		parts = append(parts, fmt.Sprintf("%s: %s", c.Name, state))
	}
	return strings.Join(parts, " · ")
}
