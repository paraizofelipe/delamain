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
	for _, s := range []struct {
		name, container string
		placeholder     bool
	}{
		{"daily", "$('daily')", false}, {"top", "document.querySelector('.report-top')", true}, {"history", "$('history')", true},
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
