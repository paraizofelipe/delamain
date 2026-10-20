package complexity

import (
	"fmt"
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
	wide := lowCriteria()
	wide.Tests = []string{"regression"}

	tests := []struct {
		name   string
		plan   artifacts.Plan
		level  Level
		signal string
		want   Level
	}{
		{"all low", plan(names(2), lowCriteria()), Low, "files", Low},
		{"four files", plan(names(4), lowCriteria()), Medium, "files", Medium},
		{"ten files", plan(names(10), lowCriteria()), Medium, "files", Medium},
		{"five files", plan(names(5), lowCriteria()), Medium, "files", Medium},
		{"many files and architectural", plan(names(11), arch), High, "files", High},
		{"architectural alone", plan(names(2), arch), Medium, "change", High},
		{"dependencies and tests", plan(names(2), heavy), High, "tests", High},
		{"regression suite", plan(names(2), wide), Medium, "tests", High},
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

func TestClassifyShouldIgnoreTestFiles(t *testing.T) {
	a := Classify(plan([]string{"calc.go", "calc_test.go"}, lowCriteria()))
	if a.Files != 1 {
		t.Errorf("Files = %d, want 1", a.Files)
	}
}

func TestSummaryShouldRenderTheExactText(t *testing.T) {
	c := lowCriteria()
	c.Change = "architectural"
	c.Dependencies = []string{"database", "queue"}
	if got, want := Classify(plan(names(5), c)).Summary(), "high (5 arquivos; sinais altos: change, dependencies)"; got != want {
		t.Errorf("Summary() = %q, want %q", got, want)
	}
	if got, want := Classify(plan(names(1), lowCriteria())).Summary(), "low (1 arquivo)"; got != want {
		t.Errorf("Summary() = %q, want %q", got, want)
	}
}
