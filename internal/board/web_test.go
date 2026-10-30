package board

import (
	"io/fs"
	"path"
	"regexp"
	"strings"
	"testing"
)

// readAsset is an embedded file of the page, as the binary serves it.
func readAsset(t *testing.T, name string) string {
	t.Helper()
	b, err := fs.ReadFile(web, "web/"+name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

var (
	declaresEl     = regexp.MustCompile(`(?m)^\s*(function\s+el\s*\(|(const|let|var)\s+el\s*=)`)
	declaresDollar = regexp.MustCompile(`(?m)^\s*(function\s+\$\s*\(|(const|let|var)\s+\$\s*=)`)
)

func TestBoardScriptsShareTheCommonHelpers(t *testing.T) {
	common := readAsset(t, "common.js")
	if !declaresEl.MatchString(common) || !declaresDollar.MatchString(common) {
		t.Fatalf("common.js does not declare el and $:\n%s", common)
	}
	// A second top-level `const $` is a SyntaxError that takes the whole board down.
	board := readAsset(t, "board.js")
	if declaresEl.MatchString(board) || declaresDollar.MatchString(board) {
		t.Fatal("board.js still declares el or $: they live in common.js")
	}
}

var (
	scriptTag   = regexp.MustCompile(`(?i)<script\b[^>]*>`)
	scriptSrc   = regexp.MustCompile(`src="([^"]+)"`)
	styleTag    = regexp.MustCompile(`(?i)<style\b`)
	inlineStyle = regexp.MustCompile(`(?i)<[^>]*\sstyle\s*=`)
	inlineEvent = regexp.MustCompile(`(?i)<[^>]*\son[a-z]+\s*=`)
	styleUse    = regexp.MustCompile(`\.style\.`)
)

// The CSP is default-src 'self': inline script, inline style and markup built from strings either
// break the page or force the policy open.
func TestBoardPagesKeepToTheCSP(t *testing.T) {
	seen := map[string]bool{}
	err := fs.WalkDir(web, "web", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		name := path.Base(p)
		src := readAsset(t, strings.TrimPrefix(p, "web/"))
		switch path.Ext(p) {
		case ".html":
			seen[name] = true
			for _, tag := range scriptTag.FindAllString(src, -1) {
				if !strings.Contains(tag, "src=") {
					t.Errorf("%s: inline script %s", name, tag)
				}
			}
			for _, re := range []*regexp.Regexp{styleTag, inlineStyle, inlineEvent} {
				if m := re.FindString(src); m != "" {
					t.Errorf("%s: inline %q", name, m)
				}
			}
		case ".js":
			seen[name] = true
			for _, bad := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "setAttribute('style'", `setAttribute("style"`, "eval(", "new Function"} {
				if strings.Contains(src, bad) {
					t.Errorf("%s uses %s", name, bad)
				}
			}
			for _, loc := range styleUse.FindAllStringIndex(src, -1) {
				if !strings.HasPrefix(src[loc[1]:], "setProperty(") {
					t.Errorf("%s: style set other than by style.setProperty at byte %d", name, loc[0])
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"index.html", "common.js", "board.js", "report.html", "report.js", "report-daily.js", "report-top.js", "report-history.js"} {
		if !seen[want] {
			t.Errorf("%s was not checked: it is not in the embedded page", want)
		}
	}
}

func TestReportPageKeepsItsFiltersAndAsksTheAPI(t *testing.T) {
	page := readAsset(t, "report.html")
	for _, p := range []struct{ period, label string }{{"7d", "7d"}, {"30d", "30d"}, {"90d", "90d"}, {"all", "tudo"}} {
		// "tudo" is only the label: the API answers 400 to anything but 7d, 30d, 90d or all.
		button := regexp.MustCompile(`<button type="button" data-period="` + p.period + `"[^>]*>` + p.label + `</button>`)
		if !button.MatchString(page) {
			t.Errorf("report.html has no %q button for period %s", p.label, p.period)
		}
	}
	for _, want := range []string{`<select id="filter-kind">`, `<option value="issue">`, `<option value="review">`, `<option value="address">`, `<select id="filter-repo">`} {
		if !strings.Contains(page, want) {
			t.Errorf("report.html has no %s", want)
		}
	}

	js := readAsset(t, "report.js")
	for _, want := range []string{"'api/report?'", "'delamain-report-filters'", "kpis.previous", "rep.repos", "$('banner')"} {
		if !strings.Contains(js, want) {
			t.Errorf("report.js has no %s", want)
		}
	}
	// Storage may be off (private mode, file policy): reading or writing it must never stop the page.
	uses := regexp.MustCompile(`localStorage\.`).FindAllStringIndex(js, -1)
	if len(uses) == 0 {
		t.Fatal("report.js does not keep the filters in localStorage")
	}
	for _, at := range uses {
		before := js[:at[0]]
		if strings.LastIndex(before, "try {") <= strings.LastIndex(before, "} catch") {
			t.Errorf("report.js touches localStorage outside a try: ...%s", js[max(0, at[0]-60):at[1]])
		}
	}
}

// Each section of the report is its own file, so the issues that fill them do not touch report.js.
func TestReportPageHandsEachSectionItsContainer(t *testing.T) {
	js := readAsset(t, "report.js")
	for _, want := range []string{"window.reportSections", "page: (offset) => fetchReport(offset)"} {
		if !strings.Contains(js, want) {
			t.Errorf("report.js has no %s", want)
		}
	}
	for _, s := range []struct{ name, container string }{
		{"daily", "$('daily')"}, {"top", "document.querySelector('.report-top')"}, {"history", "$('history')"},
	} {
		call := regexp.MustCompile(`\['` + s.name + `',\s*` + regexp.QuoteMeta(s.container))
		if !call.MatchString(js) {
			t.Errorf("report.js does not hand the %s section its container %s", s.name, s.container)
		}
		section := readAsset(t, "report-"+s.name+".js")
		if !strings.Contains(section, "window.reportSections."+s.name+" = ") {
			t.Errorf("report-%s.js does not register itself:\n%s", s.name, section)
		}
		if s.name != "history" && !strings.Contains(section, "em construção") {
			t.Errorf("report-%s.js does not register its placeholder:\n%s", s.name, section)
		}
	}
	top := readAsset(t, "report-top.js")
	if !strings.Contains(top, "'#top-runs'") || !strings.Contains(top, "'#top-agents'") {
		t.Error("report-top.js leaves #top-runs or #top-agents empty")
	}
}

func TestReportHistoryShouldListEachRunWithItsOutcomeDurationAndCost(t *testing.T) {
	js := readAsset(t, "report-history.js")
	for _, want := range []string{"report.history || []", "el('details'", "'run '", "' · '", "padStart(", "fmtDuration(", "fmtUSD(", "'badge '", "'tag kind-'", "title: r.title, text: r.title"} {
		if !strings.Contains(js, want) {
			t.Errorf("report-history.js has no %s", want)
		}
	}
	for _, head := range []string{"'Run'", "'Resultado'", "'Linha do tempo dos agentes'", "'Duração'", "'Custo'"} {
		if !strings.Contains(js, head) {
			t.Errorf("report-history.js has no column %s", head)
		}
	}
	for _, o := range []struct{ outcome, label, tone string }{
		{"approved", "aprovada", "good"},
		{"exhausted", "máx. iterações", "warn"},
		{"needs_info", "precisa de info", "warn"},
		{"halted", "interrompida", "stop"},
	} {
		entry := regexp.MustCompile(`\b` + o.outcome + `: \['[^']+', '` + regexp.QuoteMeta(o.label) + `', '` + o.tone + `'\]`)
		if !entry.MatchString(js) {
			t.Errorf("report-history.js does not badge %s as %q (%s)", o.outcome, o.label, o.tone)
		}
	}
	// Only a known kind becomes a class name: anything else from the data would be a class of its own.
	if !strings.Contains(js, "['issue', 'review', 'address']") {
		t.Error("report-history.js does not check the kind before it becomes a class")
	}
}

func TestReportHistoryShouldDrawEachSessionAsASegmentOfTheRunTimeline(t *testing.T) {
	js := readAsset(t, "report-history.js")
	for _, want := range []string{"r.sessions || []", "style.setProperty('left'", "style.setProperty('width'", ">= 14", "'running'", "Date.now()", "ended_at", "toFixed(", "' · iter '"} {
		if !strings.Contains(js, want) {
			t.Errorf("report-history.js has no %s", want)
		}
	}
	css := readAsset(t, "report-history.css")
	if !regexp.MustCompile(`\.seg\s*\{[^}]*position:\s*absolute`).MatchString(css) {
		t.Error("report-history.css does not place each segment on the track")
	}
	if !regexp.MustCompile(`\.seg\.running\s*\{[^}]*repeating-linear-gradient`).MatchString(css) {
		t.Error("report-history.css does not texture a session still running")
	}
}

// The API sends each run's sessions in start order (internal/cli/board_report.go): the page keeps it.
func TestReportHistoryShouldKeepTheSessionsInTheOrderTheAPISent(t *testing.T) {
	if strings.Contains(readAsset(t, "report-history.js"), ".sort(") {
		t.Error("report-history.js reorders what the API already ordered")
	}
	if strings.Contains(readAsset(t, "report-history.js"), "em construção") {
		t.Error("report-history.js is still the placeholder")
	}
}

func TestReportHistoryShouldPaintOnlyKnownRolesOnTheTimeline(t *testing.T) {
	js := readAsset(t, "report-history.js")
	// Only a known role reaches var(--r-…): anything else would be a CSS token from the data.
	for _, want := range []string{"['planner', 'implementer', 'reviewer', 'pr-reviewer', 'responder']", "ROLES.includes(role) ? `var(--r-${role})` : 'var(--muted)'"} {
		if !strings.Contains(js, want) {
			t.Errorf("report-history.js has no %s", want)
		}
	}
}

func TestReportHistoryShouldDetailTheSessionsWithTheTimelineNumbers(t *testing.T) {
	js := readAsset(t, "report-history.js")
	for _, want := range []string{"el('details', { class: 'detail', open: true }", "'abrir no quadro'", "href: './'", "Math.max(0,", "el('dl', { class: 'kv' }"} {
		if !strings.Contains(js, want) {
			t.Errorf("report-history.js has no %s", want)
		}
	}
	for _, head := range []string{"'agente'", "'iteração'", "'modelo'", "'início'", "'duração'", "'tokens'", "'custo'"} {
		if !strings.Contains(js, head) {
			t.Errorf("report-history.js has no session column %s", head)
		}
	}
	for _, label := range []string{"'iterações'", "'tempo dos agentes'", "'tempo fora dos agentes'"} {
		if !strings.Contains(js, label) {
			t.Errorf("report-history.js has no %s in the run summary", label)
		}
	}
	// The table and the segment's tooltip say the same start: one function writes both.
	if n := len(regexp.MustCompile(`\bsince\(`).FindAllString(js, -1)); n < 3 {
		t.Errorf("report-history.js writes the relative start in %d places, want the definition, the tooltip and the table", n)
	}
}

func TestReportHistoryShouldAppendTheNextPageWithoutDuplicatingRuns(t *testing.T) {
	js := readAsset(t, "report-history.js")
	for _, want := range []string{"api.page(", "report.next_offset", "new Set(", "shown.has(", "shown.add(", "'carregar mais 20'", "list.append(", "class: 'error'"} {
		if !strings.Contains(js, want) {
			t.Errorf("report-history.js has no %s", want)
		}
	}
	if !regexp.MustCompile(`more\.hidden = (\w+\.)?next === null`).MatchString(js) {
		t.Error("report-history.js keeps 'carregar mais 20' after the last page")
	}
	// A page asked for under the old filters must not land in the new history.
	if !regexp.MustCompile(`const gen = \+\+generation`).MatchString(js) || !strings.Contains(js, "gen !== generation") {
		t.Error("report-history.js does not drop a page that comes back after the filters changed")
	}
	handler := regexp.MustCompile(`(?s)async function loadMore\(.*?\n  \}\n`).FindString(js)
	if handler == "" {
		t.Fatal("report-history.js has no loadMore")
	}
	if strings.Contains(handler, "replaceChildren(") {
		t.Error("report-history.js redraws the rows already on the page when it loads more")
	}
	// The section is called again on every filter change: a listener on window or document would pile up.
	if regexp.MustCompile(`\b(window|document)\.addEventListener`).MatchString(js) {
		t.Error("report-history.js listens on window or document")
	}
}

func TestReportHistoryShouldSayNoRunsInThePeriodWhenTheHistoryIsEmpty(t *testing.T) {
	js := readAsset(t, "report-history.js")
	if !strings.Contains(js, "el('p', { class: 'empty', text: 'nenhuma execução no período' })") {
		t.Error("report-history.js does not say there is no run in the period")
	}
	if strings.Contains(js, "em construção") {
		t.Error("report-history.js is still the placeholder")
	}
}

func TestReportHistoryShouldShowTheLegendOfTheRolesPresent(t *testing.T) {
	js := readAsset(t, "report-history.js")
	for _, want := range []string{"class: 'legend'", "v.legend.replaceChildren("} {
		if !strings.Contains(js, want) {
			t.Errorf("report-history.js has no %s", want)
		}
	}
	// The known roles in their fixed order, then any other one the data has, painted --muted by paint().
	if !regexp.MustCompile(`ROLES\.filter\(\(role\) => roles\.has\(role\)\)`).MatchString(js) || !strings.Contains(js, "!ROLES.includes(role)") {
		t.Error("report-history.js does not list the roles present in the fixed order, unknown ones last")
	}
	if !regexp.MustCompile(`\.legend\s*\{`).MatchString(readAsset(t, "report-history.css")) {
		t.Error("report-history.css does not style the legend")
	}
}
