package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGoModShouldNotRequireBubbletea(t *testing.T) {
	mod, err := os.ReadFile(filepath.Join("..", "..", "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(mod), "github.com/charmbracelet/lipgloss ") {
		t.Errorf("status styles come from lipgloss:\n%s", mod)
	}
	if strings.Contains(string(mod), "charmbracelet/bubbletea") {
		t.Errorf("styling a table needs no TUI framework:\n%s", mod)
	}
}
