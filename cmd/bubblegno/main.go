package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/albttx/bubblegno.xyz/internal/api"
	"github.com/albttx/bubblegno.xyz/internal/config"
	"github.com/albttx/bubblegno.xyz/internal/graph"
	"github.com/albttx/bubblegno.xyz/internal/ingest"
	"github.com/albttx/bubblegno.xyz/internal/onbloc"
)

func main() {
	cfg := config.FromEnv()
	serveOnly := flag.Bool("serve-only", false, "run HTTP API without watcher")
	once := flag.Bool("once", false, "ingest until chain tip then stop watcher")
	dryRun := flag.Bool("dry-run", false, "fetch+extract only, no Dgraph writes")
	window := flag.Int64("window", 50, "block heights per Onbloc query")
	maxHeight := flag.Int64("max-height", -1, "if >=0, ingest only up to this height then stop watcher")
	flag.Parse()

	log.Printf("bubblegno starting chain=%s onbloc=%s dgraph=%s http=%s",
		cfg.ChainID, cfg.OnblocGraphQL, cfg.DgraphHTTP, cfg.HTTPAddr)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	dg := graph.New(cfg.DgraphHTTP)
	ob := onbloc.New(cfg.OnblocGraphQL)

	if !*dryRun {
		if err := dg.Ping(ctx); err != nil {
			log.Printf("warn: dgraph not reachable: %v (make up && make schema)", err)
		}
	}

	if !*serveOnly {
		w := &ingest.Watcher{
			Cfg:       cfg,
			Onbloc:    ob,
			Graph:     dg,
			Window:    *window,
			PollEvery: 5 * time.Second,
			Once:      *once || *maxHeight >= 0,
			DryRun:    *dryRun,
			MaxHeight: *maxHeight,
		}
		go func() {
			if err := w.Run(ctx); err != nil && ctx.Err() == nil {
				log.Printf("watcher stopped: %v", err)
			}
		}()
	}

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           api.New(cfg, dg).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		log.Printf("api listening on %s", cfg.HTTPAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("api: %v", err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	log.Printf("bubblegno stopped")
}
