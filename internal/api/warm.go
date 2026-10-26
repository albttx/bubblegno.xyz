package api

import (
	"context"
	"encoding/json"
	"time"
)

// warmDefault keeps the landing-page graph (no seed, depth 1, limit 300) hot so
// first paint never waits on Dgraph.
func (s *Server) warmDefault() {
	f := Filter{MinWeight: 1}
	key := graphKey("", 1, 300, f)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		resp := s.buildGraph(ctx, "", 1, 300, f)
		cancel()
		if resp.Warning == "" {
			if b, err := json.Marshal(resp); err == nil {
				s.gcache.put(key, b)
			}
		}
		time.Sleep(30 * time.Second)
	}
}
