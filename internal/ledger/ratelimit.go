package ledger

import (
	"crypto/sha256"
	"fmt"
	"math"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// RateLimit wraps h so that each credential (the Authorization header, or
// the client address for requests without one) may make about rate requests
// per second, with bursts of up to burst. Requests over the limit get 429
// with Retry-After. GET /healthz is never limited.
//
// Limiting per credential keeps one party (or one stolen token) from
// starving the others. Clients that rotate random tokens are limited per
// token; put the ledger behind a proxy that limits by address if it faces
// the internet.
func RateLimit(h http.Handler, rate float64, burst int) http.Handler {
	if rate <= 0 {
		return h
	}
	if burst < 1 {
		burst = 1
	}
	l := &limiter{rate: rate, burst: float64(burst), buckets: map[[32]byte]*bucket{}, now: time.Now}
	return l.wrap(h)
}

// maxBuckets bounds the limiter's memory: past it, full buckets (clients
// that have been quiet long enough to be back at their burst) are dropped.
const maxBuckets = 10000

type bucket struct {
	tokens float64
	seen   time.Time
}

type limiter struct {
	rate, burst float64
	mu          sync.Mutex
	buckets     map[[32]byte]*bucket
	now         func() time.Time
}

// allow takes one token from key's bucket. It returns how long to wait when
// the bucket is empty.
func (l *limiter) allow(key [32]byte) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b, ok := l.buckets[key]
	if !ok {
		if len(l.buckets) >= maxBuckets {
			l.prune(now)
		}
		b = &bucket{tokens: l.burst, seen: now}
		l.buckets[key] = b
	}
	b.tokens = math.Min(l.burst, b.tokens+now.Sub(b.seen).Seconds()*l.rate)
	b.seen = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	wait := time.Duration((1 - b.tokens) / l.rate * float64(time.Second))
	return false, wait
}

func (l *limiter) prune(now time.Time) {
	for k, b := range l.buckets {
		if b.tokens+now.Sub(b.seen).Seconds()*l.rate >= l.burst {
			delete(l.buckets, k)
		}
	}
}

func (l *limiter) wrap(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/healthz" {
			h.ServeHTTP(w, r)
			return
		}
		cred := r.Header.Get("Authorization")
		if cred == "" {
			host, _, err := net.SplitHostPort(r.RemoteAddr)
			if err != nil {
				host = r.RemoteAddr
			}
			cred = "addr " + host
		}
		ok, wait := l.allow(sha256.Sum256([]byte(cred)))
		if !ok {
			secs := int(math.Ceil(wait.Seconds()))
			if secs < 1 {
				secs = 1
			}
			w.Header().Set("Retry-After", strconv.Itoa(secs))
			writeError(w, fail(http.StatusTooManyRequests, "too many requests; retry in %s", fmt.Sprint(time.Duration(secs)*time.Second)))
			return
		}
		h.ServeHTTP(w, r)
	})
}
