package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/paraizo/delamain/internal/forge"
	"github.com/paraizo/delamain/internal/herdr"
	"github.com/paraizo/delamain/internal/omp/rpc"
)

// reply is what a simulated agent does for one prompt.
type reply struct {
	write    string // artifact content; "" writes nothing
	err      error  // returned by Prompt
	provider string // non-empty makes the omp session end with this provider error
	mutate   func(g *fakeGit)
	usage    int64 // input tokens the turn consumes
	hang     bool  // the turn only ends when the agent is aborted
	lost     bool  // the final message never reaches the client (only the session file has it)
}

// behavior decides the reply for a role's n-th prompt (1-based).
type behavior func(role string, n int, text string) reply

type fakeHerdr struct {
	mu        sync.Mutex
	dir       string // where fake session files go
	git       *fakeGit
	behave    behavior
	checks    func(name string, n int) int // exit code of the n-th run of a check
	checkHang map[string]bool

	tabs        []string          // labels of created tabs, in order
	tabPanes    map[string]string // label -> root pane of the created tab
	labels      map[string]string // tab id -> label after the last rename
	tabErr      error
	started     map[string][]string // agent name -> args
	starts      map[string]AgentOptions
	prompts     map[string][]string // role -> prompt texts
	promptCalls int
	keys        map[string][]string // agent name -> "abort"/"close" calls, in order
	checkRuns   map[string]int
	startErr    func(role string) error // fails Agents.Start for a role
	worktreeOps int
	liveAgents  []herdr.Agent
	closed      []string
	lastCheck   string
	tails       map[string]string // pane -> log file it follows
	onWorktree  func(path string) // seeds the base checkout (e.g. go.mod, .delamain.yaml)
	interrupt   chan string       // aborts that end a hanging turn
	metadata    []metaCall
}

type metaCall struct {
	seq    int64
	tokens map[string]string
}

func newFakeHerdr(dir string, g *fakeGit, b behavior) *fakeHerdr {
	return &fakeHerdr{
		dir: dir, git: g, behave: b,
		checks:    defaultChecks,
		checkHang: map[string]bool{},
		started:   map[string][]string{}, prompts: map[string][]string{},
		tabPanes: map[string]string{}, labels: map[string]string{}, starts: map[string]AgentOptions{},
		keys: map[string][]string{}, checkRuns: map[string]int{}, tails: map[string]string{},
		interrupt: make(chan string, 4),
	}
}

// defaultChecks passes every gate; the red proof "fails on base", as a real regression test does.
func defaultChecks(name string, _ int) int {
	if name == "redproof" {
		return 1
	}
	return 0
}

func (f *fakeHerdr) WorktreeCreate(_ context.Context, o herdr.WorktreeOptions) (herdr.Worktree, error) {
	f.worktreeOps++
	if err := os.MkdirAll(o.Path, 0o700); err != nil {
		return herdr.Worktree{}, err
	}
	f.git.branch, f.git.wt = o.Branch, o.Path
	if f.onWorktree != nil {
		f.onWorktree(o.Path)
	}
	return herdr.Worktree{WorkspaceID: "w9", TabID: "w9:t1", RootPaneID: "w9:p1", Path: o.Path, Branch: o.Branch}, nil
}

func (f *fakeHerdr) TabCreate(_ context.Context, workspace, _, label string) (herdr.Tab, error) {
	if f.tabErr != nil {
		return herdr.Tab{}, f.tabErr
	}
	f.tabs = append(f.tabs, label)
	n := len(f.tabs) + 1
	tab := herdr.Tab{ID: fmt.Sprintf("%s:t%d", workspace, n), RootPaneID: fmt.Sprintf("%s:p%d", workspace, n)}
	f.tabPanes[label], f.labels[tab.ID] = tab.RootPaneID, label
	return tab, nil
}

func (f *fakeHerdr) TabRename(_ context.Context, tab, label string) error {
	f.labels[tab] = label
	return nil
}

// Start makes fakeHerdr the run's Agents too: one fake records every herdr and omp call.
func (f *fakeHerdr) Start(_ context.Context, o AgentOptions) (Agent, error) {
	if f.startErr != nil {
		if err := f.startErr(roleOf(o.Name)); err != nil {
			return nil, err
		}
	}
	f.mu.Lock()
	f.started[o.Name], f.starts[o.Name] = o.Args, o
	f.mu.Unlock()
	return &fakeAgent{f: f, name: o.Name}, nil
}

type fakeAgent struct {
	f    *fakeHerdr
	name string
}

var artifactRe = regexp.MustCompile("DONE `?([^`\\s]+)`?")

func roleOf(name string) string { return name[:strings.LastIndex(name, "-r")] }

