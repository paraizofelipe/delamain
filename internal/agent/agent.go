// Package agent holds what every agent harness (omp, claude) reports to the pipeline.
package agent

import (
	"errors"
	"regexp"
	"strings"
)

// ErrTimeout means a turn did not end in time; it was aborted.
var ErrTimeout = errors.New("agente: turno não terminou no prazo")

// Usage is a session's token spend. The JSON keys are omp's, which decodes its frames into it.
type Usage struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cacheRead"`
	CacheWrite int64 `json:"cacheWrite"`
	Cost       Cost  `json:"cost"`
}

type Cost struct {
	Total float64 `json:"total"`
}

func (u Usage) Add(o Usage) Usage {
	return Usage{u.Input + o.Input, u.Output + o.Output, u.CacheRead + o.CacheRead, u.CacheWrite + o.CacheWrite, Cost{u.Cost.Total + o.Cost.Total}}
}

// Turn is the outcome of one prompt. StopReason is "error" on a provider error, "aborted" when
// the turn was stopped, and empty when no final message arrived; anything else is a normal end.
type Turn struct {
	StopReason   string
	ErrorMessage string
	Model        string
	Usage        Usage
}

func (t Turn) Failed() bool { return t.StopReason == "error" }

var (
	// exitRe matches the exit trailer of omp's bash ("Command exited with code N") and of
	// Claude Code's ("Exit code N").
	exitRe = regexp.MustCompile(`(?m)^(?:Command exited with code|Exit code) (\d+)\s*$`)
	// errRe spots the line that says what went wrong, e.g. "--- FAIL: TestX" after make echoes its recipe.
	errRe = regexp.MustCompile(`(?i)\b(fail|error|erro|panic|fatal|denied|not found|no such|undefined|cannot)\b`)
	// The harnesses' own trailer lines, never the cause.
	noiseRe = regexp.MustCompile(`^(Command exited with code \d+|Exit code \d+|Wall time: .*|\[raw output: .*\])$`)
)

// ToolFailure summarises a failed tool's output for the log: " (exit N): <first error line>
// | <next line>", so the cause shows without opening the session transcript.
func ToolFailure(out string) string {
	var exit string
	if m := exitRe.FindStringSubmatch(out); m != nil {
		exit = " (exit " + m[1] + ")"
	}
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" && !noiseRe.MatchString(l) {
			lines = append(lines, l)
		}
	}
	if len(lines) == 0 {
		return exit
	}
	at := 0
	for i, l := range lines {
		if errRe.MatchString(l) {
			at = i
			break
		}
	}
	cause := lines[at]
	if at+1 < len(lines) {
		cause += " | " + lines[at+1]
	}
	return exit + ": " + Truncate(cause, 300)
}

func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
