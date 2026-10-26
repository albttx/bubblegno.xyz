package api

// /v0/sync — live ingest progress (SyncState) + cached Onbloc tip.

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/albttx/bubblegno.xyz/internal/onbloc"
)

type tipCache struct {
	mu  sync.Mutex
	ob  *onbloc.Client
	tip int64
	at  time.Time
}

func (t *tipCache) get(ctx context.Context) int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	if time.Since(t.at) < 10*time.Second {
		return t.tip
	}
	t.at = time.Now()
	if h, err := t.ob.LatestHeight(ctx); err == nil {
		t.tip = h
	}
	return t.tip
}

func (s *Server) handleSync(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	si := s.syncInfo(ctx)
	resp := map[string]any{"chainId": si.ChainID, "lastHeight": si.LastHeight}
	if tip := s.tip.get(ctx); tip > 0 {
		resp["tip"] = tip
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, resp)
}
