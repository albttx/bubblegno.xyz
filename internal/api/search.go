package api

// /v0/search — seed autocomplete over Account address / pkgPath / label.
//
// Strategy:
//   - q >= 3 chars → Dgraph regexp (trigram index, case-insensitive substring)
//     on address, pkgPath and label.
//   - q < 3 chars, or regexp unavailable (schema without `regexp`) → substring
//     scan over a cached in-memory account list (refreshed every 60s).
// Candidates are then enriched with activity counts (aggregate edges) and ranked:
// exact match > prefix match > substring, then by activity.

import (
	"context"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

type SearchHit struct {
	Address  string `json:"address"`
	Kind     string `json:"kind"`
	PkgPath  string `json:"pkgPath,omitempty"`
	Label    string `json:"label,omitempty"`
	Activity int64  `json:"activity"`
	Match    string `json:"match"` // exact | prefix | substring
}

type SearchResponse struct {
	Query   string      `json:"query"`
	Hits    []SearchHit `json:"hits"`
	Source  string      `json:"source"` // dgraph-regexp | cache
	Warning string      `json:"warning,omitempty"`
}

const searchCandidates = 200

const qSearchRegexp = `
query Search($re: String!, $n: Int!) {
  queryAccount(filter: { or: [
    { address: { regexp: $re } },
    { pkgPath: { regexp: $re } },
    { label: { regexp: $re } }
  ] }, first: $n) { address kind pkgPath label }
}`

const qSearchActivity = `
query Activity($addrs: [String!]!) {
  queryAccount(filter: { address: { in: $addrs } }) {
    address kind pkgPath label
    transfersFromAggregate { count }
    transfersToAggregate { count }
    callsAsCallerAggregate { count }
    callsAsRealmAggregate { count }
    deploysAggregate { count }
  }
}`

const qAllAccounts = `
query All($n: Int!, $off: Int!) {
  queryAccount(first: $n, offset: $off) { address kind pkgPath label }
}`

type countAgg struct {
	Count int64 `json:"count"`
}

type accActivity struct {
	acc
	TF *countAgg `json:"transfersFromAggregate"`
	TT *countAgg `json:"transfersToAggregate"`
	CC *countAgg `json:"callsAsCallerAggregate"`
	CR *countAgg `json:"callsAsRealmAggregate"`
	DP *countAgg `json:"deploysAggregate"`
}

func (a accActivity) total() int64 {
	var t int64
	for _, c := range []*countAgg{a.TF, a.TT, a.CC, a.CR, a.DP} {
		if c != nil {
			t += c.Count
		}
	}
	return t
}

// ---- cached account list (fallback / short queries) ----

type accCache struct {
	mu   sync.Mutex
	at   time.Time
	list []acc
}

func (c *accCache) get(ctx context.Context, s *Server) ([]acc, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.list != nil && time.Since(c.at) < 60*time.Second {
		return c.list, nil
	}
	const page = 10000
	var all []acc
	for off := 0; ; off += page {
		var out struct {
			QueryAccount []acc `json:"queryAccount"`
		}
		if err := s.dg.GraphQL(ctx, qAllAccounts, map[string]any{"n": page, "off": off}, &out); err != nil {
			if c.list != nil {
				return c.list, nil // serve stale
			}
			return nil, err
		}
		all = append(all, out.QueryAccount...)
		if len(out.QueryAccount) < page || off > 500_000 {
			break
		}
	}
	c.list, c.at = all, time.Now()
	return all, nil
}

// ---- matching / ranking ----

func normQ(q string) string {
	q = strings.ToLower(strings.TrimSpace(q))
	q = strings.TrimPrefix(q, "https://")
	q = strings.TrimPrefix(q, "http://")
	return q
}

// matchTier: 0 exact, 1 prefix, 2 substring, -1 none.
func matchTier(a acc, q string) int {
	best := -1
	for _, f := range []string{a.Address, a.PkgPath, a.Label} {
		f = strings.ToLower(f)
		if f == "" {
			continue
		}
		t := -1
		switch {
		case f == q:
			t = 0
		case strings.HasPrefix(f, q), strings.HasPrefix(strings.TrimPrefix(f, "gno.land/"), q),
			strings.HasPrefix(strings.TrimPrefix(f, "gno.land"), q), hasPkgPrefix(f, q):
			t = 1
		case strings.Contains(f, q):
			t = 2
		}
		if t >= 0 && (best < 0 || t < best) {
			best = t
		}
	}
	return best
}

var tierName = []string{"exact", "prefix", "substring"}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("q")
	q := normQ(raw)
	limit := clampInt(atoiDef(r.URL.Query().Get("limit"), 20), 1, 50)
	resp := SearchResponse{Query: raw, Hits: []SearchHit{}}
	if q == "" {
		writeJSON(w, http.StatusOK, resp)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var cands []acc
	if len([]rune(q)) >= 3 {
		var out struct {
			QueryAccount []acc `json:"queryAccount"`
		}
		// DQL regex literal is /…/flags: escape inner slashes (pkgPaths).
		re := "/" + strings.ReplaceAll(regexp.QuoteMeta(q), "/", `\/`) + "/i"
		if err := s.dg.GraphQL(ctx, qSearchRegexp, map[string]any{"re": re, "n": searchCandidates}, &out); err == nil {
			cands, resp.Source = out.QueryAccount, "dgraph-regexp"
		} else {
			resp.Warning = "regexp search unavailable, using cache: " + err.Error()
		}
	}
	if resp.Source == "" {
		all, err := s.search.get(ctx, s)
		if err != nil {
			resp.Warning = "dgraph: " + err.Error()
			writeJSON(w, http.StatusOK, resp)
			return
		}
		resp.Source = "cache"
		for _, a := range all {
			if matchTier(a, q) >= 0 {
				cands = append(cands, a)
			}
		}
		// keep the best tiers when the scan is broad (e.g. "g1")
		sort.SliceStable(cands, func(i, j int) bool { return matchTier(cands[i], q) < matchTier(cands[j], q) })
		if len(cands) > searchCandidates {
			cands = cands[:searchCandidates]
		}
	}

	hits := make([]SearchHit, 0, len(cands))
	tiers := map[string]int{}
	for _, a := range cands {
		t := matchTier(a, q)
		if t < 0 {
			t = 2
		}
		tiers[a.Address] = t
		hits = append(hits, SearchHit{Address: a.Address, Kind: a.Kind, PkgPath: a.PkgPath, Label: a.Label, Match: tierName[t]})
	}

	// activity enrichment (best effort)
	if len(hits) > 0 {
		addrs := make([]string, len(hits))
		for i, h := range hits {
			addrs[i] = h.Address
		}
		var out struct {
			QueryAccount []accActivity `json:"queryAccount"`
		}
		if err := s.dg.GraphQL(ctx, qSearchActivity, map[string]any{"addrs": addrs}, &out); err == nil {
			act := map[string]int64{}
			for _, a := range out.QueryAccount {
				act[a.Address] = a.total()
			}
			for i := range hits {
				hits[i].Activity = act[hits[i].Address]
			}
		}
	}

	sort.SliceStable(hits, func(i, j int) bool {
		ti, tj := tiers[hits[i].Address], tiers[hits[j].Address]
		if ti != tj {
			return ti < tj
		}
		if hits[i].Activity != hits[j].Activity {
			return hits[i].Activity > hits[j].Activity
		}
		return len(hits[i].Address) < len(hits[j].Address)
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}
	resp.Hits = hits
	writeJSON(w, http.StatusOK, resp)
}

// hasPkgPrefix: "gnops/val" is a prefix of "gno.land/r/gnops/valopers".
func hasPkgPrefix(f, q string) bool {
	for _, p := range []string{"gno.land/r/", "gno.land/p/"} {
		if strings.HasPrefix(f, p) && strings.HasPrefix(f[len(p):], q) {
			return true
		}
	}
	return false
}
