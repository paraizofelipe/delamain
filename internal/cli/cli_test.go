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

func TestPromptsLintShouldPassForShippedPrompts(t *testing.T) {
	out, err := execute(t, "--config", filepath.Join(t.TempDir(), "none.yaml"), "prompts", "lint")
	if err != nil || !strings.Contains(out, "prompts válidos") {
		t.Fatalf("got %q %v", out, err)
	}
}

func TestPromptsRenderShouldUseOverrideDirWithoutRebuild(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "stages"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stages", "handshake.md"), []byte("MEU HANDSHAKE {{.ArtifactPath}}"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DELAMAIN_PROMPTS_DIR", dir)
	out, err := execute(t, "--config", filepath.Join(t.TempDir(), "none.yaml"), "prompts", "render", "handshake")
	if err != nil || !strings.HasPrefix(out, "MEU HANDSHAKE /tmp/run/") {
		t.Fatalf("edited .md must take effect: %q %v", out, err)
	}
}

func TestPromptsRenderShouldFailForUnknownStage(t *testing.T) {
	if _, err := execute(t, "--config", filepath.Join(t.TempDir(), "none.yaml"), "prompts", "render", "deploy"); err == nil {
		t.Fatal("unknown stage must fail")
	}
}

func TestPromptsShouldFailWhenOverrideDirIsMissing(t *testing.T) {
	t.Setenv("DELAMAIN_PROMPTS_DIR", filepath.Join(t.TempDir(), "absent"))
	if _, err := execute(t, "--config", filepath.Join(t.TempDir(), "none.yaml"), "prompts", "lint"); err == nil {
		t.Fatal("a typo in prompts_dir must not silently fall back to defaults")
	}
}
