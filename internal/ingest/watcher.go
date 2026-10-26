package ingest

import (
	"context"
	"log"
	"time"

	"github.com/albttx/bubblegno.xyz/internal/config"
	"github.com/albttx/bubblegno.xyz/internal/graph"
	"github.com/albttx/bubblegno.xyz/internal/onbloc"
)

type Watcher struct {
	Cfg       config.Config
	Onbloc    *onbloc.Client
	Graph     *graph.Client
	Window    int64
	PollEvery time.Duration
	// IdlePoll is used once caught up with the tip (default 20s).
	IdlePoll  time.Duration
	Once      bool
	DryRun    bool
	MaxHeight int64
}

func (w *Watcher) Run(ctx context.Context) error {
	if w.Window <= 0 {
		w.Window = 50
	}
	if w.PollEvery <= 0 {
		w.PollEvery = 5 * time.Second
	}
	if w.IdlePoll <= 0 {
		w.IdlePoll = 20 * time.Second
	}

	start := w.Cfg.StartHeight
	if !w.DryRun {
		// Never fall back to StartHeight on a read error (e.g. Dgraph still
		// booting after a reboot): that would rewind SyncState. Retry instead.
		for {
			h, ok, err := GetSyncHeight(ctx, w.Graph, w.Cfg.ChainID)
			if err == nil {
				if ok {
					start = h + 1
					log.Printf("ingest: resume from sync height %d → next %d", h, start)
				} else {
					log.Printf("ingest: no sync state, starting at %d", start)
				}
				break
			}
			log.Printf("ingest: sync state read failed: %v (retrying)", err)
			if err := sleep(ctx, 3*time.Second); err != nil {
				return err
			}
		}
	}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		tip, err := w.Onbloc.LatestHeight(ctx)
		if err != nil {
			log.Printf("ingest: latest height: %v", err)
			if w.Once {
				return err
			}
			if err := sleep(ctx, w.PollEvery); err != nil {
				return err
			}
			continue
		}
		if w.MaxHeight >= 0 && tip > w.MaxHeight {
			tip = w.MaxHeight
		}

		if start > tip {
			log.Printf("ingest: caught up at height %d", tip)
			if w.Once || w.MaxHeight >= 0 {
				return nil
			}
			if err := sleep(ctx, w.IdlePoll); err != nil {
				return err
			}
			continue
		}

		to := start + w.Window - 1
		if to > tip {
			to = tip
		}
		if err := w.ingestWindow(ctx, start, to); err != nil {
			log.Printf("ingest: window [%d,%d]: %v", start, to, err)
			if w.Once {
				return err
			}
			if err := sleep(ctx, w.PollEvery); err != nil {
				return err
			}
			continue
		}
		start = to + 1
	}
}

func (w *Watcher) ingestWindow(ctx context.Context, fromH, toH int64) error {
	txs, err := w.Onbloc.TransactionsBetween(ctx, fromH, toH)
	if err != nil {
		return err
	}
	batch := Extract(txs)
	log.Printf("ingest: heights [%d,%d] txs=%d accounts=%d transfers=%d calls=%d deploys=%d",
		fromH, toH, len(txs), len(batch.Accounts), len(batch.Transfers), len(batch.Calls), len(batch.Deploys))

	if w.DryRun {
		return nil
	}
	if len(batch.Accounts) > 0 || len(batch.Transfers) > 0 || len(batch.Calls) > 0 || len(batch.Deploys) > 0 {
		if err := UpsertBatch(ctx, w.Graph, batch); err != nil {
			return err
		}
	}
	return SetSyncHeight(ctx, w.Graph, w.Cfg.ChainID, toH)
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
