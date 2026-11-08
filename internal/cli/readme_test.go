package cli

import (
	"os"
	"strings"
	"testing"
)

// The README is checked by its key terms only: a rewritten sentence must not break the test.
func TestReadmeShouldDocumentDependenciesBetweenIssues(t *testing.T) {
	b, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatalf("the README documents the dependencies between issues: %v", err)
	}
	readme := string(b)
	_, section, ok := strings.Cut(readme, "### Dependências entre issues")
	if !ok {
		t.Fatalf("the README has no section on the dependencies between issues:\n%s", readme)
	}
	if next := strings.Index(section, "\n#"); next >= 0 {
		section = section[:next]
	}
	for _, want := range []string{
		"depends_on: #", "bloco de código", // the canonical marker and where it does not count
		"depende de", "blocked by", "bloqueado por", "início da linha", // the legacy forms
		"is_blocked_by", "GitLab Free", // the native relations
		"formas antigas são ignoradas", "sempre se somam", // the precedence
		"delamain run --issue", "needs-info", // a run refuses the issue
		"WAITING", "queue list", // the queue keeps it waiting
		"épico", "ciclo", // the good practices
	} {
		if !strings.Contains(section, want) {
			t.Errorf("the section on the dependencies between issues should mention %q", want)
		}
	}
}

func TestReadmeShouldDocumentOptionalDecisions(t *testing.T) {
	b, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatalf("the README documents the optional decisions: %v", err)
	}
	readme := string(b)
	_, section, ok := strings.Cut(readme, "## Decisões (opcional)")
	if !ok {
		t.Fatalf("the README has no section on the optional decisions:\n%s", readme)
	}
	if next := strings.Index(section, "\n## "); next >= 0 {
		section = section[:next]
	}
	for _, want := range []string{
		"api_key_env: DELAMAIN_DECISIONS_API_KEY", "typesafe/jev-1.14", // the config block and its defaults
		"`off`", "`shadow`", "`on`", "triage", // the modes and the only point built today
		"dedicada", "limite de gasto", "nunca vai no YAML", // the key
		"DELAMAIN_DECISIONS_ENABLED", "DELAMAIN_DECISIONS_MODEL", // the overrides
		"decision point=triage",                           // the calibration
		"delamain doctor",                                 // the check
		"DELAMAIN_IT_DECISIONS_MODEL", "-run Integration", // the opt-in contract test
	} {
		if !strings.Contains(section, want) {
			t.Errorf("the section on the optional decisions should mention %q", want)
		}
	}
}

func TestReadmeShouldDocumentTheRemoteAPI(t *testing.T) {
	b, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatalf("the README documents the remote API: %v", err)
	}
	_, section, ok := strings.Cut(string(b), "## API remota")
	if !ok {
		t.Fatalf("the README has no section on the remote API")
	}
	if next := strings.Index(section, "\n## "); next >= 0 {
		section = section[:next]
	}
	for _, want := range []string{
		"delamain serve", "openapi.yaml", // the command and the contract
		"api.addr", "loopback", "cloudflared tunnel", "ngrok http", // the tunnel setup
		"Authorization: Bearer", "/healthz", "auth_failures", "15 minutos", "64 KiB", // the security model
		"delamain serve token", "token_env", "EnvironmentFile", "ambiente das runs", // the token
		"trust_proxy", "CF-Connecting-IP", "X-Forwarded-For", // the proxy trade-off
		"api-audit.log", "Nunca o token nem o body", // the audit
		"mesma máquina",                              // the environment
		"problem+json", "repo_not_allowed", "por id", // the routes
		"/api/v1/events", "hello", "run_removed", "operation", ": ping", "max_streams", // the events
		"exit_code", "5 sem slot", // the operations
		"check `api`", // doctor
	} {
		if !strings.Contains(section, want) {
			t.Errorf("the section on the remote API should mention %q", want)
		}
	}
}

func TestReadmeShouldDocumentNtfyPush(t *testing.T) {
	b, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatalf("the README documents the ntfy push: %v", err)
	}
	_, section, ok := strings.Cut(string(b), "## Notificações push (ntfy)")
	if !ok {
		t.Fatalf("the README has no section on the ntfy push")
	}
	if next := strings.Index(section, "\n## "); next >= 0 {
		section = section[:next]
	}
	for _, want := range []string{
		"ntfy.sh", "notify.events", "topic", "token_env", "priority", // the config
		"único segredo", "título da issue", // the privacy note
		"clique", "três", // the mapping
		"notify --test --via ntfy", "check `ntfy`", // the test and doctor
		"DELAMAIN_NTFY_TOPIC", // the overrides
	} {
		if !strings.Contains(section, want) {
			t.Errorf("the section on the ntfy push should mention %q", want)
		}
	}
}
