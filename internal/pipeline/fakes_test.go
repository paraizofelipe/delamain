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
)

// reply is what a simulated agent does for one prompt.
type reply struct {
	write    string // artifact content; "" writes nothing
	status   string // final herdr status; default done
	err      error  // returned by AgentPrompt
	provider string // non-empty makes the omp session end with this provider error
	mutate   func(g *fakeGit)
}

// behavior decides the reply for a role's n-th non-handshake prompt (1-based).
type behavior func(role string, n int, text string) reply

type fakeHerdr struct {
	mu        sync.Mutex
	dir       string // where fake session files go
	git       *fakeGit
	behave    behavior
	checks    func(name string, n int) int // exit code of the n-th run of a check
	checkHang map[string]bool

	splits      int
	started     map[string][]string // agent name -> args
	prompts     map[string][]string // role -> prompt texts (excluding handshakes)
	promptCalls int
	waits       int
	keys        map[string][]string
	checkRuns   map[string]int
	handshake   func(role string) bool // false skips writing the ready file
	worktreeOps int
	busyStarts  int // AgentStart fails with agent_pane_busy this many times first
	liveAgents  []herdr.Agent
	closed      []string
	lastCheck   string
	exports     map[string]string // pane -> export command typed before the agent started
	onWorktree  func(path string) // seeds the base checkout (e.g. go.mod, .delamain.yaml)
}

func newFakeHerdr(dir string, g *fakeGit, b behavior) *fakeHerdr {
	return &fakeHerdr{
		dir: dir, git: g, behave: b,
		checks:    defaultChecks,
		checkHang: map[string]bool{},
		started:   map[string][]string{}, prompts: map[string][]string{},
		keys: map[string][]string{}, checkRuns: map[string]int{}, exports: map[string]string{},
		handshake: func(string) bool { return true },
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

func (f *fakeHerdr) PaneSplit(context.Context, string, string, string) (string, error) {
	f.splits++
	return fmt.Sprintf("w9:p%d", f.splits+1), nil
}

func (f *fakeHerdr) AgentStart(_ context.Context, name, kind, _ string, _ time.Duration, args ...string) (herdr.Agent, error) {
	if kind != "omp" {
		return herdr.Agent{}, fmt.Errorf("unexpected kind %s", kind)
	}
	if f.busyStarts > 0 {
		f.busyStarts--
		return herdr.Agent{}, &herdr.Error{Code: herdr.CodeAgentPaneBusy, Message: "not an available shell"}
	}
	f.started[name] = args
	return herdr.Agent{Name: name, Status: herdr.StatusIdle}, nil
}

var (
	artifactRe  = regexp.MustCompile("DONE `?([^`\\s]+)`?")
	handshakeRe = regexp.MustCompile("Crie o arquivo `([^`]+)`")
)

func roleOf(name string) string { return name[:strings.LastIndex(name, "-r")] }

func (f *fakeHerdr) AgentPrompt(_ context.Context, name, text string, _ time.Duration) (herdr.Agent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.promptCalls++
	role := roleOf(name)
	sess := filepath.Join(f.dir, name+".jsonl")
	agent := herdr.Agent{Name: name, Status: herdr.StatusDone, Session: herdr.AgentSession{Kind: "path", Value: sess}}

	if m := handshakeRe.FindStringSubmatch(text); m != nil {
		if f.handshake(role) {
			_ = os.WriteFile(m[1], []byte("ok"), 0o600)
		}
		return agent, writeSession(sess, "")
	}
	f.prompts[role] = append(f.prompts[role], text)
	rep := f.behave(role, len(f.prompts[role]), text)
	if m := artifactRe.FindStringSubmatch(text); m != nil && rep.write != "" {
		if err := os.WriteFile(m[1], []byte(rep.write), 0o600); err != nil {
			return agent, err
		}
	}
	if rep.mutate != nil {
		rep.mutate(f.git)
	}
	if rep.status != "" {
		agent.Status = rep.status
	}
	if err := writeSession(sess, rep.provider); err != nil {
		return agent, err
	}
	return agent, rep.err
}

func writeSession(path, providerErr string) error {
	line := `{"type":"message","message":{"role":"assistant","stopReason":"stop","model":"fake"}}`
	if providerErr != "" {
		line = fmt.Sprintf(`{"type":"message","message":{"role":"assistant","stopReason":"error","errorMessage":%q,"model":"fake"}}`, providerErr)
	}
	return os.WriteFile(path, []byte(line+"\n"), 0o600)
}

func (f *fakeHerdr) AgentWait(_ context.Context, name string, _ time.Duration) (herdr.Agent, error) {
	f.waits++
	return herdr.Agent{Name: name, Status: herdr.StatusDone, Session: herdr.AgentSession{Value: filepath.Join(f.dir, name+".jsonl")}}, nil
}

func (f *fakeHerdr) AgentList(context.Context) ([]herdr.Agent, error) { return f.liveAgents, nil }

func (f *fakeHerdr) WorkspaceClose(_ context.Context, id string) error {
	f.closed = append(f.closed, id)
	return nil
}

func (f *fakeHerdr) AgentSendKeys(_ context.Context, name string, keys ...string) error {
	f.keys[name] = append(f.keys[name], keys...)
	return nil
}

var scriptRe = regexp.MustCompile(`verify-([a-z]+)\.sh`)

func (f *fakeHerdr) PaneRun(_ context.Context, pane, command string) error {
	if strings.HasPrefix(command, "export ") {
		f.exports[pane] = command
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
	branch   string
	head     string
	status   string
	commits  []string
	existing map[string]bool
	remote   map[string]bool // branches on origin
	pushed   []string
	pushErr  error
	wt       string   // worktree path, set by the fake herdr
	changed  []string // files changed since base
	redproof []string // log of red-proof git operations
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
func (f *fakeForge) OpenPR(context.Context, string) (*forge.PR, error) { return f.open, nil }
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
