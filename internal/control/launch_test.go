package control

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/paraizo/delamain/internal/store"
)

// launchScript is an executable at dir/delamain that runs body, as the delamain a launch runs.
func launchScript(t *testing.T, dir, body string) func() (string, error) {
	t.Helper()
	exe := filepath.Join(dir, "delamain")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"+body), 0o700); err != nil { //nolint:gosec // a test script
		t.Fatal(err)
	}
	return func() (string, error) { return exe, nil }
}

// launchLogs are the logs written under state.
func launchLogs(t *testing.T, state string) []string {
	t.Helper()
	logs, err := filepath.Glob(filepath.Join(state, "launch", "*"))
	if err != nil {
		t.Fatal(err)
	}
	return logs
}

func TestLauncherCommandShouldBeAnArgumentListInItsOwnSessionWithoutTheStrippedEnv(t *testing.T) {
	l := Launcher{StripEnv: []string{"DELAMAIN_API_TOKEN"}, Environ: func() []string {
		return []string{"PATH=/bin", "DELAMAIN_API_TOKEN=secret", "GH_TOKEN=gh", "DELAMAIN_API_TOKEN_ENV=X"}
	}}
	cmd := l.Command("/bin/delamain", os.Stderr, "run", "--issue", "42", "--repo", "/src/a b; rm -rf ~")
	if cmd.Path != "/bin/delamain" || !slices.Equal(cmd.Args, []string{"/bin/delamain", "run", "--issue", "42", "--repo", "/src/a b; rm -rf ~"}) {
		t.Fatalf("the run is the binary with a list of arguments, no shell: %q %q", cmd.Path, cmd.Args)
	}
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setsid {
		t.Fatal("the run starts a session of its own")
	}
	if cmd.Stdin != nil || cmd.Stdout != os.Stderr || cmd.Stderr != os.Stderr {
		t.Fatalf("the run reads nothing and writes to the log alone: %v %v %v", cmd.Stdin, cmd.Stdout, cmd.Stderr)
	}
	if !slices.Equal(cmd.Env, []string{"PATH=/bin", "GH_TOKEN=gh", "DELAMAIN_API_TOKEN_ENV=X"}) {
		t.Fatalf("the stripped variable is gone and the others stay: %v", cmd.Env)
	}
	if cmd := (Launcher{}).Command("/bin/delamain", os.Stderr, "run"); cmd.Env != nil {
		t.Fatalf("nothing to strip means the environment is inherited as is: %v", cmd.Env)
	}
}

