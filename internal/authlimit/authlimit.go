// Package authlimit protects the public, pre-authentication API surface
// (/api/auth/, /api/users/, /api/accounts/) against online brute-force and
// bcrypt-driven CPU exhaustion. It provides per-IP token buckets (strict for
// login/register, generous for everything else), a per-email login throttle
// so IP rotation cannot keep hammering one account, and a global concurrency
// cap on bcrypt operations as a DoS backstop.
//
// The deployment is single-replica, so in-memory buckets are sufficient; a
// shared/distributed limiter is intentionally out of scope.
package authlimit

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"strings"
	"sync"
	"time"
)

// Config carries the limiter rates. Rates are requests per minute; a value
// <= 0 disables that limiter. BcryptConcurrency <= 0 means no cap.
type Config struct {
	LoginPerMinIP     int // POST /api/auth/login, per client IP
	LoginPerMinEmail  int // POST /api/auth/login, per target email
	RegisterPerMinIP  int // POST /api/auth/register + /api/accounts/register, per client IP
	GeneralPerMinIP   int // all other requests on the public mounts, per client IP
	BcryptConcurrency int // global cap on simultaneous bcrypt operations
}

// pruneInterval is how often idle buckets are evicted; idleCutoff is the
// minimum idle time before eviction. Both match the admin sessionLimiter.
const (
	pruneInterval = 1 * time.Minute
	idleCutoff    = 10 * time.Minute
)

type bucket struct {
	tokens     float64
	lastRefill time.Time
	lastAccess time.Time
}

// keyedLimiter is a string-keyed token-bucket limiter with an injectable
// clock, modeled on the admin package's sessionLimiter.
type keyedLimiter struct {
	mu         sync.Mutex
	buckets    map[string]*bucket
	capacity   float64
	refillRate float64 // tokens per second
	nowFn      func() time.Time
}

func newKeyedLimiter(perMinute int, nowFn func() time.Time) *keyedLimiter {
	return &keyedLimiter{
		buckets:    make(map[string]*bucket),
		capacity:   float64(perMinute),
		refillRate: float64(perMinute) / 60.0,
		nowFn:      nowFn,
	}
}

// allow consumes one token for key. It returns whether the request may
// proceed and, when denied, how many seconds until a token is available.
func (l *keyedLimiter) allow(key string) (bool, float64) {
	if l == nil || l.capacity <= 0 {
		return true, 0
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.nowFn()
	b, exists := l.buckets[key]
	if !exists {
		l.buckets[key] = &bucket{
			tokens:     l.capacity - 1,
			lastRefill: now,
			lastAccess: now,
		}
		return true, 0
	}

	elapsed := now.Sub(b.lastRefill).Seconds()
	b.tokens = math.Min(l.capacity, b.tokens+elapsed*l.refillRate)
	b.lastRefill = now
	b.lastAccess = now

	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	return false, (1 - b.tokens) / l.refillRate
}

func (l *keyedLimiter) prune(cutoffAge time.Duration) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := l.nowFn().Add(-cutoffAge)
	for key, b := range l.buckets {
		if b.lastAccess.Before(cutoff) {
			delete(l.buckets, key)
		}
	}
}

// Guard bundles the auth-surface limiters and the global bcrypt semaphore.
// All methods are nil-safe: a nil *Guard allows everything, which keeps
// tests and callers that opt out of limiting free of conditionals.
type Guard struct {
	loginIP    *keyedLimiter
	loginEmail *keyedLimiter
	registerIP *keyedLimiter
	generalIP  *keyedLimiter
	bcryptSem  chan struct{}
}

// New builds a Guard on the wall clock and starts the background prune
// goroutine. Use this from main.
func New(cfg Config) *Guard {
	g := NewWithClock(cfg, time.Now)
	go func() {
		ticker := time.NewTicker(pruneInterval)
		defer ticker.Stop()
		for range ticker.C {
			g.loginIP.prune(idleCutoff)
			g.loginEmail.prune(idleCutoff)
			g.registerIP.prune(idleCutoff)
			g.generalIP.prune(idleCutoff)
		}
	}()
	return g
}

// NewWithClock builds a Guard with an injectable clock and no prune
// goroutine. Exported for deterministic tests in other packages.
func NewWithClock(cfg Config, nowFn func() time.Time) *Guard {
	g := &Guard{
		loginIP:    newKeyedLimiter(cfg.LoginPerMinIP, nowFn),
		loginEmail: newKeyedLimiter(cfg.LoginPerMinEmail, nowFn),
		registerIP: newKeyedLimiter(cfg.RegisterPerMinIP, nowFn),
		generalIP:  newKeyedLimiter(cfg.GeneralPerMinIP, nowFn),
	}
	if cfg.BcryptConcurrency > 0 {
		g.bcryptSem = make(chan struct{}, cfg.BcryptConcurrency)
	}
	return g
}

// AllowLoginIP consumes a login token for the given client IP.
func (g *Guard) AllowLoginIP(ip string) (bool, float64) {
	if g == nil {
		return true, 0
	}
	return g.loginIP.allow(ip)
}

// AllowLoginEmail consumes a login token for the given target email. The
// bucket key is the SHA-256 of the lowercased email: casing cannot bypass
// the throttle, and an attacker submitting huge unique "emails" retains at
// most 64 bytes of key per bucket instead of the raw string.
func (g *Guard) AllowLoginEmail(email string) (bool, float64) {
	if g == nil {
		return true, 0
	}
	sum := sha256.Sum256([]byte(strings.ToLower(email)))
	return g.loginEmail.allow(hex.EncodeToString(sum[:]))
}

// AllowRegisterIP consumes a registration token for the given client IP.
// User and service registration share this bucket.
func (g *Guard) AllowRegisterIP(ip string) (bool, float64) {
	if g == nil {
		return true, 0
	}
	return g.registerIP.allow(ip)
}

// AllowGeneralIP consumes a general-surface token for the given client IP.
func (g *Guard) AllowGeneralIP(ip string) (bool, float64) {
	if g == nil {
		return true, 0
	}
	return g.generalIP.allow(ip)
}

// TryAcquireBcrypt reserves a bcrypt slot without blocking. Callers must
// pair a successful acquire with ReleaseBcrypt.
func (g *Guard) TryAcquireBcrypt() bool {
	if g == nil || g.bcryptSem == nil {
		return true
	}
	select {
	case g.bcryptSem <- struct{}{}:
		return true
	default:
		return false
	}
}

// ReleaseBcrypt frees a slot taken by TryAcquireBcrypt.
func (g *Guard) ReleaseBcrypt() {
	if g == nil || g.bcryptSem == nil {
		return
	}
	select {
	case <-g.bcryptSem:
	default:
	}
}
