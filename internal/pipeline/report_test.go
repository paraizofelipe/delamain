package pipeline

import (
	"strings"
	"testing"

	"github.com/paraizo/delamain/internal/artifacts"
	"github.com/paraizo/delamain/internal/prompts"
)

func finding(sev, cat, file string, line int, msg string) artifacts.Finding {
	return artifacts.Finding{Severity: sev, Category: cat, File: file, Line: line, Message: msg, Rule: "regra " + msg, Evidence: "evid " + msg}
}

func approvedRun(reviews ...*artifacts.Review) *run {
	ru := &run{res: &Result{Outcome: OutcomeApproved, Iterations: len(reviews)}, issue: 82, maxIter: 5,
		plan: artifacts.Plan{Summary: "Resumo do plano."}}
	for i, r := range reviews {
		ru.res.History = append(ru.res.History, Iteration{N: i + 1, Verified: true, Review: r})
	}
	return ru
}

const nonBlockingHeading = "### Achados não bloqueantes\n\nNão impediram a aprovação; ficam para o revisor decidir.\n\n"

func TestNonBlockingFeedbackShouldKeepReportOrderAcrossAxes(t *testing.T) {
	r := artifacts.Review{Axes: []artifacts.Axis{
		{Axis: "spec", Findings: []artifacts.Finding{finding("major", "acceptance", "a.go", 1, "A"), finding("minor", "docs", "b.go", 2, "B")}},
		{Axis: "standards", Findings: []artifacts.Finding{finding("minor", "naming", "c.go", 3, "C")}},
		{Axis: "security", Findings: []artifacts.Finding{finding("critical", "secrets", "d.go", 4, "D"), finding("minor", "logging", "e.go", 5, "E")}},
	}}
	want := "- [minor · spec · docs] b.go:2 — B\n  regra: regra B\n  evidência: evid B\n" +
		"- [minor · standards · naming] c.go:3 — C\n  regra: regra C\n  evidência: evid C\n" +
		"- [minor · security · logging] e.go:5 — E\n  regra: regra E\n  evidência: evid E"
	if got := nonBlockingFeedback(r); got != want {
		t.Errorf("nonBlockingFeedback:\nesperado:\n%s\nveio:\n%s", want, got)
	}

	for name, r := range map[string]artifacts.Review{
		"só bloqueantes": {Axes: []artifacts.Axis{{Axis: "spec", Findings: []artifacts.Finding{finding("major", "acceptance", "a.go", 1, "A")}}}},
		"sem achados":    {Axes: []artifacts.Axis{{Axis: "spec"}, {Axis: "standards"}}},
		"vazia":          {},
	} {
		if got := nonBlockingFeedback(r); got != "" {
			t.Errorf("%s: deveria vir vazio, veio %q", name, got)
		}
	}
}

func TestReviewFeedbackShouldKeepListingOnlyBlockingFindings(t *testing.T) {
	r := artifacts.Review{Axes: []artifacts.Axis{
		{Axis: "spec", Findings: []artifacts.Finding{finding("major", "acceptance", "a.go", 1, "A"), finding("minor", "docs", "b.go", 2, "B")}},
		{Axis: "security", Findings: []artifacts.Finding{finding("critical", "secrets", "d.go", 4, "D")}},
	}}
	want := "- [major · spec · acceptance] a.go:1 — A\n  regra: regra A\n  evidência: evid A\n" +
		"- [critical · security · secrets] d.go:4 — D\n  regra: regra D\n  evidência: evid D"
	if got := reviewFeedback(r); got != want {
		t.Errorf("reviewFeedback mudou:\nesperado:\n%s\nveio:\n%s", want, got)
	}
}

func TestPRBodyShouldListNonBlockingFindingsOfTheLastReview(t *testing.T) {
	ru := approvedRun(&artifacts.Review{Verdict: "approve", Axes: []artifacts.Axis{
		{Axis: "spec", Findings: []artifacts.Finding{finding("minor", "docs", "b.go", 2, "doc faltando")}},
		{Axis: "standards", Findings: []artifacts.Finding{finding("minor", "naming", "c.go", 3, "nome vago")}},
	}})
	body := prBody(ru, false)
	assertBotPost(t, "prBody", body, false)
	if !strings.Contains(body, nonBlockingHeading) {
		t.Errorf("faltou a seção de não bloqueantes:\n%s", body)
	}
	for _, s := range []string{"b.go:2 — doc faltando", "c.go:3 — nome vago"} {
		if !strings.Contains(body, s) {
			t.Errorf("achado %q ausente:\n%s", s, body)
		}
	}
	if strings.Contains(body, "### Achados bloqueantes") {
		t.Errorf("sem bloqueantes, a seção bloqueante não deveria aparecer:\n%s", body)
	}
}

