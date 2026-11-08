// Package control holds the actions on runs that the CLI, the watch TUI and the remote API
// share: stop, recover and remove. Each action answers with a typed result or a typed error and
// prints nothing; how it is shown is the caller's.
package control

import (
	"context"
	"sync"
	"time"

	"github.com/paraizo/delamain/internal/config"
	"github.com/paraizo/delamain/internal/forge"
	"github.com/paraizo/delamain/internal/gitx"
	"github.com/paraizo/delamain/internal/pipeline"
	"github.com/paraizo/delamain/internal/store"
)

// Deps is what the actions need from the process hosting them.
type Deps struct {
	Cfg   config.Config
	Store *store.Store
	// NewRunner builds the pipeline runner that finalizes an orphan. It is called once, on the
	// first recover, and Close closes its notifier. Nil means no recover: Stop fails on an orphan.
	NewRunner func() (*pipeline.Runner, error)
	// ForgeFor picks the forge of a repository, for the recover that delivers a draft.
	ForgeFor func(ctx context.Context, repo string) (forge.Forge, error)
	// Alive reports whether a process is alive; nil means pipeline.ProcessAlive.
	Alive func(pid int) bool
	// Git runs git; the zero value runs the git on PATH.
	Git gitx.Git
	// Launcher starts runs and reviews; nil means one on Cfg's state dir running this binary.
	Launcher *Launcher
	// Operations is where the asynchronous actions report; nil means a new registry.
	Operations *Operations
	// Context bounds the asynchronous actions; nil means context.Background().
	Context context.Context //nolint:containedctx // the lifetime of the host, not of a call
	// RunIDPoll and RunIDWait pace the discovery of the run a launch created; zero means the defaults.
	RunIDPoll, RunIDWait time.Duration
}

// Actions performs the shared actions on the store of Deps.
type Actions struct {
	d Deps

	mu     sync.Mutex
	runner *pipeline.Runner
}

func New(d Deps) *Actions {
	if d.Alive == nil {
		d.Alive = pipeline.ProcessAlive
	}
	if d.Git.Binary == "" {
		d.Git = gitx.New()
	}
	if d.Launcher == nil {
		d.Launcher = &Launcher{StateDir: d.Cfg.Paths.StateDir}
	}
	if d.Operations == nil {
		d.Operations = NewOperations()
	}
	if d.Context == nil {
		d.Context = context.Background()
	}
	if d.RunIDPoll <= 0 {
		d.RunIDPoll = runIDPoll
	}
	if d.RunIDWait <= 0 {
		d.RunIDWait = runIDWait
	}
	return &Actions{d: d}
}

// Config is the configuration the actions run under.
func (a *Actions) Config() config.Config { return a.d.Cfg }

// Store is the state database the actions act on.
func (a *Actions) Store() *store.Store { return a.d.Store }

// Close releases what the actions opened: the notifier of the runner that recovered.
func (a *Actions) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.runner != nil && a.runner.Notify != nil {
		return a.runner.Notify.Close()
	}
	return nil
}

// runnerFor builds the recover runner on first need.
func (a *Actions) runnerFor() (*pipeline.Runner, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.runner != nil {
		return a.runner, nil
	}
	if a.d.NewRunner == nil {
		return nil, ErrNoRunner
	}
	r, err := a.d.NewRunner()
	if err != nil {
		return nil, err
	}
	a.runner = r
	return r, nil
}

// getRun reads run id, wrapping a missing one as ErrNotFound.
func (a *Actions) getRun(ctx context.Context, id int64) (store.Run, error) {
	rec, err := a.d.Store.GetRun(ctx, id)
	if err == store.ErrNotFound { //nolint:errorlint // the store returns the sentinel itself
		return rec, &RunError{ID: id, Err: ErrNotFound}
	}
	return rec, err
}
