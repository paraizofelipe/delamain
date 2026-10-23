package cli

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/paraizo/delamain/internal/budget"
	"github.com/paraizo/delamain/internal/config"
)

// selectedBackground highlights the selected run's row.
var selectedBackground = lipgloss.Color("236")

func (m watchModel) View() string {
	if m.width <= 0 || m.height <= 0 {
		return ""
	}
	if m.layout == layoutTooSmall {
		return m.tooSmall()
	}
	f := m.frame()
	p := f.panels
	statusTitle, logsTitle := "status", "logs"
	v, ok := m.selected()
	if ok {
		statusTitle, logsTitle = fmt.Sprintf("status · run %d", v.ID), fmt.Sprintf("logs · run %d", v.ID)
		logsTitle += " · " + m.logs.state(m.st, v)
	}
	listLook, logsLook := m.st.info, m.st.muted
	if m.focusLogs {
		listLook, logsLook = logsLook, listLook
	}
	parts := []placed{
		{p.list, panel("runs", scroll(f.list, f.sel, p.list.h-2), p.list.w, p.list.h, listLook)},
		{p.status, panel(statusTitle, f.status, p.status.w, p.status.h, m.st.muted)},
		{p.logs, panel(logsTitle, m.logs.view(m.st, v, p.logs.w-4, p.logs.h-2), p.logs.w, p.logs.h, logsLook)},
	}
	lines := append([]string{m.header()}, compose(max(m.height-1-len(f.footer), 0), parts...)...)
	lines = append(lines, f.footer...)
	return strings.Join(lines[:min(len(lines), m.height)], "\n")
}

// watchFrame is the screen of a layout: the panels' rectangles and the content they are sized by.
type watchFrame struct {
	panels watchPanels
	footer []string
	list   []string
	sel    int // the selected run's line of list
	status []string
}

// frame lays the screen of the current layout out.
func (m watchModel) frame() watchFrame {
	f := watchFrame{footer: []string{m.footer()}}
	f.list, f.sel = m.listLines(leftWidth(m.width) - 4)
	f.status = m.statusLines()
	must := len(f.list)
	if len(m.data.active) > 0 || len(m.data.done) > 0 {
		must = 2 + len(m.data.active) // column header, ACTIVE
		if len(m.data.done) > 0 {
			must++ // FINISHED
		}
	}
	f.panels = panelsFor(m.layout, m.width, m.height, panelNeeds{
		footer: len(f.footer), list: len(f.list), listMust: must, status: len(f.status), zoom: m.zoom,
	})
	return f
}

// tooSmall is the screen of a terminal smaller than every layout: why, in the middle.
func (m watchModel) tooSmall() string {
	msg := fmt.Sprintf("terminal too small: %d×%d (minimum 40×12)", m.width, m.height)
	text := strings.Split(ansi.Wrap(msg, m.width, ""), "\n")
	text = text[:min(len(text), m.height)]
	out := make([]string, m.height)
	top := (m.height - len(text)) / 2
	for i, l := range text {
		l = ansi.Truncate(l, m.width, "")
		out[top+i] = strings.Repeat(" ", (m.width-lipgloss.Width(l))/2) + paint(m.st.warn, l)
	}
	return strings.Join(out, "\n")
}

// scroll keeps the column header and as many rows as fit in n lines around the selected one.
func scroll(lines []string, sel, n int) []string {
	if len(lines) <= n || n < 2 || sel < n {
		return lines // the panel cuts what does not fit
	}
	return append([]string{lines[0]}, lines[sel-n+2:sel+1]...)
}

// footer shows the keys, or why the last reload failed until one succeeds.
func (m watchModel) footer() string {
	st := m.st
	if m.err != nil {
		return paint(st.bad, ansi.Truncate("error: "+clean(m.err.Error()), m.width, "…"))
	}
	zoom := "full screen"
	if m.zoom {
		zoom = "exit full screen"
	}
	var keys []string
	for _, k := range [][2]string{{"↑↓", "navigate"}, {"tab", "focus"}, {"1-6", "source"}, {"f", "follow"}, {"z", zoom}, {"q", "quit"}} {
		keys = append(keys, paint(st.info.Bold(true), k[0])+paint(st.muted, " "+k[1]))
	}
	return ansi.Truncate(strings.Join(keys, "  "), m.width, "…")
}