func TestPRBodyShouldOmitNonBlockingSectionWhenThereAreNone(t *testing.T) {
	for name, ru := range map[string]*run{
		"só major":    approvedRun(&artifacts.Review{Axes: []artifacts.Axis{{Axis: "spec", Findings: []artifacts.Finding{finding("major", "acceptance", "a.go", 1, "A")}}}}),
		"sem achados": approvedRun(&artifacts.Review{Axes: []artifacts.Axis{{Axis: "spec"}}}),
		// The last iteration never got reviewed: an older minor speaks about code that changed.
		"sem revisão": approvedRun(&artifacts.Review{Axes: []artifacts.Axis{{Axis: "spec", Findings: []artifacts.Finding{finding("minor", "docs", "b.go", 2, "antigo")}}}}, nil),
	} {
		body := prBody(ru, false)
		if strings.Contains(body, "Achados não bloqueantes") || strings.Contains(body, "antigo") {
			t.Errorf("%s: não deveria ter a seção de não bloqueantes:\n%s", name, body)
		}
		if name == "só major" && !strings.Contains(body, "### Achados bloqueantes") {
			t.Errorf("%s: a seção bloqueante sumiu:\n%s", name, body)
		}
	}
}

func TestPRBodyShouldUseOnlyTheLastIterationsReview(t *testing.T) {
	ru := approvedRun(
		&artifacts.Review{Axes: []artifacts.Axis{{Axis: "spec", Findings: []artifacts.Finding{finding("minor", "docs", "x.go", 1, "achado X")}}}},
		&artifacts.Review{Axes: []artifacts.Axis{{Axis: "spec", Findings: []artifacts.Finding{finding("minor", "docs", "y.go", 2, "achado Y")}}}},
	)
	body := prBody(ru, false)
	if !strings.Contains(body, "achado Y") || strings.Contains(body, "achado X") {
		t.Errorf("o corpo deveria citar só o achado da última revisão (Y):\n%s", body)
	}
}

func TestPRBodyShouldListBothSectionsInDraft(t *testing.T) {
	ru := approvedRun(&artifacts.Review{Axes: []artifacts.Axis{
		{Axis: "spec", Findings: []artifacts.Finding{finding("major", "acceptance", "a.go", 1, "achado M")}},
		{Axis: "standards", Findings: []artifacts.Finding{finding("minor", "naming", "c.go", 3, "achado m")}},
	}})
	ru.res.Outcome, ru.res.Reason = OutcomeExhausted, "limite de iterações"
	body := prBody(ru, true)
	assertBotPost(t, "prBody/draft", body, false)
	blocking, nonBlocking := strings.Index(body, "### Achados bloqueantes"), strings.Index(body, nonBlockingHeading)
	if blocking < 0 || nonBlocking < 0 || blocking > nonBlocking {
		t.Fatalf("esperava bloqueantes antes de não bloqueantes (%d, %d):\n%s", blocking, nonBlocking, body)
	}
	first, second := body[blocking:nonBlocking], body[nonBlocking:]
	if !strings.Contains(first, "achado M") || strings.Contains(first, "achado m") {
		t.Errorf("a seção bloqueante deveria ter só M:\n%s", first)
	}
	if !strings.Contains(second, "achado m") || strings.Contains(second, "achado M") {
		t.Errorf("a seção não bloqueante deveria ter só m:\n%s", second)
	}
}

