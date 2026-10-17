package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func execute(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	root := NewRoot()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

func TestConfigShouldPrintEffectiveValuesIncludingEnvOverrides(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgFile, []byte("pipeline:\n  max_iterations: 4\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DELAMAIN_MAX_PARALLEL", "2")
	out, err := execute(t, "--config", cfgFile, "config")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"maxiterations: 4", "maxparallel: 2", "model: openai-codex/gpt-5.6-terra"} {
		if !strings.Contains(strings.ToLower(strings.ReplaceAll(out, "_", "")), want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestConfigShouldFailOnInvalidConfiguration(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgFile, []byte("pipeline:\n  max_iterations: 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := execute(t, "--config", cfgFile, "config"); err == nil {
		t.Fatal("an invalid config must not be printed as if it were usable")
	}
}

func TestVersionShouldPrintBuildVersion(t *testing.T) {
	out, err := execute(t, "version")
	if err != nil || strings.TrimSpace(out) != Version {
		t.Fatalf("got %q %v", out, err)
	}
}
