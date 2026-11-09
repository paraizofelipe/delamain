package notify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/paraizo/delamain/internal/config"
)

// ntfyServer records every publish it receives and answers with status.
type ntfyServer struct {
	*httptest.Server
	mu     sync.Mutex
	got    []ntfyRequest
	status int
}

type ntfyRequest struct {
	Auth, Path, ContentType string
	Body                    ntfyPublish
}

func newNtfyServer(t *testing.T, status int) *ntfyServer {
	t.Helper()
	s := &ntfyServer{status: status}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body ntfyPublish
		if r.Method != http.MethodPost || r.URL.Path == "/v1/health" {
			if r.URL.Path == "/v1/health" {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"healthy":true}`)
				return
			}
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		s.got = append(s.got, ntfyRequest{Auth: r.Header.Get("Authorization"), Path: r.URL.Path, ContentType: r.Header.Get("Content-Type"), Body: body})
		s.mu.Unlock()
		w.WriteHeader(s.status)
		_, _ = io.WriteString(w, `{"error":"`+strings.Repeat("x", 10000)+`"}`)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *ntfyServer) requests() []ntfyRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.got)
}

func ntfyConfig(url string) config.Ntfy {
	return config.Ntfy{Enabled: true, URL: url, Topic: "dlm-topic", TokenEnv: "NTFY_TOKEN", Priority: config.NtfyPriority{Low: 2, Normal: 3, Critical: 4}}
}

func TestNtfyShouldPublishTitleBodyPriorityTagsClickAndActionsAsJSON(t *testing.T) {
	srv := newNtfyServer(t, http.StatusOK)
	b := newNtfy(ntfyConfig(srv.URL), "tk-secret", srv.Client())
	m := Message{
		Event: EventPROpened, RunID: 1, Title: "✅ PR opened · delamain #12", Body: "line 1\nline 2", Urgency: Critical,
		Actions: []Action{
			{Key: "default", Label: "Open PR", Target: "https://github.com/o/r/pull/31"},
			{Key: "open-pr", Label: "Open PR", Target: "https://github.com/o/r/pull/31"},
			{Key: "open-issue", Label: "Open issue", Target: "https://github.com/o/r/issues/12"},
			{Key: "open-report", Label: "Open report", Target: "/home/me/.local/state/delamain/runs/r/1/report.md"},
		},
	}
	if err := b.send(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	got := srv.requests()
	if len(got) != 1 {
		t.Fatalf("one publish, got %d", len(got))
	}
	r := got[0]
	if r.Path != "/" || r.ContentType != "application/json" || r.Auth != "Bearer tk-secret" {
		t.Fatalf("the JSON publish endpoint with the bearer token: %+v", r)
	}
	want := ntfyPublish{
		Topic: "dlm-topic", Title: "✅ PR opened · delamain #12", Message: "line 1\nline 2", Priority: 4,
		Tags:    []string{"delamain", "pr_opened"},
		Click:   "https://github.com/o/r/pull/31",
		Actions: []ntfyAction{{Action: "view", Label: "Open issue", URL: "https://github.com/o/r/issues/12"}},
	}
	if gotJSON, wantJSON := jsonOf(t, r.Body), jsonOf(t, want); gotJSON != wantJSON {
		t.Fatalf("publish body\n got %s\nwant %s", gotJSON, wantJSON)
	}
}

func jsonOf(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestNtfyShouldMapEveryUrgencyAndSendNoAuthWithoutAToken(t *testing.T) {
	srv := newNtfyServer(t, http.StatusOK)
	cfg := ntfyConfig(srv.URL)
	cfg.Priority = config.NtfyPriority{Low: 1, Normal: 3, Critical: 5}
	b := newNtfy(cfg, "", srv.Client())
	for _, u := range []Urgency{Low, Normal, Critical} {
		if err := b.send(context.Background(), Message{Event: EventHalted, Title: "t", Urgency: u}); err != nil {
			t.Fatal(err)
		}
	}
	got := srv.requests()
	var prios []int
	for _, r := range got {
		prios = append(prios, r.Body.Priority)
		if r.Auth != "" {
			t.Fatalf("no token means no Authorization header, got %q", r.Auth)
		}
		if r.Body.Click != "" || len(r.Body.Actions) != 0 {
			t.Fatalf("a toast without actions has no click: %+v", r.Body)
		}
	}
	if !slices.Equal(prios, []int{1, 3, 5}) {
		t.Fatalf("priorities = %v, want the configured 1 3 5", prios)
	}
}

func TestNtfyShouldKeepAtMostThreeDistinctHTTPSActions(t *testing.T) {
	srv := newNtfyServer(t, http.StatusOK)
	b := newNtfy(ntfyConfig(srv.URL), "", srv.Client())
	var acts []Action
	for i := range 6 {
		acts = append(acts, Action{Key: "k", Label: "L", Target: "https://x.test/" + string(rune('a'+i))})
	}
	acts = append(acts, Action{Key: "dup", Label: "L", Target: "https://x.test/a"}, Action{Key: "http", Label: "L", Target: "http://x.test/plain"})
	if err := b.send(context.Background(), Message{Event: EventHalted, Title: "t", Actions: acts}); err != nil {
		t.Fatal(err)
	}
	body := srv.requests()[0].Body
	if body.Click != "" {
		t.Fatalf("no default action means no click, got %q", body.Click)
	}
	var urls []string
	for _, a := range body.Actions {
		urls = append(urls, a.URL)
	}
	if !slices.Equal(urls, []string{"https://x.test/a", "https://x.test/b", "https://x.test/c"}) {
		t.Fatalf("ntfy takes three actions; https only, no duplicates, in order: %v", urls)
	}
}

func TestNtfyShouldReportTheStatusOfARefusedPublishWithoutTheToken(t *testing.T) {
	srv := newNtfyServer(t, http.StatusForbidden)
	b := newNtfy(ntfyConfig(srv.URL), "tk-secret", srv.Client())
	err := b.send(context.Background(), Message{Event: EventHalted, Title: "t"})
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("a refused publish is an error naming the status, got %v", err)
	}
	if strings.Contains(err.Error(), "tk-secret") || len(err.Error()) > 600 {
		t.Fatalf("the error never carries the token and keeps the body short: %d bytes", len(err.Error()))
	}
	if err := b.send(context.Background(), Message{Event: EventHalted, Title: "t"}); err == nil {
		t.Fatal("every refused publish is an error")
	}
	if err := b.close(); err != nil {
		t.Fatal(err)
	}
}

func TestNtfyShouldKeepTheResponseBodyOutOfAPublishError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"bad credentials: `+r.Header.Get("Authorization")+`"}`)
	}))
	t.Cleanup(srv.Close)
	b := newNtfy(ntfyConfig(srv.URL), "tk-secret", srv.Client())
	err := b.send(context.Background(), Message{Event: EventHalted, Title: "t"})
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("a refused publish is an error naming the status, got %v", err)
	}
	if strings.Contains(err.Error(), "tk-secret") || strings.Contains(err.Error(), "bad credentials") {
		t.Fatalf("a body echoing the request never reaches the error: %v", err)
	}
}

