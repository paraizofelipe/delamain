package cli

import (
	"io"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
)

// styles are the looks of `status`; a renderer without colors (not a terminal, NO_COLOR) makes
// every one of them print the text unchanged.
type styles struct {
	label   lipgloss.Style // labels of the first block
	heading lipgloss.Style // section titles: WORKTREES, AGENTS
	header  lipgloss.Style // column headers
}

func newStyles(r *lipgloss.Renderer) styles {
	bold := r.NewStyle().Bold(true)
	return styles{label: bold, heading: bold, header: bold}
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