func TestLauncherShouldRunTheBinaryDetachedAndLogItsOutput(t *testing.T) {
	dir := t.TempDir()
	state, args := filepath.Join(dir, "state"), filepath.Join(dir, "args")
	repo := filepath.Join(dir, "a b; touch pwned")
	t.Setenv("DELAMAIN_API_TOKEN", "secret")
	exe := launchScript(t, dir, "printf '%s\\n' \"$@\" > '"+args+"'\necho to stdout\necho \"token=[$DELAMAIN_API_TOKEN]\" >&2\nread line || echo stdin is empty\n")
	l := Launcher{StateDir: state, Executable: exe, StripEnv: []string{"DELAMAIN_API_TOKEN"}}
	path, err := l.StartIssue(repo, 42)
	if err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(args); err != nil || string(b) != "run\n--issue\n42\n--repo\n"+repo+"\n" { //nolint:gosec // inside the test's temp dir
		t.Fatalf("the run gets the issue and the repo as arguments: %q %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "pwned")); err == nil {
		t.Fatal("the repo never goes through a shell")
	}
	logs := launchLogs(t, state)
	if len(logs) != 1 || logs[0] != path || !strings.HasSuffix(path, "-issue-42.log") {
		t.Fatalf("one log in launch/, named after the issue, is returned: %v %q", logs, path)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "to stdout\ntoken=[]\nstdin is empty\n" { //nolint:gosec // inside the test's temp dir
		t.Fatalf("stdout and stderr go to the log, the token is not in the environment, stdin is empty: %q %v", b, err)
	}
	for p, want := range map[string]os.FileMode{filepath.Join(state, "launch"): 0o700, path: 0o600} {
		if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != want {
			t.Fatalf("%s is %v, want %v: %v", p, fi.Mode().Perm(), want, err)
		}
	}
	if path, err := l.StartReview(repo, 12); err != nil || !strings.HasSuffix(path, "-review-12.log") {
		t.Fatalf("a review logs to -review-<pr>.log: %q %v", path, err)
	}
	if b, _ := os.ReadFile(args); string(b) != "review\n--pr\n12\n--repo\n"+repo+"\n" { //nolint:gosec // inside the test's temp dir
		t.Fatalf("the review gets the PR and the repo as arguments: %q", b)
	}
}

func TestLauncherShouldReportTheExitCodeAndTheLog(t *testing.T) {
	for _, c := range []struct {
		code int
		want string
	}{
		{5, "issue #7 not started: maximum parallelism reached; see "},
		{4, "issue #7 needs more information (exit 4); see "},
		{2, "issue #7 failed: exit 2; see "},
	} {
		dir := t.TempDir()
		state := filepath.Join(dir, "state")
		l := Launcher{StateDir: state, Executable: launchScript(t, dir, "exit "+strconv.Itoa(c.code)+"\n")}
		path, err := l.StartIssue(dir, 7)
		exit, ok := errors.AsType[StartExit](err)
		if !ok || exit.Kind != OpStartRun || exit.Issue != 7 || exit.Code != c.code || exit.Log != path {
			t.Fatalf("exit %d is a StartExit with its code and the log: %+v %v", c.code, exit, err)
		}
		if got := err.Error(); got != c.want+path {
			t.Errorf("exit %d says %q, want %q", c.code, got, c.want+path)
		}
	}
	dir := t.TempDir()
	l := Launcher{StateDir: filepath.Join(dir, "state"), Executable: launchScript(t, dir, "exit 1\n")}
	path, err := l.StartReview(dir, 12)
	if exit, ok := errors.AsType[StartExit](err); !ok || exit.Kind != OpStartReview || exit.PR != 12 || err.Error() != "review of PR #12 failed: exit 1; see "+path {
		t.Fatalf("a review's exit: %+v %v", exit, err)
	}
	missing := Launcher{StateDir: filepath.Join(dir, "state"), Executable: func() (string, error) { return filepath.Join(dir, "missing"), nil }}
	if _, err := missing.StartIssue(dir, 7); err == nil || errors.As(err, new(StartExit)) {
		t.Fatalf("a run that cannot start is an error of its own: %v", err)
	}
}

// waitOp waits for operation id to leave running, through the subscription ch.
func waitOp(t *testing.T, ops *Operations, ch <-chan Operation, id string) Operation {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case op := <-ch:
			if op.ID == id && op.Status != OpRunning {
				return op
			}
		case <-deadline:
			got, _ := ops.Get(id)
			t.Fatalf("the operation never ended: %+v", got)
		}
	}
}

func TestStartIssueShouldFollowTheLaunchAsAnOperation(t *testing.T) {
	f := newFixture(t)
	f.cfg.Daemon.Repos = []string{f.repo}
	dir := t.TempDir()
	exe := launchScript(t, dir, "echo started\nexit 5\n")
	acts := New(Deps{Cfg: f.cfg, Store: f.st, Launcher: &Launcher{StateDir: f.cfg.Paths.StateDir, Executable: exe}})
	ch, unsub := acts.Operations().Subscribe()
	defer unsub()

	op, err := acts.StartIssue(context.Background(), f.repo+"/", 7)
	if err != nil {
		t.Fatal(err)
	}
	if op.Status != OpRunning || op.Kind != OpStartRun || op.Repo != f.repo || op.Issue != 7 || op.ID == "" || op.StartedAt.IsZero() {
		t.Fatalf("the operation starts running, with the cleaned repo: %+v", op)
	}
	done := waitOp(t, acts.Operations(), ch, op.ID)
	if done.Status != OpFailed || done.ExitCode == nil || *done.ExitCode != 5 || done.EndedAt == nil {
		t.Fatalf("exit 5 fails the operation with its code: %+v", done)
	}
	if !strings.HasPrefix(done.Error, "issue #7 not started: maximum parallelism reached") || done.LogTail != "started\n" || !strings.HasSuffix(done.LogPath, "-issue-7.log") {
		t.Fatalf("the operation says why and keeps the log: %+v", done)
	}
	if got, ok := acts.Operations().Get(op.ID); !ok || got.Status != OpFailed {
		t.Fatalf("Get returns the ended operation: %+v %v", got, ok)
	}
	if list := acts.Operations().List(0); len(list) != 1 || list[0].ID != op.ID {
		t.Fatalf("List has it: %+v", list)
	}
}

