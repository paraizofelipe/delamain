// Package artifacts parses and validates the JSON files agents write at the end of each step.
// The orchestrator trusts these files, never the agent's chat reply; the formats are described
// to the agents in prompts/stages/*.md and must stay in sync with these types.
package artifacts

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

type Commands struct {
	Test        string `json:"test"`
	TestFile    string `json:"test_file"`
	Lint        string `json:"lint"`
	Typecheck   string `json:"typecheck"`
	FormatCheck string `json:"format_check"`
}

type Slice struct {
	ID       string   `json:"id"`
	Behavior string   `json:"behavior"`
	Seam     string   `json:"seam"`
	Files    []string `json:"files"`
	Tests    []string `json:"tests"`
}

type Acceptance struct {
	Criterion string   `json:"criterion"`
	Tests     []string `json:"tests"`
	Flow      bool     `json:"flow"`
}

// Complexity holds the criteria the planner reports about the issue; the orchestrator derives
// the complexity level from them, never from the agent.
type Complexity struct {
	Components   string   `json:"components"`   // single_module | multiple_modules | multiple_services
	Change       string   `json:"change"`       // localized | cross_cutting | architectural
	Dependencies []string `json:"dependencies"` // subset of api, database, queue, integration
	Tests        []string `json:"tests"`        // non-empty subset of unit, integration, migration, regression
	Rationale    string   `json:"rationale"`
}

var (
	validComponents   = map[string]bool{"single_module": true, "multiple_modules": true, "multiple_services": true}
	validChanges      = map[string]bool{"localized": true, "cross_cutting": true, "architectural": true}
	validDependencies = map[string]bool{"api": true, "database": true, "queue": true, "integration": true}
	validTestKinds    = map[string]bool{"unit": true, "integration": true, "migration": true, "regression": true}
)

// validateSet checks that every item is in the allowed set and none repeats.
func validateSet(field string, items []string, allowed map[string]bool) []error {
	var errs []error
	seen := map[string]bool{}
	for _, it := range items {
		if !allowed[it] {
			errs = append(errs, fmt.Errorf("%s: unknown value %q", field, it))
		}
		if seen[it] {
			errs = append(errs, fmt.Errorf("%s: %q repeated", field, it))
		}
		seen[it] = true
	}
	return errs
}

func (c Complexity) validate() []error {
	var errs []error
	if !validComponents[c.Components] {
		errs = append(errs, fmt.Errorf("complexity.components must be single_module, multiple_modules or multiple_services, got %q", c.Components))
	}
	if !validChanges[c.Change] {
		errs = append(errs, fmt.Errorf("complexity.change must be localized, cross_cutting or architectural, got %q", c.Change))
	}
	errs = append(errs, validateSet("complexity.dependencies", c.Dependencies, validDependencies)...)
	if len(c.Tests) == 0 {
		errs = append(errs, errors.New("complexity.tests must list at least one of unit, integration, migration, regression"))
	}
	errs = append(errs, validateSet("complexity.tests", c.Tests, validTestKinds)...)
	return errs
}

type Plan struct {
	Summary         string       `json:"summary"`
	Kind            string       `json:"kind"`
	Assumptions     []string     `json:"assumptions"`
	Slices          []Slice      `json:"slices"`
	Acceptance      []Acceptance `json:"acceptance"`
	RegressionTests []string     `json:"regression_tests"`
	Commands        Commands     `json:"commands"`
	Complexity      Complexity   `json:"complexity"`
	Risks           []string     `json:"risks"`
}

