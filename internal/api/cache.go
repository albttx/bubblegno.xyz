package api

import (
	"compress/gzip"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const graphTTL = 40 * time.Second

type graphEntry struct {
	body []byte
	at   time.Time
}

// graphCache is a tiny TTL cache for /v0/graph responses with per-key
// request coalescing (concurrent misses for the same key share one build).
type graphCache struct {
	mu       sync.Mutex
	entries  map[string]graphEntry
	inflight map[string]*sync.WaitGroup
}

func graphKey(seed string, depth, limit int, f Filter) string {
	ks := make([]string, 0, len(f.Kinds))
	for k := range f.Kinds {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return strings.Join([]string{strings.ToLower(seed), strconv.Itoa(depth), strconv.Itoa(limit), strconv.Itoa(f.MinWeight), strings.Join(ks, ",")}, "|")
}

// get returns a cached body or builds one. build returns (body, cacheable, err).
func (c *graphCache) get(key string, build func() ([]byte, bool, error)) ([]byte, error) {
	for {
		c.mu.Lock()
		if c.entries == nil {
			c.entries = map[string]graphEntry{}
			c.inflight = map[string]*sync.WaitGroup{}
		}
		if e, ok := c.entries[key]; ok && time.Since(e.at) < graphTTL {
			c.mu.Unlock()
			return e.body, nil
		}
		if wg, ok := c.inflight[key]; ok {
			c.mu.Unlock()
			wg.Wait()
			c.mu.Lock()
			e, ok := c.entries[key]
			c.mu.Unlock()
			if ok && time.Since(e.at) < graphTTL {
				return e.body, nil
			}
			continue // leader failed / uncacheable: try ourselves
		}
		wg := &sync.WaitGroup{}
		wg.Add(1)
		c.inflight[key] = wg
		c.mu.Unlock()

		body, cacheable, err := build()

		c.mu.Lock()
		delete(c.inflight, key)
		if err == nil && cacheable {
			c.entries[key] = graphEntry{body: body, at: time.Now()}
			if len(c.entries) > 256 { // crude bound: drop expired
				for k, e := range c.entries {
					if time.Since(e.at) >= graphTTL {
						delete(c.entries, k)
					}
				}
			}
		}
		c.mu.Unlock()
		wg.Done()
		return body, err
	}
}

func writeMaybeGzip(w http.ResponseWriter, r *http.Request, body []byte) {
	w.Header().Add("Vary", "Accept-Encoding")
	if len(body) > 1024 && strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusOK)
		gz, _ := gzip.NewWriterLevel(w, gzip.BestSpeed)
		_, _ = gz.Write(body)
		_ = gz.Close()
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func (c *graphCache) put(key string, body []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]graphEntry{}
		c.inflight = map[string]*sync.WaitGroup{}
	}
	c.entries[key] = graphEntry{body: body, at: time.Now()}
}
