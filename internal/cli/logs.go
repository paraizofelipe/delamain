package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/paraizo/delamain/internal/config"
	"github.com/paraizo/delamain/internal/pipeline"
)

// followInterval paces `logs -f`; tests shorten it.
var followInterval = 250 * time.Millisecond

func newLogsCmd(load func() (config.Config, error)) *cobra.Command {
	var follow bool
	var roles []string
	var tail int
	cmd := &cobra.Command{
		Use:   "logs RUN_ID",
		Short: "Show a run's logs: orchestrator, gates and each omp agent",
		Long: `Interleaves the orchestrator (delamain) log, gate output (checks) and each omp agent log
(planner, implementer, reviewer) by line time. With -f it follows the live run and exits when it
finishes.`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			id, err := runID(args[0])
			if err != nil {
				return err
			}
			ctx := c.Context()
			in, done, err := openInspector(ctx, load)
			if err != nil {
				return err
			}
			defer done()
			rec, err := in.st.GetRun(ctx, id)
			if err != nil {
				return fmt.Errorf("run %d: %w", id, err)
			}
			fl := &logFollower{dir: filepath.Join(pipeline.RunDir(in.cfg.Paths.StateDir, rec.Repo, rec.ID), "logs"), roles: roles}
			finished := func() bool {
				r, err := in.st.GetRun(ctx, id)
				return err != nil || r.State.Terminal()
			}
			return fl.run(ctx, c.OutOrStdout(), follow, tail, finished)
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "follow the live run until it finishes (Ctrl+C exits first)")
	cmd.Flags().StringSliceVar(&roles, "role", nil, "only these sources: delamain, checks, planner, implementer, reviewer")
	cmd.Flags().IntVar(&tail, "tail", 0, "only the last N written lines (0: all)")
	return cmd
}

// logFollower reads a run's logs/<source>.log files incrementally.
type logFollower struct {
	dir     string
	roles   []string
	offsets map[string]int64  // source -> bytes already read
	partial map[string][]byte // source -> trailing line not yet terminated
	lastTS  map[string]string // source -> time of its last stamped line
}

type logLine struct{ ts, source, text string }

var stampRe = regexp.MustCompile(`^\d\d:\d\d:\d\d `)

func (fl *logFollower) run(ctx context.Context, w io.Writer, follow bool, tail int, finished func() bool) error {
	fl.offsets, fl.partial, fl.lastTS = map[string]int64{}, map[string][]byte{}, map[string]string{}
	lines, err := fl.read(false)
	if err != nil {
		return err
	}
	if tail > 0 && len(lines) > tail {
		lines = lines[len(lines)-tail:]
	}
	if len(lines) == 0 && !follow {
		_, err := fmt.Fprintf(w, "no logs in %s\n", fl.dir)
		return err
	}
	if err := emit(w, lines); err != nil || !follow {
		return err
	}
	t := time.NewTicker(followInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
		end := finished() // checked before reading, so the final lines are never lost
		lines, err := fl.read(end)
		if err != nil {
			return err
		}
		if err := emit(w, lines); err != nil {
			return err
		}
		if end {
			return nil
		}
	}
}

// read returns the lines appended since the last call, ordered by their time stamp. flush also
// returns unterminated last lines (the run has ended, nothing more will complete them).
func (fl *logFollower) read(flush bool) ([]logLine, error) {
	paths, err := filepath.Glob(filepath.Join(fl.dir, "*.log"))
	if err != nil {
		return nil, err
	}
	var out []logLine
	for _, p := range paths {
		source := strings.TrimSuffix(filepath.Base(p), ".log")
		if len(fl.roles) > 0 && !slices.Contains(fl.roles, source) {
			continue
		}
		chunk, err := fl.readFrom(p, source)
		if err != nil {
			return nil, err
		}
		data := append(fl.partial[source], chunk...)
		cut := bytes.LastIndexByte(data, '\n') + 1
		if flush {
			cut = len(data)
		}
		fl.partial[source] = slices.Clone(data[cut:])
		if cut == 0 {
			continue
		}
		for _, text := range strings.Split(strings.TrimSuffix(string(data[:cut]), "\n"), "\n") {
			if stampRe.MatchString(text) {
				fl.lastTS[source] = text[:8]
			}
			// A line without a stamp (gate output) belongs with the stamped line before it.
			out = append(out, logLine{ts: fl.lastTS[source], source: source, text: text})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ts < out[j].ts })
	return out, nil
}

func (fl *logFollower) readFrom(path, source string) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // a log of the run dir
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() < fl.offsets[source] { // truncated or recreated: start over
		fl.offsets[source], fl.partial[source] = 0, nil
	}
	if _, err := f.Seek(fl.offsets[source], io.SeekStart); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(f)
	fl.offsets[source] += int64(len(data))
	return data, err
}

func emit(w io.Writer, lines []logLine) error {
	for _, l := range lines {
		if _, err := fmt.Fprintf(w, "%-11s │ %s\n", l.source, l.text); err != nil {
			return err
		}
	}
	return nil
}
