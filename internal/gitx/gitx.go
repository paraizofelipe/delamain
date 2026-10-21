// Package gitx wraps the few git operations the orchestrator performs itself.
// It never merges, rebases, resets or pushes to a protected branch.
package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"

	"github.com/paraizo/delamain/internal/forge"
)

// Protected branches are never used as work branches.
var Protected = []string{"main", "master"}

type Git struct{ Binary string }

func New() Git { return Git{Binary: "git"} }

func (g Git) run(ctx context.Context, dir string, args ...string) (string, error) {
	return g.runEnv(ctx, dir, nil, args...)
}

// runEnv is run with extra environment variables.
func (g Git) runEnv(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	var out, errb bytes.Buffer
	cmd := exec.CommandContext(ctx, g.Binary, append([]string{"-C", dir}, args...)...)
	cmd.Stdout, cmd.Stderr = &out, &errb
	cmd.Env = append(append(os.Environ(), "GIT_TERMINAL_PROMPT=0"), env...) // fail instead of waiting for credentials
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// Toplevel resolves the repository root containing dir.
func (g Git) Toplevel(ctx context.Context, dir string) (string, error) {
	return g.run(ctx, dir, "rev-parse", "--show-toplevel")
}

// HasRemote reports whether the repository has the named remote.
func (g Git) HasRemote(ctx context.Context, repo, name string) bool {
	_, err := g.run(ctx, repo, "remote", "get-url", name)
	return err == nil
}

// RemoteURL returns the URL of a remote.
func (g Git) RemoteURL(ctx context.Context, repo, name string) (string, error) {
	return g.run(ctx, repo, "remote", "get-url", name)
}

// ResolveBase picks the ref new work branches start from: origin's default branch after a
// fetch when a remote exists, otherwise the repository's current local branch.
func (g Git) ResolveBase(ctx context.Context, repo string) (string, error) {
	if g.HasRemote(ctx, repo, "origin") {
		if _, err := g.run(ctx, repo, "fetch", "--quiet", "origin"); err != nil {
			return "", err
		}
		ref, err := g.run(ctx, repo, "symbolic-ref", "--quiet", "refs/remotes/origin/HEAD")
		if err == nil {
			return strings.TrimPrefix(ref, "refs/remotes/"), nil
		}
		for _, b := range Protected {
			if _, err := g.run(ctx, repo, "rev-parse", "--verify", "--quiet", "origin/"+b); err == nil {
				return "origin/" + b, nil
			}
		}
		return "", errors.New("cannot resolve origin's default branch")
	}
	return g.run(ctx, repo, "rev-parse", "--abbrev-ref", "HEAD")
}

func (g Git) Head(ctx context.Context, dir string) (string, error) {
	return g.run(ctx, dir, "rev-parse", "HEAD")
}

func (g Git) CurrentBranch(ctx context.Context, dir string) (string, error) {
	return g.run(ctx, dir, "rev-parse", "--abbrev-ref", "HEAD")
}

// Status returns `git status --porcelain` (empty means clean).
func (g Git) Status(ctx context.Context, dir string) (string, error) {
	return g.run(ctx, dir, "status", "--porcelain")
}

// CommitsSince lists `<sha> <subject>` of commits in base..HEAD, oldest first.
func (g Git) CommitsSince(ctx context.Context, dir, base string) ([]string, error) {
	out, err := g.run(ctx, dir, "log", "--reverse", "--format=%h %s", base+"..HEAD")
	if err != nil || out == "" {
		return nil, err
	}
	return strings.Split(out, "\n"), nil
}

// CommitMessages returns the whole message (subject and body) of each commit in base..HEAD,
// oldest first.
func (g Git) CommitMessages(ctx context.Context, dir, base string) ([]string, error) {
	out, err := g.run(ctx, dir, "log", "--reverse", "-z", "--format=%B", base+"..HEAD")
	if err != nil {
		return nil, err
	}
	var msgs []string
	for _, m := range strings.Split(out, "\x00") {
		if m = strings.TrimSpace(m); m != "" {
			msgs = append(msgs, m)
		}
	}
	return msgs, nil
}

// ChangedFiles lists files added or modified in base...HEAD (deletions excluded).
func (g Git) ChangedFiles(ctx context.Context, dir, base string) ([]string, error) {
	out, err := g.run(ctx, dir, "diff", "--name-only", "--diff-filter=d", base+"...HEAD")
	if err != nil || out == "" {
		return nil, err
	}
	return strings.Split(out, "\n"), nil
}

// AddWorktree creates branch at base and checks it out at path. The branch must be a new work
// branch: protected branches are refused and an existing branch makes git fail.
func (g Git) AddWorktree(ctx context.Context, repo, path, branch, base string) error {
	if err := GuardWorkBranch(branch, base); err != nil {
		return err
	}
	_, err := g.run(ctx, repo, "worktree", "add", "--quiet", "-b", branch, path, base)
	return err
}

// AddWorktreeOnBranch fetches branch from origin and checks it out at path, the local branch
// reset to the origin head. Protected and base branches are refused before any git command.
func (g Git) AddWorktreeOnBranch(ctx context.Context, repo, path, branch, base string) error {
	if err := GuardWorkBranch(branch, base); err != nil {
		return err
	}
	if err := g.FetchRef(ctx, repo, branch); err != nil {
		return err
	}
	_, err := g.run(ctx, repo, "worktree", "add", "--quiet", "-B", branch, path, "refs/remotes/origin/"+branch)
	return err
}

// Worktree is one checkout of a repository, as listed by `git worktree list`.
type Worktree struct {
	Path     string `json:"path" yaml:"path"`
	Branch   string `json:"branch,omitempty" yaml:"branch,omitempty"`
	Head     string `json:"head" yaml:"head"`
	Detached bool   `json:"detached,omitempty" yaml:"detached,omitempty"`
}

// ListWorktrees returns every checkout of repo, the main one first.
func (g Git) ListWorktrees(ctx context.Context, repo string) ([]Worktree, error) {
	out, err := g.run(ctx, repo, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	var list []Worktree
	for _, block := range strings.Split(out, "\n\n") {
		var w Worktree
		for _, line := range strings.Split(block, "\n") {
			key, val, _ := strings.Cut(line, " ")
			switch key {
			case "worktree":
				w.Path = val
			case "HEAD":
				w.Head = val
			case "branch":
				w.Branch = strings.TrimPrefix(val, "refs/heads/")
			case "detached":
				w.Detached = true
			}
		}
		if w.Path != "" {
			list = append(list, w)
		}
	}
	return list, nil
}

// AddDetachedWorktree checks ref out at path without creating a branch (used for the red proof).
func (g Git) AddDetachedWorktree(ctx context.Context, repo, path, ref string) error {
	_, err := g.run(ctx, repo, "worktree", "add", "--quiet", "--detach", path, ref)
	return err
}

// AddUntrustedWorktree checks out ref, third-party code such as a fork's PR head, at path
// without running anything the checkout could steer: no hooks (a relative core.hooksPath, as
// husky sets, may resolve inside it), no fsmonitor and no filter driver its .gitattributes
// names (git-lfs included).
func (g Git) AddUntrustedWorktree(ctx context.Context, repo, path, ref string) error {
	safe, err := g.untrustedConfig(ctx, repo)
	if err != nil {
		return err
	}
	_, err = g.runEnv(ctx, repo, []string{"GIT_LFS_SKIP_SMUDGE=1"}, append(safe, "worktree", "add", "--quiet", "--detach", path, ref)...)
	return err
}

// UntrustedStatus is Status for a checkout made by AddUntrustedWorktree, with the same guards:
// status may run fsmonitor and clean filters.
func (g Git) UntrustedStatus(ctx context.Context, dir string) (string, error) {
	safe, err := g.untrustedConfig(ctx, dir)
	if err != nil {
		return "", err
	}
	return g.runEnv(ctx, dir, []string{"GIT_LFS_SKIP_SMUDGE=1"}, append(safe, "status", "--porcelain")...)
}

// untrustedConfig is the -c overrides that turn off hooks, fsmonitor and every configured filter
// driver (system, global or local) for one command.
func (g Git) untrustedConfig(ctx context.Context, repo string) ([]string, error) {
	safe := []string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false"}
	out, err := g.run(ctx, repo, "config", "--name-only", "--get-regexp", `^filter\.`)
	var exit *exec.ExitError
	noFilter := errors.As(err, &exit) && exit.ExitCode() == 1
	if err != nil && !noFilter {
		return nil, err
	}
	seen := map[string]bool{}
	for _, key := range strings.Split(out, "\n") {
		name := strings.TrimPrefix(key, "filter.")
		i := strings.LastIndexByte(name, '.')
		if i < 0 || seen[name[:i]] {
			continue
		}
		name = name[:i]
		if strings.Contains(name, "=") { // -c would split it: refuse rather than leave it active
			return nil, fmt.Errorf("filter driver %q cannot be disabled", name)
		}
		seen[name] = true
		for _, k := range []string{"smudge=", "clean=", "process=", "required=false"} {
			safe = append(safe, "-c", "filter."+name+"."+k)
		}
	}
	return safe, nil
}

// RemoveWorktree drops a temporary worktree created by AddDetachedWorktree.
func (g Git) RemoveWorktree(ctx context.Context, repo, path string) error {
	_, err := g.run(ctx, repo, "worktree", "remove", "--force", path)
	return err
}

// PRRef is the local ref a PR/MR head is fetched into.
func PRRef(n int) string { return fmt.Sprintf("refs/delamain/pr/%d", n) }

// FetchPRHead fetches the head of PR/MR n into PRRef(n) and returns its SHA. Only that ref is
// forced ("+" on its refspec): a force-pushed PR moves it, and no other ref can change.
func (g Git) FetchPRHead(ctx context.Context, repo string, kind forge.Kind, n int) (string, error) {
	if n <= 0 {
		return "", fmt.Errorf("invalid PR/MR number %d", n)
	}
	var src string
	switch kind {
	case forge.GitHub:
		src = fmt.Sprintf("refs/pull/%d/head", n)
	case forge.GitLab:
		src = fmt.Sprintf("refs/merge-requests/%d/head", n)
	default:
		return "", fmt.Errorf("no PR/MR refspec for forge %q", kind)
	}
	if _, err := g.run(ctx, repo, "fetch", "--quiet", "--no-tags", "origin", "+"+src+":"+PRRef(n)); err != nil {
		return "", err
	}
	return g.run(ctx, repo, "rev-parse", "--verify", PRRef(n)+"^{commit}")
}

// FetchRef fetches branch ref from origin, updating origin/<ref>. The refspec is forced, as
// git's own remote.origin.fetch is: a base that was rebased or reset on the origin would
// otherwise be rejected as non-fast-forward and stop every review against it. Only the remote
// tracking ref moves; no local branch is touched.
func (g Git) FetchRef(ctx context.Context, repo, ref string) error {
	if ref == "" || strings.HasPrefix(ref, "-") {
		return fmt.Errorf("invalid ref %q", ref)
	}
	_, err := g.run(ctx, repo, "fetch", "--quiet", "--no-tags", "origin", "+refs/heads/"+ref+":refs/remotes/origin/"+ref)
	return err
}

// ShowFile returns path at ref; a path missing at an existing ref is fs.ErrNotExist.
func (g Git) ShowFile(ctx context.Context, repo, ref, path string) ([]byte, error) {
	if _, err := g.run(ctx, repo, "rev-parse", "--verify", "--quiet", "--end-of-options", ref+"^{commit}"); err != nil {
		return nil, fmt.Errorf("ref %s: %w", ref, err)
	}
	if _, err := g.run(ctx, repo, "cat-file", "-e", ref+":"+path); err != nil {
		return nil, fmt.Errorf("%s at %s: %w", path, ref, fs.ErrNotExist)
	}
	var out, errb bytes.Buffer
	cmd := exec.CommandContext(ctx, g.Binary, "-C", repo, "show", ref+":"+path) //nolint:gosec // ref verified above
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git show %s:%s: %w: %s", ref, path, err, strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), nil
}

// LineRange is a closed range of lines of the new file.
type LineRange struct{ Start, End int }

var hunkHeader = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@`)

// ChangedLines maps each file of base...HEAD to the lines added or modified on the head side,
// by the head path. Removal-only hunks, deleted and binary files have no lines. No external diff
// or textconv runs: the checkout's .gitattributes would pick which driver handles its files.
func (g Git) ChangedLines(ctx context.Context, dir, base string) (map[string][]LineRange, error) {
	out, err := g.run(ctx, dir, "-c", "core.quotePath=false", "diff", "--no-color", "--no-ext-diff", "--no-textconv", "--unified=0",
		"--find-renames", "--src-prefix=a/", "--dst-prefix=b/", base+"...HEAD")
	if err != nil {
		return nil, err
	}
	lines := map[string][]LineRange{}
	file, header := "", false // header: between "diff --git" and the first hunk
	for _, l := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(l, "diff --git "):
			file, header = "", true
		case header && strings.HasPrefix(l, "+++ "):
			file = diffPath(strings.TrimPrefix(l, "+++ "))
		case strings.HasPrefix(l, "@@ "):
			header = false
			if file == "" {
				continue
			}
			m := hunkHeader.FindStringSubmatch(l)
			if m == nil {
				return nil, fmt.Errorf("unexpected hunk header %q", l)
			}
			start, _ := strconv.Atoi(m[1])
			count := 1
			if m[2] != "" {
				count, _ = strconv.Atoi(m[2])
			}
			if count > 0 {
				lines[file] = append(lines[file], LineRange{Start: start, End: start + count - 1})
			}
		}
	}
	return lines, nil
}

// diffPath turns a "+++" target into the head path; /dev/null (a deletion) is "".
func diffPath(p string) string {
	p = strings.TrimSuffix(p, "\t")
	if unq, err := strconv.Unquote(p); err == nil && strings.HasPrefix(p, `"`) {
		p = unq
	}
	path, ok := strings.CutPrefix(p, "b/")
	if !ok {
		return ""
	}
	return path
}

