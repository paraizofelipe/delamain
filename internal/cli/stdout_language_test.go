package cli

import (
	"strings"
	"testing"
)

func TestCLIStdoutShouldUseEnglishForSupportedCommands(t *testing.T) {
	for _, args := range [][]string{
		{"run", "--help"},
		{"daemon", "--help"},
		{"prompts", "--help"},
		{"recover", "--help"},
		{"upgrade", "--help"},
		{"list", "--help"},
		{"status", "--help"},
		{"logs", "--help"},
	} {
		out, err := execute(t, args...)
		if err != nil {
			t.Fatalf("%q: %v", args, err)
		}
		for _, portuguese := range []string{"Executa uma task", "Processa continuamente", "Valida, instala", "Finaliza runs", "Atualiza o delamain", "última release", "Lista as runs", "Mostra uma run", "Mostra os logs"} {
			if strings.Contains(out, portuguese) {
				t.Errorf("%q contains Portuguese interface text %q:\n%s", args, portuguese, out)
			}
		}
	}
}
