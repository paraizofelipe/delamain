package pipeline

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/paraizo/delamain/internal/herdr"
)

// Display shows a run to a human while it happens. It is optional: the orchestrator drives
// agents and gates itself, and a run without a display only writes its logs.
type Display interface {
	// Open creates the run's view at its worktree. A non-nil view comes back whenever the view
	// exists, even when part of its setup failed (err says what).
	Open(ctx context.Context, worktree, label, checksLog string) (View, error)
	// Attach is the view a process that died left open, so recovery can close it.
	Attach(id string) View
}

// View is one run on a Display. Every method is display only: the run never depends on it.
type View interface {
	ID() string
	// Show follows a role's log as soon as its agent starts.
	Show(ctx context.Context, role, logPath string) error
	// Done marks a role whose agent stopped; its log stays readable.
	Done(ctx context.Context, role string) error
	Meta(ctx context.Context, tokens map[string]string) error
	Close(ctx context.Context) error
}

// HerdrClient is the subset of the herdr client a herdr view drives.
type HerdrClient interface {
	WorkspaceCreate(ctx context.Context, cwd, label string, env []string) (herdr.Workspace, error)
	TabCreate(ctx context.Context, workspace, cwd, label string, env []string) (herdr.Tab, error)
	TabRename(ctx context.Context, tab, label string) error
	PaneRun(ctx context.Context, pane, command string) error
	WorkspaceClose(ctx context.Context, workspaceID string) error
	WorkspaceReportMetadata(ctx context.Context, workspaceID, source string, seq int64, tokens map[string]string) error
}

// HerdrDisplay gives each run a herdr workspace: the root tab follows the gates, and every
// agent gets a tab that follows its log.
type HerdrDisplay struct {
	Herdr HerdrClient
	// QuietShell, when set, is an empty directory used as ZDOTDIR for the tabs' shells: they only
	// run `tail`, and a user's .zshrc may block on startup (e.g. a gpg prompt) before the
	// command is read.
	QuietShell string
}

func (d HerdrDisplay) Open(ctx context.Context, worktree, label, checksLog string) (View, error) {
	var env []string
	if d.QuietShell != "" {
		if err := quietShell(d.QuietShell); err != nil {
			return nil, err
		}
		env = []string{"ZDOTDIR=" + d.QuietShell}
	}
	ws, err := d.Herdr.WorkspaceCreate(ctx, worktree, label, env)
	if err != nil {
		return nil, err
	}
	v := &herdrView{h: d.Herdr, id: ws.ID, cwd: worktree, env: env, tabs: map[string]string{"checks": ws.TabID}}
	return v, errors.Join(
		d.Herdr.TabRename(ctx, ws.TabID, "checks"),
		d.Herdr.PaneRun(ctx, ws.RootPaneID, tailCommand(checksLog)),
	)
}

// quietShell prepares dir as a ZDOTDIR with an empty .zshrc: without one, zsh's new-user
// wizard reads a key and eats the start of the command typed into the pane.
func quietShell(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, ".zshrc"), os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // delamain's own state dir
	if err != nil {
		return err
	}
	return f.Close()
}

func (d HerdrDisplay) Attach(id string) View { return &herdrView{h: d.Herdr, id: id} }

type herdrView struct {
	h       HerdrClient
	id, cwd string
	env     []string
	tabs    map[string]string // role -> tab id
	seq     int64
}

func (v *herdrView) ID() string { return v.id }

func (v *herdrView) Show(ctx context.Context, role, logPath string) error {
	tab, err := v.h.TabCreate(ctx, v.id, v.cwd, role, v.env)
	if err != nil {
		return err
	}
	v.tabs[role] = tab.ID
	return v.h.PaneRun(ctx, tab.RootPaneID, tailCommand(logPath))
}

func (v *herdrView) Done(ctx context.Context, role string) error {
	if v.tabs[role] == "" {
		return nil
	}
	return v.h.TabRename(ctx, v.tabs[role], role+" ✓")
}

func (v *herdrView) Meta(ctx context.Context, tokens map[string]string) error {
	v.seq = max(v.seq+1, time.Now().UnixMilli()) // herdr drops non-increasing seqs
	return v.h.WorkspaceReportMetadata(ctx, v.id, "delamain", v.seq, tokens)
}

func (v *herdrView) Close(ctx context.Context) error { return v.h.WorkspaceClose(ctx, v.id) }

func tailCommand(path string) string { return "tail -n +1 -F " + shellQuote(path) }