// DeleteRef removes a ref delamain created under refs/delamain/; any other ref is refused.
func (g Git) DeleteRef(ctx context.Context, repo, ref string) error {
	if !strings.HasPrefix(ref, "refs/delamain/") {
		return fmt.Errorf("refusing to delete %q: not a delamain ref", ref)
	}
	_, err := g.run(ctx, repo, "update-ref", "-d", ref)
	return err
}

// CheckoutFiles overwrites files in dir with their version at ref.
func (g Git) CheckoutFiles(ctx context.Context, dir, ref string, files []string) error {
	_, err := g.run(ctx, dir, append([]string{"checkout", ref, "--"}, files...)...)
	return err
}

// DiscardEmptyWork removes the worktree and branch of a run that produced nothing: no commits
// beyond base and no uncommitted changes. Anything else is refused and left in place.
func (g Git) DiscardEmptyWork(ctx context.Context, repo, worktree, branch, base string) error {
	if err := GuardWorkBranch(branch, base); err != nil {
		return err
	}
	ahead, err := g.run(ctx, repo, "rev-list", "--count", base+".."+branch)
	if err != nil {
		return err
	}
	if ahead != "0" {
		return fmt.Errorf("branch %s has %s commit(s) beyond %s", branch, ahead, base)
	}
	if _, err := g.run(ctx, repo, "worktree", "remove", worktree); err != nil { // no --force: refuses dirty trees
		return err
	}
	_, err = g.run(ctx, repo, "branch", "-D", branch) // safe: verified to hold no commits of its own
	return err
}

