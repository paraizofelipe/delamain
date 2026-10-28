package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"

	"github.com/paraizo/delamain/internal/config"
	"github.com/paraizo/delamain/internal/daemon"
	"github.com/paraizo/delamain/internal/forge"
	"github.com/paraizo/delamain/internal/gitx"
	"github.com/paraizo/delamain/internal/pipeline"
	"github.com/paraizo/delamain/internal/store"
)

// queueForge opens the forge of a queued issue; tests replace it.
var queueForge = forgeFor

func newQueueCmd(load func() (config.Config, error)) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "queue",
		Short: "Order the issues the daemon runs next",
		Long: `The queue holds issues in the order the daemon runs them, one at a time: position 1 is the
next one, and it starts when the previous one ends. An issue leaves the queue when it becomes a
run. While the queue has issues, one of the pipeline.max_parallel slots is reserved for it, and
delamain run and the issues labeled forge.labels.ready share the others.`,
	}
	withStore := func(run func(*cobra.Command, config.Config, *store.Store, []string) error) func(*cobra.Command, []string) error {
		return func(c *cobra.Command, args []string) error {
			cfg, err := load()
			if err != nil {
				return err
			}
			st, err := openStore(c.Context(), cfg)
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()
			return run(c, cfg, st, args)
		}
	}

	var repo string
	add := &cobra.Command{
		Use:   "add ISSUE... [--repo <path>]",
		Short: "Append issues to the end of the queue",
		Args:  cobra.MinimumNArgs(1),
		RunE: withStore(func(c *cobra.Command, cfg config.Config, st *store.Store, args []string) error {
			ns, err := positions(args, "issue")
			if err != nil {
				return err
			}
			return queueAdd(c, cfg, st, repo, ns)
		}),
	}
	add.Flags().StringVar(&repo, "repo", "", "repository of the issues (default: current directory)")

	cmd.AddCommand(add,
		&cobra.Command{
			Use:     "list",
			Aliases: []string{"ls"},
			Short:   "List the queue in execution order",
			Args:    cobra.NoArgs,
			RunE: withStore(func(c *cobra.Command, _ config.Config, st *store.Store, _ []string) error {
				items, err := st.Queue(c.Context())
				if err != nil {
					return err
				}
				return renderQueue(c.OutOrStdout(), items, newStyles(lipgloss.NewRenderer(c.OutOrStdout())))
			}),
		},
		&cobra.Command{
			Use:     "remove POSITION...",
			Aliases: []string{"rm"},
			Short:   "Remove the items at these positions",
			Args:    cobra.MinimumNArgs(1),
			RunE: withStore(func(c *cobra.Command, _ config.Config, st *store.Store, args []string) error {
				ps, err := positions(args, "position")
				if err != nil {
					return err
				}
				// The highest first: removing a position moves every later one up.
				slices.Sort(ps)
				ps = slices.Compact(ps)
				failed := 0
				for _, p := range slices.Backward(ps) {
					it, err := st.RemoveQueueAt(c.Context(), p)
					if err != nil {
						fmt.Fprintln(c.ErrOrStderr(), err)
						failed++
						continue
					}
					fmt.Fprintf(c.OutOrStdout(), "removed #%d from position %d\n", it.Issue, p)
				}
				if failed > 0 {
					return ExitError{Code: 1}
				}
				return nil
			}),
		},
		&cobra.Command{
			Use:   "change POSITION POSITION",
			Short: "Swap the items at two positions",
			Args:  cobra.ExactArgs(2),
			RunE: withStore(func(c *cobra.Command, _ config.Config, st *store.Store, args []string) error {
				ps, err := positions(args, "position")
				if err != nil {
					return err
				}
				a, b, err := st.SwapQueue(c.Context(), ps[0], ps[1])
				if err != nil {
					return err
				}
				fmt.Fprintf(c.OutOrStdout(), "#%d ↔ #%d (positions %d and %d)\n", a.Issue, b.Issue, ps[0], ps[1])
				return nil
			}),
		},
	)
	return cmd
}

// positions parses the arguments as numbers from 1; what names them in an error.
func positions(args []string, what string) ([]int, error) {
	out := make([]int, 0, len(args))
	for _, a := range args {
		n, err := strconv.Atoi(a)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("%s %q: want a number from 1", what, a)
		}
		out = append(out, n)
	}
	return out, nil
}

