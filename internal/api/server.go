package api

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/albttx/bubblegno.xyz/internal/config"
	"github.com/albttx/bubblegno.xyz/internal/graph"
	"github.com/albttx/bubblegno.xyz/internal/onbloc"
)

// Server is the public read-only HTTP API (+ optional static SPA from web/dist).
type Server struct {
	cfg    config.Config
	dg     *graph.Client
	mux    *http.ServeMux
	webDir string
	search accCache
	tip    tipCache
	gcache graphCache
}

func New(cfg config.Config, dg *graph.Client) *Server {
	webDir := os.Getenv("BUBBLEGNO_WEB_DIR")
	if webDir == "" {
		webDir = filepath.Join("web", "dist")
	}
	s := &Server{cfg: cfg, dg: dg, mux: http.NewServeMux(), webDir: webDir, tip: tipCache{ob: onbloc.New(cfg.OnblocGraphQL)}}
	s.routes()
	go s.warmDefault()
	return s
}

func (s *Server) Handler() http.Handler { return cors(s.mux) }

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", s.healthz)
	s.mux.HandleFunc("GET /v0/graph", s.handleGraph)
	s.mux.HandleFunc("GET /v0/sync", s.handleSync)
	s.mux.HandleFunc("GET /v0/search", s.handleSearch)
	s.mux.HandleFunc("GET /v0/node/{id...}", s.handleNode) // {id...}: realm pkgPaths contain '/'
	s.mux.HandleFunc("GET /", s.spa)
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	resp := map[string]any{"ok": true, "service": "bubblegno", "chain": s.cfg.ChainID, "dgraph": true}
	if err := s.dg.Ping(ctx); err != nil {
		resp["dgraph"] = false
		resp["dgraphError"] = err.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleGraph(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	seed := strings.TrimSpace(q.Get("seed"))
	depth := clampInt(atoiDef(q.Get("depth"), 1), 1, 5)
	limit := clampInt(atoiDef(q.Get("limit"), 300), 1, 1000)

	f := Filter{MinWeight: clampInt(atoiDef(q.Get("minWeight"), 1), 1, 1000), Kinds: parseKinds(q.Get("kinds"))}
	key := graphKey(seed, depth, limit, f)
	body, err := s.gcache.get(key, func() ([]byte, bool, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		resp := s.buildGraph(ctx, seed, depth, limit, f)
		b, err := json.Marshal(resp)
		return b, resp.Warning == "" || resp.Warning == "seed not found", err
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=15")
	writeMaybeGzip(w, r, body)
}

func (s *Server) handleNode(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing id"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	detail, err := s.nodeDetail(ctx, id)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	if detail == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found", "id": id})
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

// spa serves web/dist when present; unknown paths fall back to index.html.
func (s *Server) spa(w http.ResponseWriter, r *http.Request) {
	index := filepath.Join(s.webDir, "index.html")
	if _, err := os.Stat(index); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error": "frontend not built (cd web && npm run build) — API lives under /v0",
		})
		return
	}
	clean := filepath.Clean("/" + r.URL.Path)
	p := filepath.Join(s.webDir, clean)
	if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
		http.ServeFile(w, r, p)
		return
	}
	http.ServeFile(w, r, index)
}

// cors allows local dev frontends (vite :5173 etc.) and bubblegno.xyz. Read-only API.
func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" && allowedOrigin(origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func allowedOrigin(o string) bool {
	for _, p := range []string{"http://localhost", "http://127.0.0.1", "https://bubblegno.xyz", "https://www.bubblegno.xyz"} {
		if o == p || strings.HasPrefix(o, p+":") {
			return true
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func atoiDef(s string, def int) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// parseKinds reads ?kinds=EOA,REALM,PACKAGE (case-insensitive). Empty or
// unrecognised → nil (no kind filter).
func parseKinds(raw string) map[string]bool {
	out := map[string]bool{}
	for _, k := range strings.Split(raw, ",") {
		k = strings.ToUpper(strings.TrimSpace(k))
		if k == "PKG" {
			k = "PACKAGE"
		}
		for _, a := range AllKinds {
			if k == a {
				out[k] = true
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