// Validate enforces the contract: every acceptance criterion is proven by a test.
func (p Plan) Validate() error {
	var errs []error
	if strings.TrimSpace(p.Summary) == "" {
		errs = append(errs, errors.New("summary is empty"))
	}
	if p.Kind != "feature" && p.Kind != "bug" {
		errs = append(errs, fmt.Errorf("kind must be feature or bug, got %q", p.Kind))
	}
	if len(p.Slices) == 0 {
		errs = append(errs, errors.New("no slices"))
	}
	ids := map[string]bool{}
	for i, s := range p.Slices {
		if s.ID == "" || strings.TrimSpace(s.Behavior) == "" {
			errs = append(errs, fmt.Errorf("slice %d: id and behavior are required", i+1))
		}
		if ids[s.ID] {
			errs = append(errs, fmt.Errorf("slice id %q repeated", s.ID))
		}
		ids[s.ID] = true
	}
	if len(p.Acceptance) == 0 {
		errs = append(errs, errors.New("no acceptance criteria"))
	}
	for i, a := range p.Acceptance {
		if strings.TrimSpace(a.Criterion) == "" {
			errs = append(errs, fmt.Errorf("acceptance %d: criterion is empty", i+1))
		}
		if len(a.Tests) == 0 {
			errs = append(errs, fmt.Errorf("acceptance %q has no test", a.Criterion))
		}
	}
	if p.Kind == "bug" && len(p.RegressionTests) == 0 {
		errs = append(errs, errors.New("bug plans require regression_tests"))
	}
	if strings.TrimSpace(p.Commands.Test) == "" {
		errs = append(errs, errors.New("commands.test is empty: nothing would verify the change"))
	}
	errs = append(errs, p.Complexity.validate()...)
	return errors.Join(errs...)
}

type Run struct {
	Command string `json:"command"`
	Output  string `json:"output"`
}

type SliceResult struct {
	ID      string   `json:"id"`
	Status  string   `json:"status"`
	Red     Run      `json:"red"`
	Green   Run      `json:"green"`
	Commits []string `json:"commits"`
}

type Suite struct {
	Command string `json:"command"`
	Passed  int    `json:"passed"`
	Failed  int    `json:"failed"`
	Skipped int    `json:"skipped"`
}

type Check struct {
	Command string `json:"command"`
	OK      bool   `json:"ok"`
}

type FeedbackResolution struct {
	Item       string `json:"item"`
	Resolution string `json:"resolution"`
}

type Impl struct {
	Slices            []SliceResult        `json:"slices"`
	FeedbackAddressed []FeedbackResolution `json:"feedback_addressed"`
	FullSuite         Suite                `json:"full_suite"`
	Lint              Check                `json:"lint"`
	Typecheck         Check                `json:"typecheck"`
	Pending           []string             `json:"pending"`
}

// Validate checks shape only; the orchestrator re-verifies every claim by running the checks itself.
func (im Impl) Validate() error {
	var errs []error
	if len(im.Slices) == 0 {
		errs = append(errs, errors.New("no slices reported"))
	}
	for _, s := range im.Slices {
		if s.Status != "done" && s.Status != "pending" {
			errs = append(errs, fmt.Errorf("slice %q: status must be done or pending, got %q", s.ID, s.Status))
		}
		if s.Status == "done" && strings.TrimSpace(s.Red.Output) == "" {
			errs = append(errs, fmt.Errorf("slice %q is done without red evidence", s.ID))
		}
	}
	return errors.Join(errs...)
}

// Severity values; critical and major block approval.
const (
	SeverityCritical = "critical"
	SeverityMajor    = "major"
	SeverityMinor    = "minor"
)

type Finding struct {
	Severity string `json:"severity"`
	Category string `json:"category"`
	File     string `json:"file"`
	Line     int    `json:"line"`
	Evidence string `json:"evidence"`
	Rule     string `json:"rule"`
	Message  string `json:"message"`
}

func (f Finding) Blocking() bool {
	return f.Severity == SeverityCritical || f.Severity == SeverityMajor
}

type Axis struct {
	Axis     string    `json:"axis"`
	Executed bool      `json:"executed"`
	Findings []Finding `json:"findings"`
}

type Review struct {
	Verdict string `json:"verdict"`
	Axes    []Axis `json:"axes"`
}

const (
	VerdictApprove          = "approve"
	VerdictChangesRequested = "changes_requested"
)

// RequiredAxes must all have run for a review to count.
var RequiredAxes = []string{"spec", "standards", "security"}

