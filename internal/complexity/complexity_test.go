package complexity

import (
	"fmt"
	"strings"
	"testing"

	"github.com/paraizo/delamain/internal/artifacts"
)

func plan(files []string, c artifacts.Complexity) artifacts.Plan {
	return artifacts.Plan{Slices: []artifacts.Slice{{ID: "s1", Files: files}}, Complexity: c}
}

func names(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("f%d.go", i)
	}
	return out
}

func lowCriteria() artifacts.Complexity {
	return artifacts.Complexity{Components: "single_module", Change: "localized", Dependencies: []string{}, Tests: []string{"unit"}}
}

func TestClassifyShouldApplyTheThresholds(t *testing.T) {
	arch := lowCriteria()
	arch.Change = "architectural"
	heavy := lowCriteria()
	heavy.Dependencies = []string{"database", "queue"}
	heavy.Tests = []string{"unit", "migration"}
	api := lowCriteria()
	api.Dependencies = []string{"api"}

	tests := []struct {
		name   string
		plan   artifacts.Plan
		level  Level
		signal string
		want   Level
	}{
		{"all low", plan(names(2), lowCriteria()), Low, "files", Low},
		{"five files", plan(names(5), lowCriteria()), Medium, "files", Medium},
		{"many files and architectural", plan(names(11), arch), High, "files", High},
		{"architectural alone", plan(names(2), arch), Medium, "change", High},
		{"dependencies and tests", plan(names(2), heavy), High, "tests", High},
		{"one dependency", plan(names(2), api), Medium, "dependencies", Medium},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := Classify(tt.plan)
			if a.Level != tt.level {
				t.Errorf("level = %s, want %s", a.Level, tt.level)
			}
			if a.Signals[tt.signal] != tt.want {
				t.Errorf("signal %s = %s, want %s", tt.signal, a.Signals[tt.signal], tt.want)
			}
		})
	}
}

func TestClassifyShouldCountARepeatedFileOnce(t *testing.T) {
	p := plan([]string{"calc.go"}, lowCriteria())
	p.Slices = append(p.Slices, artifacts.Slice{ID: "s2", Files: []string{"calc.go"}})
	if got := Classify(p).Files; got != 1 {
		t.Errorf("Files = %d, want 1", got)
	}
}

func TestSummaryShouldNameTheHighSignals(t *testing.T) {
	c := lowCriteria()
	c.Change = "architectural"
	c.Dependencies = []string{"database", "queue"}
	got := Classify(plan(names(5), c)).Summary()
	for _, want := range []string{"high", "change", "dependencies"} {
		if !strings.Contains(got, want) {
			t.Errorf("Summary() = %q, missing %q", got, want)
		}
	}
}
