package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/paraizo/delamain/internal/gitx"
)

// Checks runs the orchestrator's gate commands.
type Checks interface {
	// Run executes c and returns its exit code. ErrCheckTimeout means it was killed at c.Timeout.
	Run(ctx context.Context, c Check) (int, error)
}

type Check struct {
	Name    string
	Command string // a shell command line, from the repo's config, the stack or the planner
	Dir     string
	LogPath string // the gate's own output
	// Echo also receives the output, framed by start and end lines (the run's checks.log).
	Echo    string
	Timeout time.Duration
}

var ErrCheckTimeout = errors.New("check timed out")

// ExecChecks runs gates as `sh -c` in their own process group, so a timeout kills everything
// they spawned. Gates get the agents' env: nothing they run can push.
type ExecChecks struct{}

func (ExecChecks) Run(ctx context.Context, c Check) (int, error) {
	out, err := os.Create(c.LogPath)
	if err != nil {
		return 0, err
	}
	defer out.Close()
	if c.Echo == "" {
		return runGroup(ctx, c, out)
	}
	echo, err := os.OpenFile(c.Echo, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	defer echo.Close()
	fmt.Fprintf(echo, "%s ▶ %s: %s\n", clock(), c.Name, c.Command)
	code, err := runGroup(ctx, c, io.MultiWriter(out, echo))
	end := fmt.Sprintf("código %d", code)
	switch {
	case errors.Is(err, ErrCheckTimeout):
		end = "estourou o tempo"
	case err != nil:
		end = err.Error()
	}
	fmt.Fprintf(echo, "%s ■ %s: %s\n", clock(), c.Name, end)
	return code, err
}

func runGroup(ctx context.Context, c Check, w io.Writer) (int, error) {
	tctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	cmd := exec.CommandContext(tctx, "sh", "-c", c.Command) //nolint:gosec // gate commands are the repo's own checks
	cmd.Dir, cmd.Stdout, cmd.Stderr = c.Dir, w, w
	cmd.Env = append(os.Environ(), gitx.AgentEnv()...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second // a grandchild holding the output pipe cannot hang the run
	err := cmd.Run()
	switch {
	case errors.Is(tctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil:
		return 0, ErrCheckTimeout
	case ctx.Err() != nil:
		return 0, ctx.Err()
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if code := ee.ExitCode(); code > 0 {
			return code, nil
		}
		return 1, nil // killed by a signal: a failure, never a pass
	}
	return 0, err
}

func clock() string { return time.Now().Format("15:04:05") }
