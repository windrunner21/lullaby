package main

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestPacingFailureIsReported(t *testing.T) {
	// The voice server can't say: the result says so, rather than showing
	// zero late packets as if it had measured them.
	old := http.DefaultTransport
	http.DefaultTransport = roundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Status: "503 Service Unavailable",
			Body: io.NopCloser(strings.NewReader("no stats"))}, nil
	})
	defer func() { http.DefaultTransport = old }()
	r := result{Engine: "lullaby", Title: "test"}
	now := time.Now()
	pacing(&r, lullabyGuild, now, now.Add(time.Second))
	if r.Err == "" {
		t.Errorf("a failed measurement looks fine: %+v", r)
	}
}