func (a *fakeAgent) Prompt(ctx context.Context, text string, timeout time.Duration) (rpc.Turn, error) {
	rep, err := a.f.turn(a.name, text)
	if err != nil {
		return rpc.Turn{}, err
	}
	if rep.hang {
		select {
		case <-a.f.interrupt:
			return rpc.Turn{StopReason: "aborted", Model: "fake"}, nil
		case <-time.After(timeout):
			return rpc.Turn{}, rpc.ErrTimeout
		case <-ctx.Done():
			return rpc.Turn{}, ctx.Err()
		}
	}
	if rep.err != nil {
		return rpc.Turn{}, rep.err
	}
	if rep.lost {
		return rpc.Turn{}, nil
	}
	if rep.provider != "" {
		return rpc.Turn{StopReason: "error", ErrorMessage: rep.provider, Model: "fake"}, nil
	}
	return rpc.Turn{StopReason: "stop", Model: "fake"}, nil
}

func (a *fakeAgent) Abort() error {
	a.f.mu.Lock()
	a.f.keys[a.name] = append(a.f.keys[a.name], "abort")
	a.f.mu.Unlock()
	select {
	case a.f.interrupt <- "abort":
	default:
	}
	return nil
}

func (a *fakeAgent) Close() error {
	a.f.mu.Lock()
	defer a.f.mu.Unlock()
	a.f.keys[a.name] = append(a.f.keys[a.name], "close")
	return nil
}

func (f *fakeHerdr) turn(name, text string) (reply, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.promptCalls++
	role := roleOf(name)
	f.prompts[role] = append(f.prompts[role], text)
	rep := f.behave(role, len(f.prompts[role]), text)
	if m := artifactRe.FindStringSubmatch(text); m != nil && rep.write != "" {
		if err := os.WriteFile(m[1], []byte(rep.write), 0o600); err != nil {
			return rep, err
		}
	}
	if rep.mutate != nil {
		rep.mutate(f.git)
	}
	return rep, writeSession(f.sessionPath(name), rep.provider, rep.usage)
}

// writeSession appends one assistant turn, as omp does, so usage accumulates across prompts.
// sessionPath honors --session-dir like omp does, so the budget meter finds the usage.
func (f *fakeHerdr) sessionPath(name string) string {
	dir := f.dir
	if args := f.started[name]; len(args) > 0 {
		if i := slices.Index(args, "--session-dir"); i >= 0 && i+1 < len(args) {
			dir = args[i+1]
		}
	}
	return filepath.Join(dir, name+".jsonl")
}

