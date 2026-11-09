package cli

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

const serveTestToken = "dlm_0123456789abcdefghijklmnopqrstuvwxyzABCDEF"

func TestServeTokenShouldPrintARandomBase64URLToken(t *testing.T) {
	out, err := execute(t, "serve", "token", "--quiet")
	if err != nil {
		t.Fatal(err)
	}
	token := strings.TrimSpace(out)
	if !regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`).MatchString(token) {
		t.Fatalf("32 random bytes as base64url are 43 characters: %q", token)
	}
	again, _ := execute(t, "serve", "token", "--quiet")
	if strings.TrimSpace(again) == token {
		t.Fatal("every token is new")
	}
	out, err = execute(t, "serve", "token")
	if err != nil || !strings.Contains(out, "export DELAMAIN_API_TOKEN=") || !strings.Contains(out, "EnvironmentFile") {
		t.Fatalf("without --quiet the token comes with how to set it: %v\n%s", err, out)
	}
}

func TestServeShouldRefuseAMissingOrShortTokenWithoutEchoingIt(t *testing.T) {
	cfgFile, _ := isolatedConfig(t, "")
	for _, v := range []string{"", "short-secret"} {
		t.Setenv("DELAMAIN_API_TOKEN", v)
		out, err := execute(t, "--config", cfgFile, "serve", "--addr", "127.0.0.1:0")
		if err == nil || !strings.Contains(err.Error(), "DELAMAIN_API_TOKEN") || !strings.Contains(err.Error(), "serve token") {
			t.Fatalf("token %q: the error names the variable and the fix: %v\n%s", v, err, out)
		}
		if v != "" && strings.Contains(err.Error()+out, v) {
			t.Fatalf("the value is never echoed: %v", err)
		}
	}
	cfgFile, _ = isolatedConfig(t, "api:\n  token_env: MY_API_TOKEN\n")
	t.Setenv("MY_API_TOKEN", "")
	if _, err := execute(t, "--config", cfgFile, "serve", "--addr", "127.0.0.1:0"); err == nil || !strings.Contains(err.Error(), "MY_API_TOKEN") {
		t.Fatalf("the configured variable is the one named: %v", err)
	}
}

func TestServeShouldRefuseANonLoopbackAddress(t *testing.T) {
	cfgFile, _ := isolatedConfig(t, "")
	t.Setenv("DELAMAIN_API_TOKEN", serveTestToken)
	for _, addr := range []string{"0.0.0.0:7778", ":7778", "192.168.0.10:7778"} {
		_, err := execute(t, "--config", cfgFile, "serve", "--addr", addr)
		if err == nil || !strings.Contains(err.Error(), "api.addr") {
			t.Fatalf("%s: the API only listens on loopback: %v", addr, err)
		}
	}
	cfgFile, _ = isolatedConfig(t, "api:\n  addr: 0.0.0.0:7778\n")
	if _, err := execute(t, "--config", cfgFile, "serve"); err == nil || !strings.Contains(err.Error(), "api.addr") {
		t.Fatalf("a non-loopback api.addr in the file is refused too: %v", err)
	}
	if _, err := execute(t, "--config", cfgFile, "serve", "--interval", "0"); err == nil || !strings.Contains(err.Error(), "--interval") {
		t.Fatalf("--interval 0: %v", err)
	}
}

// Only serve uses the API token: any other command drops it, so the git, gates and agents a run
// starts, hooks and tests an agent wrote included, never inherit it.
func TestCommandsOtherThanServeShouldDropTheAPITokenFromTheEnvironment(t *testing.T) {
	cfgFile, _ := isolatedConfig(t, "api:\n  token_env: MY_API_TOKEN\n")
	t.Setenv("MY_API_TOKEN", serveTestToken)
	t.Setenv("DELAMAIN_API_TOKEN", "not-the-configured-variable")
	if _, err := execute(t, "--config", cfgFile, "config"); err != nil {
		t.Fatal(err)
	}
	if _, set := os.LookupEnv("MY_API_TOKEN"); set {
		t.Fatal("the variable api.token_env names is dropped once the config is loaded")
	}
	if os.Getenv("DELAMAIN_API_TOKEN") != "not-the-configured-variable" {
		t.Fatal("only the configured variable is dropped")
	}
}

func TestCommandsShouldKeepTheEnvironmentWhenTheConfigFailsToLoad(t *testing.T) {
	cfgFile, _ := isolatedConfig(t, "api: [not, a, mapping]\n")
	t.Setenv("DELAMAIN_API_TOKEN", serveTestToken)
	if _, err := execute(t, "--config", cfgFile, "config"); err == nil {
		t.Fatal("a broken config must fail to load")
	}
	if os.Getenv("DELAMAIN_API_TOKEN") != serveTestToken {
		t.Fatal("nothing is dropped from a config that did not load: the command fails before starting anything")
	}
}

func TestServeShouldAnswerHealthzOpenlyAndTheAPIBehindTheToken(t *testing.T) {
	cfgFile, state := isolatedConfig(t, "")
	t.Setenv("DELAMAIN_API_TOKEN", serveTestToken)
	ctx, cancel := context.WithCancel(context.Background())
	out := &syncBuffer{}
	errc := make(chan error, 1)
	go func() {
		root := NewRoot()
		root.SetOut(out)
		root.SetErr(out)
		root.SetArgs([]string{"--config", cfgFile, "serve", "--addr", "127.0.0.1:0"})
		errc <- root.ExecuteContext(ctx)
	}()
	urlRe := regexp.MustCompile(`api at (http://127\.0\.0\.1:\d+/)`)
	var base string
	waitFor(t, func() bool {
		m := urlRe.FindStringSubmatch(out.String())
		if m != nil {
			base = strings.TrimSuffix(m[1], "/")
		}
		return m != nil
	}, "serve to say where it listens")
	if !strings.Contains(out.String(), filepath.Join(state, "api-audit.log")) {
		t.Fatalf("serve names its audit log:\n%s", out)
	}
	if _, set := os.LookupEnv("DELAMAIN_API_TOKEN"); set {
		t.Fatal("serve must drop the token from its environment once read: the git and forge calls of a recover would inherit it")
	}

	get := func(path, token string) (int, string) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b)
	}
	if code, body := get("/healthz", ""); code != http.StatusOK || !strings.Contains(body, `"ok":true`) {
		t.Fatalf("healthz: %d %s", code, body)
	}
	if code, _ := get("/api/v1/status", ""); code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", code)
	}
	code, body := get("/api/v1/status", serveTestToken)
	if code != http.StatusOK || !strings.Contains(body, `"daemon":false`) || !strings.Contains(body, `"version":`) {
		t.Fatalf("status: %d %s", code, body)
	}
	if code, body := get("/api/v1/repos", serveTestToken); code != http.StatusOK || strings.TrimSpace(body) != `{"items":[]}` {
		t.Fatalf("repos: %d %s", code, body)
	}
	if code, body := get("/api/v1/runs", serveTestToken); code != http.StatusOK || strings.TrimSpace(body) != `{"items":[]}` {
		t.Fatalf("runs: %d %s", code, body)
	}
	if code, _ := get("/api/v1/operations", serveTestToken); code != http.StatusOK {
		t.Fatalf("the actions are wired: %d", code)
	}
	if _, err := os.Stat(filepath.Join(state, "api-audit.log")); err != nil {
		t.Fatalf("the audit log is created: %v", err)
	}
	cancel()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("serve ends cleanly with its context: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve must end with its context")
	}
}
