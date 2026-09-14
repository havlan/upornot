package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const maxURLLength = 2048

var (
	errInvalidURL   = errors.New("invalid URL")
	errUnsafeTarget = errors.New("unsafe target")
)

type checker struct {
	store   siteStore
	client  *http.Client
	timeout time.Duration

	// ponytail: checks are serial per instance; add a queue only if the site count outgrows one interval.
	mu sync.Mutex
}

func newChecker(store siteStore, timeout time.Duration) *checker {
	checker := &checker{
		store:   store,
		timeout: timeout,
	}
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           safeDialContext,
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
	}
	checker.client = &http.Client{
		Transport: transport,
		Timeout:   timeout,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			if err := validateRequestURL(request.URL); err != nil {
				return fmt.Errorf("redirect blocked: %w", err)
			}
			return nil
		},
	}
	return checker
}

func (c *checker) Check(ctx context.Context, site Site) (Site, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.check(ctx, site)
}

func (c *checker) check(ctx context.Context, site Site) (Site, error) {
	requestCtx, cancel := context.WithTimeout(ctx, c.timeout)
	statusCode, err := c.probe(requestCtx, site.URL)
	cancel()

	checkedAt := time.Now().UTC()
	status := "up"
	lastError := ""
	if err != nil {
		status = "down"
		lastError = checkError(err)
	} else if statusCode < http.StatusOK || statusCode >= http.StatusBadRequest {
		status = "down"
		lastError = fmt.Sprintf("HTTP %d", statusCode)
	}

	if err := c.store.SaveCheck(ctx, site.ID, status, statusCode, checkedAt, lastError); err != nil {
		return Site{}, err
	}
	site.Status = status
	site.StatusCode = statusCode
	site.LastCheckedAt = &checkedAt
	site.LastError = lastError
	return site, nil
}

func (c *checker) probe(ctx context.Context, target string) (int, error) {
	targetURL, err := url.Parse(target)
	if err != nil {
		return 0, err
	}
	if err := validateRequestURL(targetURL); err != nil {
		return 0, err
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL.String(), nil)
	if err != nil {
		return 0, err
	}
	request.Header.Set("Accept", "*/*")
	request.Header.Set("User-Agent", "upornot/1.0")
	response, err := c.client.Do(request)
	if err != nil {
		if response != nil {
			response.Body.Close()
		}
		return 0, err
	}
	defer response.Body.Close()
	return response.StatusCode, nil
}

func (c *checker) RunDue(ctx context.Context, interval time.Duration) {
	sites, err := c.store.Due(ctx, time.Now().UTC().Add(-interval))
	if err != nil {
		log.Printf("find due sites: %v", err)
		return
	}
	for _, site := range sites {
		if err := ctx.Err(); err != nil {
			return
		}
		if _, err := c.Check(ctx, site); err != nil {
			log.Printf("check %s: %v", site.URL, err)
		}
	}
}

func runScheduler(ctx context.Context, checker *checker, interval time.Duration) {
	go func() {
		checker.RunDue(ctx, interval)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				checker.RunDue(ctx, interval)
			}
		}
	}()
}

func normalizeURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("%w: enter a web address", errInvalidURL)
	}
	if len(raw) > maxURLLength {
		return "", fmt.Errorf("%w: address is too long", errInvalidURL)
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}

	target, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("%w: enter a valid web address", errInvalidURL)
	}
	if err := validateRequestURL(target); err != nil {
		return "", fmt.Errorf("%w: %v", errInvalidURL, err)
	}

	host := strings.TrimSuffix(strings.ToLower(target.Hostname()), ".")
	if host == "" || !isASCII(host) {
		return "", fmt.Errorf("%w: host must use ASCII characters", errInvalidURL)
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return "", fmt.Errorf("%w: local addresses cannot be checked", errInvalidURL)
	}
	if ip := net.ParseIP(host); ip != nil && !isPublicIP(ip) {
		return "", fmt.Errorf("%w: private addresses cannot be checked", errInvalidURL)
	}

	target.Scheme = strings.ToLower(target.Scheme)
	port := target.Port()
	if port == "" || (target.Scheme == "http" && port == "80") || (target.Scheme == "https" && port == "443") {
		if strings.Contains(host, ":") {
			target.Host = "[" + host + "]"
		} else {
			target.Host = host
		}
	} else {
		target.Host = net.JoinHostPort(host, port)
	}
	if target.Path == "" {
		target.Path = "/"
	}
	target.Fragment = ""
	target.RawFragment = ""
	return target.String(), nil
}

func validateRequestURL(target *url.URL) error {
	if target == nil || target.Opaque != "" || (target.Scheme != "http" && target.Scheme != "https") {
		return errors.New("only http and https addresses are supported")
	}
	if target.User != nil {
		return errors.New("addresses cannot include credentials")
	}
	if target.Hostname() == "" {
		return errors.New("address must include a host")
	}
	if port := target.Port(); port != "" && port != "80" && port != "443" {
		return errors.New("only ports 80 and 443 are supported")
	}
	return nil
}

func safeDialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	if port != "80" && port != "443" {
		return nil, fmt.Errorf("%w: blocked port", errUnsafeTarget)
	}

	ips, err := lookupPublicIPs(ctx, host)
	if err != nil {
		return nil, err
	}
	dialer := net.Dialer{}
	var lastErr error
	for _, ip := range ips {
		connection, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return connection, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("connect to %s: %w", host, lastErr)
}

func lookupPublicIPs(ctx context.Context, host string) ([]net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		if !isPublicIP(ip) {
			return nil, fmt.Errorf("%w: private address", errUnsafeTarget)
		}
		return []net.IP{ip}, nil
	}

	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("%w: no public address", errUnsafeTarget)
	}
	for _, ip := range ips {
		if !isPublicIP(ip) {
			return nil, fmt.Errorf("%w: host resolves to a private address", errUnsafeTarget)
		}
	}
	return ips, nil
}

func isPublicIP(ip net.IP) bool {
	if ip == nil || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		if v4[0] == 0 || v4[0] == 127 || v4[0] >= 224 {
			return false
		}
		if v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
			return false
		}
		if v4[0] == 198 && (v4[1] == 18 || v4[1] == 19) {
			return false
		}
	}
	return true
}

func isASCII(value string) bool {
	for i := 0; i < len(value); i++ {
		if value[i] > 127 {
			return false
		}
	}
	return true
}

func checkError(err error) string {
	switch {
	case errors.Is(err, errUnsafeTarget):
		return "Blocked an unsafe target."
	case errors.Is(err, context.DeadlineExceeded):
		return "Request timed out."
	default:
		var dnsError *net.DNSError
		if errors.As(err, &dnsError) {
			return "Host could not be resolved."
		}
		return truncate(err.Error(), 240)
	}
}

func truncate(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "..."
}
