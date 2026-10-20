package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/paraizo/delamain/internal/agent"
	"github.com/paraizo/delamain/internal/budget"
	"github.com/paraizo/delamain/internal/forge"
	"github.com/paraizo/delamain/internal/gitx"
	"github.com/paraizo/delamain/internal/herdr"
	"github.com/paraizo/delamain/internal/notify"
)

// reply is what a simulated agent does for one prompt.
type reply struct {
	write    string // artifact content; "" writes nothing
	err      error  // returned by Prompt
	provider string // non-empty makes the omp session end with this provider error
	mutate   func(g *fakeGit)
	usage    int64 // input tokens the turn consumes
	// unwritten tokens the turn reports over RPC but omp has not written to the session yet
	unwritten int64
	hang      bool // the turn only ends when the agent is aborted
	lost      bool // the final message never reaches the client (only the session file has it)
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
	starts      map[string]AgentOptions
	prompts     map[string][]string // role -> prompt texts
	promptCalls int
	keys        map[string][]string // agent name -> "abort"/"close" calls, in order
	checkRuns   map[string]int
	startErr    func(role string) error // fails Agents.Start for a role
	closeErr    error                   // returned by every Agent.Close
	workspaces  int                     // herdr workspaces opened
	shellEnv    [][]string              // env of each workspace/tab shell, in order
	closed      []string
	tails       map[string]string // pane -> log file it follows
	interrupt   chan string       // aborts that end a hanging turn
	metadata    []metaCall
	checkCalls  []Check
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
		prompts:   map[string][]string{},
		tabPanes:  map[string]string{}, labels: map[string]string{}, starts: map[string]AgentOptions{},
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

func (f *fakeHerdr) WorkspaceCreate(_ context.Context, _, _ string, env []string) (herdr.Workspace, error) {
	f.workspaces++
	f.shellEnv = append(f.shellEnv, env)
	return herdr.Workspace{ID: "w9", TabID: "w9:t1", RootPaneID: "w9:p1"}, nil
}

func (f *fakeHerdr) TabCreate(_ context.Context, workspace, _, label string, env []string) (herdr.Tab, error) {
	f.shellEnv = append(f.shellEnv, env)
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
	f.starts[o.Name] = o
	f.mu.Unlock()
	return &fakeAgent{f: f, name: o.Name}, nil
}

type fakeAgent struct {
	f     *fakeHerdr
	name  string
	spent int64 // guarded by f.mu
}

var artifactRe = regexp.MustCompile("DONE `?([^`\\s]+)`?")

func roleOf(name string) string { return name[:strings.LastIndex(name, "-r")] }

func (a *fakeAgent) Prompt(ctx context.Context, text string, timeout time.Duration) (agent.Turn, error) {
	rep, err := a.f.turn(a.name, text)
	if err != nil {
		return agent.Turn{}, err
	}
	a.f.mu.Lock()
	a.spent += rep.usage + rep.unwritten
	a.f.mu.Unlock()
	if rep.hang {
		select {
		case <-a.f.interrupt:
			return agent.Turn{StopReason: "aborted", Model: "fake"}, nil
		case <-time.After(timeout):
			return agent.Turn{}, agent.ErrTimeout
		case <-ctx.Done():
			return agent.Turn{}, ctx.Err()
		}
	}
	if rep.err != nil {
		return agent.Turn{}, rep.err
	}
	if rep.lost {
		return agent.Turn{}, nil
	}
	if rep.provider != "" {
		return agent.Turn{StopReason: "error", ErrorMessage: rep.provider, Model: "fake"}, nil
	}
	return agent.Turn{StopReason: "stop", Model: "fake"}, nil
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

func (a *fakeAgent) Spent() agent.Usage {
	a.f.mu.Lock()
	defer a.f.mu.Unlock()
	return agent.Usage{Input: a.spent}
}

func (a *fakeAgent) PID() int { return 4242 }

func (a *fakeAgent) Close() error {
	a.f.mu.Lock()
	defer a.f.mu.Unlock()
	a.f.keys[a.name] = append(a.f.keys[a.name], "close")
	return a.f.closeErr
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
// sessionPath honors the session dir like omp does, so the budget meter finds the usage.
func (f *fakeHerdr) sessionPath(name string) string {
	dir := f.dir
	if o, ok := f.starts[name]; ok && o.SessionDir != "" {
		dir = o.SessionDir
	}
	return filepath.Join(dir, name+".jsonl")
}

// Sessions reads what the fake wrote: omp's session format.
func (f *fakeHerdr) Sessions(sessionsDir string) ([]budget.SessionUsage, error) {
	return budget.OmpSessions(sessionsDir)
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

func (f *fakeHerdr) WorkspaceClose(_ context.Context, id string) error {
	f.closed = append(f.closed, id)
	return nil
}

func (f *fakeHerdr) WorkspaceReportMetadata(_ context.Context, _, _ string, seq int64, tokens map[string]string) error {
	f.metadata = append(f.metadata, metaCall{seq: seq, tokens: tokens})
	return nil
}

func (f *fakeHerdr) PaneRun(_ context.Context, pane, command string) error {
	log, ok := strings.CutPrefix(command, "tail -n +1 -F ")
	if !ok {
		return fmt.Errorf("a pane only ever follows a log, got %q", command)
	}
	f.tails[pane] = log
	return nil
}

// Run makes fakeHerdr the run's Checks too: a gate's exit code comes from f.checks.
func (f *fakeHerdr) Run(_ context.Context, c Check) (int, error) {
	f.checkRuns[c.Name]++
	f.checkCalls = append(f.checkCalls, c)
	if f.checkHang[c.Name] {
		return 0, ErrCheckTimeout
	}
	return f.checks(c.Name, f.checkRuns[c.Name]), nil
}

// fakeGit models just enough repository state for the guardrails.
type fakeGit struct {
	branch      string
	head        string
	status      string
	commits     []string
	existing    map[string]bool
	remote      map[string]bool // branches on origin
	pushed      []string
	pushErr     error
	wt          string            // worktree path, set by AddWorktree
	onWorktree  func(path string) // seeds the base checkout (e.g. go.mod, .delamain.yaml)
	worktreeOps int
	changed     []string // files changed since base
	redproof    []string // log of red-proof git operations
	discarded   []string
	discardErr  error
	// PR review state.
	prHead     string                      // SHA FetchPRHead returns; "" returns head
	fetchedPR  []string                    // "<kind> <n>" per FetchPRHead call
	fetchedRef []string                    // FetchRef calls
	detached   []string                    // refs AddDetachedWorktree checked out
	onDetached func(path string)           // seeds a detached checkout (e.g. the PR's .delamain.yaml)
	files      map[string]string           // "<ref>:<path>" -> content ShowFile returns
	shown      []string                    // "<ref>:<path>" per ShowFile call
	lines      map[string][]gitx.LineRange // ChangedLines result
	deleted    []string                    // refs DeleteRef removed
	linesErr   error                       // ChangedLines failure
}

func newFakeGit() *fakeGit {
	return &fakeGit{head: "aaaaaaaa", existing: map[string]bool{}, remote: map[string]bool{}, files: map[string]string{}}
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
func (g *fakeGit) AddWorktree(_ context.Context, _, path, branch, _ string) error {
	g.worktreeOps++
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	g.branch, g.wt = branch, path
	if g.onWorktree != nil {
		g.onWorktree(path)
	}
	return nil
}

func (g *fakeGit) AddDetachedWorktree(_ context.Context, _, path, ref string) error {
	g.redproof = append(g.redproof, "add "+ref)
	g.detached = append(g.detached, ref)
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	if g.onDetached != nil {
		g.onDetached(path)
	}
	return nil
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

func (g *fakeGit) FetchPRHead(_ context.Context, _ string, kind forge.Kind, n int) (string, error) {
	g.fetchedPR = append(g.fetchedPR, fmt.Sprintf("%s %d", kind, n))
	if g.prHead != "" {
		return g.prHead, nil
	}
	return g.head, nil
}

func (g *fakeGit) FetchRef(_ context.Context, _, ref string) error {
	g.fetchedRef = append(g.fetchedRef, ref)
	return nil
}

func (g *fakeGit) ShowFile(_ context.Context, _, ref, path string) ([]byte, error) {
	g.shown = append(g.shown, ref+":"+path)
	if body, ok := g.files[ref+":"+path]; ok {
		return []byte(body), nil
	}
	return nil, fs.ErrNotExist
}

func (g *fakeGit) ChangedLines(context.Context, string, string) (map[string][]gitx.LineRange, error) {
	return g.lines, g.linesErr
}

func (g *fakeGit) DeleteRef(_ context.Context, _, ref string) error {
	g.deleted = append(g.deleted, ref)
	return nil
}

func (g *fakeGit) RemoteBranchExists(_ context.Context, _, branch string) (bool, error) {
	return g.remote[branch], nil
}

// fakeForge records deliveries and issue updates.
type fakeForge struct {
	open       *forge.PR
	created    []forge.PROptions
	createErr  error
	comments   []string
	relabels   [][2][]string // {add, remove}
	relabelErr error
	issues     map[int]forge.Issue
	// PR review state.
	kind    forge.Kind // "" is GitHub
	pr      forge.PRDetail
	linked  []forge.Issue
	threads []forge.Thread
	posted  []forge.ReviewOptions
	// Failures of the review calls.
	threadsErr, postErr error
}

func (f *fakeForge) Repo() forge.Repo {
	if f.kind == forge.GitLab {
		return forge.Repo{Kind: forge.GitLab, Host: "gitlab.com", Path: "acme/api"}
	}
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
	return f.relabelErr
}
func (f *fakeForge) PRView(_ context.Context, n int) (forge.PRDetail, error) {
	if f.pr.Number != n {
		return forge.PRDetail{}, &forge.APIError{Status: 404}
	}
	return f.pr, nil
}
func (f *fakeForge) ListPRs(context.Context, string) ([]forge.PRDetail, error) { return nil, nil }
func (f *fakeForge) LinkedIssues(context.Context, int) ([]forge.Issue, error)  { return f.linked, nil }
func (f *fakeForge) Threads(context.Context, int) ([]forge.Thread, error) {
	return f.threads, f.threadsErr
}
func (f *fakeForge) PostReview(_ context.Context, _ int, o forge.ReviewOptions) error {
	if f.postErr != nil {
		return f.postErr
	}
	f.posted = append(f.posted, o)
	return nil
}
func (f *fakeForge) PRComment(context.Context, int, string) error             { return nil }
func (f *fakeForge) Reply(context.Context, int, string, string) error         { return nil }
func (f *fakeForge) ResolveThread(context.Context, int, string) error         { return nil }
func (f *fakeForge) RelabelPR(context.Context, int, []string, []string) error { return nil }

// fakeNotifier records every toast and how many issue comments existed when it was sent.
type fakeNotifier struct {
	mu       sync.Mutex
	sent     []notify.Message
	comments []int
	forge    *fakeForge
	err      error
	onSend   func(notify.Message) // inspects the world as the toast goes out
}

func (n *fakeNotifier) Notify(_ context.Context, m notify.Message) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.sent = append(n.sent, m)
	seen := 0
	if n.forge != nil {
		seen = len(n.forge.comments)
	}
	n.comments = append(n.comments, seen)
	if n.onSend != nil {
		n.onSend(m)
	}
	return n.err
}

func (n *fakeNotifier) Close() error { return nil }

// of returns the toasts of one event, with the comment count seen by each.
func (n *fakeNotifier) of(e notify.Event) ([]notify.Message, []int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []notify.Message
	var seen []int
	for i, m := range n.sent {
		if m.Event == e {
			out, seen = append(out, m), append(seen, n.comments[i])
		}
	}
	return out, seen
}
