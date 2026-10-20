// Package complexity derives an issue's complexity level from the planner's criteria.
// The level is computed here, never reported by the agent.
package complexity

import (
	"fmt"
	"sort"
	"strings"

	"github.com/paraizo/delamain/internal/artifacts"
	"github.com/paraizo/delamain/internal/gates"
)

type Level string

const (
	Low    Level = "low"
	Medium Level = "medium"
	High   Level = "high"
)

// Assessment is the outcome of classifying a plan, with the per-criterion signals behind it.
type Assessment struct {
	Level   Level
	Files   int              // unique non-test paths across plan.Slices[].Files
	Signals map[string]Level // keys: files, components, change, dependencies, tests
}

// Classify applies the thresholds: high with two or more high signals, low when all are low,
// medium otherwise.
func Classify(p artifacts.Plan) Assessment {
	seen := map[string]bool{}
	for _, s := range p.Slices {
		for _, f := range s.Files {
			if !gates.IsTest(f) {
				seen[f] = true
			}
		}
	}
	c := p.Complexity
	signals := map[string]Level{
		"files":        filesSignal(len(seen)),
		"components":   byName(c.Components, "multiple_modules", "multiple_services"),
		"change":       byName(c.Change, "cross_cutting", "architectural"),
		"dependencies": dependenciesSignal(len(c.Dependencies)),
		"tests":        testsSignal(c.Tests),
	}

	high, low := 0, 0
	for _, l := range signals {
		switch l {
		case High:
			high++
		case Low:
			low++
		}
	}
	level := Medium
	switch {
	case high >= 2:
		level = High
	case low == len(signals):
		level = Low
	}
	return Assessment{Level: level, Files: len(seen), Signals: signals}
}

func filesSignal(n int) Level {
	switch {
	case n > 10:
		return High
	case n >= 4:
		return Medium
	}
	return Low
}

func byName(v, medium, high string) Level {
	switch v {
	case high:
		return High
	case medium:
		return Medium
	}
	return Low
}

func dependenciesSignal(n int) Level {
	switch {
	case n >= 2:
		return High
	case n == 1:
		return Medium
	}
	return Low
}

// testsSignal is high for anything beyond plain unit tests.
func testsSignal(tests []string) Level {
	for _, t := range tests {
		if t != "unit" {
			return High
		}
	}
	return Low
}

// Summary renders the assessment for the user, e.g. "high (5 arquivos; sinais altos: change, dependencies)".
func (a Assessment) Summary() string {
	var high []string
	for k, l := range a.Signals {
		if l == High {
			high = append(high, k)
		}
	}
	sort.Strings(high)
	s := fmt.Sprintf("%s (%s", a.Level, plural(a.Files))
	if len(high) > 0 {
		s += "; sinais altos: " + strings.Join(high, ", ")
	}
	return s + ")"
}

func plural(n int) string {
	if n == 1 {
		return "1 arquivo"
	}
	return fmt.Sprintf("%d arquivos", n)
}
