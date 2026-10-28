package cli

import (
	"os"
	"strings"
	"testing"
)

// The README is checked by its key terms only: a rewritten sentence must not break the test.
func TestReadmeShouldDocumentDependenciesBetweenIssues(t *testing.T) {
	b, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatalf("the README documents the dependencies between issues: %v", err)
	}
	readme := string(b)
	_, section, ok := strings.Cut(readme, "### Dependências entre issues")
	if !ok {
		t.Fatalf("the README has no section on the dependencies between issues:\n%s", readme)
	}
	if next := strings.Index(section, "\n#"); next >= 0 {
		section = section[:next]
	}
	for _, want := range []string{
		"depends_on: #", "bloco de código", // the canonical marker and where it does not count
		"depende de", "blocked by", "bloqueado por", "início da linha", // the legacy forms
		"is_blocked_by", "GitLab Free", // the native relations
		"formas antigas são ignoradas", "sempre se somam", // the precedence
		"delamain run --issue", "needs-info", // a run refuses the issue
		"WAITING", "queue list", // the queue keeps it waiting
		"épico", "ciclo", // the good practices
	} {
		if !strings.Contains(section, want) {
			t.Errorf("the section on the dependencies between issues should mention %q", want)
		}
	}
}