func writeSession(path, providerErr string, input int64) error {
	usage := fmt.Sprintf(`"usage":{"input":%d,"output":0,"cacheRead":0,"cacheWrite":0,"cost":{"total":0}}`, input)
	line := `{"type":"message","message":{"role":"assistant","stopReason":"stop","model":"fake",` + usage + `}}`
	if providerErr != "" {
		line = fmt.Sprintf(`{"type":"message","message":{"role":"assistant","stopReason":"error","errorMessage":%q,"model":"fake",%s}}`, providerErr, usage)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // test temp dir
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(line + "\n")
	return err
}

func (f *fakeHerdr) AgentList(context.Context) ([]herdr.Agent, error) { return f.liveAgents, nil }

func (f *fakeHerdr) WorkspaceClose(_ context.Context, id string) error {
	f.closed = append(f.closed, id)
	return nil
}

func (f *fakeHerdr) WorkspaceReportMetadata(_ context.Context, _, _ string, seq int64, tokens map[string]string) error {
	f.metadata = append(f.metadata, metaCall{seq: seq, tokens: tokens})
	return nil
}

var scriptRe = regexp.MustCompile(`verify-([a-z]+)\.sh`)

func (f *fakeHerdr) PaneRun(_ context.Context, pane, command string) error {
	if log, ok := strings.CutPrefix(command, "tail -n +1 -F "); ok {
		f.tails[pane] = log
		return nil
	}
	m := scriptRe.FindStringSubmatch(command)
	if m == nil {
		return fmt.Errorf("unexpected pane command %q", command)
	}
	f.checkRuns[m[1]]++
	f.lastCheck = m[1]
	return nil
}

func (f *fakeHerdr) PaneWaitOutput(_ context.Context, _, regex string, _ time.Duration) (string, error) {
	name := f.lastCheck
	if f.checkHang[name] {
		return "", &herdr.Error{Code: herdr.CodeTimeout, Message: "timeout"}
	}
	nonce := strings.TrimSuffix(strings.TrimPrefix(regex, "__DLM_"), "_EXIT=[0-9]+")
	return fmt.Sprintf("__DLM_%s_EXIT=%d", nonce, f.checks(name, f.checkRuns[name])), nil
}

// fakeGit models just enough repository state for the guardrails.
type fakeGit struct {
	branch     string
	head       string
	status     string
	commits    []string
	existing   map[string]bool
	remote     map[string]bool // branches on origin
	pushed     []string
	pushErr    error
	wt         string   // worktree path, set by the fake herdr
	changed    []string // files changed since base
	redproof   []string // log of red-proof git operations
	discarded  []string
	discardErr error
}

func newFakeGit() *fakeGit {
	return &fakeGit{head: "aaaaaaaa", existing: map[string]bool{}, remote: map[string]bool{}}
}

func (g *fakeGit) commit(msg string) {
	g.head = fmt.Sprintf("%08x", len(g.commits)+1)
	g.commits = append(g.commits, g.head+" "+msg)
	g.status = ""
}

// commitFiles writes files into the worktree and commits them.
func (g *fakeGit) commitFiles(msg string, files map[string]string) {
	for name, body := range files {
		_ = os.WriteFile(filepath.Join(g.wt, name), []byte(body), 0o600)
		if !slices.Contains(g.changed, name) {
			g.changed = append(g.changed, name)
		}
	}
	g.commit(msg)
}

func (g *fakeGit) ChangedFiles(context.Context, string, string) ([]string, error) {
	return g.changed, nil
}
func (g *fakeGit) AddDetachedWorktree(_ context.Context, _, path, ref string) error {
	g.redproof = append(g.redproof, "add "+ref)
	return os.MkdirAll(path, 0o700)
}
func (g *fakeGit) CheckoutFiles(_ context.Context, _, ref string, files []string) error {
	g.redproof = append(g.redproof, "checkout "+ref+" "+strings.Join(files, ","))
	return nil
}
func (g *fakeGit) RemoveWorktree(_ context.Context, _, path string) error {
	g.redproof = append(g.redproof, "remove")
	return os.RemoveAll(path)
}

func (g *fakeGit) DiscardEmptyWork(_ context.Context, _, worktree, branch, _ string) error {
	if g.discardErr != nil {
		return g.discardErr
	}
	g.discarded = append(g.discarded, branch)
	return os.RemoveAll(worktree)
}

func (g *fakeGit) Toplevel(_ context.Context, dir string) (string, error) {
	if dir == "" {
		return "", errors.New("not a repo")
	}
	return dir, nil
}
func (g *fakeGit) ResolveBase(context.Context, string) (string, error) { return "main", nil }
func (g *fakeGit) BranchExists(_ context.Context, _, b string) bool    { return g.existing[b] }
func (g *fakeGit) Head(context.Context, string) (string, error)        { return g.head, nil }
func (g *fakeGit) CurrentBranch(context.Context, string) (string, error) {
	return g.branch, nil
}
func (g *fakeGit) Status(context.Context, string) (string, error) { return g.status, nil }
func (g *fakeGit) CommitsSince(context.Context, string, string) ([]string, error) {
	return g.commits, nil
}

func (g *fakeGit) Push(_ context.Context, _, branch, _ string) error {
	if g.pushErr != nil {
		return g.pushErr
	}
	g.pushed = append(g.pushed, branch)
	g.remote[branch] = true
	return nil
}

func (g *fakeGit) RemoteBranchExists(_ context.Context, _, branch string) (bool, error) {
	return g.remote[branch], nil
}

// fakeForge records deliveries and issue updates.
type fakeForge struct {
	open      *forge.PR
	created   []forge.PROptions
	createErr error
	comments  []string
	relabels  [][2][]string // {add, remove}
	issues    map[int]forge.Issue
}

func (f *fakeForge) Repo() forge.Repo {
	return forge.Repo{Kind: forge.GitHub, Host: "github.com", Path: "acme/api"}
}
func (f *fakeForge) Issue(_ context.Context, n int) (forge.Issue, error) {
	if is, ok := f.issues[n]; ok {
		return is, nil
	}
	return forge.Issue{}, fmt.Errorf("issue #%d: HTTP 404", n)
}
func (f *fakeForge) ListReady(context.Context, string) ([]forge.Issue, error) { return nil, nil }
func (f *fakeForge) OpenPR(context.Context, string) (*forge.PR, error)        { return f.open, nil }
func (f *fakeForge) CreatePR(_ context.Context, o forge.PROptions) (forge.PR, error) {
	if f.createErr != nil {
		return forge.PR{}, f.createErr
	}
	f.created = append(f.created, o)
	return forge.PR{Number: 42, URL: "https://github.com/acme/api/pull/42", Draft: o.Draft}, nil
}
func (f *fakeForge) Comment(_ context.Context, _ int, body string) error {
	f.comments = append(f.comments, body)
	return nil
}
func (f *fakeForge) Relabel(_ context.Context, _ int, add, remove []string) error {
	f.relabels = append(f.relabels, [2][]string{add, remove})
	return nil
}
