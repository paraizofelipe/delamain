package pipeline

import (
	"strings"
	"testing"

	"github.com/paraizo/delamain/internal/artifacts"
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
