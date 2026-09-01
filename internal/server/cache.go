package server

import (
	"context"
	"net/http"

	"github.com/Mampiz/llm-gateway/internal/cache"
	"github.com/Mampiz/llm-gateway/internal/provider"
)

// Cache status reported to the client, so a cheap answer is recognisable as
// one without reading the logs.
const (
	cacheHeader = "X-Cache"
	cacheHit    = "HIT"
	cacheMiss   = "MISS"

	// servedByCache stands in for a provider name when nobody upstream was
	// asked, so logs and metrics can tell a cheap answer from a paid one.
	servedByCache = "cache"
)

// cacheKeyFor digests the request, scoping the entry when configured to.
func (s *Server) cacheKeyFor(r *http.Request, req provider.ChatRequest) string {
	scope := ""
	if s.cacheScope == "caller" {
		scope = CallerFrom(r.Context())
	}
	return cache.Key(scope, req)
}

// cachedChat serves a completion through the cache.
//
// Two things happen here that a plain lookup would not do. Identical
// concurrent requests are collapsed with singleflight, so a cold cache under a
// burst of the same question fetches one answer rather than N. And every cache
// failure degrades to a miss: a broken cache must make the gateway slower,
// never broken.
func (s *Server) cachedChat(ctx context.Context, r *http.Request, req provider.ChatRequest) (*provider.ChatResponse, string, bool, error) {
	requestID := RequestIDFrom(ctx)

	if s.cache == nil || s.cacheTTL <= 0 {
		resp, served, err := s.router.Chat(ctx, req)
		return resp, served, false, err
	}

	key := s.cacheKeyFor(r, req)
	if key == "" {
		// Unhashable input. A key that could collide is worse than none.
		resp, served, err := s.router.Chat(ctx, req)
		return resp, served, false, err
	}

	if resp, ok, err := s.cache.Get(ctx, key); err != nil {
		s.logger.Warn("cache read failed, treating it as a miss",
			"error", err, "request_id", requestID)
	} else if ok {
		return resp, servedByCache, true, nil
	}

	// result carries what the shared call produced, since singleflight can
	// only hand back one value.
	type result struct {
		resp   *provider.ChatResponse
		served string
	}

	v, err, shared := s.inflight.Do(key, func() (any, error) {
		// Check again inside the group: a request that waited here may find
		// the answer the leader just stored.
		if resp, ok, err := s.cache.Get(ctx, key); err == nil && ok {
			return result{resp: resp, served: servedByCache}, nil
		}

		resp, served, err := s.router.Chat(ctx, req)
		if err != nil {
			return nil, err
		}

		if err := s.cache.Set(ctx, key, resp, s.cacheTTL); err != nil {
			s.logger.Warn("cache write failed",
				"error", err, "request_id", requestID)
		}
		return result{resp: resp, served: served}, nil
	})
	if err != nil {
		return nil, "", false, err
	}

	res, _ := v.(result)
	// A follower of a shared call did not pay for the answer either, which is
	// the whole point of collapsing them.
	return res.resp, res.served, res.served == servedByCache || shared, nil
}