// BranchExists reports whether a local branch exists.
func (g Git) BranchExists(ctx context.Context, repo, branch string) bool {
	_, err := g.run(ctx, repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	return err == nil
}

// Push publishes the work branch to origin under the same name. It never forces and re-checks
// the branch guard itself, so no caller can push a protected branch through it.
func (g Git) Push(ctx context.Context, dir, branch, base string) error {
	if err := GuardWorkBranch(branch, base); err != nil {
		return err
	}
	ref := "refs/heads/" + branch
	_, err := g.run(ctx, dir, "push", "--quiet", "origin", ref+":"+ref)
	return err
}

// IsAncestor reports whether ref descends from (or is) ancestor; an unknown commit is an error.
func (g Git) IsAncestor(ctx context.Context, dir, ancestor, ref string) (bool, error) {
	_, err := g.run(ctx, dir, "merge-base", "--is-ancestor", ancestor, ref)
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return false, nil
	}
	return err == nil, err
}

// ErrRemoteMoved means the branch on origin is no longer at the SHA recorded before the work.
var ErrRemoteMoved = errors.New("remote branch moved")

// RemoteSHA asks origin for the head of branch; a branch missing on origin is "".
func (g Git) RemoteSHA(ctx context.Context, repo, branch string) (string, error) {
	out, err := g.run(ctx, repo, "ls-remote", "--heads", "origin", "refs/heads/"+branch)
	sha, _, _ := strings.Cut(out, "\t")
	return sha, err
}

