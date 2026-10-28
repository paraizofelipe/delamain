package pipeline

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/paraizo/delamain/internal/artifacts"
)

// maxContextLines caps the code lines planContext injects, so refs cannot inflate every turn's
// prompt; a ref that does not fit whole is only listed.
const maxContextLines = 800

// planContext renders the excerpts the plan's slices point to in refs, read from the worktree
// as it is now, so the implementer starts from what the planner already read. Reads never leave
// the worktree; a ref that cannot be read is listed as not included and never fails the run.
func planContext(worktree string, plan artifacts.Plan) string {
	root, rootErr := os.OpenRoot(worktree)
	if rootErr == nil {
		defer root.Close()
	}
	var blocks []string
	left := maxContextLines
	for _, s := range plan.Slices {
		for _, ref := range s.Refs {
			first, last, body, reason := excerpt(root, worktree, ref)
			if reason != "" {
				blocks = append(blocks, fmt.Sprintf("- %s · %s:%s — não incluído: %s", s.ID, ref.Path, ref.Lines, reason))
				continue
			}
			if len(body) > left {
				blocks = append(blocks, fmt.Sprintf("- %s · %s:%d-%d — %s (não incluído: limite de contexto)", s.ID, ref.Path, first, last, ref.Why))
				continue
			}
			left -= len(body)
			blocks = append(blocks, fmt.Sprintf("### %s · %s:%d-%d — %s\n\n```\n%s\n```", s.ID, ref.Path, first, last, ref.Why,
				strings.Join(body, "\n")))
		}
	}
	return strings.Join(blocks, "\n\n")
}

// excerpt returns the lines ref points to, with the range's end clamped to the file's last
// line. The reason is empty on success.
func excerpt(root *os.Root, worktree string, ref artifacts.Ref) (first, last int, body []string, reason string) {
	first, last, ok := parseLines(ref.Lines)
	if !ok {
		return 0, 0, nil, "intervalo inválido"
	}
	lines, reason := readRef(root, worktree, ref.Path)
	if reason != "" {
		return 0, 0, nil, reason
	}
	if first > len(lines) {
		return 0, 0, nil, "intervalo além do fim do arquivo"
	}
	last = min(last, len(lines))
	return first, last, lines[first-1 : last], ""
}

// readRef reads the lines of name through root, which refuses anything outside the worktree,
// symlinks included. A nil root (the worktree could not be opened) reads nothing. The reason
// is empty on success.
func readRef(root *os.Root, worktree, name string) ([]string, string) {
	if !filepath.IsLocal(name) {
		return nil, "fora da worktree"
	}
	if root == nil {
		return nil, "ilegível"
	}
	raw, err := root.ReadFile(name)
	switch {
	case err == nil:
		return strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n"), ""
	case escapes(worktree, name):
		return nil, "fora da worktree"
	case errors.Is(err, fs.ErrNotExist):
		return nil, "arquivo ausente"
	default:
		return nil, "ilegível"
	}
}

// escapes reports whether name resolves, through symlinks, to outside the worktree. It only
// names why root refused a read; it never grants one.
func escapes(worktree, name string) bool {
	base, err := filepath.EvalSymlinks(worktree)
	if err != nil {
		return false
	}
	target, err := filepath.EvalSymlinks(filepath.Join(worktree, name))
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(base, target)
	return err == nil && !filepath.IsLocal(rel)
}

// parseLines reads an "A-B" range of 1-based, inclusive line numbers.
func parseLines(r string) (first, last int, ok bool) {
	a, z, found := strings.Cut(r, "-")
	first, errA := strconv.Atoi(a)
	last, errZ := strconv.Atoi(z)
	return first, last, found && errA == nil && errZ == nil && first >= 1 && first <= last
}
