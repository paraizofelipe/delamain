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
"commands":{"test":"go test ./...","test_file":"","lint":"go vet ./...","typecheck":"","format_check":""},"risks":[]}`

func TestReadPlanShouldAcceptContractCompliantPlan(t *testing.T) {
	p, err := Read[Plan](write(t, validPlan))
	if err != nil {
		t.Fatal(err)
	}
	if p.Commands.Test != "go test ./..." || len(p.Acceptance) != 1 {
		t.Fatalf("decoded wrong: %+v", p)
	}
}

func TestReadPlanShouldRejectContractBreaks(t *testing.T) {
	cases := map[string]struct{ from, to, want string }{
		"criterion without test": {`"tests":["calc_test.go::TestAdd"],"flow":true`, `"tests":[],"flow":true`, "has no test"},
		"bug without regression": {`"regression_tests":["calc_test.go::TestAdd"]`, `"regression_tests":[]`, "regression_tests"},
		"no test command":        {`"test":"go test ./..."`, `"test":""`, "commands.test is empty"},
		"unknown kind":           {`"kind":"bug"`, `"kind":"chore"`, "kind must be"},
		"typo in field name":     {`"risks":[]`, `"riskz":[]`, "unknown field"},
		"duplicate slice":        {`"slices":[{"id":"S1"`, `"slices":[{"id":"S1","behavior":"x"},{"id":"S1"`, "repeated"},
		"no acceptance":          {`"acceptance":[{"criterion":"Add(2,3)=5","tests":["calc_test.go::TestAdd"],"flow":true}]`, `"acceptance":[]`, "no acceptance"},
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
