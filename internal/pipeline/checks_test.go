package pipeline

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func runExecCheck(t *testing.T, command string, timeout time.Duration) (Check, int, error) {
	t.Helper()
	dir := t.TempDir()
	c := Check{
		Name: "test", Command: command, Dir: dir, Timeout: timeout,
		LogPath: filepath.Join(dir, "verify-test.log"), Echo: filepath.Join(dir, "checks.log"),
	}
	code, err := ExecChecks{}.Run(context.Background(), c)
	return c, code, err
}

func TestExecChecksShouldReportTheExitCodeAndKeepTheOutput(t *testing.T) {
	c, code, err := runExecCheck(t, "echo out; echo err >&2; pwd; exit 3", 10*time.Second)
	if err != nil || code != 3 {
		t.Fatalf("code=%d err=%v", code, err)
	}
	own, _ := os.ReadFile(c.LogPath)
	if !strings.Contains(string(own), "out\nerr\n") || !strings.Contains(string(own), c.Dir) {
		t.Fatalf("the gate's log has stdout+stderr and ran in Dir:\n%s", own)
	}
	echo, _ := os.ReadFile(c.Echo)
	lines := strings.Split(strings.TrimSpace(string(echo)), "\n")
	if !strings.HasSuffix(lines[0], "▶ test: "+c.Command) || !strings.HasSuffix(lines[len(lines)-1], "■ test: código 3") || !strings.Contains(string(echo), "out") {
		t.Fatalf("checks.log frames the output with start and end lines:\n%s", echo)
	}
}

func TestExecChecksShouldPassAZeroExit(t *testing.T) {
	if _, code, err := runExecCheck(t, "true", 10*time.Second); err != nil || code != 0 {
		t.Fatalf("code=%d err=%v", code, err)
	}
}

func TestExecChecksShouldKillTheWholeGroupOnTimeout(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	start := time.Now()
	c, _, err := runExecCheck(t, "sleep 60 & echo $! > "+shellQuote(pidFile)+"; wait", 300*time.Millisecond)
	if !errors.Is(err, ErrCheckTimeout) {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("a hanging gate must not hold the run")
	}
	raw, _ := os.ReadFile(pidFile)
	var pid int
	for _, ch := range strings.TrimSpace(string(raw)) {
		pid = pid*10 + int(ch-'0')
	}
	if pid == 0 {
		t.Fatal("child pid not recorded")
	}
	deadline := time.Now().Add(5 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("the gate's child %d survived the timeout", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if echo, _ := os.ReadFile(c.Echo); !strings.Contains(string(echo), "■ test: estourou o tempo") {
		t.Fatalf("checks.log says why it ended:\n%s", echo)
	}
}

func TestExecChecksShouldRunWithPushBlocked(t *testing.T) {
	c, code, _ := runExecCheck(t, `git config --get-regexp '^url\..*\.pushinsteadof$'`, 10*time.Second)
	if own, _ := os.ReadFile(c.LogPath); code != 0 || len(own) == 0 {
		t.Fatalf("gates inherit the agents' push-blocking env (code %d):\n%s", code, own)
	}
}

func TestExecChecksShouldFailAGateKilledByASignal(t *testing.T) {
	if _, code, err := runExecCheck(t, "kill -9 $$", 10*time.Second); err != nil || code == 0 {
		t.Fatalf("a gate that died never passes: code=%d err=%v", code, err)
	}
}