// header is the one line above the panels: version, daemon, run counts, the day's spend and the
// clock at the right edge.
func (m watchModel) header() string {
	st, today := m.st, m.data.today
	daemon := paint(st.muted, "daemon ○")
	if m.data.daemon {
		daemon = paint(st.ok, "daemon ●")
	}
	spend := "today " + budget.Format(today.Day, today.Mode)
	if today.Daily > 0 {
		spend += " of " + budget.Format(today.Daily, today.Mode)
	}
	look := map[budget.Level]lipgloss.Style{budget.OK: st.muted, budget.Soft: st.warn, budget.Hard: st.bad}[today.Level]
	left := strings.Join([]string{
		paint(st.magenta.Bold(true), "delamain"), paint(st.muted, "watch · "+Version), daemon,
		paint(st.info.Bold(true), fmt.Sprintf("%d active · %d finished", len(m.data.active), len(m.data.done))),
		paint(look, spend),
	}, "  ")
	clock := m.opts.now().Format("15:04:05")
	if m.width < len(clock)+1 {
		return ansi.Truncate(left, m.width, "")
	}
	left = ansi.Truncate(left, m.width-len(clock)-1, "…")
	return left + strings.Repeat(" ", m.width-lipgloss.Width(left)-len(clock)) + paint(st.muted, clock)
}

// panel draws a rounded box of exactly width × height cells with title in its top border and
// the border in look; the lines that do not fit are cut.
func panel(title string, lines []string, width, height int, look lipgloss.Style) []string {
	if height <= 0 {
		return nil
	}
	if width < 4 {
		out := make([]string, height)
		for i := range out {
			out[i] = strings.Repeat(" ", max(width, 0))
		}
		return out
	}
	inner := width - 4
	title = ansi.Truncate(" "+title+" ", width-2, "…")
	out := []string{paint(look, "╭") + title + paint(look, strings.Repeat("─", width-2-lipgloss.Width(title))+"╮")}
	if height == 1 {
		return out
	}
	for i := range height - 2 {
		l := ""
		if i < len(lines) {
			l = lines[i]
		}
		out = append(out, paint(look, "│")+" "+fit(l, inner)+" "+paint(look, "│"))
	}
	return append(out, paint(look, "╰"+strings.Repeat("─", max(width-2, 0))+"╯"))
}

// fit cuts s to w cells with "…" and pads it to exactly w.
func fit(s string, w int) string {
	s = ansi.Truncate(s, w, "…")
	return s + strings.Repeat(" ", max(w-lipgloss.Width(s), 0))
}

// listLines are the rows of the runs panel: column header, the active runs, then the finished;
// sel is the selected run's line.
func (m watchModel) listLines(inner int) (lines []string, sel int) {
	st, runs := m.st, m.runs()
	if len(runs) == 0 {
		return []string{paint(st.muted, "no runs yet: label an issue with delamain:ready")}, 0
	}
	cols := [5]int{len("RUN"), len("ISSUE"), len("STATE"), len("CPLX"), len("ITER")}
	for _, v := range runs {
		for i, s := range watchColumns(v) {
			cols[i] = max(cols[i], lipgloss.Width(s))
		}
	}
	titleW := inner - 2
	for _, w := range cols {
		titleW -= w + 1
	}
	pad := func(s string, w int) string { return s + strings.Repeat(" ", max(w-lipgloss.Width(s), 0)) }
	header := "  "
	for i, h := range []string{"RUN", "ISSUE", "STATE", "CPLX", "ITER"} {
		header += pad(h, cols[i]) + " "
	}
	lines = []string{paint(st.header, header+"TITLE")}
	section := func(name string, n int, look lipgloss.Style) {
		lines = append(lines, paint(look.Bold(true), fmt.Sprintf("%s · %d", name, n)))
	}
	section("ACTIVE", len(m.data.active), st.info)
	for i, v := range runs {
		if i == len(m.data.active) {
			section("FINISHED", len(m.data.done), st.muted)
		}
		selected := v.ID == m.selectedID
		if selected {
			sel = len(lines)
		}
		look := func(s lipgloss.Style) lipgloss.Style {
			if selected {
				return s.Background(selectedBackground)
			}
			return s
		}
		marker, run := " ", st.none
		if selected {
			marker, run = "▸", st.none.Bold(true)
		}
		text := watchColumns(v)
		state := st.runStateStyle(v.State)
		title := st.none
		if v.terminal {
			state, title = st.resultStyle(text[2]), st.muted
		}
		cells := []string{
			paint(look(st.info), marker),
			paint(look(run), pad(text[0], cols[0])), paint(look(st.info), pad(text[1], cols[1])),
			paint(look(state), pad(text[2], cols[2])), paint(look(v.complexityStyle(st)), pad(text[3], cols[3])),
			paint(look(st.none), pad(text[4], cols[4])),
		}
		row := strings.Join(cells, paint(look(st.none), " ")) + paint(look(st.none), " ")
		row += paint(look(title), pad(ansi.Truncate(v.Title, max(titleW, 0), "…"), max(titleW, 0)))
		lines = append(lines, row)
	}
	return lines, sel
}

