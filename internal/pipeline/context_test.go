package pipeline

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/paraizo/delamain/internal/artifacts"
)

// seedFile writes body to name inside dir, creating parent directories.
func seedFile(t *testing.T, dir, name, body string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// planWithRefs is a one-slice plan whose slice S1 points to refs.
func planWithRefs(refs ...artifacts.Ref) artifacts.Plan {
	return artifacts.Plan{Slices: []artifacts.Slice{{ID: "S1", Refs: refs}}}
}

func TestPlanContextShouldReturnEmptyWhenPlanHasNoRefs(t *testing.T) {
	wt := t.TempDir()
	seedFile(t, wt, "calc.go", "package calc\n")
	if got := planContext(wt, planWithRefs()); got != "" {
		t.Fatalf("a plan without refs injects nothing, got:\n%s", got)
	}
}

func TestPlanContextShouldRenderRequestedLinesWithHeader(t *testing.T) {
	wt := t.TempDir()
	seedFile(t, wt, "calc.go", "package calc\n\nfunc Add(a, b int) int { return a - b }\n// tail\n")
	got := planContext(wt, planWithRefs(artifacts.Ref{Path: "calc.go", Lines: "2-3", Why: "Add subtrai"}))

	want := "### S1 · calc.go:2-3 — Add subtrai\n\n```\n\nfunc Add(a, b int) int { return a - b }\n```"
	if got != want {
		t.Fatalf("the ref must become a header plus exactly the requested lines:\ngot:\n%q\nwant:\n%q", got, want)
	}
}