func TestNtfyShouldClickTheDefaultActionEvenWhenAnEarlierButtonOpensTheSamePage(t *testing.T) {
	srv := newNtfyServer(t, http.StatusOK)
	b := newNtfy(ntfyConfig(srv.URL), "", srv.Client())
	acts := []Action{
		{Key: "open-pr", Label: "PR", Target: "https://x.test/pr/1"},
		{Key: "open-issue", Label: "Issue", Target: "https://x.test/issue/2"},
		{Key: "default", Label: "Open", Target: "https://x.test/pr/1"},
	}
	if err := b.send(context.Background(), Message{Event: EventPROpened, Title: "t", Actions: acts}); err != nil {
		t.Fatal(err)
	}
	body := srv.requests()[0].Body
	if body.Click != "https://x.test/pr/1" {
		t.Fatalf("tapping the notification opens the default target, got click %q", body.Click)
	}
	if len(body.Actions) != 1 || body.Actions[0].URL != "https://x.test/issue/2" {
		t.Fatalf("the page the click opens is not repeated as a button: %+v", body.Actions)
	}
}

func TestMaskTopicShouldHideAShortTopicWhole(t *testing.T) {
	for topic, want := range map[string]string{"": "…", "ab": "…", "abc": "…", "abcd": "abc…", "dlm-secret": "dlm…"} {
		if got := maskTopic(topic); got != want {
			t.Fatalf("maskTopic(%q) = %q, want %q", topic, got, want)
		}
	}
}

func TestClientShouldFanOutToDesktopAndNtfyAndKeepGoingWhenOneFails(t *testing.T) {
	srv := newNtfyServer(t, http.StatusOK)
	bus := &fakeBus{err: errors.New("bus down")}
	s := dbusSys(bus)
	s.getenv = env(map[string]string{"DBUS_SESSION_BUS_ADDRESS": "unix:path=/run/user/1000/bus", "NTFY_TOKEN": "tk"})
	s.http = srv.Client()
	cfg := config.Notify{Enabled: true, Backend: "dbus", Events: []string{"halted"}, Ntfy: ntfyConfig(srv.URL)}
	c := newWith(cfg, Options{StateDir: t.TempDir(), Log: discardLog()}, s)
	defer func() { _ = c.Close() }()
	if got := c.Backends(); !slices.Equal(got, []string{"dbus", "ntfy"}) {
		t.Fatalf("Backends() = %v", got)
	}
	if c.Backend() != "dbus" {
		t.Fatalf("Backend() stays the desktop kind, got %q", c.Backend())
	}
	err := c.Send(context.Background(), Message{Event: EventHalted, RunID: 1, Title: "t"})
	if err == nil || !strings.Contains(err.Error(), "dbus") || !strings.Contains(err.Error(), "bus down") {
		t.Fatalf("the failing sink is reported by name: %v", err)
	}
	if got := srv.requests(); len(got) != 1 || got[0].Auth != "Bearer tk" {
		t.Fatalf("ntfy still gets the toast, with the token from the environment: %+v", got)
	}
	if err := c.Notify(context.Background(), Message{Event: EventStarted, RunID: 1, Title: "t"}); err != nil || len(srv.requests()) != 1 {
		t.Fatalf("a disabled event reaches no sink: %v %d", err, len(srv.requests()))
	}
}