// PushFastForward publishes HEAD of dir to branch on origin only as a fast-forward of expected,
// the origin head recorded before the work started. A moved origin is ErrRemoteMoved and a HEAD
// that does not descend from expected is refused; the lease makes git itself recheck expected,
// so the push never overwrites anything. Protected and base branches are refused first.
func (g Git) PushFastForward(ctx context.Context, dir, branch, base, expected string) error {
	if err := GuardWorkBranch(branch, base); err != nil {
		return err
	}
	if expected == "" {
		return errors.New("no recorded origin head to fast-forward from")
	}
	if remote, err := g.RemoteSHA(ctx, dir, branch); err != nil {
		return err
	} else if remote != expected {
		return fmt.Errorf("%s is at %.12s on origin, not %.12s: %w", branch, remote, expected, ErrRemoteMoved)
	}
	if ok, err := g.IsAncestor(ctx, dir, expected, "HEAD"); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("HEAD does not descend from %.12s", expected)
	}
	ref := "refs/heads/" + branch
	if _, err := g.run(ctx, dir, "push", "--quiet", "--force-with-lease="+ref+":"+expected, "origin", "HEAD:"+ref); err != nil {
		if remote, rerr := g.RemoteSHA(ctx, dir, branch); rerr == nil && remote != expected {
			return fmt.Errorf("%w: %w", ErrRemoteMoved, err)
		}
		return err
	}
	return nil
}

