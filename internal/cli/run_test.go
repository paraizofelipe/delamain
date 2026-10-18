package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paraizo/delamain/internal/config"
	"github.com/paraizo/delamain/internal/forge"
	"github.com/paraizo/delamain/internal/pipeline"
)

func TestRunShouldPrintResultInEnglishWhenPipelineCompletes(t *testing.T) {
	repo := gitRepo(t, "")
	taskFile := filepath.Join(t.TempDir(), "task.md")
	if err := os.WriteFile(taskFile, []byte("# Example task\n\n## Acceptance criteria\n- [ ] completes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgFile := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgFile, []byte("paths:\n  state_dir: "+filepath.Join(t.TempDir(), "state")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	previous := runTaskCommand
	runTaskCommand = func(ctx context.Context, cfg config.Config, _ pipeline.Task, _ forge.Forge) (pipeline.Result, error) {
		st, err := openStore(ctx, cfg)
		if err != nil {
			return pipeline.Result{}, err
		}
		defer func() { _ = st.Close() }()
		return pipeline.Result{
			Outcome:    pipeline.OutcomeExhausted,
			Reason:     "2 iterations without approval",
			Branch:     "feat/2-example",
			Base:       "main",
			Worktree:   "/tmp/example",
			Iterations: 2,
			Commits:    []string{"abc123"},
			Delivery:   "no commits on branch: nothing to deliver",
			Budget:     "1K of 2M tokens",
			ReportPath: "/tmp/report.md",
		}, nil
	}
	t.Cleanup(func() { runTaskCommand = previous })

	out, err := execute(t, "--config", cfgFile, "run", "--task", taskFile, "--repo", repo)
	if code, ok := IsExit(err); !ok || code != 3 {
		t.Fatalf("exit = %v, %v; want exit 3", code, err)
	}
	for _, want := range []string{
		"iterations: 2 · commits: 1",
		"delivery:  no commits on branch: nothing to deliver",
		"usage:     1K of 2M tokens",
		"report:    /tmp/report.md",
		"2 iterations without approval",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}