// statusLines describe the selected run as `status` does, with "-" for what it does not have yet.
func (m watchModel) statusLines() []string {
	v, ok := m.selected()
	if !ok {
		return nil
	}
	st := m.st
	field := func(label string, value cell) string {
		return paint(st.label, fmt.Sprintf("%-14s", label)) + value.styled
	}
	dash := func(text string, look lipgloss.Style) cell {
		if text == "" {
			return plain("-")
		}
		return styled(look, text)
	}
	spend, branch, result := "-", "-", plain("-")
	if m.data.statusOf == v.ID && m.data.status.Budget.Summary != "" {
		spend = m.data.status.Budget.Summary
	}
	if v.Branch != "" {
		branch = fmt.Sprintf("%s (base %s)", v.Branch, v.Base)
	}
	if v.Result != "" {
		result = resultCell(st, v.Result)
	}
	lines := []string{
		field("RUN", join(plain(fmt.Sprintf("%d · ", v.ID)), styled(st.info, v.Issue), plain(" "+v.Title))),
		field("KIND", plain(orDash(v.Kind))),
		field("HARNESS", plain(orDash(v.Harness))),
		field("STATE", styled(st.runStateStyle(v.State), v.State)),
		field("INTERACTIONS", plain(v.iter())),
		field("COMPLEXITY", dash(v.Complexity, st.complexityStyle(v.Complexity))),
		field("BRANCH", plain(branch)),
		field("OWNER", plain(v.owner())),
		field("BUDGET", plain(spend)),
		field("PR/MR", dash(v.PR, st.info.Underline(true))),
		field("RESULT", result),
		paint(st.heading, "AGENTS"),
	}
	if len(v.Agents) == 0 {
		return append(lines, paint(st.muted, "no agents yet"))
	}
	var roleW, stateW, tookW int
	took := make([]string, len(v.Agents))
	for i, a := range v.Agents {
		end := m.opts.now()
		if a.EndedAt != nil {
			end = *a.EndedAt
		}
		took[i] = end.Sub(a.StartedAt).Round(time.Second).String()
		roleW, stateW, tookW = max(roleW, lipgloss.Width(a.Role)), max(stateW, lipgloss.Width(a.State)), max(tookW, len(took[i]))
	}
	for i, a := range v.Agents {
		lines = append(lines, fmt.Sprintf("%s  %s  %-*s  %s · %.2f",
			paint(st.sourceStyle(a.Role), a.Role)+strings.Repeat(" ", roleW-lipgloss.Width(a.Role)),
			paint(st.agentStateStyle(a.State), a.State)+strings.Repeat(" ", stateW-lipgloss.Width(a.State)),
			tookW, took[i], budget.Format(float64(a.Tokens), config.BudgetTokens), a.CostUSD))
	}
	return lines
}

// watchColumns are the texts of a run's RUN, ISSUE, STATE, CPLX and ITER cells; a finished
// run shows its outcome as STATE.
func watchColumns(v RunView) [5]string {
	state := v.State
	if v.terminal {
		state, _, _ = strings.Cut(v.Result, ":")
		state = orDash(state)
	}
	return [5]string{strconv.FormatInt(v.ID, 10), v.Issue, state, v.complexity(), v.iter()}
}

// cleanData drops the control characters of every text read from the store: an issue or PR
// title is written by anyone, and ESC sequences in it would rewrite the screen or the clipboard.
func cleanData(d watchData) watchData {
	d.active, d.done = cleanViews(d.active), cleanViews(d.done)
	d.status.Run = cleanViews([]RunView{d.status.Run})[0]
	d.status.Budget.Summary = clean(d.status.Budget.Summary)
	return d
}

func cleanViews(vs []RunView) []RunView {
	out := make([]RunView, len(vs))
	for i, v := range vs {
		for _, f := range []*string{&v.Kind, &v.Repo, &v.Issue, &v.Title, &v.State, &v.Harness, &v.Complexity, &v.Branch, &v.Base, &v.Worktree, &v.Workspace, &v.PR, &v.Result} {
			*f = clean(*f)
		}
		v.Agents = append([]AgentView{}, v.Agents...)
		for j := range v.Agents {
			v.Agents[j].Role, v.Agents[j].State = clean(v.Agents[j].Role), clean(v.Agents[j].State)
		}
		out[i] = v
	}
	return out
}

// clean drops control characters (C0, DEL and C1, ESC included) and invalid UTF-8; tabs and line
// breaks become spaces.
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\t' || r == '\n' || r == '\r':
			return ' '
		case r == utf8.RuneError || unicode.IsControl(r):
			return -1
		}
		return r
	}, s)
}