// RemoteBranchExists asks origin itself, not the possibly stale remote-tracking refs.
func (g Git) RemoteBranchExists(ctx context.Context, repo, branch string) (bool, error) {
	out, err := g.run(ctx, repo, "ls-remote", "--heads", "origin", "refs/heads/"+branch)
	return out != "", err
}

// pushBlocked is a URL scheme with no git remote helper: rewriting push URLs to it makes every
// push fail while fetch keeps the real URL.
const pushBlocked = "delamain-push-blocked://"

// AgentEnv is the environment agents run with: git push to any remote fails and the forge CLIs
// lose their credentials, so only the orchestrator can publish work (after its own guards).
func AgentEnv() []string {
	prefixes := []string{"/", "https://", "http://", "ssh://", "git@", "git://", "file://"}
	env := []string{fmt.Sprintf("GIT_CONFIG_COUNT=%d", len(prefixes))}
	for i, p := range prefixes {
		env = append(env, fmt.Sprintf("GIT_CONFIG_KEY_%d=url.%s.pushInsteadOf", i, pushBlocked), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, p))
	}
	return append(env, "GH_TOKEN="+pushBlocked, "GITLAB_TOKEN="+pushBlocked)
}

// GuardWorkBranch refuses protected or base branches as the place agents write to.
func GuardWorkBranch(branch, base string) error {
	short := strings.TrimPrefix(base, "origin/")
	for _, p := range append([]string{short}, Protected...) {
		if branch == p {
			return fmt.Errorf("refusing to work on protected branch %q", branch)
		}
	}
	if strings.TrimSpace(branch) == "" || branch == "HEAD" {
		return errors.New("refusing to work on a detached or empty branch")
	}
	return nil
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// Slug turns a title into a kebab-case ASCII fragment of at most max runes.
func Slug(title string, max int) string {
	var b strings.Builder
	for _, r := range norm.NFD.String(strings.ToLower(title)) {
		if unicode.Is(unicode.Mn, r) {
			continue // drop accents: "ção" -> "cao"
		}
		b.WriteRune(r)
	}
	s := strings.Trim(nonSlug.ReplaceAllString(b.String(), "-"), "-")
	if len(s) > max {
		cut := s[:max]
		// Prefer a word boundary so branches never end in half a word.
		if s[max] != '-' {
			if i := strings.LastIndexByte(cut, '-'); i > 0 {
				cut = cut[:i]
			}
		}
		s = strings.TrimRight(cut, "-")
	}
	if s == "" {
		s = "task"
	}
	return s
}

// BranchName builds `fix/<slug>` for bugs and `feat/<slug>` otherwise.
func BranchName(kind, ref, title string) string {
	prefix := "feat"
	if kind == "bug" {
		prefix = "fix"
	}
	id := Slug(ref, 30) // task refs are up to "task-" + 24 chars; cutting them makes tasks collide
	return prefix + "/" + id + "-" + Slug(title, 40)
}

// harnessCredentials are the *_TOKEN keys the agent harness itself logs in with (Claude Code's
// OAuth token and Anthropic's auth token); without them every agent fails its first turn.
var harnessCredentials = []string{"CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_AUTH_TOKEN"}

// AgentEnviron is the whole environment an agent runs with: base without forge tokens or any
// *_TOKEN, *_PAT or *_SECRET (except harnessCredentials), plus AgentEnv's push block. Model
// provider *_API_KEY variables stay: agents need them to run.
func AgentEnviron(base []string) []string {
	env := make([]string, 0, len(base))
	for _, kv := range base {
		key, _, _ := strings.Cut(kv, "=")
		upper := strings.ToUpper(key)
		secret := strings.HasSuffix(upper, "_TOKEN") || strings.HasSuffix(upper, "_PAT") || strings.HasSuffix(upper, "_SECRET")
		if secret && !slices.Contains(harnessCredentials, key) {
			continue
		}
		env = append(env, kv)
	}
	return append(env, AgentEnv()...)
}
