package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/paraizo/delamain/internal/pipeline"
)

func TestRunShouldPrintResultInEnglishWhenPipelineCompletes(t *testing.T) {
	var out bytes.Buffer
	root := &cobra.Command{}
	cmd := &cobra.Command{}
	root.AddCommand(cmd)
	root.SetOut(&out)

	res := pipeline.Result{
		Outcome:    pipeline.OutcomeApproved,
		Branch:     "feat/2-example",
		Base:       "main",
		Worktree:   "/tmp/example",
		Iterations: 1,
		Commits:    []string{"abc123"},
		Delivery:   "no commits on branch: nothing to deliver",
		Budget:     "1K of 2M tokens",
		ReportPath: "/tmp/report.md",
	}
	printResult(cmd, res)

	for _, want := range []string{
		"branch:", "worktree:", "iterations: 1 · commits: 1",
		"delivery:", "usage:", "report:",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q:\n%s", want, out.String())
		}
	}
}