func TestStartIssueShouldRefuseAnUnknownRepoAnActiveRunAndABadNumber(t *testing.T) {
	f := newFixture(t)
	f.cfg.Daemon.Repos = []string{f.repo}
	acts := New(Deps{Cfg: f.cfg, Store: f.st})
	ctx := context.Background()
	if _, err := acts.StartIssue(ctx, "/elsewhere", 7); !errors.Is(err, ErrRepoNotAllowed) || !strings.Contains(err.Error(), "/elsewhere") {
		t.Fatalf("a repo outside the allowlist is refused naming it: %v", err)
	}
	if _, err := acts.StartIssue(ctx, f.repo, 0); !errors.Is(err, ErrInvalid) {
		t.Fatalf("issue 0 is invalid: %v", err)
	}
	rec := f.run(t, "#7", "", store.StateImplement, store.KindIssue)
	_, err := acts.StartIssue(ctx, f.repo, 7)
	var re *RunError
	if !errors.Is(err, ErrRunActive) || !errors.As(err, &re) || re.ID != rec.ID || err.Error() != "#7 already has active run 1 (implement)" {
		t.Fatalf("an active run of the issue refuses the start: %v", err)
	}
	if len(acts.Operations().List(0)) != 0 {
		t.Fatal("a refused start leaves no operation")
	}
	if _, err := acts.StartReview(ctx, f.repo, 3); !errors.Is(err, ErrReviewDisabled) {
		t.Fatalf("reviews off refuse a review: %v", err)
	}
}

