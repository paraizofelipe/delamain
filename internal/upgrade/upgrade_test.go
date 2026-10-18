package upgrade

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeBinary stands in for a release asset: `<bin> version` prints tag.
func fakeBinary(tag string) []byte { return []byte("#!/bin/sh\necho " + tag + "\n") }

func sum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// forge serves a GitHub-like API: /releases/latest and its assets.
type forge struct {
	t        *testing.T
	tag      string
	files    map[string][]byte // asset name -> content
	checksum map[string]string // overrides the real sha256 in checksums.txt
	status   int               // non-zero: /releases/latest answers this
	mu       sync.Mutex
	auth     []string // Authorization header of each request, by path
}

func newForge(t *testing.T, tag string) (*forge, *httptest.Server) {
	f := &forge{t: t, tag: tag, files: map[string][]byte{"delamain_linux_amd64": fakeBinary(tag)}, checksum: map[string]string{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *forge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.auth = append(f.auth, r.URL.Path+"="+r.Header.Get("Authorization"))
	f.mu.Unlock()
	base := "http://" + r.Host
	switch {
	case r.URL.Path == "/releases/latest":
		if f.status != 0 {
			w.WriteHeader(f.status)
			return
		}
		type asset struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		}
		assets := []asset{{Name: "checksums.txt", URL: base + "/download/checksums.txt"}}
		for name := range f.files {
			assets = append(assets, asset{Name: name, URL: base + "/download/" + name})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": f.tag, "assets": assets})
	case r.URL.Path == "/download/checksums.txt":
		for name, b := range f.files {
			s := sum(b)
			if o, ok := f.checksum[name]; ok {
				s = o
			}
			_, _ = w.Write([]byte(s + "  " + name + "\n"))
		}
	case strings.HasPrefix(r.URL.Path, "/download/"):
		b, ok := f.files[strings.TrimPrefix(r.URL.Path, "/download/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
	default:
		http.NotFound(w, r)
	}
}

func upgrader(srv *httptest.Server) Upgrader {
	return Upgrader{API: srv.URL, HTTP: srv.Client(), OS: "linux", Arch: "amd64"}
}

// installed writes the binary being replaced.
func installed(t *testing.T) string {
	t.Helper()
	target := filepath.Join(t.TempDir(), "delamain")
	if err := os.WriteFile(target, fakeBinary("v0.0.9"), 0o755); err != nil { //nolint:gosec // an executable fixture
		t.Fatal(err)
	}
	return target
}

func assertUntouched(t *testing.T, target string) {
	t.Helper()
	if b, err := os.ReadFile(target); err != nil || string(b) != string(fakeBinary("v0.0.9")) {
		t.Fatalf("the installed binary must stay as it was: %q %v", b, err)
	}
	if entries, _ := os.ReadDir(filepath.Dir(target)); len(entries) != 1 {
		t.Fatalf("no temporary file is left behind: %v", entries)
	}
}

func TestLatestShouldReturnTheTagAndItsAssets(t *testing.T) {
	_, srv := newForge(t, "v0.2.0")
	rel, err := upgrader(srv).Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rel.Tag != "v0.2.0" || rel.Assets["delamain_linux_amd64"] != srv.URL+"/download/delamain_linux_amd64" || rel.Assets["checksums.txt"] == "" {
		t.Fatalf("release = %+v", rel)
	}
}

func TestLatestShouldSayWhenNothingWasPublished(t *testing.T) {
	f, srv := newForge(t, "")
	f.status = http.StatusNotFound
	if _, err := upgrader(srv).Latest(context.Background()); !errors.Is(err, ErrNoRelease) {
		t.Fatalf("a 404 means no release yet: %v", err)
	}
	f.status = http.StatusForbidden // rate limited
	if _, err := upgrader(srv).Latest(context.Background()); err == nil || errors.Is(err, ErrNoRelease) || !strings.Contains(err.Error(), "403") {
		t.Fatalf("other statuses are errors naming the status: %v", err)
	}
}

func TestLatestShouldSendTheTokenOnlyWhenSet(t *testing.T) {
	f, srv := newForge(t, "v0.2.0")
	u := upgrader(srv)
	_, _ = u.Latest(context.Background())
	u.Token = "tkn"
	rel, _ := u.Latest(context.Background())
	_ = u.Install(context.Background(), rel, installed(t))
	want := []string{"/releases/latest=", "/releases/latest=Bearer tkn", "/download/delamain_linux_amd64=", "/download/checksums.txt="}
	if strings.Join(f.auth, " ") != strings.Join(want, " ") {
		t.Fatalf("the token goes to the API, never to asset downloads: %v", f.auth)
	}
}

func TestInstallShouldReplaceTheBinaryWithTheVerifiedRelease(t *testing.T) {
	_, srv := newForge(t, "v0.2.0")
	u := upgrader(srv)
	rel, _ := u.Latest(context.Background())
	target := installed(t)
	if err := u.Install(context.Background(), rel, target); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(target)
	info, _ := os.Stat(target)
	if string(b) != string(fakeBinary("v0.2.0")) || info.Mode().Perm() != 0o755 {
		t.Fatalf("target = %q %v", b, info.Mode())
	}
	if entries, _ := os.ReadDir(filepath.Dir(target)); len(entries) != 1 {
		t.Fatalf("no temporary file is left behind: %v", entries)
	}
}

func TestInstallShouldKeepTheBinaryWhenTheReleaseCannotBeTrusted(t *testing.T) {
	cases := map[string]func(f *forge, u *Upgrader){
		"checksum mismatch":        func(f *forge, _ *Upgrader) { f.checksum["delamain_linux_amd64"] = strings.Repeat("0", 64) },
		"not in checksums.txt":     func(f *forge, _ *Upgrader) { f.checksum["delamain_linux_amd64"] = "" },
		"no asset for platform":    func(_ *forge, u *Upgrader) { u.Arch = "riscv64" },
		"binary reports a wrong v": func(f *forge, _ *Upgrader) { f.files["delamain_linux_amd64"] = fakeBinary("v6.6.6") },
		"binary does not run":      func(f *forge, _ *Upgrader) { f.files["delamain_linux_amd64"] = []byte("\x7fELF garbage") },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f, srv := newForge(t, "v0.2.0")
			u := upgrader(srv)
			mutate(f, &u)
			rel, err := u.Latest(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			target := installed(t)
			if err := u.Install(context.Background(), rel, target); err == nil {
				t.Fatal("must fail")
			}
			assertUntouched(t, target)
		})
	}
}

func TestInstallShouldRejectAnOversizedAsset(t *testing.T) {
	f, srv := newForge(t, "v0.2.0")
	u := upgrader(srv)
	u.MaxSize = 8
	f.files["delamain_linux_amd64"] = fakeBinary("v0.2.0") // longer than 8 bytes
	rel, _ := u.Latest(context.Background())
	target := installed(t)
	if err := u.Install(context.Background(), rel, target); err == nil || !strings.Contains(err.Error(), "limite") {
		t.Fatalf("got %v", err)
	}
	assertUntouched(t, target)
}
