package api

// Graph building for /v0/graph and /v0/node/{id}.
//
// Plan (v0): ingest does not maintain Link aggregates yet, so we build the
// bubble graph on the fly from raw Transfer / Call / Deploy edges:
//   - seed given  → BFS over Account edges (transfersFrom/To, callsAsCaller/Realm,
//                   deploys + queryDeploy by pkgPath) up to `depth` hops.
//   - no seed     → recent activity window (latest N transfers + calls + deploys),
//                   keep the top `limit` nodes by degree/volume.
// Links are undirected: key = min(a,b)|max(a,b). Later: read Link aggregates
// directly once ingest maintains them.

import (
	"context"
	"math"
	"sort"
	"strings"

	"github.com/albttx/bubblegno.xyz/internal/ingest"
)

const (
	edgesPerRelation = 300  // per account per relation when expanding a hop
	recentWindow     = 3000 // recent rows per type when no seed
)

type Node struct {
	ID          string  `json:"id"`
	Address     string  `json:"address"`
	Kind        string  `json:"kind"`
	PkgPath     string  `json:"pkgPath,omitempty"`
	Label       string  `json:"label"`
	TxCount     int64   `json:"txCount"`
	VolumeUgnot int64   `json:"volumeUgnot"`
	Degree      int64   `json:"degree"`
	Size        float64 `json:"size"`
}

type Link struct {
	Source      string   `json:"source"`
	Target      string   `json:"target"`
	Weight      int64    `json:"weight"`
	VolumeUgnot int64    `json:"volumeUgnot"`
	Kinds       []string `json:"kinds"`
}

type SyncInfo struct {
	ChainID    string `json:"chainId"`
	LastHeight int64  `json:"lastHeight"`
}

type GraphResponse struct {
	Nodes   []Node   `json:"nodes"`
	Links   []Link   `json:"links"`
	Seed    string   `json:"seed"`
	Depth   int      `json:"depth"`
	Limit   int      `json:"limit"`
	Sync    SyncInfo `json:"sync"`
	Warning string   `json:"warning,omitempty"`
}

// ---- Dgraph GraphQL shapes ----

type acc struct {
	Address     string `json:"address"`
	Kind        string `json:"kind"`
	PkgPath     string `json:"pkgPath"`
	Label       string `json:"label"`
	TxCount     int64  `json:"txCount"`
	VolumeUgnot int64  `json:"volumeUgnot"`
	Degree      int64  `json:"degree"`
}

type xferRow struct {
	Hash   string `json:"hash"`
	Height int64  `json:"height"`
	Amount int64  `json:"amount"`
	Denom  string `json:"denom"`
	From   *acc   `json:"from"`
	To     *acc   `json:"to"`
}

type callRow struct {
	Hash    string `json:"hash"`
	Height  int64  `json:"height"`
	Func    string `json:"func"`
	PkgPath string `json:"pkgPath"`
	Send    string `json:"send"`
	Caller  *acc   `json:"caller"`
	Realm   *acc   `json:"realm"`
}

type deployRow struct {
	Hash    string `json:"hash"`
	Height  int64  `json:"height"`
	PkgPath string `json:"pkgPath"`
	Creator *acc   `json:"creator"`
	Package *acc   `json:"package"`
}

type accWithEdges struct {
	acc
	TransfersFrom []xferRow   `json:"transfersFrom"`
	TransfersTo   []xferRow   `json:"transfersTo"`
	CallsAsCaller []callRow   `json:"callsAsCaller"`
	CallsAsRealm  []callRow   `json:"callsAsRealm"`
	Deploys       []deployRow `json:"deploys"`
}

const accFields = `address kind pkgPath label txCount volumeUgnot degree`

const qExpand = `
query Expand($addrs: [String!]!, $n: Int!) {
  queryAccount(filter: { address: { in: $addrs } }) {
    ` + accFields + `
    transfersFrom(first: $n, order: { desc: height }) { hash height amount denom to { ` + accFields + ` } }
    transfersTo(first: $n, order: { desc: height }) { hash height amount denom from { ` + accFields + ` } }
    callsAsCaller(first: $n, order: { desc: height }) { hash height func pkgPath send realm { ` + accFields + ` } }
    callsAsRealm(first: $n, order: { desc: height }) { hash height func pkgPath send caller { ` + accFields + ` } }
    deploys(first: $n, order: { desc: height }) { hash height pkgPath package { ` + accFields + ` } }
  }
  queryDeploy(filter: { pkgPath: { in: $addrs } }, first: $n) {
    hash height pkgPath creator { ` + accFields + ` } package { ` + accFields + ` }
  }
}`

