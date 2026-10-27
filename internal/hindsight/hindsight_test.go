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
