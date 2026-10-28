package pipeline

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/paraizo/delamain/internal/artifacts"
)

// planContext renders the excerpts the plan's slices point to in refs, read from the worktree
// as it is now, so the implementer starts from what the planner already read.
func planContext(worktree string, plan artifacts.Plan) string {
	var blocks []string
	for _, s := range plan.Slices {
		for _, ref := range s.Refs {
			first, last, _ := parseLines(ref.Lines)
			raw, _ := os.ReadFile(filepath.Join(worktree, ref.Path))
			lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
			blocks = append(blocks, fmt.Sprintf("### %s · %s:%d-%d — %s\n\n```\n%s\n```", s.ID, ref.Path, first, last, ref.Why,
				strings.Join(lines[first-1:last], "\n")))
		}
	}
	return strings.Join(blocks, "\n\n")
}

// parseLines reads an "A-B" range of 1-based, inclusive line numbers.
func parseLines(r string) (first, last int, ok bool) {
	a, z, found := strings.Cut(r, "-")
	first, errA := strconv.Atoi(a)
	last, errZ := strconv.Atoi(z)
	return first, last, found && errA == nil && errZ == nil && first >= 1 && first <= last
}
