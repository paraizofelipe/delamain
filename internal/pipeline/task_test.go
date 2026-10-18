package pipeline

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseTaskFileShouldSplitTitleAndBody(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.md")
	if err := os.WriteFile(p, []byte("<!-- nota -->\n# Corrigir soma\r\n\nAdd subtrai.\n\n- [ ] Add(2,3)=5\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	title, body, err := ParseTaskFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if title != "Corrigir soma" || body != "<!-- nota -->\n\nAdd subtrai.\n\n- [ ] Add(2,3)=5" {
		t.Fatalf("title=%q body=%q", title, body)
	}
}

func TestParseTaskFileShouldRequireTitleHeading(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.md")
	_ = os.WriteFile(p, []byte("## Só subtítulo\ntexto\n"), 0o600)
	if _, _, err := ParseTaskFile(p); err == nil {
		t.Fatal("a task without '# Title' is ambiguous")
	}
}

func TestHasAcceptanceCriteriaShouldRecognizeChecklistsAndHeadings(t *testing.T) {
	yes := []string{"- [ ] faz X", "* [x] feito", "## Critérios de aceite\nalgo", "Acceptance criteria: y", "## Criterios de aceitação"}
	no := []string{"", "faça X", "- item sem checkbox", "- [ ]"}
	for _, b := range yes {
		if !HasAcceptanceCriteria(b) {
			t.Errorf("%q should count as acceptance criteria", b)
		}
	}
	for _, b := range no {
		if HasAcceptanceCriteria(b) {
			t.Errorf("%q should not count", b)
		}
	}
}

func TestInferKindShouldDetectBugsFromLabelOrTitle(t *testing.T) {
	cases := []struct {
		title  string
		labels []string
		want   string
	}{
		{"Fix login", nil, "bug"},
		{"Corrigir soma", nil, "bug"},
		{"Add export", []string{"Bug"}, "bug"},
		{"Add export", []string{"enhancement"}, "feature"},
		{"Prefix handling", nil, "feature"},
	}
	for _, c := range cases {
		if got := InferKind(c.title, c.labels); got != c.want {
			t.Errorf("InferKind(%q,%v)=%s want %s", c.title, c.labels, got, c.want)
		}
	}
}

func TestPreflightShouldListEveryProblem(t *testing.T) {
	if p := (Task{Kind: "chore"}).Preflight(); len(p) != 3 {
		t.Fatalf("title, body and kind problems expected, got %v", p)
	}
	ok := Task{Title: "x", Body: "- [ ] y", Kind: "feature"}
	if p := ok.Preflight(); len(p) != 0 {
		t.Fatalf("ready task flagged: %v", p)
	}
}