func TestStartIssueShouldFindTheRunTheLaunchCreated(t *testing.T) {
	f := newFixture(t)
	f.cfg.Daemon.Repos = []string{f.repo}
	dir := t.TempDir()
	release := filepath.Join(dir, "release")
	exe := launchScript(t, dir, "while [ ! -e '"+release+"' ]; do sleep 0.02; done\n")
	acts := New(Deps{
		Cfg: f.cfg, Store: f.st, RunIDPoll: 5 * time.Millisecond,
		Launcher: &Launcher{StateDir: f.cfg.Paths.StateDir, Executable: exe},
	})
	ch, unsub := acts.Operations().Subscribe()
	defer unsub()
	op, err := acts.StartIssue(context.Background(), f.repo, 7)
	if err != nil {
		t.Fatal(err)
	}
	rec := f.run(t, "#7", "", store.StateImplement, store.KindIssue) // what the child would create
	deadline := time.After(10 * time.Second)
	for found := false; !found; {
		select {
		case got := <-ch:
			found = got.ID == op.ID && got.RunID == rec.ID && got.Status == OpRunning
		case <-deadline:
			t.Fatal("the run id never reached the operation while it ran")
		}
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	done := waitOp(t, acts.Operations(), ch, op.ID)
	if done.Status != OpSucceeded || done.RunID != rec.ID || done.ExitCode == nil || *done.ExitCode != 0 || done.Error != "" {
		t.Fatalf("exit 0 succeeds with the run id kept: %+v", done)
	}
}

func TestStartReviewShouldFindARunThatEndedBeforeTheFirstPoll(t *testing.T) {
	f := newFixture(t)
	f.cfg.Daemon.Repos = []string{f.repo}
	f.cfg.Review.Enabled = true
	dir := t.TempDir()
	release := filepath.Join(dir, "release")
	// The child exits only once the test has seeded the run it would have created.
	exe := launchScript(t, dir, "while [ ! -e '"+release+"' ]; do sleep 0.02; done\n")
	acts := New(Deps{
		Cfg: f.cfg, Store: f.st, RunIDPoll: time.Hour, // never polls: the id comes from the lookup at the end
		Launcher: &Launcher{StateDir: f.cfg.Paths.StateDir, Executable: exe},
	})
	ch, unsub := acts.Operations().Subscribe()
	defer unsub()
	op, err := acts.StartReview(context.Background(), f.repo, 12)
	if err != nil {
		t.Fatal(err)
	}
	rec := f.run(t, "pr:12", "", store.StateRecorded, store.KindReview) // finished before anyone looked
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	done := waitOp(t, acts.Operations(), ch, op.ID)
	if done.Kind != OpStartReview || done.PR != 12 || done.Status != OpSucceeded || done.RunID != rec.ID {
		t.Fatalf("the finished run is found by repo and ref: %+v", done)
	}
}

func TestRecoverOperationShouldFinalizeInTheBackground(t *testing.T) {
	f := newFixture(t)
	n, built := &recorder{}, new(int)
	rec := f.run(t, "pr:7", "review/7", store.StateReview, store.KindReview)
	f.orphan(t, rec)
	acts := f.actions(n, built)
	ch, unsub := acts.Operations().Subscribe()
	defer unsub()
	op, err := acts.RecoverOperation(context.Background(), rec.ID)
	if err != nil || op.Kind != OpRecover || op.RunID != rec.ID || op.Status != OpRunning {
		t.Fatalf("recover starts as an operation: %+v %v", op, err)
	}
	done := waitOp(t, acts.Operations(), ch, op.ID)
	if done.Status != OpSucceeded || done.Result != "halted" || done.ExitCode != nil || !strings.HasSuffix(done.LogPath, "/logs/delamain.log") {
		t.Fatalf("the recover ends with how the run ended: %+v", done)
	}
	if got := f.get(t, rec.ID); got.State != store.StateRecorded {
		t.Fatalf("the orphan is recorded: %+v", got)
	}
	if len(n.sent) != 1 {
		t.Fatalf("one recover toast: %+v", n.sent)
	}
	if _, err := acts.RecoverOperation(context.Background(), rec.ID); !errors.Is(err, ErrRunFinished) {
		t.Fatalf("a finished run is refused before any operation: %v", err)
	}
	live := f.run(t, "#2", "", store.StateImplement, store.KindIssue)
	if _, err := acts.RecoverOperation(context.Background(), live.ID); !errors.Is(err, ErrOwnerAlive) {
		t.Fatalf("a live owner is refused: %v", err)
	}
	noRunner := New(Deps{Cfg: f.cfg, Store: f.st})
	orphan := f.run(t, "#3", "", store.StateImplement, store.KindIssue)
	f.orphan(t, orphan)
	if _, err := noRunner.RecoverOperation(context.Background(), orphan.ID); !errors.Is(err, ErrNoRunner) {
		t.Fatalf("no runner fails now, not in the background: %v", err)
	}
}

func TestOperationsShouldKeepARingAndNotBlockOnASlowSubscriber(t *testing.T) {
	ops := NewOperations()
	ch, unsub := ops.Subscribe()
	defer unsub()
	var first string
	for i := range opRing + 10 {
		op := ops.add(Operation{Kind: OpStartRun, Issue: i})
		if i == 0 {
			first = op.ID
		}
	}
	if list := ops.List(0); len(list) != opRing || list[0].Issue != opRing+9 || list[len(list)-1].Issue != 10 {
		t.Fatalf("the ring keeps the newest %d, newest first: %d %d..%d", opRing, len(list), list[0].Issue, list[len(list)-1].Issue)
	}
	if _, ok := ops.Get(first); ok {
		t.Fatal("the oldest fell out of the ring")
	}
	if list := ops.List(3); len(list) != 3 {
		t.Fatalf("List honors its limit: %d", len(list))
	}
	if len(ch) != cap(ch) {
		t.Fatalf("a subscriber that does not read keeps the first %d changes and loses the rest: %d", cap(ch), len(ch))
	}
	ids := map[string]bool{}
	for _, op := range ops.List(0) {
		if ids[op.ID] {
			t.Fatalf("ids are unique: %s", op.ID)
		}
		ids[op.ID] = true
	}
}

func TestAllowedReposShouldJoinDaemonAndAPIReposCleaned(t *testing.T) {
	f := newFixture(t)
	f.cfg.Daemon.Repos = []string{"/b/", "/a"}
	f.cfg.API.Repos = []string{"/a/", "/c/./d", ""}
	acts := New(Deps{Cfg: f.cfg, Store: f.st})
	if got := acts.AllowedRepos(); !slices.Equal(got, []string{"/a", "/b", "/c/d"}) {
		t.Fatalf("AllowedRepos = %v", got)
	}
}

// Two requests in a row for the same issue reach the store before the first child creates its
// run: the reservation, not the store, must refuse the second.
func TestStartIssueShouldRefuseASecondStartOfTheIssueWhileTheFirstIsLaunching(t *testing.T) {
	f := newFixture(t)
	f.cfg.Daemon.Repos = []string{f.repo}
	dir := t.TempDir()
	release := filepath.Join(dir, "release")
	exe := launchScript(t, dir, "while [ ! -e '"+release+"' ]; do sleep 0.02; done\n")
	acts := New(Deps{Cfg: f.cfg, Store: f.st, RunIDPoll: time.Hour, Launcher: &Launcher{StateDir: f.cfg.Paths.StateDir, Executable: exe}})
	ch, unsub := acts.Operations().Subscribe()
	defer unsub()
	ctx := context.Background()
	first, err := acts.StartIssue(ctx, f.repo, 7)
	if err != nil {
		t.Fatal(err)
	}
	_, err = acts.StartIssue(ctx, f.repo+"/", 7)
	var re *RunError
	if !errors.Is(err, ErrRunActive) || !errors.As(err, &re) || re.ID != 0 || re.Operation != first.ID {
		t.Fatalf("the second start is refused naming the pending one: %v", err)
	}
	if err.Error() != "#7 is already being started by operation "+first.ID {
		t.Fatalf("the message says the issue is being started: %q", err)
	}
	other, err := acts.StartIssue(ctx, f.repo, 8)
	if err != nil {
		t.Fatalf("another issue starts meanwhile: %v", err)
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitOp(t, acts.Operations(), ch, first.ID) // the other one may end first: its event is gone
	deadline := time.After(10 * time.Second)
	for {
		op, err := acts.StartIssue(ctx, f.repo, 7)
		if err == nil {
			waitOp(t, acts.Operations(), ch, op.ID)
			if got, _ := acts.Operations().Get(other.ID); got.Status != OpSucceeded {
				t.Fatalf("the other issue's launch ended too: %+v", got)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatalf("once the launch ended the issue can start again: %v", err)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestStartIssueShouldLeaveTheExitCodeOutWhenTheProcessNeverStarted(t *testing.T) {
	f := newFixture(t)
	f.cfg.Daemon.Repos = []string{f.repo}
	missing := func() (string, error) { return "", errors.New("no delamain binary") }
	acts := New(Deps{Cfg: f.cfg, Store: f.st, Launcher: &Launcher{StateDir: f.cfg.Paths.StateDir, Executable: missing}})
	ch, unsub := acts.Operations().Subscribe()
	defer unsub()
	op, err := acts.StartIssue(context.Background(), f.repo, 7)
	if err != nil {
		t.Fatal(err)
	}
	done := waitOp(t, acts.Operations(), ch, op.ID)
	if done.Status != OpFailed || done.ExitCode != nil || !strings.Contains(done.Error, "no delamain binary") {
		t.Fatalf("a launch that never ran fails without an exit code a client could read as 0: %+v", done)
	}
}

func TestLaunchShouldGiveEveryLaunchALogOfItsOwnEvenInTheSameSecond(t *testing.T) {
	state := t.TempDir()
	l := Launcher{StateDir: state, Executable: launchScript(t, t.TempDir(), "echo \"$@\"\n")}
	a, errA := l.StartIssue("/repo/a", 42)
	b, errB := l.StartIssue("/repo/b", 42)
	if errA != nil || errB != nil {
		t.Fatal(errA, errB)
	}
	if a == b || !strings.HasSuffix(a, "-issue-42.log") || !strings.HasSuffix(b, "-issue-42.log") {
		t.Fatalf("each launch has its own log, named after the issue: %q %q", a, b)
	}
	for path, repo := range map[string]string{a: "/repo/a", b: "/repo/b"} {
		got, err := os.ReadFile(path) //nolint:gosec // a test log
		if err != nil || strings.TrimSpace(string(got)) != "run --issue 42 --repo "+repo {
			t.Fatalf("%s holds only its own launch: %q %v", path, got, err)
		}
	}
}
