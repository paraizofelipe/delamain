package notify

import (
	"os"
	"os/exec"
	"testing"
)

// The release binaries are built with CGO_ENABLED=0 (make dist): the notifier must never need C.
func TestNotifyShouldBuildWithoutCGOForEveryReleasePlatform(t *testing.T) {
	for _, p := range []struct{ os, arch string }{{"linux", "amd64"}, {"linux", "arm64"}, {"darwin", "amd64"}, {"darwin", "arm64"}} {
		cmd := exec.Command("go", "build", "-o", os.DevNull, ".")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+p.os, "GOARCH="+p.arch)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("%s/%s: %v\n%s", p.os, p.arch, err, out)
		}
	}
}
