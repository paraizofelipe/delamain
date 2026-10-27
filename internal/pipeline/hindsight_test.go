package pipeline

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/paraizo/delamain/internal/config"
	"github.com/paraizo/delamain/internal/gitx"
)

// hindsightToken stands for a real Hindsight API token: no run file may ever carry it.
const hindsightToken = "hs-sentinel-token-5e1f"

// memoryHarness is an issue run on claude with the memory enabled and the coding-agent.json
// fixture built by file from the run's main checkout. It logs into the run dir only.
func memoryHarness(t *testing.T, file func(repo string) map[string]any) (*harness, string) {
	t.Helper()
	hs := newHarness(t, happy)
	hs.r.Display = nil
	hs.r.Cfg.Agent.Harness = config.HarnessClaude
	path := filepath.Join(t.TempDir(), "coding-agent.json")
	if file != nil {
		data, err := json.Marshal(file(hs.task.Repo))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	hs.r.Cfg.Claude.Hindsight = &config.Hindsight{Enabled: true, Config: path, Runtime: "/rt"}
	return hs, path
}

func mappedBank(repo string) map[string]any {
	return map[string]any{"optInOnly": true, "apiToken": hindsightToken, "mapPathToBank": map[string]string{repo: "delamain"}}
}

func runLog(t *testing.T, res Result) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(res.RunDir, "logs", "delamain.log"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

var issueRoles = []string{"planner", "implementer", "reviewer"}

func TestRunShouldTurnMemoryOnForAnOptedInRepoOnClaude(t *testing.T) {
	cases := map[string]struct {
		file func(repo string) map[string]any
		log  string
	}{
		"mapped to a bank": {file: mappedBank, log: "hindsight: banco delamain run=1"},
		"opted in without a bank": {
			file: func(repo string) map[string]any {
				return map[string]any{"optInOnly": true, "optInPaths": []string{repo}}
			},
			log: "hindsight: ligado run=1",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			hs, path := memoryHarness(t, c.file)
			res := hs.run(t)

			if res.Outcome != OutcomeApproved {
				t.Fatalf("the memory must not change the run's outcome: %s (%s)", res.Outcome, res.Reason)
			}
			for _, role := range issueRoles {
				o := hs.h.starts[role+"-r1"]
				if !o.Memory {
					t.Errorf("%s must start with the memory on: %+v", role, o)
				}
				if !slices.Contains(o.Env, "HINDSIGHT_CONFIG="+path) {
					t.Errorf("%s must get HINDSIGHT_CONFIG pointing at the configured file: %v", role, o.Env)
				}
				for _, kv := range gitx.AgentEnv() {
					if !slices.Contains(o.Env, kv) {
						t.Errorf("%s keeps the push-blocking env, missing %q", role, kv)
					}
				}
				if p := hs.h.prompts[role]; len(p) == 0 || !strings.Contains(p[0], "delamain-memory") {
					t.Errorf("the %s stage must point to the delamain-memory skill", role)
				}
			}
			if log := runLog(t, res); !strings.Contains(log, c.log) {
				t.Errorf("delamain.log must say the memory is on, missing %q:\n%s", c.log, log)
			}
		})
	}
}
