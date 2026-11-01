package ledger

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestLimiterBuckets(t *testing.T) {
	now := time.Unix(1000, 0)
	l := &limiter{rate: 2, burst: 3, buckets: map[[32]byte]*bucket{}, now: func() time.Time { return now }}
	a, b := [32]byte{1}, [32]byte{2}
	for i := 0; i < 3; i++ {
		if ok, _ := l.allow(a); !ok {
			t.Fatalf("request %d within the burst refused", i)
		}
	}
	ok, wait := l.allow(a)
	if ok || wait != 500*time.Millisecond {
		t.Fatalf("over the burst: %v %v", ok, wait)
	}
	// Another credential has its own bucket.
	if ok, _ := l.allow(b); !ok {
		t.Fatal("one credential starved another")
	}
	// Tokens come back at the rate, up to the burst.
	now = now.Add(time.Second)
	for i := 0; i < 2; i++ {
		if ok, _ := l.allow(a); !ok {
			t.Fatalf("refill %d refused", i)
		}
	}
	if ok, _ := l.allow(a); ok {
		t.Fatal("refilled past the rate")
	}
	now = now.Add(time.Hour)
	l.prune(now)
	if len(l.buckets) != 0 {
		t.Fatalf("idle buckets kept: %d", len(l.buckets))
	}
}

func TestRateLimitHandlerAndClientRetry(t *testing.T) {
	calls := 0
	h := RateLimit(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		writeJSON(w, http.StatusOK, Info{Contract: "c"})
	}), 1, 1)
	srv := httptest.NewServer(h)
	defer srv.Close()
	c, _ := NewClient(srv.URL, "token-a")

	old := MaxRetryWait
	defer func() { MaxRetryWait = old }()
	MaxRetryWait = 0 // no retries: the 429 comes back
	if _, err := c.Info(); err != nil {
		t.Fatal(err)
	}
	_, err := c.Info()
	if status(err) != http.StatusTooManyRequests {
		t.Fatalf("second request: %v", err)
	}
	var ae *APIError
	if !errors.As(err, &ae) || ae.RetryAfter != time.Second {
		t.Fatalf("Retry-After: %+v", err)
	}
	// Other tokens and health checks are not affected.
	c2, _ := NewClient(srv.URL, "token-b")
	if _, err := c2.Info(); err != nil {
		t.Fatalf("other token: %v", err)
	}
	for i := 0; i < 3; i++ {
		resp, err := http.Get(srv.URL + "/healthz")
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("healthz limited: %v", err)
		}
	}
	// With retries, the client waits and succeeds.
	MaxRetryWait = 5 * time.Second
	start := time.Now()
	if _, err := c.Info(); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if time.Since(start) < 500*time.Millisecond {
		t.Fatal("the client did not wait before retrying")
	}
	off := httptest.NewServer(RateLimit(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}), 0, 1))
	defer off.Close()
	for i := 0; i < 5; i++ {
		req, _ := http.NewRequest("GET", off.URL+"/v1/info", nil)
		req.Header.Set("Authorization", "Bearer x")
		resp, err := http.DefaultClient.Do(req)
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("rate 0 must disable the limiter: %v %v", err, resp.StatusCode)
		}
	}
}