func (r Review) Validate() error {
	var errs []error
	seen := map[string]bool{}
	for _, a := range r.Axes {
		seen[a.Axis] = a.Executed
		for _, f := range a.Findings {
			switch f.Severity {
			case SeverityCritical, SeverityMajor, SeverityMinor:
			default:
				errs = append(errs, fmt.Errorf("axis %s: unknown severity %q", a.Axis, f.Severity))
			}
		}
	}
	for _, name := range RequiredAxes {
		if !seen[name] {
			errs = append(errs, fmt.Errorf("axis %s missing or not executed", name))
		}
	}
	return errors.Join(errs...)
}

// EffectiveVerdict ignores the verdict the agent wrote and derives it from the findings.
func (r Review) EffectiveVerdict() string {
	if len(r.Blocking()) > 0 {
		return VerdictChangesRequested
	}
	return VerdictApprove
}

// Blocking returns critical and major findings across all axes, in report order.
func (r Review) Blocking() []Finding {
	var out []Finding
	for _, a := range r.Axes {
		for _, f := range a.Findings {
			if f.Blocking() {
				out = append(out, f)
			}
		}
	}
	return out
}

// Dropped is a finding the review verifier could not confirm, with why.
type Dropped struct {
	File    string `json:"file"`
	Line    int    `json:"line"`
	Message string `json:"message"`
	Reason  string `json:"reason"`
}

// PRReview is the artifact of a PR/MR review: findings to post, never a verdict to act on.
type PRReview struct {
	Verdict   string    `json:"verdict"`   // comment | concerns (informative)
	Adherence string    `json:"adherence"` // matches | partial | diverges
	Summary   string    `json:"summary"`
	Axes      []Axis    `json:"axes"`
	Dropped   []Dropped `json:"dropped"`
	Learnings []string  `json:"learnings"`
}

var (
	validPRVerdicts = map[string]bool{"comment": true, "concerns": true}
	validAdherence  = map[string]bool{"matches": true, "partial": true, "diverges": true}
)

// Validate checks the shape; ValidateLimit checks the size against review.max_comments.
func (r PRReview) Validate() error {
	var errs []error
	if !validPRVerdicts[r.Verdict] {
		errs = append(errs, fmt.Errorf("verdict must be comment or concerns, got %q", r.Verdict))
	}
	if !validAdherence[r.Adherence] {
		errs = append(errs, fmt.Errorf("adherence must be matches, partial or diverges, got %q", r.Adherence))
	}
	if strings.TrimSpace(r.Summary) == "" {
		errs = append(errs, errors.New("summary is empty"))
	}
	if err := (Review{Axes: r.Axes}).Validate(); err != nil {
		errs = append(errs, err)
	}
	for _, a := range r.Axes {
		for _, f := range a.Findings {
			if f.File != "" && f.Line <= 0 {
				errs = append(errs, fmt.Errorf("axis %s: finding in %s needs a line > 0, got %d", a.Axis, f.File, f.Line))
			}
		}
	}
	for _, d := range r.Dropped {
		if d.File != "" && d.Line <= 0 {
			errs = append(errs, fmt.Errorf("dropped finding in %s needs a line > 0, got %d", d.File, d.Line))
		}
	}
	return errors.Join(errs...)
}

// ValidateLimit rejects more critical/major findings than max (review.max_comments).
func (r PRReview) ValidateLimit(max int) error {
	if n := len(r.Blocking()); n > max {
		return fmt.Errorf("%d critical/major findings exceed review.max_comments (%d)", n, max)
	}
	return nil
}

// Blocking returns critical and major findings across all axes, in report order.
func (r PRReview) Blocking() []Finding { return Review{Axes: r.Axes}.Blocking() }

type validator interface{ Validate() error }

// Read decodes path strictly (unknown fields rejected) and validates it.
func Read[T validator](path string) (T, error) {
	var v T
	data, err := os.ReadFile(path) //nolint:gosec // path is built by the orchestrator inside the run dir
	if err != nil {
		return v, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		return v, fmt.Errorf("%s: invalid JSON: %w", path, err)
	}
	if err := v.Validate(); err != nil {
		return v, fmt.Errorf("%s: %w", path, err)
	}
	return v, nil
}