func TestRenderReportShouldListNonBlockingFindingsPerIteration(t *testing.T) {
	res := Result{Outcome: OutcomeApproved, Iterations: 3, History: []Iteration{
		{N: 1, Review: &artifacts.Review{Axes: []artifacts.Axis{{Axis: "spec", Findings: []artifacts.Finding{finding("minor", "docs", "x.go", 1, "achado X")}}}}},
		{N: 2, Review: &artifacts.Review{Axes: []artifacts.Axis{
			{Axis: "spec", Findings: []artifacts.Finding{finding("major", "acceptance", "z.go", 3, "achado Z")}},
			{Axis: "standards", Findings: []artifacts.Finding{finding("minor", "naming", "y.go", 2, "achado Y")}},
		}}},
		{N: 3},
	}}
	report := renderReport(res, prompts.Issue{Ref: "#82", Title: "t"})
	_, rest, _ := strings.Cut(report, "## Iteração 1\n")
	it1, rest, _ := strings.Cut(rest, "## Iteração 2\n")
	it2, it3, _ := strings.Cut(rest, "## Iteração 3\n")

	const heading = "\n\n#### Achados não bloqueantes\n\n"
	if !strings.Contains(it1, "- spec: 1 achados"+heading+"- [minor · spec · docs] x.go:1 — achado X") {
		t.Errorf("iteração 1 deveria listar X depois da contagem por eixo:\n%s", it1)
	}
	if !strings.Contains(it2, "- standards: 1 achados"+heading+"- [minor · standards · naming] y.go:2 — achado Y") {
		t.Errorf("iteração 2 deveria listar Y depois da contagem por eixo:\n%s", it2)
	}
	if _, list, _ := strings.Cut(it2, heading); strings.Contains(list, "achado Z") || strings.Contains(it2, "achado X") {
		t.Errorf("iteração 2 deveria listar só o não bloqueante dela:\n%s", it2)
	}
	if strings.Contains(it3, "Achados não bloqueantes") {
		t.Errorf("iteração sem revisão não deveria ter a seção:\n%s", it3)
	}
	if strings.Contains(it1, "achado Y") {
		t.Errorf("iteração 1 não deveria citar o achado da 2:\n%s", it1)
	}
}

func TestPRBodyShouldShowMinorPassStatus(t *testing.T) {
	ru := approvedRun(&artifacts.Review{Axes: []artifacts.Axis{{Axis: "spec", Findings: []artifacts.Finding{finding("minor", "docs", "b.go", 2, "doc faltando")}}}})
	ru.res.MinorPass = "descartado: gates test falharam"
	body := prBody(ru, false)
	_, after, ok := strings.Cut(body, nonBlockingHeading)
	if !ok || !strings.Contains(after, "Passe de ajustes não bloqueantes: descartado: gates test falharam\n") {
		t.Errorf("o status do passe deveria vir depois da seção de não bloqueantes:\n%s", body)
	}

	ru.res.MinorPass = ""
	if body := prBody(ru, false); strings.Contains(body, "Passe de ajustes") {
		t.Errorf("sem passe, a linha não deveria aparecer:\n%s", body)
	}
}

func TestRenderReportShouldShowMinorPassStatus(t *testing.T) {
	minor := &artifacts.Review{Axes: []artifacts.Axis{{Axis: "spec", Findings: []artifacts.Finding{finding("minor", "docs", "x.go", 1, "achado X")}}}}
	res := Result{Outcome: OutcomeApproved, Iterations: 2, MinorPass: "aplicado", History: []Iteration{{N: 1, Review: minor}, {N: 2, Review: minor}}}
	report := renderReport(res, prompts.Issue{Ref: "#83", Title: "t"})
	it1, it2, _ := strings.Cut(report, "## Iteração 2\n")
	_, after, ok := strings.Cut(it2, "#### Achados não bloqueantes\n")
	if !ok || !strings.Contains(after, "\nPasse de ajustes não bloqueantes: aplicado\n") {
		t.Errorf("o status do passe deveria vir depois dos não bloqueantes da última iteração:\n%s", it2)
	}
	if strings.Contains(it1, "Passe de ajustes") {
		t.Errorf("o passe só segue a iteração aprovada, a última:\n%s", it1)
	}

	res.MinorPass = ""
	if report := renderReport(res, prompts.Issue{Ref: "#83", Title: "t"}); strings.Contains(report, "Passe de ajustes") {
		t.Errorf("sem passe, a linha não deveria aparecer:\n%s", report)
	}
}
