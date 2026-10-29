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
	for _, want := range []string{"index.html", "common.js", "board.js"} {
		if !seen[want] {
			t.Errorf("%s was not checked: it is not in the embedded page", want)
		}
	}
}
