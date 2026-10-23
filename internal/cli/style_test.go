package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The watch TUI uses the v1 Charm stack: the v2 modules (charm.land/...) require lipgloss v2.
func TestGoModShouldUseBubbleteaV1WithLipglossV1(t *testing.T) {
	mod, err := os.ReadFile(filepath.Join("..", "..", "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"github.com/charmbracelet/lipgloss v1.", "github.com/charmbracelet/bubbletea v1.", "github.com/charmbracelet/bubbles v0."} {
		if !strings.Contains(string(mod), want) {
			t.Errorf("go.mod must require %q:\n%s", want, mod)
		}
	}
	for _, banned := range []string{"charm.land/", "bubbletea/v2", "lipgloss/v2", "bubbles/v2"} {
		if strings.Contains(string(mod), banned) {
			t.Errorf("go.mod must not require the v2 stack %q:\n%s", banned, mod)
		}
	}
}
