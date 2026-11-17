package notion

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

func testClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := NewClient("tok")
	c.http = &http.Client{Transport: rewrite{srv.URL}}
	c.limiter = rate.NewLimiter(rate.Inf, 1)
	return c
}

type rewrite struct{ target string }

func (r rewrite) RoundTrip(req *http.Request) (*http.Response, error) {
	u, _ := http.NewRequest(req.Method, r.target+req.URL.Path, req.Body)
	u.Header = req.Header
	return http.DefaultTransport.RoundTrip(u.WithContext(req.Context()))
}

func TestRetriesOn429ThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) < 3 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Write([]byte(`{}`))
	})
	if err := c.SetSelect(context.Background(), "p", "Cat", "x"); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 {
		t.Fatalf("calls = %d, want 3", calls.Load())
	}
}

func TestGivesUpAfterMaxRetries(t *testing.T) {
	var calls atomic.Int32
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	if err := c.SetSelect(context.Background(), "p", "Cat", "x"); err == nil {
		t.Fatal("want error")
	}
	if calls.Load() != maxRetries+1 {
		t.Fatalf("calls = %d, want %d", calls.Load(), maxRetries+1)
	}
}

func TestDoesNotRetryOn400(t *testing.T) {
	var calls atomic.Int32
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	})
	_ = c.SetSelect(context.Background(), "p", "Cat", "x")
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1", calls.Load())
	}
}

func TestLimiterThrottlesRequests(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(`{}`)) })
	c.limiter = rate.NewLimiter(rate.Every(100*time.Millisecond), 1)
	start := time.Now()
	for range 3 {
		if err := c.SetSelect(context.Background(), "p", "Cat", "x"); err != nil {
			t.Fatal(err)
		}
	}
	if d := time.Since(start); d < 180*time.Millisecond {
		t.Fatalf("3 requests took %v; limiter not applied", d)
	}
}
