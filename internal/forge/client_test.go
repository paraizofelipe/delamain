package forge

import (
	"context"
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// counted serves h and reports how many requests reached the server; h gets the 1-based count.
func counted(t *testing.T, h func(n int, w http.ResponseWriter, r *http.Request)) (string, func() int) {
	t.Helper()
	var n atomic.Int64
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) { h(int(n.Add(1)), w, r) })
	return srv.URL, func() int { return int(n.Load()) }
}

// fresh never reuses connections, so net/http's own retry of a broken idle connection cannot
// add requests the forge client did not make.
func fresh() *http.Client { return &http.Client{Transport: &http.Transport{DisableKeepAlives: true}} }

// clients builds one client per forge against base, with a millisecond backoff.
func clients(t *testing.T, base string, opts ...Option) map[Kind]Forge {
	t.Helper()
	out := map[Kind]Forge{}
	for _, k := range []Kind{GitHub, GitLab} {
		path := "acme/api"
		f, err := New(Repo{k, "example.com", path}, "tok", append([]Option{WithBaseURL(base + "/"), WithHTTPClient(fresh()), WithBackoff(time.Millisecond)}, opts...)...)
		if err != nil {
			t.Fatal(err)
		}
		out[k] = f
	}
	return out
}

const okIssue = `{"number":1,"iid":1,"state":"open"}`

func hangUp(w http.ResponseWriter) {
	conn, _, err := w.(http.Hijacker).Hijack()
	if err == nil {
		_ = conn.Close()
	}
}

func TestClientShouldRetryTransientErrorsThreeTimes(t *testing.T) {
	for _, kind := range []Kind{GitHub, GitLab} {
		base, calls := counted(t, func(n int, w http.ResponseWriter, r *http.Request) {
			switch n {
			case 1:
				hangUp(w) // network error
			case 2:
				writeStatus(w, http.StatusBadGateway, `{"message":"bad gateway"}`)
			default:
				writeJSON(w, okIssue)
			}
		})
		if is, err := clients(t, base)[kind].Issue(context.Background(), 1); err != nil || is.Number != 1 {
			t.Fatalf("%s: a blip must not fail the call: issue=%+v err=%v calls=%d", kind, is, err, calls())
		}
		if calls() != 3 {
			t.Fatalf("%s: calls = %d, want 3", kind, calls())
		}

		for _, status := range []int{http.StatusServiceUnavailable, http.StatusGatewayTimeout} {
			base, calls = counted(t, func(_ int, w http.ResponseWriter, r *http.Request) {
				writeStatus(w, status, `{"message":"down"}`)
			})
			if _, err := clients(t, base)[kind].Issue(context.Background(), 1); err == nil || calls() != 3 {
				t.Fatalf("%s: a persistent %d fails after 3 attempts: err=%v calls=%d", kind, status, err, calls())
			}
		}
	}
}

func TestClientShouldNotRetryClientErrors(t *testing.T) {
	for _, kind := range []Kind{GitHub, GitLab} {
		for _, status := range []int{400, 401, 404, 422, 500} {
			base, calls := counted(t, func(_ int, w http.ResponseWriter, r *http.Request) {
				writeStatus(w, status, `{"message":"rejected"}`)
			})
			if _, err := clients(t, base)[kind].Issue(context.Background(), 1); err == nil || calls() != 1 {
				t.Errorf("%s: a %d fails at once: err=%v calls=%d", kind, status, err, calls())
			}
		}
	}
}

func TestGitHubClientShouldHonorRetryAfterOnSecondaryRateLimit(t *testing.T) {
	var waited time.Duration
	var first time.Time
	base, calls := counted(t, func(n int, w http.ResponseWriter, r *http.Request) {
		if n == 1 {
			first = time.Now()
			w.Header().Set("Retry-After", "1")
			writeStatus(w, http.StatusForbidden, `{"message":"You have exceeded a secondary rate limit. Please wait a few minutes before you try again."}`)
			return
		}
		waited = time.Since(first)
		writeJSON(w, okIssue)
	})
	if _, err := clients(t, base)[GitHub].Issue(context.Background(), 1); err != nil || calls() != 2 {
		t.Fatalf("err=%v calls=%d", err, calls())
	}
	if waited < 900*time.Millisecond {
		t.Fatalf("Retry-After: 1 must be honored, retried after %v", waited)
	}

	// Without Retry-After, the secondary limit message still asks for a backed-off retry.
	base, calls = counted(t, func(n int, w http.ResponseWriter, r *http.Request) {
		if n == 1 {
			writeStatus(w, http.StatusTooManyRequests, `{"message":"secondary rate limit"}`)
			return
		}
		writeJSON(w, okIssue)
	})
	if _, err := clients(t, base)[GitHub].Issue(context.Background(), 1); err != nil || calls() != 2 {
		t.Fatalf("err=%v calls=%d", err, calls())
	}
}

func TestGitHubClientShouldGiveUpWhenRetryAfterExceedsTheCap(t *testing.T) {
	base, calls := counted(t, func(_ int, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", strconv.Itoa(3600))
		writeStatus(w, http.StatusForbidden, `{"message":"You have exceeded a secondary rate limit."}`)
	})
	start := time.Now()
	if _, err := clients(t, base)[GitHub].Issue(context.Background(), 1); err == nil || calls() != 1 {
		t.Fatalf("an hour-long Retry-After is not waited for: err=%v calls=%d", err, calls())
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("gave up after %v", d)
	}
}

func TestClientShouldStopRetryingWhenContextIsCancelled(t *testing.T) {
	for _, kind := range []Kind{GitHub, GitLab} {
		base, calls := counted(t, func(_ int, w http.ResponseWriter, r *http.Request) {
			writeStatus(w, http.StatusServiceUnavailable, `{"message":"down"}`)
		})
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		start := time.Now()
		_, err := clients(t, base, WithBackoff(time.Hour))[kind].Issue(ctx, 1)
		cancel()
		if err == nil || calls() != 1 {
			t.Fatalf("%s: shutdown must not wait out the backoff: err=%v calls=%d", kind, err, calls())
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Fatalf("%s: returned after %v", kind, d)
		}
	}
}

func TestClientShouldTimeOutSlowResponses(t *testing.T) {
	for _, kind := range []Kind{GitHub, GitLab} {
		base, _ := counted(t, func(_ int, w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(10 * time.Second):
			}
		})
		start := time.Now()
		_, err := clients(t, base, WithTimeout(100*time.Millisecond))[kind].Issue(context.Background(), 1)
		if err == nil {
			t.Fatalf("%s: a hung forge must fail", kind)
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Fatalf("%s: timed out after %v", kind, d)
		}
	}
}

func TestIssueShouldFailOnAnEmptyResponse(t *testing.T) {
	for _, kind := range []Kind{GitHub, GitLab} {
		base, _ := counted(t, func(_ int, w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
		if is, err := clients(t, base)[kind].Issue(context.Background(), 1); err == nil {
			t.Errorf("%s: an empty answer is not an issue: %+v", kind, is)
		}
	}
}