// queueAdd appends the open issues ns of repo; one that cannot be queued does not keep the others.
func queueAdd(c *cobra.Command, cfg config.Config, st *store.Store, repo string, ns []int) error {
	ctx := c.Context()
	if repo == "" {
		var err error
		if repo, err = os.Getwd(); err != nil {
			return err
		}
	}
	top, err := gitx.New().MainWorktree(ctx, repo)
	if err != nil {
		return fmt.Errorf("repository: %w", err)
	}
	fg, err := queueForge(ctx, cfg, top)
	if err != nil {
		return err
	}
	failed := 0
	for _, n := range ns {
		it, err := enqueueIssue(ctx, st, fg, top, n)
		if err != nil {
			fmt.Fprintln(c.ErrOrStderr(), err)
			failed++
			continue
		}
		fmt.Fprintf(c.OutOrStdout(), "#%d added at position %d\n", it.Issue, it.Position)
	}
	if !daemonRunning(cfg.Paths.StateDir) {
		fmt.Fprintln(c.ErrOrStderr(), "the daemon is not running: the queue only moves with delamain daemon")
	}
	if failed > 0 {
		return ExitError{Code: 1}
	}
	return nil
}

func enqueueIssue(ctx context.Context, st *store.Store, fg forge.Forge, repo string, n int) (store.QueueItem, error) {
	task, err := issueTask(ctx, fg, n, repo)
	if err != nil {
		return store.QueueItem{}, err
	}
	if r, err := st.ActiveRun(ctx, repo, task.Ref); err == nil {
		return store.QueueItem{}, fmt.Errorf("issue #%d already has active run %d", n, r.ID)
	} else if !errors.Is(err, store.ErrNotFound) {
		return store.QueueItem{}, err
	}
	return st.Enqueue(ctx, repo, n, task.Title)
}

func renderQueue(w io.Writer, items []store.QueueItem, st styles) error {
	if len(items) == 0 {
		_, err := fmt.Fprintln(w, "queue is empty")
		return err
	}
	rows := [][]cell{{}}
	for _, h := range []string{"POS", "ISSUE", "REPO", "ADDED", "WAITING", "TITLE"} {
		rows[0] = append(rows[0], styled(st.header, h))
	}
	for _, it := range items {
		waiting := cmp.Or(clean(it.Waiting), "-") // the forge writes it: no control characters
		rows = append(rows, []cell{plain(strconv.Itoa(it.Position)), styled(st.info, fmt.Sprintf("#%d", it.Issue)),
			plain(filepath.Base(it.Repo)), styled(st.muted, ago(it.AddedAt)), plain(waiting), plain(clean(it.Title))})
	}
	return writeTable(w, rows)
}

// queueWaitEvery is how long a queue item that waits keeps its reason before the forge is asked
// again: the queue is polled every few seconds, and each ask costs several requests. Tests shorten it.
var queueWaitEvery = time.Minute

// queueChecked is what the forge last said about a queue item that waits, and when.
type queueChecked struct {
	reason string
	at     time.Time
}

// queueJobs lists the daemon's jobs of the queue, in its order. An item leaves the queue when its
// job starts; a forge is opened once per repository.
func queueJobs(cfg config.Config, st *store.Store, run func(ctx context.Context, t pipeline.Task, fg forge.Forge) (pipeline.Result, error)) func(context.Context) ([]daemon.Job, error) {
	forges := map[string]forge.Forge{} // only the daemon's loop calls it: no lock
	checked := map[int64]queueChecked{}
	return func(ctx context.Context) ([]daemon.Job, error) {
		items, err := st.Queue(ctx)
		if err != nil {
			return nil, err
		}
		jobs := make([]daemon.Job, 0, len(items))
		var errs []error
		for _, it := range items {
			fg, ok := forges[it.Repo]
			if !ok {
				if fg, err = queueForge(ctx, cfg, it.Repo); err == nil && fg == nil {
					err = errors.New("the queue requires a GitHub or GitLab origin")
				}
				if err != nil { // the item waits for its forge; the next ones do not wait for it
					errs = append(errs, fmt.Errorf("queue #%d of %s: %w", it.Issue, it.Repo, err))
					continue
				}
				forges[it.Repo] = fg
			}
			jobs = append(jobs, daemon.Job{
				Key: fmt.Sprintf("%s#%d", it.Repo, it.Issue), Queue: true,
				Run: func(ctx context.Context) (pipeline.Result, error) {
					return runQueued(ctx, st, fg, it, run)
				},
				Wait: func(ctx context.Context) (string, error) {
					if c, ok := checked[it.ID]; ok && time.Since(c.at) < queueWaitEvery {
						return c.reason, nil // still within the pause: no request to the forge
					}
					reason, err := queueWait(ctx, st, fg, it)
					if reason == "" || err != nil {
						delete(checked, it.ID) // a free item or a failed read asks again next time
						return reason, err
					}
					checked[it.ID] = queueChecked{reason: reason, at: time.Now()}
					return reason, nil
				},
			})
		}
		return jobs, errors.Join(errs...)
	}
}

