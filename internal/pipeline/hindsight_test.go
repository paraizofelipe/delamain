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

// assertMemoryOff proves a run's agents started as they did before the memory existed.
func assertMemoryOff(t *testing.T, hs *harness) {
	t.Helper()
	if len(hs.h.starts) == 0 {
		t.Fatal("no agent started")
	}
	for name, o := range hs.h.starts {
		if o.Memory || !slices.Equal(o.Env, gitx.AgentEnv()) {
			t.Errorf("%s must start without the memory, with today's env: memory=%v env=%v", name, o.Memory, o.Env)
		}
	}
	for role, texts := range hs.h.prompts {
		for _, text := range texts {
			if strings.Contains(text, "delamain-memory") {
				t.Errorf("the %s stage must not point to the memory skill", role)
			}
		}
	}
}

func TestRunShouldKeepMemoryOffOutsideTheOptIn(t *testing.T) {
	hs, _ := memoryHarness(t, func(repo string) map[string]any {
		return map[string]any{"optInOnly": true, "mapPathToBank": map[string]string{filepath.Join(filepath.Dir(repo), "other"): "other"}}
	})
	res := hs.run(t)

	if res.Outcome != OutcomeApproved {
		t.Fatalf("the run goes on without the memory: %s (%s)", res.Outcome, res.Reason)
	}
	assertMemoryOff(t, hs)
	if log := runLog(t, res); !strings.Contains(log, "hindsight: repo fora do opt-in, memória desligada run=1") {
		t.Errorf("delamain.log must say why the memory is off:\n%s", log)
	}
}

func TestRunShouldKeepMemoryOffOnOmpOrWhenDisabled(t *testing.T) {
	cases := map[string]func(hs *harness){
		"omp":      func(hs *harness) { hs.r.Cfg.Agent.Harness = config.HarnessOmp },
		"disabled": func(hs *harness) { hs.r.Cfg.Claude.Hindsight.Enabled = false },
		"no block": func(hs *harness) { hs.r.Cfg.Claude.Hindsight = nil },
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			hs, _ := memoryHarness(t, mappedBank)
			setup(hs)
			res := hs.run(t)

			if res.Outcome != OutcomeApproved {
				t.Fatalf("got %s (%s)", res.Outcome, res.Reason)
			}
			assertMemoryOff(t, hs)
			if log := runLog(t, res); strings.Contains(log, "hindsight") {
				t.Errorf("who does not use the memory sees today's log:\n%s", log)
			}
		})
	}
}

func TestRunShouldKeepMemoryOffWhenTheHindsightConfigIsMissingOrInvalid(t *testing.T) {
	cases := map[string]struct {
		write func(t *testing.T, path string)
		log   string
	}{
		"missing": {write: func(*testing.T, string) {}, log: "hindsight: config ausente, memória desligada run=1"},
		"invalid": {
			write: func(t *testing.T, path string) {
				if err := os.WriteFile(path, []byte(`{"optInOnly": tru`), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			log: "WARN hindsight: config ilegível, memória desligada run=1 err=",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			hs, path := memoryHarness(t, nil)
			c.write(t, path)
			res := hs.run(t)

			if res.Outcome != OutcomeApproved {
				t.Fatalf("a broken memory config must not fail the run: %s (%s)", res.Outcome, res.Reason)
			}
			assertMemoryOff(t, hs)
			if log := runLog(t, res); !strings.Contains(log, c.log) {
				t.Errorf("delamain.log must say why the memory is off, missing %q:\n%s", c.log, log)
			}
		})
	}
}

func TestRunReviewAndAddressShouldNeverTurnMemoryOn(t *testing.T) {
	enable := func(rh *reviewHarness) {
		rh.r.Cfg.Agent.Harness = config.HarnessClaude
		path := filepath.Join(t.TempDir(), "coding-agent.json")
		data, _ := json.Marshal(mappedBank(rh.repo))
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		rh.r.Cfg.Claude.Hindsight = &config.Hindsight{Enabled: true, Config: path, Runtime: "/rt"}
	}
	t.Run("review", func(t *testing.T) {
		rh := newReviewHarness(t, reviewsPR(prReviewJSON("")))
		enable(rh)
		if res := rh.review(t); res.Outcome != OutcomeReviewed {
			t.Fatalf("got %s (%s)", res.Outcome, res.Reason)
		}
		assertMemoryOff(t, rh.harness)
	})
	t.Run("address", func(t *testing.T) {
		rh := newAddressHarness(t, answers(happyAnswers()))
		enable(rh)
		rh.address(t)
		assertMemoryOff(t, rh.harness)
	})
}

func TestRunShouldForwardTheHindsightTokenOnlyWhenMemoryIsOn(t *testing.T) {
	outside := func(repo string) map[string]any {
		return map[string]any{"optInOnly": true, "mapPathToBank": map[string]string{filepath.Join(filepath.Dir(repo), "other"): "other"}}
	}
	cases := map[string]struct {
		file  func(repo string) map[string]any
		token string
		want  bool
	}{
		"memory on":           {file: mappedBank, token: hindsightToken, want: true},
		"memory on, no token": {file: mappedBank, token: "", want: false},
		"memory off":          {file: outside, token: hindsightToken, want: false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv("HINDSIGHT_API_TOKEN", c.token)
			hs, _ := memoryHarness(t, c.file)
			hs.run(t)

			for _, role := range issueRoles {
				env := hs.h.starts[role+"-r1"].Env
				if got := slices.Contains(env, "HINDSIGHT_API_TOKEN="+hindsightToken); got != c.want {
					t.Errorf("%s gets the token=%v, want %v: %v", role, got, c.want, env)
				}
				if !c.want && slices.ContainsFunc(env, func(kv string) bool { return strings.HasPrefix(kv, "HINDSIGHT_API_TOKEN=") }) {
					t.Errorf("%s must not get an empty or stray token entry: %v", role, env)
				}
			}
		})
	}
}

func TestRunShouldNeverWriteTheHindsightTokenToTheRunDir(t *testing.T) {
	cases := map[string]func(t *testing.T, path string){
		"valid config": func(*testing.T, string) {},
		"invalid config": func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte(`{"apiToken": "`+hindsightToken+`", "optInOnly": tru`), 0o600); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, write := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv("HINDSIGHT_API_TOKEN", hindsightToken)
			hs, path := memoryHarness(t, mappedBank)
			write(t, path)
			res := hs.run(t)

			if strings.Contains(res.Reason, hindsightToken) {
				t.Errorf("the outcome must not carry the token: %s", res.Reason)
			}
			for role, texts := range hs.h.prompts {
				for _, text := range texts {
					if strings.Contains(text, hindsightToken) {
						t.Errorf("the %s prompt must not carry the token", role)
					}
				}
			}
			files := 0
			err := filepath.WalkDir(res.RunDir, func(p string, d os.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					return err
				}
				files++
				data, err := os.ReadFile(p)
				if err == nil && strings.Contains(string(data), hindsightToken) {
					t.Errorf("%s carries the token", p)
				}
				return err
			})
			if err != nil || files == 0 {
				t.Fatalf("the run dir must be scanned: files=%d err=%v", files, err)
			}
		})
	}
}
