package pipeline

import (
	"os"
	"path/filepath"
	"strings"
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

func TestPlanContextShouldNotReadSymlinkEscapingWorktree(t *testing.T) {
	outside := t.TempDir()
	seedFile(t, outside, "secret.txt", "TOP-SECRET\n")
	wt := t.TempDir()
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(wt, "link.txt")); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}
	got := planContext(wt, planWithRefs(artifacts.Ref{Path: "link.txt", Lines: "1-1", Why: "segue o link"}))

	if strings.Contains(got, "TOP-SECRET") {
		t.Fatalf("a symlink must not pull content from outside the worktree:\n%s", got)
	}
	if !strings.Contains(got, "link.txt:1-1 — não incluído: fora da worktree") {
		t.Fatalf("the escaping ref must be listed as not included:\n%s", got)
	}
}

func TestPlanContextShouldNotReadPathEscapingWorktree(t *testing.T) {
	parent := t.TempDir()
	seedFile(t, parent, "secret.txt", "TOP-SECRET\n")
	wt := filepath.Join(parent, "wt")
	seedFile(t, wt, "calc.go", "package calc\n")
	abs := filepath.Join(parent, "secret.txt")
	got := planContext(wt, planWithRefs(
		artifacts.Ref{Path: "../secret.txt", Lines: "1-1", Why: "sobe"},
		artifacts.Ref{Path: abs, Lines: "1-1", Why: "absoluto"},
	))

	if strings.Contains(got, "TOP-SECRET") {
		t.Fatalf("no content from outside the worktree may reach the prompt:\n%s", got)
	}
	for _, want := range []string{"../secret.txt:1-1 — não incluído: fora da worktree", abs + ":1-1 — não incluído: fora da worktree"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
}

func TestPlanContextShouldReportMissingFileWithoutFailing(t *testing.T) {
	wt := t.TempDir()
	seedFile(t, wt, "calc.go", "package calc\n")
	got := planContext(wt, planWithRefs(
		artifacts.Ref{Path: "gone.go", Lines: "3-9", Why: "apagado"},
		artifacts.Ref{Path: "calc.go", Lines: "1-1", Why: "pacote"},
	))

	if !strings.Contains(got, "gone.go:3-9 — não incluído: arquivo ausente") {
		t.Fatalf("a missing file is listed as not included:\n%s", got)
	}
	if !strings.Contains(got, "### S1 · calc.go:1-1 — pacote\n\n```\npackage calc\n```") {
		t.Fatalf("a missing file must not hide the refs that can be read:\n%s", got)
	}
}

func TestPlanContextShouldListRefsAsUnreadableWhenWorktreeCannotBeOpened(t *testing.T) {
	wt := filepath.Join(t.TempDir(), "never-created")
	got := planContext(wt, planWithRefs(artifacts.Ref{Path: "calc.go", Lines: "1-1", Why: "pacote"}))

	if got != "- S1 · calc.go:1-1 — não incluído: ilegível" {
		t.Fatalf("a worktree that cannot be opened lists every ref as unreadable:\n%q", got)
	}
}

func TestPlanContextShouldClampEndLineToFileLength(t *testing.T) {
	wt := t.TempDir()
	seedFile(t, wt, "calc.go", "one\ntwo\nthree\n")
	got := planContext(wt, planWithRefs(artifacts.Ref{Path: "calc.go", Lines: "2-40", Why: "fim"}))

	if want := "### S1 · calc.go:2-3 — fim\n\n```\ntwo\nthree\n```"; got != want {
		t.Fatalf("the range ends at the file's last line and the header says so:\ngot:\n%q\nwant:\n%q", got, want)
	}
}

func TestPlanContextShouldSkipRefStartingPastEndOfFile(t *testing.T) {
	wt := t.TempDir()
	seedFile(t, wt, "calc.go", "one\ntwo\nthree\n")
	got := planContext(wt, planWithRefs(
		artifacts.Ref{Path: "calc.go", Lines: "4-9", Why: "além"},
		artifacts.Ref{Path: "calc.go", Lines: "1-1", Why: "início"},
	))

	if !strings.Contains(got, "- S1 · calc.go:4-9 — não incluído: intervalo além do fim do arquivo") {
		t.Fatalf("a range starting past the end is listed as not included:\n%s", got)
	}
	if !strings.Contains(got, "### S1 · calc.go:1-1 — início\n\n```\none\n```") {
		t.Fatalf("the next ref is still read:\n%s", got)
	}
}

func TestPlanContextShouldReportMalformedLinesWithoutFailing(t *testing.T) {
	wt := t.TempDir()
	seedFile(t, wt, "calc.go", "one\n")
	for _, lines := range []string{"", "7", "3-1", "0-2", "a-b"} {
		got := planContext(wt, planWithRefs(artifacts.Ref{Path: "calc.go", Lines: lines, Why: "x"}))
		if want := "- S1 · calc.go:" + lines + " — não incluído: intervalo inválido"; got != want {
			t.Errorf("lines %q: got %q, want %q", lines, got, want)
		}
	}
}
