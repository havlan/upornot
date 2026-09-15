package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNormalizeURL(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"github.com", "https://github.com/"},
		{" HTTPS://GitHub.COM:443/status#details ", "https://github.com/status"},
		{"http://example.com:80", "http://example.com/"},
	}

	for _, test := range tests {
		got, err := normalizeURL(test.input)
		if err != nil {
			t.Fatalf("normalizeURL(%q): %v", test.input, err)
		}
		if got != test.want {
			t.Errorf("normalizeURL(%q) = %q, want %q", test.input, got, test.want)
		}
	}
}

func TestNormalizeURLRejectsUnsafeInputs(t *testing.T) {
	for _, input := range []string{
		"ftp://github.com",
		"https://user:password@github.com",
		"http://127.0.0.1",
		"http://localhost",
		"https://github.com:8080",
	} {
		_, err := normalizeURL(input)
		if !errors.Is(err, errInvalidURL) {
			t.Errorf("normalizeURL(%q) error = %v, want invalid URL", input, err)
		}
	}
}

func TestMemoryStoreTracksChecksAndDueSites(t *testing.T) {
	store := newMemoryStore()
	ctx := context.Background()
	site, err := store.Create(ctx, "https://github.com/")
	if err != nil {
		t.Fatal(err)
	}

	checkedAt := time.Date(2026, time.September, 14, 9, 0, 0, 0, time.UTC)
	if err := store.SaveCheck(ctx, site.ID, "up", 200, checkedAt, ""); err != nil {
		t.Fatal(err)
	}
	sites, err := store.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sites) != 1 || sites[0].Status != "up" || sites[0].StatusCode != 200 || sites[0].LastCheckedAt == nil {
		t.Fatalf("unexpected saved site: %#v", sites)
	}

	notDue, err := store.Due(ctx, checkedAt.Add(-time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(notDue) != 0 {
		t.Fatalf("got %d sites before the next check is due", len(notDue))
	}
	due, err := store.Due(ctx, checkedAt)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].ID != site.ID {
		t.Fatalf("unexpected due sites: %#v", due)
	}
}

func TestMemoryStoreRefreshesAndEvictsSites(t *testing.T) {
	store := newMemoryStore()
	ctx := context.Background()
	site, err := store.Create(ctx, "https://github.com/")
	if err != nil {
		t.Fatal(err)
	}

	expiredAt := time.Now().UTC().Add(-siteTTL)
	site.RequestedAt = expiredAt
	store.sites[site.ID] = site

	refreshed, err := store.Create(ctx, site.URL)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.ID != site.ID || !refreshed.RequestedAt.After(expiredAt) {
		t.Fatalf("site was not refreshed: %#v", refreshed)
	}
	if err := store.Evict(ctx, time.Now().UTC().Add(-siteTTL)); err != nil {
		t.Fatal(err)
	}
	sites, err := store.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sites) != 1 {
		t.Fatalf("refreshed site was evicted: %#v", sites)
	}

	refreshed.RequestedAt = expiredAt
	store.sites[site.ID] = refreshed
	if err := store.Evict(ctx, time.Now().UTC().Add(-siteTTL)); err != nil {
		t.Fatal(err)
	}
	sites, err = store.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sites) != 0 {
		t.Fatalf("expired site was not evicted: %#v", sites)
	}
}

func TestPublicIPFilter(t *testing.T) {
	for _, value := range []string{"127.0.0.1", "10.0.0.1", "100.64.0.1", "169.254.169.254", "198.18.0.1"} {
		if isPublicIP(net.ParseIP(value)) {
			t.Errorf("%s must not be reachable", value)
		}
	}
	if !isPublicIP(net.ParseIP("1.1.1.1")) {
		t.Error("1.1.1.1 should be reachable")
	}
}

func TestRateLimiter(t *testing.T) {
	limiter := newRateLimiter()
	now := time.Date(2026, time.September, 14, 12, 0, 0, 0, time.UTC)
	for attempt := 0; attempt < submissionLimit; attempt++ {
		if !limiter.Allow("203.0.113.10", now) {
			t.Fatalf("attempt %d was unexpectedly rate limited", attempt+1)
		}
	}
	if limiter.Allow("203.0.113.10", now) {
		t.Fatal("submission after the limit was allowed")
	}
	if !limiter.Allow("203.0.113.10", now.Add(submissionWindow)) {
		t.Fatal("submission was not allowed after the window reset")
	}
}

func TestClientIPUsesAzureForwardedAddress(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/api/sites", nil)
	request.RemoteAddr = "10.0.0.1:1234"
	request.Header.Set("X-Forwarded-For", "198.51.100.10, 203.0.113.20")
	if got := clientIP(request); got != "203.0.113.20" {
		t.Fatalf("clientIP() = %q, want rightmost forwarded address", got)
	}
}

func TestSiteSubmissionLimit(t *testing.T) {
	store := newMemoryStore()
	app := newApp(store, newChecker(store, time.Second))
	for attempt := 0; attempt < submissionLimit; attempt++ {
		request := httptest.NewRequest(http.MethodPost, "/api/sites", strings.NewReader(`{"url":"http://127.0.0.1"}`))
		request.RemoteAddr = "203.0.113.10:1234"
		recorder := httptest.NewRecorder()
		app.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("attempt %d returned %d, want %d", attempt+1, recorder.Code, http.StatusBadRequest)
		}
	}

	request := httptest.NewRequest(http.MethodPost, "/api/sites", strings.NewReader(`{"url":"http://127.0.0.1"}`))
	request.RemoteAddr = "203.0.113.10:1234"
	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("rate-limited request returned %d, want %d", recorder.Code, http.StatusTooManyRequests)
	}
	if got := recorder.Header().Get("Retry-After"); got != "60" {
		t.Fatalf("Retry-After = %q, want 60", got)
	}
}
