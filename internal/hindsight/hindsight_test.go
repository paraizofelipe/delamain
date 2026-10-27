package hindsight

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// load writes body as coding-agent.json and loads it, so every Resolve test also checks the JSON tags.
func load(t *testing.T, body string) File {
	t.Helper()
	p := filepath.Join(t.TempDir(), "coding-agent.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	f, ok, err := Load(p)
	if err != nil || !ok {
		t.Fatalf("Load(%s) = ok %v, err %v; want a loaded file", p, ok, err)
	}
	return f
}

func TestResolveShouldTurnMemoryOnForARepoMappedInMapPathToBank(t *testing.T) {
	f := load(t, `{"optInOnly": true, "mapPathToBank": {"/home/dev/projects/delamain": "coding-agent::delamain", "/home/dev/projects/other/": "coding-agent::other"}}`)
	cases := map[string]struct{ dir, bank string }{
		"exact dir":           {"/home/dev/projects/delamain", "coding-agent::delamain"},
		"subdir":              {"/home/dev/projects/delamain/internal/hindsight", "coding-agent::delamain"},
		"key with trailing /": {"/home/dev/projects/other", "coding-agent::other"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			bank, on := f.Resolve(c.dir)
			if !on || bank != c.bank {
				t.Fatalf("Resolve(%q) = (%q, %v), want (%q, true)", c.dir, bank, on, c.bank)
			}
		})
	}
}

func TestLoadShouldIgnoreTheApiToken(t *testing.T) {
	const secret = "hs-secret-token-123"
	f := load(t, `{"apiToken": "`+secret+`", "mapPathToBank": {"/r": "b"}, "harnesses": {"claude-code": {"apiToken": "`+secret+`", "disabled": false}}}`)
	if f.MapPathToBank["/r"] != "b" {
		t.Fatalf("decision fields must still be decoded, got %+v", f)
	}
	if got := fmt.Sprintf("%+v", f); strings.Contains(got, secret) {
		t.Fatalf("apiToken must never be kept in File, got %s", got)
	}
}

func TestLoadShouldReportAMissingFileWithoutError(t *testing.T) {
	_, ok, err := Load(filepath.Join(t.TempDir(), "absent.json"))
	if ok || err != nil {
		t.Fatalf("missing file = ok %v, err %v; want ok false and no error", ok, err)
	}
}

func TestLoadShouldFailOnInvalidJSON(t *testing.T) {
	const secret = "hs-secret-token-456"
	p := filepath.Join(t.TempDir(), "coding-agent.json")
	if err := os.WriteFile(p, []byte(`{"apiToken": "`+secret+`", "optInPaths": "not-a-list"`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, ok, err := Load(p)
	if ok || err == nil {
		t.Fatalf("invalid JSON = ok %v, err %v; want ok false and an error", ok, err)
	}
	if !strings.Contains(err.Error(), p) {
		t.Fatalf("error must name the file, got %q", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error must not leak the file content, got %q", err)
	}
}

func TestResolveShouldTurnMemoryOnUnderOptInPathsWithoutABank(t *testing.T) {
	f := load(t, `{"optInOnly": true, "optInPaths": ["/home/dev/work"]}`)
	for _, dir := range []string{"/home/dev/work", "/home/dev/work/repo"} {
		bank, on := f.Resolve(dir)
		if !on || bank != "" {
			t.Fatalf("Resolve(%q) = (%q, %v), want (\"\", true)", dir, bank, on)
		}
	}
}

func TestResolveShouldTurnMemoryOffOutsideTheOptInWhenOptInOnly(t *testing.T) {
	cases := map[string]struct{ body, dir string }{
		"outside optInPaths and mapPathToBank": {`{"optInOnly": true, "optInPaths": ["/home/dev/work"], "mapPathToBank": {"/home/dev/projects/delamain": "b"}}`, "/home/dev/other"},
		"config without any path":              {`{"optInOnly": true}`, "/home/dev/work"},
		"empty dir":                            {`{"optInOnly": true, "optInPaths": ["/"], "mapPathToBank": {"/": "b"}}`, ""},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			bank, on := load(t, c.body).Resolve(c.dir)
			if on || bank != "" {
				t.Fatalf("Resolve(%q) = (%q, %v), want (\"\", false)", c.dir, bank, on)
			}
		})
	}
}

func TestResolveShouldTurnMemoryOnEverywhereWhenOptInOnlyIsFalse(t *testing.T) {
	cases := map[string]struct{ body, dir, bank string }{
		"optInOnly false, unmapped":  {`{"optInOnly": false, "mapPathToBank": {"/home/dev/projects/delamain": "b"}}`, "/tmp/scratch", ""},
		"optInOnly false, mapped":    {`{"optInOnly": false, "mapPathToBank": {"/home/dev/projects/delamain": "b"}}`, "/home/dev/projects/delamain", "b"},
		"optInOnly absent, unmapped": {`{}`, "/tmp/scratch", ""},
		"optInOnly absent, mapped":   {`{"mapPathToBank": {"/home/dev/projects/delamain": "b"}}`, "/home/dev/projects/delamain/cmd", "b"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			bank, on := load(t, c.body).Resolve(c.dir)
			if !on || bank != c.bank {
				t.Fatalf("Resolve(%q) = (%q, %v), want (%q, true)", c.dir, bank, on, c.bank)
			}
		})
	}
}