func TestClientShouldSendToOneNamedBackend(t *testing.T) {
	srv := newNtfyServer(t, http.StatusOK)
	bus := &fakeBus{}
	s := dbusSys(bus)
	s.http = srv.Client()
	cfg := config.Notify{Enabled: true, Backend: "dbus", Events: allEvents(), Ntfy: ntfyConfig(srv.URL)}
	c := newWith(cfg, Options{StateDir: t.TempDir(), Log: discardLog()}, s)
	defer func() { _ = c.Close() }()
	if err := c.SendTo(context.Background(), "ntfy", Message{Event: EventHalted, Title: "t"}); err != nil {
		t.Fatal(err)
	}
	if len(srv.requests()) != 1 || len(bus.calls()) != 0 {
		t.Fatalf("only ntfy gets it: ntfy=%d dbus=%d", len(srv.requests()), len(bus.calls()))
	}
	if err := c.SendTo(context.Background(), "dbus", Message{Event: EventHalted, Title: "t"}); err != nil {
		t.Fatal(err)
	}
	if len(srv.requests()) != 1 || len(bus.calls()) != 1 {
		t.Fatalf("only dbus gets it: ntfy=%d dbus=%d", len(srv.requests()), len(bus.calls()))
	}
	if err := c.SendTo(context.Background(), "macos", Message{Event: EventHalted, Title: "t"}); err == nil || !strings.Contains(err.Error(), "macos") {
		t.Fatalf("an absent backend is an error naming it, got %v", err)
	}
}

func TestClientWithoutDesktopServerShouldStillPushToNtfy(t *testing.T) {
	srv := newNtfyServer(t, http.StatusOK)
	s := sys{goos: "linux", getenv: env(nil), http: srv.Client()}
	cfg := config.Notify{Enabled: true, Backend: "auto", Events: allEvents(), Ntfy: ntfyConfig(srv.URL)}
	c := newWith(cfg, Options{StateDir: t.TempDir(), Log: discardLog()}, s)
	defer func() { _ = c.Close() }()
	if c.Backend() != "none" || !slices.Equal(c.Backends(), []string{"ntfy"}) {
		t.Fatalf("Backend()=%q Backends()=%v", c.Backend(), c.Backends())
	}
	if err := c.Send(context.Background(), Message{Event: EventHalted, Title: "t"}); err != nil || len(srv.requests()) != 1 {
		t.Fatalf("ntfy alone delivers: %v %d", err, len(srv.requests()))
	}
}

func TestDiagnoseShouldProbeNtfyHealthWhenEnabled(t *testing.T) {
	srv := newNtfyServer(t, http.StatusOK)
	s := dbusSys(&fakeBus{})
	s.getenv = env(map[string]string{"DBUS_SESSION_BUS_ADDRESS": "unix:path=/run/user/1000/bus", "NTFY_TOKEN": "tk"})
	s.http = srv.Client()
	cfg := config.Notify{Enabled: true, Backend: "dbus", Ntfy: ntfyConfig(srv.URL)}
	p := diagnoseAll(context.Background(), cfg, s)
	if p.Backend != "dbus" || p.Ntfy == nil {
		t.Fatalf("probe = %+v", p)
	}
	if p.Ntfy.URL != srv.URL || p.Ntfy.Topic != "dlm…" || !p.Ntfy.Auth || p.Ntfy.Err != nil {
		t.Fatalf("ntfy probe = %+v", p.Ntfy)
	}

	cfg.Ntfy.Enabled = false
	if p := diagnoseAll(context.Background(), cfg, s); p.Ntfy != nil {
		t.Fatalf("disabled ntfy is not probed: %+v", p.Ntfy)
	}

	cfg.Ntfy.Enabled, cfg.Ntfy.URL, cfg.Ntfy.TokenEnv = true, "http://127.0.0.1:1", ""
	s.http = &http.Client{}
	p = diagnoseAll(context.Background(), cfg, s)
	if p.Ntfy == nil || p.Ntfy.Err == nil || p.Ntfy.Auth {
		t.Fatalf("an unreachable server is the probe's error, no token means no auth: %+v", p.Ntfy)
	}
}