const qRecent = `
query Recent($n: Int!) {
  queryTransfer(first: $n, order: { desc: height }) { hash height amount denom from { ` + accFields + ` } to { ` + accFields + ` } }
  queryCall(first: $n, order: { desc: height }) { hash height func pkgPath send caller { ` + accFields + ` } realm { ` + accFields + ` } }
  queryDeploy(first: $n, order: { desc: height }) { hash height pkgPath creator { ` + accFields + ` } package { ` + accFields + ` } }
}`

const qResolve = `
query Resolve($id: String!) {
  queryAccount(filter: { or: [{ address: { eq: $id } }, { pkgPath: { eq: $id } }] }, first: 1) { address }
}`

// ---- aggregation ----

type agg struct {
	nodes map[string]*Node
	links map[string]*Link
	kinds map[string]map[string]bool
	nbrs  map[string]map[string]bool
}

func newAgg() *agg {
	return &agg{
		nodes: map[string]*Node{}, links: map[string]*Link{},
		kinds: map[string]map[string]bool{}, nbrs: map[string]map[string]bool{},
	}
}

func (g *agg) node(a *acc) *Node {
	if a == nil || a.Address == "" {
		return nil
	}
	n, ok := g.nodes[a.Address]
	if !ok {
		n = &Node{ID: a.Address, Address: a.Address, Kind: a.Kind, PkgPath: a.PkgPath, Label: a.Label}
		g.nodes[a.Address] = n
	}
	if n.Kind == "" || n.Kind == "UNKNOWN" {
		n.Kind = a.Kind
	}
	if n.PkgPath == "" {
		n.PkgPath = a.PkgPath
	}
	if n.Label == "" {
		n.Label = a.Label
	}
	// stored aggregates (if ingest ever fills them) act as a floor
	n.TxCount = max(n.TxCount, a.TxCount)
	n.VolumeUgnot = max(n.VolumeUgnot, a.VolumeUgnot)
	n.Degree = max(n.Degree, a.Degree)
	return n
}

// edge records one interaction. key = hash|kind|a|b dedups the same event seen
// from both endpoints during BFS.
func (g *agg) edge(seen map[string]bool, hash, kind string, a, b *acc, ugnot int64) {
	na, nb := g.node(a), g.node(b)
	if na == nil || nb == nil || na.ID == nb.ID {
		return
	}
	src, dst := na.ID, nb.ID
	if dst < src {
		src, dst = dst, src
	}
	ek := hash + "|" + kind + "|" + src + "|" + dst
	if seen[ek] {
		return
	}
	seen[ek] = true
	lk := src + "|" + dst
	l, ok := g.links[lk]
	if !ok {
		l = &Link{Source: src, Target: dst}
		g.links[lk] = l
		g.kinds[lk] = map[string]bool{}
	}
	l.Weight++
	l.VolumeUgnot += ugnot
	g.kinds[lk][kind] = true
	for _, n := range []*Node{na, nb} {
		n.TxCount++
		n.VolumeUgnot += ugnot
	}
	if g.nbrs[src] == nil {
		g.nbrs[src] = map[string]bool{}
	}
	if g.nbrs[dst] == nil {
		g.nbrs[dst] = map[string]bool{}
	}
	g.nbrs[src][dst] = true
	g.nbrs[dst][src] = true
}

func ugnotOf(amount int64, denom string) int64 {
	if denom == "" || denom == "ugnot" {
		return amount
	}
	return 0
}

func sendUgnot(send string) int64 {
	a, d := ingest.ParseCoin(send)
	return ugnotOf(a, d)
}

func (g *agg) addTransfer(seen map[string]bool, t xferRow, self *acc) {
	from, to := t.From, t.To
	if from == nil {
		from = self
	}
	if to == nil {
		to = self
	}
	g.edge(seen, t.Hash, "transfer", from, to, ugnotOf(t.Amount, t.Denom))
}

