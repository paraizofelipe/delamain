package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const oldBinary = "#!/bin/sh\necho v0.1.0\n"

// releaseServer publishes tag with a fake binary for this platform.
func releaseServer(t *testing.T, tag string) {
	t.Helper()
	bin := []byte("#!/bin/sh\necho " + tag + "\n")
	h := sha256.Sum256(bin)
	name := "delamain_" + runtime.GOOS + "_" + runtime.GOARCH
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases/latest":
			_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": tag, "assets": []map[string]string{
				{"name": name, "browser_download_url": srv.URL + "/bin"},
				{"name": "checksums.txt", "browser_download_url": srv.URL + "/sums"},
			}})
		case "/bin":
			_, _ = w.Write(bin)
		case "/sums":
			_, _ = w.Write([]byte(hex.EncodeToString(h[:]) + "  " + name + "\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	prevAPI, prevExe := upgradeAPI, executable
	upgradeAPI = srv.URL
	t.Cleanup(func() { upgradeAPI, executable = prevAPI, prevExe })
}

// installedAt fakes the running binary as ~/.local/bin/delamain at version, reached through a symlink.
func installedAt(t *testing.T, version string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), ".local", "bin")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "delamain")
	if err := os.WriteFile(target, []byte(oldBinary), 0o755); err != nil { //nolint:gosec // an executable fixture
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "delamain")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	executable = func() (string, error) { return link, nil }
	prev := Version
	Version = version
	t.Cleanup(func() { Version = prev })
	return target
}

func content(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestUpgradeShouldReplaceTheInstalledBinaryWhenANewerReleaseExists(t *testing.T) {
	releaseServer(t, "v0.2.0")
	target := installedAt(t, "v0.1.0")
	out, err := execute(t, "upgrade")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "v0.1.0 → v0.2.0") || !strings.Contains(out, target) {
		t.Fatalf("out = %q", out)
	}
	if got := content(t, target); !strings.Contains(got, "echo v0.2.0") {
		t.Fatalf("the symlink's target is replaced, not the link: %q", got)
	}
}

func TestUpgradeShouldDoNothingWhenAlreadyOnTheLatestRelease(t *testing.T) {
	releaseServer(t, "v0.1.0")
	target := installedAt(t, "v0.1.0")
	out, err := execute(t, "upgrade")
	if err != nil || !strings.Contains(out, "já está na última versão (v0.1.0)") || content(t, target) != oldBinary {
		t.Fatalf("%v %q", err, out)
	}
}

func TestUpgradeCheckShouldOnlyReportANewRelease(t *testing.T) {
	releaseServer(t, "v0.2.0")
	target := installedAt(t, "v0.1.0")
	out, err := execute(t, "upgrade", "--check")
	if err != nil || !strings.Contains(out, "v0.1.0 → v0.2.0") || content(t, target) != oldBinary {
		t.Fatalf("--check never replaces: %v %q", err, out)
	}
}

func TestUpgradeShouldRefuseADevBuildUnlessForced(t *testing.T) {
	releaseServer(t, "v0.2.0")
	target := installedAt(t, "dev")
	if out, err := execute(t, "upgrade"); err == nil || !strings.Contains(err.Error(), "--force") || content(t, target) != oldBinary {
		t.Fatalf("a source build is kept: %v %q", err, out)
	}
	if out, err := execute(t, "upgrade", "--force"); err != nil || !strings.Contains(content(t, target), "echo v0.2.0") {
		t.Fatalf("--force replaces it: %v %q", err, out)
	}
}

func TestUpgradeShouldFailWhenNoReleaseWasPublished(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	prev := upgradeAPI
	upgradeAPI = srv.URL
	t.Cleanup(func() { upgradeAPI = prev })
	if _, err := execute(t, "upgrade"); err == nil || !strings.Contains(err.Error(), "nenhuma release publicada") {
		t.Fatalf("got %v", err)
	}
}
