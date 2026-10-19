package agent

import (
	"strings"
	"testing"
)

func TestToolFailureShouldShowTheExitCodeAndTheLineThatExplainsIt(t *testing.T) {
	cases := map[string]struct{ out, want string }{
		"make echoes its recipe before the failing test": {
			out:  "timeout 180s go test -race ./...\nok  \tx/a\t1.0s\n--- FAIL: TestPush (0.06s)\n    gitx_test.go:140: git push: exit status 128\nFAIL\n\n\nWall time: 14.42 seconds\nCommand exited with code 2",
			want: " (exit 2): --- FAIL: TestPush (0.06s) | gitx_test.go:140: git push: exit status 128",
		},
		"no error-looking line: the first one": {
			out:  "something odd\nmore\nCommand exited with code 1",
			want: " (exit 1): something odd | more",
		},
		"a non-bash tool without exit code": {
			out:  "Path '/x/run_test.go' not found",
			want: ": Path '/x/run_test.go' not found",
		},
		"only omp's trailer": {
			out:  "\n[raw output: artifact://2]\nCommand exited with code 3\n",
			want: " (exit 3)",
		},
		"claude code's bash trailer": {
			out:  "Exit code 2\n--- FAIL: TestX (0.01s)\n    x_test.go:9: boom",
			want: " (exit 2): --- FAIL: TestX (0.01s) | x_test.go:9: boom",
		},
		"empty output": {},
	}
	for name, tc := range cases {
		if got := ToolFailure(tc.out); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", name, got, tc.want)
		}
	}
	if got := ToolFailure("error: " + strings.Repeat("x", 1000)); len(got) > 310 {
		t.Errorf("a huge line is truncated, got %d bytes", len(got))
	}
}
