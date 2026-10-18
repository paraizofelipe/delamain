package cli

import (
	"strings"
	"testing"
)

func TestCLIStdoutShouldUseEnglishForSupportedCommands(t *testing.T) {
	for _, args := range [][]string{
		{"prompts", "--help"},
		{"recover", "--help"},
		{"upgrade", "--help"},
	} {
		out, err := execute(t, args...)
		if err != nil {
			t.Fatalf("%q: %v", args, err)
		}
		for _, portuguese := range []string{"Valida, instala", "Finaliza runs", "Atualiza o delamain", "última release"} {
			if strings.Contains(out, portuguese) {
				t.Errorf("%q contains Portuguese interface text %q:\n%s", args, portuguese, out)
			}
		}
	}
}
