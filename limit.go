package main

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	submissionLimit   = 5
	submissionWindow  = time.Minute
	maxTrackedClients = 10_000
)

type rateLimiter struct {
	mu      sync.Mutex
	clients map[string]rateWindow
}

type rateWindow struct {
	start time.Time
	count int
}

func newRateLimiter() *rateLimiter {
	return &rateLimiter{clients: make(map[string]rateWindow)}
}

func (l *rateLimiter) Allow(client string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	window, known := l.clients[client]
	if !known && len(l.clients) >= maxTrackedClients {
		for key, entry := range l.clients {
			if now.Sub(entry.start) >= submissionWindow {
				delete(l.clients, key)
			}
		}
		if len(l.clients) >= maxTrackedClients {
			return false
		}
	}
	if !known || now.Sub(window.start) >= submissionWindow {
		window = rateWindow{start: now}
	}
	if window.count >= submissionLimit {
		return false
	}
	window.count++
	l.clients[client] = window
	return true
}

func clientIP(r *http.Request) string {
	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		parts := strings.Split(forwarded, ",")
		// Container Apps appends the trusted address at the right edge.
		if ip := strings.TrimSpace(parts[len(parts)-1]); net.ParseIP(ip) != nil {
			return ip
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil && net.ParseIP(host) != nil {
		return host
	}
	return r.RemoteAddr
}
