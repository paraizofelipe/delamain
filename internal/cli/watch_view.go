package cli

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// selectedBackground highlights the selected run's row.
var selectedBackground = lipgloss.Color("236")

func (m watchModel) View() string {
	if m.width <= 0 || m.height <= 0 {
		return ""
	}
	left := min(max(m.width*40/100, 56), 72, m.width)
	body := m.height - 2 // header and footer lines
	list := m.listLines(left - 4)
	lines := append([]string{""}, m.panel("runs", list, left, min(len(list)+2, body))...)
	return strings.Join(lines, "\n")
}

// panel draws a rounded box of width × height cells with title in its top border; the lines
// that do not fit are cut.
func (m watchModel) panel(title string, lines []string, width, height int) []string {
	inner := width - 4
	title = ansi.Truncate(title, width-5, "…")
	out := []string{"╭ " + title + " " + strings.Repeat("─", max(width-5-lipgloss.Width(title), 0)) + "╮"}
	for i := range height - 2 {
		l := ""
		if i < len(lines) {
			l = lines[i]
		}
		out = append(out, "│ "+fit(l, inner)+" │")
	}
	return append(out, "╰"+strings.Repeat("─", max(width-2, 0))+"╯")
}

// fit cuts s to w cells with "…" and pads it to exactly w.
func fit(s string, w int) string {
	s = ansi.Truncate(s, w, "…")
	return s + strings.Repeat(" ", max(w-lipgloss.Width(s), 0))
}

// listLines are the rows of the runs panel: column header, the active runs, then the finished.
func (m watchModel) listLines(inner int) []string {
	st, runs := m.st, m.runs()
	if len(runs) == 0 {
		return []string{paint(st.muted, "no runs yet: label an issue with delamain:ready")}
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
	lines := []string{paint(st.header, header+"TITLE")}
	section := func(name string, n int, look lipgloss.Style) {
		lines = append(lines, paint(look.Bold(true), fmt.Sprintf("%s · %d", name, n)))
	}
	section("ACTIVE", len(m.data.active), st.info)
	for i, v := range runs {
		if i == len(m.data.active) {
			section("FINISHED", len(m.data.done), st.muted)
		}
		selected := v.ID == m.selectedID
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
