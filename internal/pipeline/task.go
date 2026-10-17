package pipeline

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// Task is one unit of work: a forge issue or a local markdown task.
type Task struct {
	Ref   string // "#12" for issues, "task:<slug>" for local files
	Issue int    // forge issue number; 0 for local tasks
	Title string
	Body  string
	URL   string
	Kind  string // "feature" | "bug"
	Repo  string // path to the target repository
}

// ParseTaskFile reads a markdown task: the first "# " heading is the title, the rest is the body.
func ParseTaskFile(path string) (title, body string, err error) {
	raw, err := os.ReadFile(path) //nolint:gosec // the user points delamain at their own task file
	if err != nil {
		return "", "", err
	}
	lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
	for i, l := range lines {
		if t, ok := strings.CutPrefix(strings.TrimSpace(l), "# "); ok {
			title = strings.TrimSpace(t)
			body = strings.TrimSpace(strings.Join(append(lines[:i:i], lines[i+1:]...), "\n"))
			break
		}
	}
	if title == "" {
		return "", "", fmt.Errorf("%s: no '# Title' heading", path)
	}
	return title, body, nil
}

var (
	checklistRe  = regexp.MustCompile(`(?m)^\s*[-*] \[[ xX]\] \S`)
	criteriaHead = regexp.MustCompile(`(?i)crit[ée]rios? de aceit|acceptance criteria|definition of done`)
)

// HasAcceptanceCriteria reports whether the body states how to tell the work is done.
func HasAcceptanceCriteria(body string) bool {
	return checklistRe.MatchString(body) || criteriaHead.MatchString(body)
}

// InferKind classifies a task as a bug when its title or labels say so.
func InferKind(title string, labels []string) string {
	for _, l := range labels {
		if strings.EqualFold(l, "bug") {
			return "bug"
		}
	}
	t := strings.ToLower(title)
	for _, p := range []string{"fix", "bug", "corrig", "erro", "quebra"} {
		if strings.HasPrefix(t, p) {
			return "bug"
		}
	}
	return "feature"
}

// Preflight returns the reasons a task cannot be worked autonomously (empty means ready).
func (t Task) Preflight() []string {
	var problems []string
	if strings.TrimSpace(t.Title) == "" {
		problems = append(problems, "a task não tem título")
	}
	if strings.TrimSpace(t.Body) == "" {
		problems = append(problems, "a task não tem descrição")
	} else if !HasAcceptanceCriteria(t.Body) {
		problems = append(problems, "a task não tem critérios de aceite (use uma seção 'Critérios de aceite' ou um checklist '- [ ]')")
	}
	if t.Kind != "feature" && t.Kind != "bug" {
		problems = append(problems, fmt.Sprintf("tipo %q inválido (feature|bug)", t.Kind))
	}
	return problems
}
