package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCLIStdoutShouldUseEnglishForSupportedCommands(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgFile, []byte("pipeline:\n  max_iterations: 4\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		args []string
		want []string
	}{
		{[]string{"--help"}, []string{"Autonomous development-agent orchestrator", "Available Commands:"}},
		{[]string{"version"}, []string{Version}},
		{[]string{"--config", cfgFile, "config"}, []string{"pipeline:", "max_iterations: 4"}},
		{[]string{"run", "--help"}, []string{"Runs a Markdown task", "Exit codes:"}},
		{[]string{"daemon", "--help"}, []string{"Polls open issues", "Only one instance runs"}},
		{[]string{"prompts", "--help"}, []string{"Validate, install and preview", "Available Commands:"}},
		{[]string{"recover", "--help"}, []string{"Finalize runs left"}},
		{[]string{"upgrade", "--help"}, []string{"Compares the installed version", "GITHUB_TOKEN"}},
		{[]string{"list", "--help"}, []string{"List active runs", "include finished runs"}},
		{[]string{"status", "--help"}, []string{"Show a run", "active agents and usage"}},
		{[]string{"logs", "--help"}, []string{"Interleaves the orchestrator", "follow the live run"}},
	} {
		out, err := execute(t, tc.args...)
		if err != nil {
			t.Fatalf("%q: %v", tc.args, err)
		}
		for _, want := range tc.want {
			if !strings.Contains(out, want) {
				t.Errorf("%q missing English interface text %q:\n%s", tc.args, want, out)
			}
		}
	}
}