func (g *agg) addCall(seen map[string]bool, c callRow, self *acc) {
	caller, realm := c.Caller, c.Realm
	if caller == nil {
		caller = self
	}
	if realm == nil {
		realm = self
	}
	g.edge(seen, c.Hash, "call", caller, realm, sendUgnot(c.Send))
}

func (g *agg) addDeploy(seen map[string]bool, d deployRow, self *acc) {
	creator, pkg := d.Creator, d.Package
	if creator == nil {
		creator = self
	}
	if pkg == nil {
		pkg = self
	}
	g.edge(seen, d.Hash, "deploy", creator, pkg, 0)
}

// bubbleSize: radius in px-ish units for the frontend.
//
//	size = 4 + 2.5*log2(1+degree) + 3*log10(1 + volumeUgnot/1e6)   (1 GNOT = 1e6 ugnot)
//
// clamped to [4, 48]. Logs keep whales from swallowing the canvas.
func bubbleSize(degree, volumeUgnot int64) float64 {
	s := 4 + 2.5*math.Log2(1+float64(degree)) + 3*math.Log10(1+float64(volumeUgnot)/1e6)
	s = math.Min(48, math.Max(4, s))
	return math.Round(s*100) / 100
}

func (g *agg) finalize(limit int, keep map[string]bool) ([]Node, []Link) {
	for id, n := range g.nodes {
		n.Degree = max(n.Degree, int64(len(g.nbrs[id])))
	}
	all := make([]*Node, 0, len(g.nodes))
	for _, n := range g.nodes {
		all = append(all, n)
	}
	score := func(n *Node) float64 {
		return float64(n.Degree)*1000 + float64(n.TxCount)*10 + math.Log10(1+float64(n.VolumeUgnot))
	}
	sort.Slice(all, func(i, j int) bool {
		if keep[all[i].ID] != keep[all[j].ID] {
			return keep[all[i].ID]
		}
		si, sj := score(all[i]), score(all[j])
		if si != sj {
			return si > sj
		}
		return all[i].ID < all[j].ID
	})
	if len(all) > limit {
		all = all[:limit]
	}
	in := map[string]bool{}
	nodes := make([]Node, 0, len(all))
	for _, n := range all {
		in[n.ID] = true
	}
	links := make([]Link, 0)
	for k, l := range g.links {
		if !in[l.Source] || !in[l.Target] {
			continue
		}
		for kind := range g.kinds[k] {
			l.Kinds = append(l.Kinds, kind)
		}
		sort.Strings(l.Kinds)
		links = append(links, *l)
	}
	sort.Slice(links, func(i, j int) bool { return links[i].Weight > links[j].Weight })
	for _, n := range all {
		if n.Label == "" {
			n.Label = shortLabel(n)
		}
		n.Size = bubbleSize(n.Degree, n.VolumeUgnot)
		nodes = append(nodes, *n)
	}
	return nodes, links
}

func shortLabel(n *Node) string {
	if n.PkgPath != "" {
		return strings.TrimPrefix(n.PkgPath, "gno.land")
	}
	if strings.Contains(n.Address, "/") {
		return strings.TrimPrefix(n.Address, "gno.land")
	}
	if len(n.Address) > 14 {
		return n.Address[:8] + "…" + n.Address[len(n.Address)-4:]
	}
	return n.Address
}

// ---- builders ----

func (s *Server) syncInfo(ctx context.Context) SyncInfo {
	h, _, err := ingest.GetSyncHeight(ctx, s.dg, s.cfg.ChainID)
	if err != nil {
		h = 0
	}
	return SyncInfo{ChainID: s.cfg.ChainID, LastHeight: h}
}

func (s *Server) resolve(ctx context.Context, id string) (string, error) {
	var out struct {
		QueryAccount []acc `json:"queryAccount"`
	}
	if err := s.dg.GraphQL(ctx, qResolve, map[string]any{"id": id}, &out); err != nil {
		return "", err
	}
	if len(out.QueryAccount) == 0 {
		return "", nil
	}
	return out.QueryAccount[0].Address, nil
}

