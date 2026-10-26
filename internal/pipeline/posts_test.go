package pipeline

import (
	"regexp"
	"strings"
	"testing"

	"github.com/paraizo/delamain/internal/artifacts"
	"github.com/paraizo/delamain/internal/forge"
	"github.com/paraizo/delamain/internal/gitx"
)

var hiddenMarker = regexp.MustCompile(`<!-- delamain:run=\d+ kind=\w+ key=[0-9a-f]+ -->`)

// assertBotPost checks what every reader of a forge sees: the post says it came from automation
// with the robot emoji, and never names the tool. The hidden marker is the one allowed mention,
// since it is how delamain recognizes its own comments.
func assertBotPost(t *testing.T, name, body string, robotFirst bool) {
	t.Helper()
	visible := hiddenMarker.ReplaceAllString(body, "")
	if strings.Contains(strings.ToLower(visible), "delamain") {
		t.Errorf("%s: o texto visível cita o delamain:\n%s", name, visible)
	}
	if robotFirst && !strings.HasPrefix(body, "🤖") {
		t.Errorf("%s: deveria abrir com 🤖, começa com %q", name, firstLine(body, 60))
	}
	if !strings.Contains(body, "🤖") {
		t.Errorf("%s: não sinaliza que foi postado por agentes (sem 🤖)", name)
	}
}

func TestIssueCommentsShouldSignAsBotWithoutNamingTheTool(t *testing.T) {
	results := map[string]Result{
		"aprovado": {RunID: 1, Outcome: OutcomeApproved, Iterations: 2, PRURL: "https://github.com/o/r/pull/9", Commits: []string{"feat: x"}},
		"draft":    {RunID: 2, Outcome: OutcomeExhausted, Iterations: 4, PRURL: "https://github.com/o/r/pull/10", Draft: true, Reason: "limite de iterações"},
		"parado":   {RunID: 3, Outcome: OutcomeHalted, Reason: "entrega falhou: push rejeitado (branch local delamain/3-x preservada)"},
		"vazio":    {},
	}
	for name, res := range results {
		body := issueComment(res)
		if name == "parado" {
			// The reason is the error as it happened: a branch name may carry the tool's name,
			// and that is data, not the signature.
			body = strings.ReplaceAll(body, "delamain/3-x", "")
		}
		assertBotPost(t, "issueComment/"+name, body, true)
	}
}

func TestNeedsInfoCommentShouldSignAsBotAndKeepTheReadyLabel(t *testing.T) {
	for name, problems := range map[string][]string{
		"um problema": {"a task não tem descrição"},
		"vários":      {"a task não tem título", "depende da issue #4, que ainda está aberta"},
		"lista vazia": nil,
	} {
		body := needsInfoComment(problems, "paraizo:ready")
		assertBotPost(t, "needsInfoComment/"+name, body, true)
		if !strings.Contains(body, "`paraizo:ready`") {
			t.Errorf("%s: deveria dizer qual label recolocar, veio:\n%s", name, body)
		}
		for _, p := range problems {
			if !strings.Contains(body, "- "+p) {
				t.Errorf("%s: problema %q ausente:\n%s", name, p, body)
			}
		}
	}
}

func TestPRBodyFooterShouldTellOnlyTheIterationCount(t *testing.T) {
	for _, tc := range []struct {
		iterations int
		draft      bool
		footer     string
	}{
		{1, false, "---\n🤖 PR/MR aberto por agentes de IA após 1 iteração.\n"},
		{4, true, "---\n🤖 PR/MR aberto por agentes de IA após 4 iterações.\n"},
		{2, false, "---\n🤖 PR/MR aberto por agentes de IA após 2 iterações.\n"},
	} {
		ru := &run{res: &Result{RunID: 5371, Outcome: OutcomeApproved, Iterations: tc.iterations}, issue: 3, maxIter: 4,
			plan: artifacts.Plan{Summary: "Resumo do plano."}}
		if tc.draft {
			ru.res.Outcome, ru.res.Reason = OutcomeExhausted, "limite atingido"
		}
		body := prBody(ru, tc.draft)
		assertBotPost(t, "prBody", body, false)
		if !strings.HasSuffix(body, tc.footer) {
			t.Errorf("iterações=%d: rodapé esperado %q, corpo:\n%s", tc.iterations, tc.footer, body)
		}
		// The run id is an internal detail, and the merge note was dropped on purpose.
		if strings.Contains(body, "5371") || strings.Contains(strings.ToLower(body), "merge") {
			t.Errorf("iterações=%d: o corpo não deveria citar a run nem o merge:\n%s", tc.iterations, body)
		}
	}
}

func TestReviewPostsShouldSignAsBotKeepingTheMarker(t *testing.T) {
	pr := forge.PRDetail{Number: 9, URL: "https://github.com/o/r/pull/9", HeadSHA: "abc", BaseSHA: "def"}
	rv := artifacts.PRReview{Adherence: "partial", Summary: "Resumo.", Axes: []artifacts.Axis{{Axis: "spec", Findings: []artifacts.Finding{
		{Severity: "major", Category: "acceptance", File: "a.go", Line: 2, Message: "inline", Evidence: "x := 1"},
		{Severity: "minor", Category: "docs", Message: "sem arquivo"},
		{Severity: "minor", Category: "naming", File: "a.go", Line: 90, Message: "fora do diff"},
	}}}, Dropped: []artifacts.Dropped{{File: "a.go", Line: 3, Message: "m", Reason: "r"}}}
	lines := map[string][]gitx.LineRange{"a.go": {{Start: 1, End: 10}}}
	noExcerpt := func(string, int) string { return "" }

	o, _ := reviewOptions(7, pr, rv, lines, map[string]bool{}, noExcerpt)
	assertBotPost(t, "resumo", o.Summary, true)
	if !strings.Contains(o.Summary, "\n---\nFeita por agentes de IA, só comentários.") || strings.Contains(strings.ToLower(o.Summary), "merge") {
		t.Errorf("o rodapé do resumo deveria só dizer que a review é de agentes, sem citar merge:\n%s", o.Summary)
	}
	if len(o.Comments) != 1 {
		t.Fatalf("esperava 1 comentário inline, veio %d", len(o.Comments))
	}
	assertBotPost(t, "inline", o.Comments[0].Body, true)
	for name, body := range map[string]string{"resumo": o.Summary, "inline": o.Comments[0].Body} {
		if _, ok := forge.ParseMarker(body); !ok {
			t.Errorf("%s: o marcador oculto sumiu, o delamain não reconheceria o post:\n%s", name, body)
		}
	}

	// Summary already on the PR, inline still missing: the short body goes instead.
	filler, _ := reviewOptions(8, pr, rv, lines, map[string]bool{forge.Key("summary", pr.HeadSHA): true}, noExcerpt)
	assertBotPost(t, "complemento", filler.Summary, true)
}

func TestRepliesShouldSignAsBotInEveryFixState(t *testing.T) {
	a := artifacts.Response{ThreadID: "t1", Verdict: "valid", Reply: "Procede.", Fixed: true}
	for name, fix := range map[string]*fixRun{
		"sem correção":    nil,
		"enviada":         {state: fixPushed, sha: "c0ffee1234567"},
		"checks falharam": {state: fixChecksFailed, failed: []string{"test"}},
		"remoto mudou":    {state: fixRemoteMoved},
	} {
		assertBotPost(t, "reply/"+name, fix.replyBody(a), true)
	}
	for _, v := range []string{"invalid", "unclear"} {
		assertBotPost(t, "reply/"+v, replyBody(artifacts.Response{Verdict: v, Reply: "texto"}), true)
	}
}
