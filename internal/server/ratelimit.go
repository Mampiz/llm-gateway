package server

import (
	"fmt"
	"math"
	"net"
	"net/http"
	"strconv"
)

// rateLimit meters each caller before the request reaches a provider.
//
// It sits inside requireAuth so the bucket is keyed on the authenticated
// caller rather than on an address: a client behind a NAT must not share an
// allowance with everyone else behind it, and a client that changes address
// must not get a fresh one.
func (s *Server) rateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.limiter == nil {
			next.ServeHTTP(w, r)
			return
		}

		key := CallerFrom(r.Context())
		if key == "" {
			// Authentication is disabled, so there is no identity to meter.
			// Falling back to the address is weaker but better than nothing.
			key = "ip:" + clientIP(r)
		}

		decision, err := s.limiter.Allow(r.Context(), key)
		if err != nil {
			// Fail open, loudly. A limiter outage should not become a gateway
			// outage: briefly over-serving is a smaller failure than refusing
			// every request because a side channel is down.
			s.logger.Error("rate limiter unavailable, allowing the request",
				"error", err,
				"caller", key,
				"request_id", RequestIDFrom(r.Context()),
			)
			next.ServeHTTP(w, r)
			return
		}

		h := w.Header()
		h.Set("X-RateLimit-Limit", strconv.Itoa(decision.Limit))
		h.Set("X-RateLimit-Remaining", strconv.Itoa(decision.Remaining))

		if !decision.Allowed {
			// Retry-After is defined in whole seconds, and rounding down
			// would invite a client to retry while still empty.
			seconds := int(math.Ceil(decision.RetryAfter.Seconds()))
			if seconds < 1 {
				seconds = 1
			}
			h.Set("Retry-After", strconv.Itoa(seconds))

			s.metrics.RateLimited()
			s.logger.Info("rate limited",
				"caller", key,
				"retry_after", decision.RetryAfter,
				"request_id", RequestIDFrom(r.Context()),
			)
			writeError(w, http.StatusTooManyRequests, "rate_limit_error",
				fmt.Sprintf("rate limit exceeded, retry in %ds", seconds))
			return
		}

		next.ServeHTTP(w, r)
	})
}

// clientIP reports the address a request came from, ignoring the port.
//
// Forwarding headers are deliberately not trusted: anyone can set them, and
// honouring them would let a caller mint a fresh allowance per request.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
