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
	touchesStorageOutsideTry(t, "report.js", js)
}

// Storage may be off (private mode, file policy): reading or writing it must never stop the page.
func touchesStorageOutsideTry(t *testing.T, name, js string) {
	t.Helper()
	uses := regexp.MustCompile(`localStorage\.`).FindAllStringIndex(js, -1)
	if len(uses) == 0 {
		t.Fatalf("%s keeps nothing in localStorage", name)
	}
	for _, at := range uses {
		before := js[:at[0]]
		if strings.LastIndex(before, "try {") <= strings.LastIndex(before, "} catch") {
			t.Errorf("%s touches localStorage outside a try: ...%s", name, js[max(0, at[0]-60):at[1]])
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
	for _, s := range []struct {
		name, container string
		placeholder     bool
	}{
		{"daily", "$('daily')", false}, {"top", "document.querySelector('.report-top')", false}, {"history", "$('history')", true},
	} {
		call := regexp.MustCompile(`\['` + s.name + `',\s*` + regexp.QuoteMeta(s.container))
		if !call.MatchString(js) {
			t.Errorf("report.js does not hand the %s section its container %s", s.name, s.container)
		}
		section := readAsset(t, "report-"+s.name+".js")
		if !strings.Contains(section, "window.reportSections."+s.name+" = ") {
			t.Errorf("report-%s.js does not register itself in window.reportSections", s.name)
		}
		if s.placeholder && !strings.Contains(section, "em construção") {
			t.Errorf("report-%s.js does not register its placeholder:\n%s", s.name, section)
		}
	}
	top := readAsset(t, "report-top.js")
	if !strings.Contains(top, "'#top-runs'") || !strings.Contains(top, "'#top-agents'") {
		t.Error("report-top.js leaves #top-runs or #top-agents empty")
	}
}

// The runs come ranked by cost from the API (internal/cli/board_report_test.go): the page keeps that order.
func TestReportTopShouldRankTheMostExpensiveRuns(t *testing.T) {
	js := readAsset(t, "report-top.js")
	for _, want := range []string{"report.top_runs", "'#top-runs'", "'run '", "fmtUSD(", "fmtTokens(", "fmtDuration(", "style.setProperty('--w'", "class: 'ref'"} {
		if !strings.Contains(js, want) {
			t.Errorf("report-top.js has no %s", want)
		}
	}
	if strings.Contains(js, ".sort(") {
		t.Error("report-top.js reorders what the API already ranked")
	}
}

func TestReportTopShouldBadgeEachOutcome(t *testing.T) {
	js := readAsset(t, "report-top.js")
	for _, o := range []struct{ outcome, label, tone string }{
		{"approved", "aprovada", "good"},
		{"exhausted", "máx. iterações", "warn"},
		{"needs_info", "precisa de info", "warn"},
		{"halted", "interrompida", "stop"},
	} {
		entry := regexp.MustCompile(`\b` + o.outcome + `: \['[^']+', '` + regexp.QuoteMeta(o.label) + `', '` + o.tone + `'\]`)
		if !entry.MatchString(js) {
			t.Errorf("report-top.js does not badge %s as %q (%s)", o.outcome, o.label, o.tone)
		}
	}
	if !strings.Contains(js, "'badge '") {
		t.Error("report-top.js does not give the outcome the badge class")
	}
}

func TestReportTopShouldSayNothingInThePeriodWhenAListIsEmpty(t *testing.T) {
	js := readAsset(t, "report-top.js")
	for _, want := range []string{"'nada no período'", "class: 'empty'"} {
		if !strings.Contains(js, want) {
			t.Errorf("report-top.js has no %s", want)
		}
	}
	// Each list falls back on its own: an empty top_sessions must not leave the old table up.
	for _, list := range []string{"report.top_runs || []", "report.top_roles || []", "report.top_sessions || []"} {
		if !strings.Contains(js, list) {
			t.Errorf("report-top.js does not read %s", list)
		}
	}
}

func TestReportTopShouldRankAgentsByRoleAndBySession(t *testing.T) {
	js := readAsset(t, "report-top.js")
	for _, want := range []string{"report.top_roles", "report.top_sessions", "'#top-agents'", "'padrão'", "'—'", "' · iter '", "style.setProperty('--role'"} {
		if !strings.Contains(js, want) {
			t.Errorf("report-top.js has no %s", want)
		}
	}
	// Only a known role reaches the CSS custom property: anything else would be a CSS token from the data.
	for _, role := range []string{"planner", "implementer", "reviewer", "pr-reviewer", "responder"} {
		if !strings.Contains(js, "'"+role+"'") {
			t.Errorf("report-top.js does not know the role %s", role)
		}
	}
	if strings.Contains(js, "em construção") {
		t.Error("report-top.js still leaves #top-agents as a placeholder")
	}
}

func TestReportTopShouldRememberTheAgentsMode(t *testing.T) {
	js := readAsset(t, "report-top.js")
	for _, want := range []string{"'delamain-report-top-mode'", "'por papel'", "'por sessão'", "aria-pressed"} {
		if !strings.Contains(js, want) {
			t.Errorf("report-top.js has no %s", want)
		}
	}
	touchesStorageOutsideTry(t, "report-top.js", js)
}

func TestReportTopStylesShouldStackAndHideSecondaryColumns(t *testing.T) {
	css := readAsset(t, "report-top.css")
	for _, q := range []struct{ width, rule string }{
		{"1100px", `\.report-top\s*\{[^}]*grid-template-columns:\s*1fr`},
		{"640px", `\.secondary\s*\{[^}]*display:\s*none`},
	} {
		media := regexp.MustCompile(`@media \(max-width: ` + q.width + `\)\s*\{(?:[^{}]*\{[^}]*\})*[^{}]*\}`).FindString(css)
		if !regexp.MustCompile(q.rule).MatchString(media) {
			t.Errorf("report-top.css has no %s under @media (max-width: %s):\n%s", q.rule, q.width, media)
		}
	}
	if !strings.Contains(css, "text-overflow: ellipsis") {
		t.Error("report-top.css does not truncate the long titles")
	}
	if !strings.Contains(readAsset(t, "report-top.js"), "'secondary'") {
		t.Error("report-top.js marks no column as secondary")
	}
}

// The cost of each day is a stacked bar, one segment per role, always in the same order and colour.
func TestReportDailyStacksTheRolesInTheirFixedOrder(t *testing.T) {
	js := readAsset(t, "report-daily.js")
	for _, want := range []string{
		"['planner', 'implementer', 'reviewer', 'pr-reviewer', 'responder']",
		"createElementNS('http://www.w3.org/2000/svg'",
		"report.daily", "by_role",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("report-daily.js has no %s", want)
		}
	}
	// A role that changes place from one day to the next makes the stacks unreadable.
	if strings.Contains(js, "sort(") {
		t.Error("report-daily.js reorders something: the roles keep their fixed order")
	}
	css := readAsset(t, "report-daily.css")
	for _, role := range []string{"planner", "implementer", "reviewer", "pr-reviewer", "responder"} {
		if !strings.Contains(css, "fill: var(--r-"+role+")") {
			t.Errorf("report-daily.css does not paint %s with var(--r-%s)", role, role)
		}
	}
	for _, want := range []string{"stroke: var(--panel)", "var(--line)"} {
		if !strings.Contains(css, want) {
			t.Errorf("report-daily.css has no %s", want)
		}
	}
}

func TestReportDailyLabelsTheChartWithThePeriodTotal(t *testing.T) {
	js := readAsset(t, "report-daily.js")
	for _, want := range []string{"'aria-label'", "Custo por dia: total ", "fmtUSD("} {
		if !strings.Contains(js, want) {
			t.Errorf("report-daily.js has no %s", want)
		}
	}
}

func TestReportDailyShowsTheEmptyStateWhenThePeriodHasNoCost(t *testing.T) {
	js := readAsset(t, "report-daily.js")
	if !regexp.MustCompile(`class: 'empty', text: 'sem custo no período'`).MatchString(js) {
		t.Error("report-daily.js has no empty state 'sem custo no período'")
	}
	if strings.Contains(js, "em construção") {
		t.Error("report-daily.js still shows its placeholder")
	}
}

// At 375px the chart shrinks with the panel instead of scrolling sideways.
func TestReportDailyFitsANarrowScreen(t *testing.T) {
	if js := readAsset(t, "report-daily.js"); !strings.Contains(js, "'viewBox'") {
		t.Error("report-daily.js does not give the svg a viewBox")
	}
	css := readAsset(t, "report-daily.css")
	if !regexp.MustCompile(`svg\s*\{[^}]*width: 100%`).MatchString(css) {
		t.Error("report-daily.css does not give the svg width: 100%")
	}
	if strings.Contains(css, "min-width") {
		t.Error("report-daily.css sets a min-width: the chart would scroll on a narrow screen")
	}
}

// Hovering a day shows its date, its total and what each role spent, with the role's colour.
func TestReportDailyTooltipShowsTheDayByRole(t *testing.T) {
	js := readAsset(t, "report-daily.js")
	for _, want := range []string{
		"el('div', { class: 'daily-tip'",
		".style.setProperty('left'", ".style.setProperty('top'",
		"dayLabel(", "fmtUSD(", "swatch",
		"class: 'hit'", "pointerleave",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("report-daily.js has no %s", want)
		}
	}
	css := readAsset(t, "report-daily.css")
	tip := regexp.MustCompile(`\.daily-tip\s*\{[^}]*\}`).FindString(css)
	for _, want := range []string{"position: absolute", "pointer-events: none"} {
		if !strings.Contains(tip, want) {
			t.Errorf("report-daily.css: .daily-tip has no %s", want)
		}
	}
}

// Every filter change redraws the section: the listeners go away with the nodes they sit on.
func TestReportDailyRedrawsWithoutPilingUpListeners(t *testing.T) {
	js := readAsset(t, "report-daily.js")
	if !strings.Contains(js, "container.replaceChildren(") {
		t.Error("report-daily.js does not replace the container's children on redraw")
	}
	listeners := regexp.MustCompile(`([\w.]+)\.addEventListener\('(\w+)'`).FindAllStringSubmatch(js, -1)
	events := map[string]bool{}
	for _, l := range listeners {
		events[l[2]] = true
		if l[1] == "window" || l[1] == "document" || strings.HasPrefix(l[1], "document.") {
			t.Errorf("report-daily.js listens to %s on %s: it outlives the redraw", l[2], l[1])
		}
	}
	for _, want := range []string{"pointermove", "pointerleave"} {
		if !events[want] {
			t.Errorf("report-daily.js does not listen to %s on the chart", want)
		}
	}
}
