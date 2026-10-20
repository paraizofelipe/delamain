package notify

import (
	"os"
	"os/exec"
	"testing"
)

// The release binaries are built with CGO_ENABLED=0: the notifier must stay pure Go on every
// platform `make dist` ships.
func TestNotifyShouldCrossCompileWithoutCGO(t *testing.T) {
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("the go toolchain is needed to prove the cross build: %v", err)
	}
	for _, p := range []struct{ goos, goarch string }{
		{"linux", "amd64"}, {"linux", "arm64"}, {"darwin", "amd64"}, {"darwin", "arm64"},
	} {
		t.Run(p.goos+"/"+p.goarch, func(t *testing.T) {
			cmd := exec.Command(gobin, "build", ".") //nolint:gosec // the go toolchain on PATH
			cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+p.goos, "GOARCH="+p.goarch)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("CGO_ENABLED=0 go build for %s/%s failed: %v\n%s", p.goos, p.goarch, err, out)
			}
		})
	}
}
