package cli

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// portuguese matches accented letters and common pt-BR words: user-facing text must be English.
var portuguese = regexp.MustCompile(`(?i)[áàâãéêíóôõúç]|\b(nenhum|nenhuma|não|sim|de|para|sem|uma|um|ou|mas|rode|informe|exige|caminho|arquivo|vivo|morto|finalizada|recuperada|fechada|segue|mostra|lista|se|ligado|formato|inclui|instala|entrega|consumo)\b`)

func TestCLISourcesShouldNotContainPortugueseStrings(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, filepath.Join("..", "upgrade", "upgrade.go"))
	fset := token.NewFileSet()
	var found []string
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.ImportSpec:
				return false
			case *ast.BasicLit:
				if n.Kind != token.STRING {
					return true
				}
				s, err := strconv.Unquote(n.Value)
				if err != nil {
					s = n.Value
				}
				if portuguese.MatchString(s) {
					pos := fset.Position(n.Pos())
					found = append(found, fmt.Sprintf("%s:%d: %s", pos.Filename, pos.Line, n.Value))
				}
			}
			return true
		})
	}
	if len(found) > 0 {
		t.Errorf("user-facing strings must be English, found %d in Portuguese:\n%s", len(found), strings.Join(found, "\n"))
	}
}
