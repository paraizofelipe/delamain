package cli

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/paraizo/delamain/internal/board"
	"github.com/paraizo/delamain/internal/config"
	"github.com/paraizo/delamain/internal/store"
)

func TestBoardShouldRejectBadFlags(t *testing.T) {
	for _, args := range [][]string{
		{"board", "--addr", "0.0.0.0:7777"},
		{"board", "--addr", ":7777"},
		{"board", "--addr", "192.168.0.10:7777"},
		{"board", "--addr", "localhost"},
		{"board", "--interval", "0s"},
		{"board", "--limit", "-1"},
	} {
		if out, err := execute(t, args...); err == nil {
			t.Errorf("%v: no error\n%s", args, out)
		}
	}
	if err := (boardOptions{addr: "0.0.0.0:7777", interval: time.Second, allowRemote: true}).validate(); err != nil {
		t.Fatalf("--allow-remote must accept any address: %v", err)
	}
	for _, addr := range []string{"127.0.0.1:0", "localhost:7777", "[::1]:7777"} {
		if err := (boardOptions{addr: addr, interval: time.Second}).validate(); err != nil {
			t.Errorf("%s: %v", addr, err)
		}
	}
}

func TestBoardShouldServeTheRunsOfTheStore(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	planning := f.run(t, "#1", "", store.StatePlan)
	orphan := f.run(t, "#2", "", store.StateImplement)
	if ok, err := f.st.ClaimRun(ctx, orphan.ID, os.Getpid(), deadPID); err != nil || !ok {
		t.Fatal(err)
	}
	done := f.run(t, "#3", "", store.StateRecorded)
	done.Reason = "approved: PR aberto"
	if err := f.st.UpdateRun(ctx, done); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Enqueue(ctx, f.repo, 4, "Next"); err != nil {
		t.Fatal(err)
	}

	in, closeIn, err := openInspector(ctx, func() (config.Config, error) { return config.Load(f.cfgFile, os.Getenv) })
	if err != nil {
		t.Fatal(err)
	}
	defer closeIn()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() {
		served <- serveBoard(ctx, ln, boardSource{in: in}, boardOptions{interval: 50 * time.Millisecond, limit: 20})
	}()

	res, err := http.Get("http://" + ln.Addr().String() + "/api/snapshot") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	var snap board.Snapshot
	if err := json.Unmarshal(body, &snap); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	got := map[string]string{}
	for _, col := range snap.Columns {
		for _, c := range col.Cards {
			got[c.Key] = col.Key
		}
	}
	want := map[string]string{
		"run-" + strconv.FormatInt(planning.ID, 10): board.ColPlan,
		"run-" + strconv.FormatInt(orphan.ID, 10):   board.ColHuman,
		"run-" + strconv.FormatInt(done.ID, 10):     board.ColDone,
		"queue-1":                                   board.ColQueue,
	}
	for k, col := range want {
		if got[k] != col {
			t.Errorf("%s in %q, want %q (board: %v)", k, got[k], col, got)
		}
	}
	if !strings.Contains(string(body), `"today_level":"ok"`) {
		t.Errorf("no day's spend in %s", body)
	}

	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("serveBoard: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serveBoard did not stop with its context")
	}
}
