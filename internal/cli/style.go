package cli

import (
	"io"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"

	"github.com/paraizo/delamain/internal/complexity"
	"github.com/paraizo/delamain/internal/pipeline"
	"github.com/paraizo/delamain/internal/store"
)

// styles are the looks of `status`, `ls` and `logs`; a renderer without colors (not a terminal,
// NO_COLOR) makes every one of them print the text unchanged.
type styles struct {
	label    lipgloss.Style // labels of the first block
	heading  lipgloss.Style // section titles: WORKTREES, AGENTS
	header   lipgloss.Style // column headers
	none     lipgloss.Style // values with no meaning to color
	ok       lipgloss.Style
	warn     lipgloss.Style
	bad      lipgloss.Style
	muted    lipgloss.Style
	info     lipgloss.Style
	magenta  lipgloss.Style
	blue     lipgloss.Style
	slot     lipgloss.Style // a place taken in the queue grid of `watch`
	slotWait lipgloss.Style // a place in that grid whose item waits for its blockers
}

func newStyles(r *lipgloss.Renderer) styles {
	bold := r.NewStyle().Bold(true)
	return styles{
		label: bold, heading: bold, header: bold, none: r.NewStyle(),
		ok:       r.NewStyle().Foreground(lipgloss.Color("2")),
		warn:     r.NewStyle().Foreground(lipgloss.Color("3")),
		bad:      r.NewStyle().Foreground(lipgloss.Color("1")),
		muted:    r.NewStyle().Faint(true),
		info:     r.NewStyle().Foreground(lipgloss.Color("6")),
		magenta:  r.NewStyle().Foreground(lipgloss.Color("5")),
		blue:     r.NewStyle().Foreground(lipgloss.Color("4")),
		slot:     r.NewStyle().Background(lipgloss.Color("62")).Foreground(lipgloss.Color("231")).Bold(true),
		slotWait: r.NewStyle().Background(lipgloss.Color("3")).Foreground(lipgloss.Color("0")).Bold(true),
	}
}

// sourceStyle is the bold color of a log source in `logs`; an unknown source is only bold.
func (st styles) sourceStyle(source string) lipgloss.Style {
	color, ok := map[string]lipgloss.Style{
		"delamain": st.magenta, "checks": st.warn, "planner": st.blue, "implementer": st.info, "reviewer": st.ok,
	}[source]
	if !ok {
		color = st.none
	}
	return color.Bold(true)
}

func (st styles) complexityStyle(level string) lipgloss.Style {
	switch complexity.Level(level) {
	case complexity.Low:
		return st.ok
	case complexity.Medium:
		return st.warn
	case complexity.High:
		return st.bad
	}
	return st.none
}

// resultStyle colors the outcome a run recorded, the part of RESULT before ": <reason>".
func (st styles) resultStyle(outcome string) lipgloss.Style {
	switch pipeline.Outcome(outcome) {
	case pipeline.OutcomeApproved, pipeline.OutcomeReviewed, pipeline.OutcomeAnswered:
		return st.ok
	case pipeline.OutcomeExhausted, pipeline.OutcomeNeedsInfo:
		return st.warn
	case pipeline.OutcomeHalted:
		return st.bad
	case pipeline.OutcomeSkipped:
		return st.muted
	}
	return st.none
}

// runStateStyle colors where a run stands: finished, waiting on someone, stopped or moving.
func (st styles) runStateStyle(state string) lipgloss.Style {
	switch store.RunState(state) {
	case store.StateRecorded:
		return st.ok
	case store.StateNeedsInfo:
		return st.warn
	case store.StateHalted:
		return st.bad
	}
	return st.info
}

func (st styles) agentStateStyle(state string) lipgloss.Style {
	switch store.AgentState(state) {
	case store.AgentDone:
		return st.ok
	case store.AgentRunning:
		return st.info
	case store.AgentFailed:
		return st.bad
	}
	return st.none
}

// cell is a table cell: its text sets the column width, styled is what is printed.
type cell struct{ text, styled string }

func plain(text string) cell { return cell{text, text} }

// styled renders only short known tokens: lipgloss rewrites tabs and pads multi-line text.
func styled(st lipgloss.Style, text string) cell {
	if text == "" {
		return plain(text)
	}
	return cell{text, st.Render(text)}
}

func join(cs ...cell) cell {
	var c cell
	for _, p := range cs {
		c.text += p.text
		c.styled += p.styled
	}
	return c
}

// writeTable lays rows out as tabwriter.NewWriter(w, 0, 0, 2, ' ', 0) would with the plain
// text, so escapes never shift a column: widths count runes and the last cell is not padded.
func writeTable(w io.Writer, rows [][]cell) error {
	var widths []int
	for _, r := range rows {
		for i, c := range r[:len(r)-1] {
			if i == len(widths) {
				widths = append(widths, 0)
			}
			widths[i] = max(widths[i], utf8.RuneCountInString(c.text)+2)
		}
	}
	var b strings.Builder
	for _, r := range rows {
		for i, c := range r {
			b.WriteString(c.styled)
			if i < len(r)-1 {
				b.WriteString(strings.Repeat(" ", widths[i]-utf8.RuneCountInString(c.text)))
			}
		}
		b.WriteByte('\n')
	}
	_, err := io.WriteString(w, b.String())
	return err
}
