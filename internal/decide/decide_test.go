package decide

import (
	"context"
	"errors"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestPackageShouldImportOnlyStandardLibrary(t *testing.T) {
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	files := 0
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		files++
		f, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			// Standard library paths have no dot in their first element; modules always do.
			if first, _, _ := strings.Cut(path, "/"); strings.Contains(first, ".") {
				t.Errorf("%s imports %q: internal/decide only uses the standard library", name, path)
			}
		}
	}
	if files == 0 {
		t.Fatal("found no production .go file in internal/decide")
	}
}

func TestOffShouldReturnErrDisabledWithoutCallingAnything(t *testing.T) {
	var d Decider = Off{}
	res, err := d.Decide(context.Background(), Request{Nouls: map[string]Noul{"is_bug": {Instructions: "Bug?"}}})
	if !errors.Is(err, ErrDisabled) {
		t.Fatalf("err = %v, want ErrDisabled", err)
	}
	if !reflect.DeepEqual(res, Result{}) {
		t.Fatalf("result = %+v, want none", res)
	}
}
