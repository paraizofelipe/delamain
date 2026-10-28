package artifacts

import (
	"os"
	"path/filepath"
	"slices"
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

func TestReadMinorImplShouldAcceptAPassThatDeclinesEveryItem(t *testing.T) {
	_, err := Read[MinorImpl](write(t, `{"slices":[],
"feedback_addressed":[{"item":"log impreciso","resolution":"não aplicado: o log já cita o id"}],
"full_suite":{},"lint":{},"typecheck":{},"pending":[]}`))
	if err != nil {
		t.Fatalf("declining every item is a valid minor pass: %v", err)
	}
}

func TestReadMinorImplShouldAcceptADoneSliceWithoutRedEvidence(t *testing.T) {
	_, err := Read[MinorImpl](write(t, `{"slices":[{"id":"S1","status":"done","red":{},"green":{},"commits":["abc1234"]}],
"feedback_addressed":[],"full_suite":{},"lint":{},"typecheck":{},"pending":[]}`))
	if err != nil {
		t.Fatalf("a fix that changes no behavior has no red to show: %v", err)
	}
}

func TestReadMinorImplShouldRejectAnEmptyReport(t *testing.T) {
	_, err := Read[MinorImpl](write(t, `{"slices":[],"feedback_addressed":[],"full_suite":{},"lint":{},"typecheck":{},"pending":[]}`))
	if err == nil {
		t.Fatal("a pass that reports nothing must be rejected")
	}
}

func TestReadMinorImplShouldRejectAnUnknownSliceStatus(t *testing.T) {
	_, err := Read[MinorImpl](write(t, `{"slices":[{"id":"S1","status":"maybe","red":{},"green":{},"commits":[]}],
"feedback_addressed":[],"full_suite":{},"lint":{},"typecheck":{},"pending":[]}`))
	if err == nil || !strings.Contains(err.Error(), "status must be done or pending") {
		t.Fatalf("slice statuses stay checked, got %v", err)
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

// validResponses answers the pending threads t1 and t2.
const validResponses = `{"responses":[
{"thread_id":"t1","verdict":"invalid","reply":"O erro já é tratado em parse.go:12.","fixed":false},
{"thread_id":"t2","verdict":"valid","reply":"Faz sentido: vale renomear.","fixed":false}],
"learnings":["Erros de parse são tratados em parse.go"]}`

func TestResponsesShouldAcceptOneReplyPerPendingThread(t *testing.T) {
	r, err := Read[Responses](write(t, validResponses))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.ValidatePending([]string{"t1", "t2"}); err != nil {
		t.Fatalf("one reply per pending thread is the contract: %v", err)
	}
	if len(r.Responses) != 2 || r.Responses[0].Verdict != "invalid" || len(r.Learnings) != 1 {
		t.Fatalf("decoded wrong: %+v", r)
	}
}

func TestResponsesShouldRejectAnUnknownThread(t *testing.T) {
	r, err := Read[Responses](write(t, validResponses))
	if err != nil {
		t.Fatal(err)
	}
	err = r.ValidatePending([]string{"t1"})
	if err == nil || !strings.Contains(err.Error(), "t2") {
		t.Fatalf("a reply to a thread that was not pending must be rejected: %v", err)
	}
}

func TestResponsesShouldRejectAPendingThreadWithoutReply(t *testing.T) {
	r, err := Read[Responses](write(t, validResponses))
	if err != nil {
		t.Fatal(err)
	}
	err = r.ValidatePending([]string{"t1", "t2", "t3"})
	if err == nil || !strings.Contains(err.Error(), "t3") {
		t.Fatalf("every pending thread needs its reply: %v", err)
	}
	if err := (Responses{}).ValidatePending(nil); err != nil {
		t.Fatalf("nothing pending, nothing to answer: %v", err)
	}
}

func TestResponsesShouldRejectTwoRepliesToTheSameThread(t *testing.T) {
	_, err := Read[Responses](write(t, strings.Replace(validResponses, `"thread_id":"t2"`, `"thread_id":"t1"`, 1)))
	if err == nil || !strings.Contains(err.Error(), "t1") {
		t.Fatalf("two replies to one thread must be rejected: %v", err)
	}
}

func TestResponsesShouldRejectAnInvalidVerdict(t *testing.T) {
	for _, v := range []string{"agree", "", "VALID"} {
		_, err := Read[Responses](write(t, strings.Replace(validResponses, `"verdict":"invalid"`, `"verdict":"`+v+`"`, 1)))
		if err == nil || !strings.Contains(err.Error(), "verdict") {
			t.Errorf("verdict %q must be rejected: %v", v, err)
		}
	}
	for _, v := range []string{"valid", "invalid", "unclear"} {
		if _, err := Read[Responses](write(t, strings.Replace(validResponses, `"verdict":"invalid"`, `"verdict":"`+v+`"`, 1))); err != nil {
			t.Errorf("verdict %q is part of the contract: %v", v, err)
		}
	}
}

func TestResponsesShouldRejectAnEmptyOrTooLongReply(t *testing.T) {
	const reply = `"reply":"O erro já é tratado em parse.go:12."`
	for name, tc := range map[string]struct {
		reply string
		ok    bool
	}{
		"empty":            {`"reply":""`, false},
		"blank":            {`"reply":"   "`, false},
		"2001 runes":       {`"reply":"` + strings.Repeat("é", 2001) + `"`, false},
		"exactly 2000 ok":  {`"reply":"` + strings.Repeat("é", 2000) + `"`, true},
		"a single rune ok": {`"reply":"x"`, true},
	} {
		_, err := Read[Responses](write(t, strings.Replace(validResponses, reply, tc.reply, 1)))
		if tc.ok != (err == nil) {
			t.Errorf("%s: ok=%v, got %v", name, tc.ok, err)
		}
		if !tc.ok && (err == nil || !strings.Contains(err.Error(), "reply")) {
			t.Errorf("%s: the error names the reply: %v", name, err)
		}
	}
}

func TestResponsesShouldRejectFixedTrue(t *testing.T) {
	_, err := Read[Responses](write(t, strings.Replace(validResponses, `"fixed":false`, `"fixed":true`, 1)))
	if err == nil || !strings.Contains(err.Error(), "fixed") {
		t.Fatalf("nothing is fixed while addressing only replies: %v", err)
	}
}

func TestFixResponsesShouldAcceptFixedOnlyOnValidThreads(t *testing.T) {
	fixedValid := strings.Replace(validResponses, `"verdict":"valid","reply":"Faz sentido: vale renomear.","fixed":false`,
		`"verdict":"valid","reply":"Faz sentido: vale renomear.","fixed":true`, 1)
	r, err := Read[FixResponses](write(t, fixedValid))
	if err != nil {
		t.Fatalf("a valid thread may be fixed when the responder can fix: %v", err)
	}
	if !r.Responses[1].Fixed || r.Responses[0].Fixed {
		t.Fatalf("fixed must be read as written: %+v", r.Responses)
	}
	for _, v := range []string{"invalid", "unclear"} {
		other := strings.Replace(validResponses, `"verdict":"invalid","reply":"O erro já é tratado em parse.go:12.","fixed":false`,
			`"verdict":"`+v+`","reply":"O erro já é tratado em parse.go:12.","fixed":true`, 1)
		if _, err := Read[FixResponses](write(t, other)); err == nil || !strings.Contains(err.Error(), "fixed") {
			t.Errorf("an %s thread is never fixed: %v", v, err)
		}
	}
	if _, err := Read[FixResponses](write(t, strings.Replace(validResponses, `"verdict":"invalid"`, `"verdict":"maybe"`, 1))); err == nil {
		t.Error("the shape rules of Responses still apply")
	}
}

func TestResponsesShouldRejectAReplyCarryingADelamainMarker(t *testing.T) {
	for _, m := range []string{"<!-- delamain:run=1 kind=reply key=abcdef012345 -->", "<!--delamain:run=1 kind=reply key=zz -->", "<!-- DELAMAIN:run=1 -->"} {
		_, err := Read[Responses](write(t, strings.Replace(validResponses, "Faz sentido: vale renomear.", "Faz sentido. "+m, 1)))
		if err == nil || !strings.Contains(err.Error(), "marker") {
			t.Errorf("a reply must not carry its own marker %q: %v", m, err)
		}
	}
}

// planWithRefs puts refsJSON as the refs of slice S1 in validPlan.
func planWithRefs(t *testing.T, refsJSON string) string {
	t.Helper()
	from := `"tests":["calc_test.go::TestAdd"]}]`
	body := strings.Replace(validPlan, from, `"tests":["calc_test.go::TestAdd"],"refs":`+refsJSON+`}]`, 1)
	if body == validPlan {
		t.Fatal("fixture replacement did not apply")
	}
	return body
}

func TestPlanValidateShouldAcceptValidRefs(t *testing.T) {
	body := planWithRefs(t, `[{"path":"calc.go","lines":"120-185","why":"Add e seus chamadores"},
{"path":"a/../calc_test.go","lines":"7-7","why":"linha única"}]`)
	if _, err := Read[Plan](write(t, body)); err != nil {
		t.Fatalf("refs with local paths and A-B lines are valid: %v", err)
	}
}

func TestPlanShouldDecodeRefsFromJSON(t *testing.T) {
	body := planWithRefs(t, `[{"path":"internal/calc/calc.go","lines":"120-185","why":"Add e seus chamadores"}]`)
	p, err := Read[Plan](write(t, body))
	if err != nil {
		t.Fatal(err)
	}
	want := []Ref{{Path: "internal/calc/calc.go", Lines: "120-185", Why: "Add e seus chamadores"}}
	if !slices.Equal(p.Slices[0].Refs, want) {
		t.Fatalf("refs decoded wrong: got %+v, want %+v", p.Slices[0].Refs, want)
	}
}

func TestPlanValidateShouldAcceptSliceWithoutRefs(t *testing.T) {
	// Plans written before refs existed carry none and must stay valid.
	p, err := Read[Plan](write(t, validPlan))
	if err != nil {
		t.Fatalf("a slice without refs stays valid: %v", err)
	}
	if len(p.Slices[0].Refs) != 0 {
		t.Fatalf("no refs in the JSON, got %+v", p.Slices[0].Refs)
	}
}

func TestPlanValidateShouldRejectAbsoluteRefPath(t *testing.T) {
	body := planWithRefs(t, `[{"path":"/home/u/.ssh/id_ed25519","lines":"1-5","why":"chave"}]`)
	_, err := Read[Plan](write(t, body))
	if err == nil || !strings.Contains(err.Error(), `slice "S1" ref 1: path`) {
		t.Fatalf("an absolute path reads outside the worktree, got %v", err)
	}
}

func TestPlanValidateShouldRejectRefPathEscapingWorktree(t *testing.T) {
	body := planWithRefs(t, `[{"path":"calc.go","lines":"1-5","why":"ok"},
{"path":"a/../../.config/gh/hosts.yml","lines":"1-5","why":"token"}]`)
	_, err := Read[Plan](write(t, body))
	if err == nil || !strings.Contains(err.Error(), `slice "S1" ref 2: path`) {
		t.Fatalf("a path that climbs out of the worktree must be rejected, got %v", err)
	}
}

func TestPlanValidateShouldRejectEmptyRefPath(t *testing.T) {
	body := planWithRefs(t, `[{"path":"","lines":"1-5","why":"nada"}]`)
	_, err := Read[Plan](write(t, body))
	if err == nil || !strings.Contains(err.Error(), `slice "S1" ref 1: path`) {
		t.Fatalf("an empty path points nowhere, got %v", err)
	}
}

func TestPlanValidateShouldRejectMalformedLines(t *testing.T) {
	cases := map[string]string{
		"empty":            "",
		"single number":    "10",
		"not numbers":      "a-b",
		"starts at zero":   "0-5",
		"reversed":         "20-10",
		"spaces":           " 1-5",
		"signed":           "+1-5",
		"overflows an int": "1-99999999999999999999",
	}
	for name, lines := range cases {
		t.Run(name, func(t *testing.T) {
			body := planWithRefs(t, `[{"path":"calc.go","lines":"`+lines+`","why":"Add"}]`)
			_, err := Read[Plan](write(t, body))
			if err == nil || !strings.Contains(err.Error(), `slice "S1" ref 1: lines`) {
				t.Fatalf("lines %q is not A-B with 1 <= A <= B, got %v", lines, err)
			}
		})
	}
}