// queueWait says which open issues the queued item it waits for and records them as its reason;
// once none is open the reason is cleared and the item may run. An error leaves the reason alone.
func queueWait(ctx context.Context, st *store.Store, fg forge.Forge, it store.QueueItem) (string, error) {
	is, err := fg.Issue(ctx, it.Issue)
	var apiErr *forge.APIError
	gone := errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound
	if gone || (err == nil && !is.Open) {
		return "", nil // its job takes it out of the queue: it would wait forever
	}
	if err != nil {
		return "", fmt.Errorf("issue #%d: %w", it.Issue, err)
	}
	blockers, err := pipeline.OpenBlockers(ctx, fg, pipeline.TaskFromIssue(is, it.Repo))
	if err != nil {
		return "", err
	}
	reason := strings.Join(blockers, ", ") // none clears the reason
	if err := st.SetQueueWaiting(ctx, it.ID, reason); err != nil {
		return "", err
	}
	return reason, nil
}

// runQueued runs the queued issue it once the forge confirms it is open. The item leaves the queue
// as the run is recorded (run's runner takes it out through leaveQueueOnCreate), so a failure
// before that keeps it queued for the next poll, as does a forge that cannot be read: nothing is
// lost. An issue closed or gone, or one the preflight finds lacking, leaves the queue unrun: that
// answer would hold every next one behind it forever.
func runQueued(ctx context.Context, st *store.Store, fg forge.Forge, it store.QueueItem, run func(ctx context.Context, t pipeline.Task, fg forge.Forge) (pipeline.Result, error)) (pipeline.Result, error) {
	is, err := fg.Issue(ctx, it.Issue)
	var apiErr *forge.APIError
	gone := errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound
	if err != nil && !gone {
		return pipeline.Result{}, fmt.Errorf("issue #%d stays queued: %w", it.Issue, err)
	}
	if in, err := st.InQueue(ctx, it.ID); err != nil || !in {
		return pipeline.Result{Outcome: pipeline.OutcomeSkipped, Reason: "no longer in the queue"}, err
	}
	if gone || !is.Open {
		_, err := st.Dequeue(ctx, it.ID)
		reason := map[bool]string{true: "not found", false: "is closed"}[gone]
		return pipeline.Result{Outcome: pipeline.OutcomeSkipped, Reason: fmt.Sprintf("issue #%d %s; removed from the queue", it.Issue, reason)}, err
	}
	res, err := run(ctx, pipeline.TaskFromIssue(is, it.Repo), fg)
	if res.RunID != 0 || res.Outcome == pipeline.OutcomeNeedsInfo {
		// Again, should the runner's removal have failed: the queue must not run it twice.
		if _, derr := st.Dequeue(context.WithoutCancel(ctx), it.ID); derr != nil {
			err = errors.Join(err, fmt.Errorf("issue #%d is still queued: %w", it.Issue, derr))
		}
	}
	return res, err
}

// leaveQueueOnCreate makes r take t out of the queue once its run is recorded: delamain run, the
// ready label and the queue itself all let a queued issue go only when it has a run.
func leaveQueueOnCreate(r *pipeline.Runner, st *store.Store, t pipeline.Task, log func(msg string, args ...any)) {
	r.Created = func(ctx context.Context, _ store.Run) { leaveQueue(ctx, st, t, log) }
}

// leaveQueue takes an issue task out of the queue as it becomes a run; a task with no issue, or a
// queue that cannot be updated, is left alone: the run matters more than the queue.
func leaveQueue(ctx context.Context, st *store.Store, t pipeline.Task, log func(msg string, args ...any)) {
	if t.Issue == 0 {
		return
	}
	top, err := gitx.New().MainWorktree(ctx, t.Repo)
	if err == nil {
		err = st.DequeueIssue(ctx, top, t.Issue)
	}
	if err != nil {
		log("could not take the issue out of the queue", "issue", t.Issue, "err", err)
	}
}
