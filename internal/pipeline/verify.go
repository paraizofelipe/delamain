package pipeline

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// CheckResult is one deterministic gate run by the orchestrator, never by an agent.
type CheckResult struct {
	Name     string
	Command  string
	ExitCode int
	LogPath  string
	Tail     string
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

	c := ru.plan.Commands
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
	return results, nil
}

func gitCheck(name string, ok bool, detail string) CheckResult {
	if ok {
		return CheckResult{Name: name, Command: "git"}
	}
	return CheckResult{Name: name, Command: "git", ExitCode: 1, Tail: detail}
}

// runCheck executes a command in the visible checks pane through a script file:
// no shell quoting of the command, any interactive shell works, and output lands in a log.
func (r *Runner) runCheck(ctx context.Context, ru *run, iterDir, name, line string) (CheckResult, error) {
	nonce := r.Nonce()
	logPath := filepath.Join(iterDir, "verify-"+name+".log")
	script := filepath.Join(iterDir, "verify-"+name+".sh")
	body := fmt.Sprintf("cd %s || exit 97\n( %s ) > %s 2>&1\nrc=$?\ntail -n 30 %s\necho \"__DLM_%s_EXIT=$rc\"\n",
		shellQuote(ru.repo.Worktree), line, shellQuote(logPath), shellQuote(logPath), nonce)
	if err := os.WriteFile(script, []byte(body), 0o600); err != nil {
		return CheckResult{}, err
	}
	pane := ru.panes["checks"]
	if err := r.Herdr.PaneRun(ctx, pane, "sh "+shellQuote(script)); err != nil {
		return CheckResult{}, err
	}
	matched, err := r.Herdr.PaneWaitOutput(ctx, pane, `__DLM_`+nonce+`_EXIT=[0-9]+`, r.Cfg.Pipeline.CheckTimeout)
	if err != nil {
		return CheckResult{}, haltf("check %s não terminou em %s: %v", name, r.Cfg.Pipeline.CheckTimeout, err)
	}
	code, err := exitCode(matched, nonce)
	if err != nil {
		return CheckResult{}, err
	}
	return CheckResult{Name: name, Command: line, ExitCode: code, LogPath: logPath, Tail: tailFile(logPath, 60, 4000)}, nil
}

func exitCode(line, nonce string) (int, error) {
	m := regexp.MustCompile(`__DLM_` + regexp.QuoteMeta(nonce) + `_EXIT=([0-9]+)`).FindStringSubmatch(line)
	if m == nil {
		return 0, fmt.Errorf("sentinel not found in %q", line)
	}
	return strconv.Atoi(m[1])
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
