package artifacts

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "a.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const validPlan = `{"summary":"fix add","kind":"bug","assumptions":[],
"slices":[{"id":"S1","behavior":"Add sums","seam":"calc.Add","files":["calc.go"],"tests":["calc_test.go::TestAdd"]}],
"acceptance":[{"criterion":"Add(2,3)=5","tests":["calc_test.go::TestAdd"],"flow":true}],
"regression_tests":["calc_test.go::TestAdd"],
"commands":{"test":"go test ./...","test_file":"","lint":"go vet ./...","typecheck":"","format_check":""},"complexity":{"components":"single_module","change":"localized","dependencies":[],"tests":["unit"],"rationale":"um arquivo"},"risks":[]}`

func TestReadPlanShouldAcceptContractCompliantPlan(t *testing.T) {
	p, err := Read[Plan](write(t, validPlan))
	if err != nil {
		t.Fatal(err)
	}
	if p.Commands.Test != "go test ./..." || len(p.Acceptance) != 1 || p.Complexity.Change != "localized" {
		t.Fatalf("decoded wrong: %+v", p)
	}
}

func TestReadPlanShouldRejectContractBreaks(t *testing.T) {
	cases := map[string]struct{ from, to, want string }{
		"criterion without test":    {`"tests":["calc_test.go::TestAdd"],"flow":true`, `"tests":[],"flow":true`, "has no test"},
		"bug without regression":    {`"regression_tests":["calc_test.go::TestAdd"]`, `"regression_tests":[]`, "regression_tests"},
		"no test command":           {`"test":"go test ./..."`, `"test":""`, "commands.test is empty"},
		"unknown kind":              {`"kind":"bug"`, `"kind":"chore"`, "kind must be"},
		"typo in field name":        {`"risks":[]`, `"riskz":[]`, "unknown field"},
		"duplicate slice":           {`"slices":[{"id":"S1"`, `"slices":[{"id":"S1","behavior":"x"},{"id":"S1"`, "repeated"},
		"unknown components":        {`"single_module"`, `"monolith"`, "complexity.components"},
		"unknown change":            {`"localized"`, `"tiny"`, "complexity.change"},
		"unknown dependency":        {`"dependencies":[]`, `"dependencies":["cache"]`, "complexity.dependencies"},
		"no test kind":              {`"tests":["unit"],"rationale"`, `"tests":[],"rationale"`, "complexity.tests"},
		"missing complexity":        {`"complexity":{"components":"single_module","change":"localized","dependencies":[],"tests":["unit"],"rationale":"um arquivo"},`, ``, "complexity.components"},
		"no acceptance":             {`"acceptance":[{"criterion":"Add(2,3)=5","tests":["calc_test.go::TestAdd"],"flow":true}]`, `"acceptance":[]`, "no acceptance"},
		"threat without mitigation": {`"risks":[]`, `"security":[{"threat":"um fork controla o head","mitigation":""}],"risks":[]`, "security 1: threat and mitigation"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			body := strings.Replace(validPlan, tc.from, tc.to, 1)
			if body == validPlan {
				t.Fatalf("fixture replacement did not apply for %q", name)
			}
			_, err := Read[Plan](write(t, body))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestReadPlanShouldAcceptAThreatModelAndAlsoItsAbsence(t *testing.T) {
	// security bounds the security review; a change that touches no untrusted input has none.
	if p, err := Read[Plan](write(t, validPlan)); err != nil || len(p.Security) != 0 {
		t.Fatalf("a plan without a threat model stays valid: %+v %v", p.Security, err)
	}
	body := strings.Replace(validPlan, `"risks":[]`,
		`"security":[{"threat":"quem abre o PR controla o head","mitigation":"checkout sem hooks nem filtros"}],"risks":[]`, 1)
	p, err := Read[Plan](write(t, body))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Security) != 1 || p.Security[0].Mitigation != "checkout sem hooks nem filtros" {
		t.Fatalf("threat model decoded wrong: %+v", p.Security)
	}
}

func TestReadShouldFailOnMissingOrMalformedFile(t *testing.T) {
	if _, err := Read[Plan](filepath.Join(t.TempDir(), "absent.json")); err == nil {
		t.Fatal("missing artifact means the step did not complete")
	}
	if _, err := Read[Plan](write(t, "DONE")); err == nil {
		t.Fatal("non-JSON artifact must fail")
	}
}

func TestReadImplShouldRequireRedEvidenceForDoneSlices(t *testing.T) {
	_, err := Read[Impl](write(t, `{"slices":[{"id":"S1","status":"done","red":{"command":"go test","output":""},"green":{},"commits":[]}],
"feedback_addressed":[],"full_suite":{},"lint":{},"typecheck":{},"pending":[]}`))
	if err == nil || !strings.Contains(err.Error(), "without red evidence") {
		t.Fatalf("TDD requires an observed red, got %v", err)
	}
}

func TestReadImplShouldAcceptPendingSliceWithoutEvidence(t *testing.T) {
	_, err := Read[Impl](write(t, `{"slices":[{"id":"S1","status":"pending","red":{},"green":{},"commits":[]}],
"feedback_addressed":[],"full_suite":{},"lint":{},"typecheck":{},"pending":["needs decision"]}`))
	if err != nil {
		t.Fatalf("pending slices are reported honestly, not rejected: %v", err)
	}
}

func review(findings string) string {
	return `{"verdict":"approve","axes":[{"axis":"spec","executed":true,"findings":` + findings + `},
{"axis":"standards","executed":true,"findings":[]},{"axis":"security","executed":true,"findings":[]}]}`
}

func TestEffectiveVerdictShouldOverrideAgentApprovalWhenBlockingFindingsExist(t *testing.T) {
	r, err := Read[Review](write(t, review(`[{"severity":"major","category":"faltando","file":"a.go","line":3,"evidence":"x","rule":"r","message":"m"}]`)))
	if err != nil {
		t.Fatal(err)
	}
	if r.EffectiveVerdict() != VerdictChangesRequested {
		t.Fatal("an agent must not approve its way past a major finding")
	}
}

func TestEffectiveVerdictShouldApproveWithOnlyMinorFindings(t *testing.T) {
	r, err := Read[Review](write(t, review(`[{"severity":"minor","category":"julgamento","file":"a.go","line":1,"evidence":"","rule":"","message":"name"}]`)))
	if err != nil {
		t.Fatal(err)
	}
	if r.EffectiveVerdict() != VerdictApprove || len(r.Blocking()) != 0 {
		t.Fatal("minor findings do not block")
	}
}

func TestReadReviewShouldRejectMissingAxisOrUnknownSeverity(t *testing.T) {
	missing := `{"verdict":"approve","axes":[{"axis":"spec","executed":true,"findings":[]}]}`
	if _, err := Read[Review](write(t, missing)); err == nil || !strings.Contains(err.Error(), "standards") {
		t.Fatalf("a review that skipped an axis is not a review: %v", err)
	}
	bad := review(`[{"severity":"blocker","category":"x","file":"a","line":1,"evidence":"","rule":"","message":""}]`)
	if _, err := Read[Review](write(t, bad)); err == nil {
		t.Fatal("unknown severity must be rejected, not silently treated as minor")
	}
}

const validPRReview = `{"verdict":"concerns","adherence":"partial","summary":"Falta tratar o erro do parse.",
"axes":[{"axis":"spec","executed":true,"findings":[{"severity":"major","category":"faltando","file":"a.go","line":3,"evidence":"x","rule":"r","message":"m"}]},
{"axis":"standards","executed":true,"findings":[]},{"axis":"security","executed":true,"findings":[]}],
"dropped":[{"file":"b.go","line":9,"message":"talvez nil","reason":"verificado via grep=nil: já tratado"}],
"learnings":["Os testes usam fakes em fakes_test.go"]}`

func TestPRReviewShouldAcceptAValidReview(t *testing.T) {
	r, err := Read[PRReview](write(t, validPRReview))
	if err != nil {
		t.Fatal(err)
	}
	if r.Adherence != "partial" || len(r.Dropped) != 1 || r.Dropped[0].Reason == "" || len(r.Learnings) != 1 {
		t.Fatalf("decoded wrong: %+v", r)
	}
	if err := r.ValidateLimit(1); err != nil {
		t.Fatalf("one blocking finding fits a limit of one: %v", err)
	}
}

func TestPRReviewShouldRequireEveryAxisExecuted(t *testing.T) {
	for name, body := range map[string]string{
		"missing axis": strings.Replace(validPRReview, `{"axis":"standards","executed":true,"findings":[]},`, ``, 1),
		"not executed": strings.Replace(validPRReview, `{"axis":"security","executed":true`, `{"axis":"security","executed":false`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if body == validPRReview {
				t.Fatal("fixture replacement did not apply")
			}
			if _, err := Read[PRReview](write(t, body)); err == nil {
				t.Fatal("a review that skipped an axis must not be posted")
			}
		})
	}
}

func TestPRReviewShouldRejectUnknownVerdictOrAdherence(t *testing.T) {
	cases := map[string]struct{ from, to, want string }{
		"approve is not a PR review verdict": {`"verdict":"concerns"`, `"verdict":"approve"`, "verdict"},
		"empty verdict":                      {`"verdict":"concerns"`, `"verdict":""`, "verdict"},
		"unknown adherence":                  {`"adherence":"partial"`, `"adherence":"mostly"`, "adherence"},
		"unknown severity":                   {`"severity":"major"`, `"severity":"blocker"`, "severity"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			body := strings.Replace(validPRReview, tc.from, tc.to, 1)
			if body == validPRReview {
				t.Fatal("fixture replacement did not apply")
			}
			if _, err := Read[PRReview](write(t, body)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestPRReviewShouldRequireASummary(t *testing.T) {
	body := strings.Replace(validPRReview, `"summary":"Falta tratar o erro do parse."`, `"summary":"  "`, 1)
	if _, err := Read[PRReview](write(t, body)); err == nil || !strings.Contains(err.Error(), "summary") {
		t.Fatalf("a review without a summary has nothing to post at PR level: %v", err)
	}
}

func TestPRReviewShouldRequirePositiveLineWhenFileIsSet(t *testing.T) {
	for name, tc := range map[string]struct{ from, to string }{
		"finding at line zero":  {`"file":"a.go","line":3`, `"file":"a.go","line":0`},
		"negative finding line": {`"file":"a.go","line":3`, `"file":"a.go","line":-2`},
		"dropped at line zero":  {`"file":"b.go","line":9`, `"file":"b.go","line":0`},
	} {
		t.Run(name, func(t *testing.T) {
			body := strings.Replace(validPRReview, tc.from, tc.to, 1)
			if body == validPRReview {
				t.Fatal("fixture replacement did not apply")
			}
			if _, err := Read[PRReview](write(t, body)); err == nil || !strings.Contains(err.Error(), "line") {
				t.Fatalf("a file without a valid line cannot be commented on: %v", err)
			}
		})
	}
	general := strings.Replace(validPRReview, `"file":"a.go","line":3`, `"file":"","line":0`, 1)
	if _, err := Read[PRReview](write(t, general)); err != nil {
		t.Fatalf("a finding without a file is PR-level and needs no line: %v", err)
	}
}

func TestPRReviewShouldRejectMoreBlockingFindingsThanMaxComments(t *testing.T) {
	two := strings.Replace(validPRReview, `{"axis":"security","executed":true,"findings":[]}`,
		`{"axis":"security","executed":true,"findings":[{"severity":"critical","category":"injecao","file":"c.go","line":1,"evidence":"x","rule":"A03","message":"m"},{"severity":"minor","category":"x","file":"c.go","line":2,"evidence":"","rule":"","message":"n"}]}`, 1)
	r, err := Read[PRReview](write(t, two))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.ValidateLimit(2); err != nil {
		t.Fatalf("minor findings do not count against max_comments: %v", err)
	}
	if err := r.ValidateLimit(1); err == nil || !strings.Contains(err.Error(), "max_comments") {
		t.Fatalf("two blocking findings exceed a limit of one: %v", err)
	}
}

func TestPRReviewShouldRejectUnknownJSONFields(t *testing.T) {
	body := strings.Replace(validPRReview, `"learnings":`, `"approve":true,"learnings":`, 1)
	if _, err := Read[PRReview](write(t, body)); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("an extra field means the agent wrote another format: %v", err)
	}
}
