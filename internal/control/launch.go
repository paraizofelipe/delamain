package control

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Exit codes of `delamain run` with a meaning of their own, as StartExit reads them.
const (
	ExitHalted    = 1
	ExitExhausted = 3
	ExitNeedsInfo = 4
	ExitNoSlot    = 5
)

// Launcher starts `delamain run` and `delamain review` as processes of their own, each writing to
// <StateDir>/launch/<unix time>-<random>-<name>.log. They start a session of their own, so the
// end of the terminal or the process that launched them does not reach them: a run goes on as
// the daemon's runs do.
type Launcher struct {
	StateDir string
	// Executable is the delamain binary to run; nil means os.Executable, the binary of this process.
	Executable func() (string, error)
	// Environ is the environment the child inherits; nil means os.Environ.
	Environ func() []string
	// StripEnv names variables the child must not see, such as the token of the remote API: the
	// agents a run starts would read them.
	StripEnv []string
	// Config is the config file the child loads, as --config; "" leaves it to the default path.
	// The host passes its own, so the child works with the same settings and state dir.
	Config string
}

// StartIssue runs `delamain run --issue issue --repo repo [--kind kind]` and waits for it to end;
// kind "" leaves the kind to the run's inference. It returns the path of its log and, for an exit
// code other than 0, a StartExit.
func (l Launcher) StartIssue(repo string, issue int, kind string) (string, error) {
	args := []string{"run", "--issue", strconv.Itoa(issue), "--repo", repo}
	if kind != "" {
		args = append(args, "--kind", kind)
	}
	path, err := l.launch(fmt.Sprintf("issue-%d", issue), args...)
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		return path, StartExit{Kind: OpStartRun, Issue: issue, Code: ee.ExitCode(), Log: path}
	}
	return path, err
}

// StartReview runs `delamain review --pr pr --repo repo` as StartIssue runs an issue.
func (l Launcher) StartReview(repo string, pr int) (string, error) {
	path, err := l.launch(fmt.Sprintf("review-%d", pr), "review", "--pr", strconv.Itoa(pr), "--repo", repo)
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		return path, StartExit{Kind: OpStartReview, PR: pr, Code: ee.ExitCode(), Log: path}
	}
	return path, err
}

// launch starts the binary with args and a new log and waits for it to end: the path of the
// log, and the *exec.ExitError of an exit code other than 0.
func (l Launcher) launch(name string, args ...string) (string, error) {
	executable := l.Executable
	if executable == nil {
		executable = os.Executable
	}
	exe, err := executable()
	if err != nil {
		return "", err
	}
	if l.Config != "" {
		args = append(args, "--config", l.Config)
	}
	dir := filepath.Join(l.StateDir, "launch")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	// Created exclusively, under a name of its own: two launches of the same issue number in
	// one second, from two repositories, never share a log.
	log, err := os.CreateTemp(dir, fmt.Sprintf("%d-*-%s.log", time.Now().Unix(), name))
	if err != nil {
		return "", err
	}
	path := log.Name()
	cmd := l.Command(exe, log, args...)
	err = cmd.Start()
	_ = log.Close() // the run holds its own descriptor
	if err != nil {
		return path, err
	}
	return path, cmd.Wait()
}

// Command is exe with args, an argument list with no shell. It reads nothing, writes to log alone
// and starts a session of its own; its environment is this process's without StripEnv.
func (l Launcher) Command(exe string, log *os.File, args ...string) *exec.Cmd {
	cmd := exec.Command(exe, args...) //nolint:gosec // an argument list, no shell
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if len(l.StripEnv) > 0 {
		environ := l.Environ
		if environ == nil {
			environ = os.Environ
		}
		cmd.Env = slices.DeleteFunc(environ(), func(kv string) bool {
			name, _, _ := strings.Cut(kv, "=")
			return slices.Contains(l.StripEnv, name)
		})
	}
	return cmd
}

// StartExit is a run or review that exited with a code other than 0: Log is what it wrote.
type StartExit struct {
	Kind      OpKind
	Issue, PR int
	Code      int
	Log       string
}

// Error says what the code means to `delamain run` or `delamain review`, and where the log is.
func (e StartExit) Error() string {
	if e.Kind == OpStartReview {
		return fmt.Sprintf("review of PR #%d failed: exit %d; see %s", e.PR, e.Code, e.Log)
	}
	switch e.Code {
	case ExitNoSlot:
		return fmt.Sprintf("issue #%d not started: maximum parallelism reached; see %s", e.Issue, e.Log)
	case ExitNeedsInfo:
		return fmt.Sprintf("issue #%d needs more information (exit %d); see %s", e.Issue, e.Code, e.Log)
	}
	return fmt.Sprintf("issue #%d failed: exit %d; see %s", e.Issue, e.Code, e.Log)
}
