package main

import (
	"context"
	"errors"
	"net"
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
