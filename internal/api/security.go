// Package api — security middleware.
// security.go provides security headers and rate limiting for the CapScope
// HTTP server.
package api

import (
	"net/http"
	"strings"
	"sync"
	"time"
)

// securityHeaders wraps a handler with security-related response headers.
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Prevent MIME-type sniffing on all responses.
		w.Header().Set("X-Content-Type-Options", "nosniff")
		// Clickjacking protection: refuse to be embedded in frames.
		w.Header().Set("X-Frame-Options", "DENY")
		// Restrict where the browser sends the Referer header.
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		// Feature policy: disable rarely-used powerful features.
		w.Header().Set("Permissions-Policy", "geolocation=(), microphone=(), camera=(), payment=(), usb=()")
		// Content Security Policy: only allow resources from our own origin.
		// The SPA is a single file served from the same origin; no inline
		// scripts or external resources are needed.
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'self'; frame-ancestors 'none';")
		// HSTS: tell browsers to only use HTTPS for this host for one year.
		// Only set when the connection is actually TLS or terminated behind
		// a reverse proxy that advertises X-Forwarded-Proto: https.
		isHTTPS := r.TLS != nil
		if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
			isHTTPS = strings.ToLower(proto) == "https"
		}
		if isHTTPS {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}

// rateLimiter implements a simple per-IP token-bucket rate limiter.
// Each IP has its own bucket; requests consume one token. Tokens refill at
// the configured rate up to the bucket capacity. When the bucket is empty,
// requests are rejected with 429 Too Many Requests.
type rateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	rate    time.Duration // time between tokens (e.g. time.Second/10 = 10 req/s)
	capacity int          // max tokens in a bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newRateLimiter(rate time.Duration, capacity int) *rateLimiter {
	rl := &rateLimiter{
		buckets: make(map[string]*bucket),
		rate:    rate,
		capacity: capacity,
	}
	// Periodically sweep stale buckets to avoid unbounded growth.
	go rl.sweep()
	return rl
}

func (rl *rateLimiter) sweep() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		rl.mu.Lock()
		now := time.Now()
		for ip, b := range rl.buckets {
			if now.Sub(b.last) > 10*time.Minute {
				delete(rl.buckets, ip)
			}
		}
		rl.mu.Unlock()
	}
}

// allow returns true if the request from ip may proceed (a token is available).
func (rl *rateLimiter) allow(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	b, ok := rl.buckets[ip]
	if !ok {
		b = &bucket{tokens: float64(rl.capacity), last: time.Now()}
		rl.buckets[ip] = b
	}
	now := time.Now()
	// Refill tokens based on elapsed time.
	elapsed := now.Sub(b.last).Seconds()
	b.tokens += elapsed / rl.rate.Seconds()
	if b.tokens > float64(rl.capacity) {
		b.tokens = float64(rl.capacity)
	}
	b.last = now
	if b.tokens < 1.0 {
		return false
	}
	b.tokens--
	return true
}

// clientIP extracts the real client IP from X-Forwarded-For (when behind a
// trusted reverse proxy) or falls back to RemoteAddr.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		// Take the leftmost entry (the original client).
		if parts := strings.Split(xff, ","); len(parts) > 0 {
			return strings.TrimSpace(parts[0])
		}
	}
	// RemoteAddr is "host:port"; take just the host.
	if idx := strings.LastIndex(r.RemoteAddr, ":"); idx >= 0 {
		return r.RemoteAddr[:idx]
	}
	return r.RemoteAddr
}

// rateLimit wraps a handler with per-IP rate limiting. Different limits can
// be applied per path prefix by checking r.URL.Path inside the handler.
func (s *Server) rateLimit(next http.Handler) http.Handler {
	// Two limiters: a strict one for auth endpoints, a looser one for the
	// rest of the API. Both share the same bucket map but have different
	// rates; we key buckets by "ip:pathclass" so login doesn't starve other
	// endpoints and vice versa.
	loginLimiter := newRateLimiter(time.Second*2, 20)   // 20 req / 2s = 600/min
	apiLimiter := newRateLimiter(time.Millisecond*50, 200) // 20 req/s, burst 200

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		var ok bool
		if strings.HasPrefix(r.URL.Path, "/api/login") {
			ok = loginLimiter.allow(ip + ":login")
		} else if strings.HasPrefix(r.URL.Path, "/api/") {
			ok = apiLimiter.allow(ip + ":api")
		} else {
			ok = apiLimiter.allow(ip + ":api")
		}
		if !ok {
			w.Header().Set("Retry-After", "30")
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}