type expandResult struct {
	QueryAccount []accWithEdges `json:"queryAccount"`
	QueryDeploy  []deployRow    `json:"queryDeploy"`
}

func (s *Server) expand(ctx context.Context, addrs []string, n int) (*expandResult, error) {
	var out expandResult
	err := s.dg.GraphQL(ctx, qExpand, map[string]any{"addrs": addrs, "n": n}, &out)
	return &out, err
}

func (g *agg) ingestExpand(seen map[string]bool, r *expandResult) {
	for i := range r.QueryAccount {
		a := &r.QueryAccount[i]
		self := &a.acc
		g.node(self)
		for _, t := range a.TransfersFrom {
			g.addTransfer(seen, t, self)
		}
		for _, t := range a.TransfersTo {
			g.addTransfer(seen, t, self)
		}
		for _, c := range a.CallsAsCaller {
			g.addCall(seen, c, self)
		}
		for _, c := range a.CallsAsRealm {
			g.addCall(seen, c, self)
		}
		for _, d := range a.Deploys {
			g.addDeploy(seen, d, self)
		}
	}
	for _, d := range r.QueryDeploy {
		g.addDeploy(seen, d, nil)
	}
}

func (s *Server) buildGraph(ctx context.Context, seed string, depth, limit int) GraphResponse {
	resp := GraphResponse{Nodes: []Node{}, Links: []Link{}, Seed: seed, Depth: depth, Limit: limit}
	resp.Sync = s.syncInfo(ctx)

	g := newAgg()
	seen := map[string]bool{}
	keep := map[string]bool{}

	if seed == "" {
		var out struct {
			QueryTransfer []xferRow   `json:"queryTransfer"`
			QueryCall     []callRow   `json:"queryCall"`
			QueryDeploy   []deployRow `json:"queryDeploy"`
		}
		if err := s.dg.GraphQL(ctx, qRecent, map[string]any{"n": recentWindow}, &out); err != nil {
			resp.Warning = "dgraph: " + err.Error()
			return resp
		}
		for _, t := range out.QueryTransfer {
			g.addTransfer(seen, t, nil)
		}
		for _, c := range out.QueryCall {
			g.addCall(seen, c, nil)
		}
		for _, d := range out.QueryDeploy {
			g.addDeploy(seen, d, nil)
		}
	} else {
		id, err := s.resolve(ctx, seed)
		if err != nil {
			resp.Warning = "dgraph: " + err.Error()
			return resp
		}
		if id == "" {
			resp.Warning = "seed not found"
			return resp
		}
		resp.Seed = id
		keep[id] = true
		visited := map[string]bool{}
		frontier := []string{id}
		for hop := 0; hop < depth && len(frontier) > 0; hop++ {
			for _, f := range frontier {
				visited[f] = true
			}
			r, err := s.expand(ctx, frontier, edgesPerRelation)
			if err != nil {
				resp.Warning = "dgraph: " + err.Error()
				break
			}
			g.ingestExpand(seen, r)
			if hop == 0 {
				for nb := range g.nbrs[id] {
					keep[nb] = true // direct neighbors survive the limit cut first
				}
			}
			next := []string{}
			for addr := range g.nodes {
				if !visited[addr] {
					next = append(next, addr)
				}
			}
			// cap fan-out: expand the busiest unvisited nodes first
			sort.Slice(next, func(i, j int) bool { return len(g.nbrs[next[i]]) > len(g.nbrs[next[j]]) })
			if len(next) > limit {
				next = next[:limit]
			}
			if len(g.nodes) >= limit*3 {
				break
			}
			frontier = next
		}
	}

	resp.Nodes, resp.Links = g.finalize(limit, keep)
	return resp
}

// ---- node detail ----

type Neighbor struct {
	Node
	Weight     int64    `json:"weight"`
	LinkVolume int64    `json:"linkVolumeUgnot"`
	Kinds      []string `json:"kinds"`
}

type NodeDetail struct {
	Node
	Neighbors       []Neighbor  `json:"neighbors"`
	RecentTransfers []xferOut   `json:"recentTransfers"`
	RecentCalls     []callOut   `json:"recentCalls"`
	Deploys         []deployOut `json:"deploys"`
	Sync            SyncInfo    `json:"sync"`
}

