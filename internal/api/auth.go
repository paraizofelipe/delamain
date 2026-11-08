package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"strings"
	"sync"
	"time"
)

// Lockout of a client that keeps failing to log in.
const (
	authWindow  = 10 * time.Minute
	authLockout = 15 * time.Minute
)

// authGuard checks the bearer token in constant time and locks out a client that fails too
// often, before even comparing, so a guessing client learns nothing from the timing.
type authGuard struct {
	digest   [32]byte
	failures int
	now      func() time.Time

	mu      sync.Mutex
	clients map[string]*client
}

type client struct {
	fails       []time.Time
	lockedUntil time.Time
}

func newAuthGuard(token []byte, failures int, now func() time.Time) *authGuard {
	return &authGuard{digest: sha256.Sum256(token), failures: failures, now: now, clients: map[string]*client{}}
}

// check reports whether the Authorization header carries the token. retry > 0 means the client
// is locked out for that long, whatever it sent.
func (g *authGuard) check(clientKey, authorization string) (ok bool, retry time.Duration) {
	now := g.now()
	g.mu.Lock()
	defer g.mu.Unlock()
	c := g.clients[clientKey]
	if c == nil {
		c = &client{}
		g.clients[clientKey] = c
	}
	if now.Before(c.lockedUntil) {
		return false, c.lockedUntil.Sub(now)
	}
	if g.matches(authorization) {
		c.fails = nil
		return true, 0
	}
	c.fails = append(c.fails, now)
	cut := 0
	for cut < len(c.fails) && now.Sub(c.fails[cut]) > authWindow {
		cut++
	}
	c.fails = c.fails[cut:]
	if len(c.fails) >= g.failures {
		c.lockedUntil = now.Add(authLockout)
		c.fails = nil
		return false, authLockout
	}
	g.prune(now)
	return false, 0
}

// matches compares the presented token's digest with the expected one in constant time.
func (g *authGuard) matches(authorization string) bool {
	scheme, token, found := strings.Cut(authorization, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return false
	}
	presented := sha256.Sum256([]byte(strings.TrimSpace(token)))
	return subtle.ConstantTimeCompare(presented[:], g.digest[:]) == 1
}

// prune forgets clients with nothing held against them, so the map does not grow with every
// address that ever knocked.
func (g *authGuard) prune(now time.Time) {
	if len(g.clients) < 1024 {
		return
	}
	for k, c := range g.clients {
		if len(c.fails) == 0 && !now.Before(c.lockedUntil) {
			delete(g.clients, k)
		}
	}
}

// rateLimiter is a token bucket over every request.
type rateLimiter struct {
	rate, burst float64
	now         func() time.Time

	mu     sync.Mutex
	tokens float64
	last   time.Time
}

func newRateLimiter(rate float64, burst int, now func() time.Time) *rateLimiter {
	return &rateLimiter{rate: rate, burst: float64(burst), tokens: float64(burst), now: now, last: now()}
}

// allow takes a token, or reports that the bucket is empty.
func (l *rateLimiter) allow() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.tokens = min(l.burst, l.tokens+now.Sub(l.last).Seconds()*l.rate)
	l.last = now
	if l.tokens < 1 {
		return false
	}
	l.tokens--
	return true
}