type xferOut struct {
	Hash   string `json:"hash"`
	Height int64  `json:"height"`
	Amount int64  `json:"amount"`
	Denom  string `json:"denom"`
	From   string `json:"from"`
	To     string `json:"to"`
}

type callOut struct {
	Hash    string `json:"hash"`
	Height  int64  `json:"height"`
	Func    string `json:"func"`
	PkgPath string `json:"pkgPath"`
	Send    string `json:"send,omitempty"`
	Caller  string `json:"caller"`
	Realm   string `json:"realm"`
}

type deployOut struct {
	Hash    string `json:"hash"`
	Height  int64  `json:"height"`
	PkgPath string `json:"pkgPath"`
	Creator string `json:"creator"`
}

func addrOr(a *acc, def string) string {
	if a == nil || a.Address == "" {
		return def
	}
	return a.Address
}

const recentDetail = 25

func (s *Server) nodeDetail(ctx context.Context, rawID string) (*NodeDetail, error) {
	id, err := s.resolve(ctx, rawID)
	if err != nil {
		return nil, err
	}
	if id == "" {
		return nil, nil
	}
	r, err := s.expand(ctx, []string{id}, 500)
	if err != nil {
		return nil, err
	}
	if len(r.QueryAccount) == 0 {
		return nil, nil
	}
	g := newAgg()
	g.ingestExpand(map[string]bool{}, r)
	nodes, links := g.finalize(1_000_000, map[string]bool{id: true})

	byID := map[string]Node{}
	for _, n := range nodes {
		byID[n.ID] = n
	}
	d := &NodeDetail{Node: byID[id], Sync: s.syncInfo(ctx),
		Neighbors: []Neighbor{}, RecentTransfers: []xferOut{}, RecentCalls: []callOut{}, Deploys: []deployOut{}}
	for _, l := range links {
		other := l.Target
		if other == id {
			other = l.Source
		}
		d.Neighbors = append(d.Neighbors, Neighbor{Node: byID[other], Weight: l.Weight, LinkVolume: l.VolumeUgnot, Kinds: l.Kinds})
	}
	sort.Slice(d.Neighbors, func(i, j int) bool { return d.Neighbors[i].Weight > d.Neighbors[j].Weight })
	if len(d.Neighbors) > 100 {
		d.Neighbors = d.Neighbors[:100]
	}

	a := r.QueryAccount[0]
	for _, t := range a.TransfersFrom {
		d.RecentTransfers = append(d.RecentTransfers, xferOut{t.Hash, t.Height, t.Amount, t.Denom, id, addrOr(t.To, "")})
	}
	for _, t := range a.TransfersTo {
		d.RecentTransfers = append(d.RecentTransfers, xferOut{t.Hash, t.Height, t.Amount, t.Denom, addrOr(t.From, ""), id})
	}
	for _, c := range a.CallsAsCaller {
		d.RecentCalls = append(d.RecentCalls, callOut{c.Hash, c.Height, c.Func, c.PkgPath, c.Send, id, addrOr(c.Realm, c.PkgPath)})
	}
	for _, c := range a.CallsAsRealm {
		d.RecentCalls = append(d.RecentCalls, callOut{c.Hash, c.Height, c.Func, c.PkgPath, c.Send, addrOr(c.Caller, ""), id})
	}
	for _, dp := range a.Deploys {
		d.Deploys = append(d.Deploys, deployOut{dp.Hash, dp.Height, dp.PkgPath, id})
	}
	for _, dp := range r.QueryDeploy {
		d.Deploys = append(d.Deploys, deployOut{dp.Hash, dp.Height, dp.PkgPath, addrOr(dp.Creator, "")})
	}
	sort.Slice(d.RecentTransfers, func(i, j int) bool { return d.RecentTransfers[i].Height > d.RecentTransfers[j].Height })
	sort.Slice(d.RecentCalls, func(i, j int) bool { return d.RecentCalls[i].Height > d.RecentCalls[j].Height })
	if len(d.RecentTransfers) > recentDetail {
		d.RecentTransfers = d.RecentTransfers[:recentDetail]
	}
	if len(d.RecentCalls) > recentDetail {
		d.RecentCalls = d.RecentCalls[:recentDetail]
	}
	return d, nil
}
