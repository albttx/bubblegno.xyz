package ingest

import (
	"context"
	"fmt"
	"time"

	"github.com/albttx/bubblegno.xyz/internal/graph"
)

const mutUpsertBatch = `
mutation UpsertBatch(
  $accounts: [AddAccountInput!]!
  $transfers: [AddTransferInput!]!
  $calls: [AddCallInput!]!
  $deploys: [AddDeployInput!]!
) {
  addAccount(input: $accounts, upsert: true) { numUids }
  addTransfer(input: $transfers, upsert: true) { numUids }
  addCall(input: $calls, upsert: true) { numUids }
  addDeploy(input: $deploys, upsert: true) { numUids }
}
`

const qSync = `
query Sync($chainId: String!) {
  querySyncState(filter: { chainId: { eq: $chainId } }) {
    lastHeight
  }
}
`

const mutSync = `
mutation SetSync($input: [AddSyncStateInput!]!) {
  addSyncState(input: $input, upsert: true) { numUids }
}
`

type syncQueryData struct {
	QuerySyncState []struct {
		LastHeight int64 `json:"lastHeight"`
	} `json:"querySyncState"`
}

func GetSyncHeight(ctx context.Context, dg *graph.Client, chainID string) (int64, bool, error) {
	var data syncQueryData
	if err := dg.GraphQL(ctx, qSync, map[string]any{"chainId": chainID}, &data); err != nil {
		return 0, false, err
	}
	if len(data.QuerySyncState) == 0 {
		return 0, false, nil
	}
	return data.QuerySyncState[0].LastHeight, true, nil
}

func SetSyncHeight(ctx context.Context, dg *graph.Client, chainID string, height int64) error {
	// guard: never move SyncState backwards
	if cur, ok, err := GetSyncHeight(ctx, dg, chainID); err == nil && ok && cur > height {
		return nil
	}
	input := []map[string]any{{
		"chainId":    chainID,
		"lastHeight": height,
		"updatedAt":  time.Now().UTC().Format(time.RFC3339),
	}}
	return dg.GraphQL(ctx, mutSync, map[string]any{"input": input}, nil)
}

func accountInput(a AccountRef) map[string]any {
	m := map[string]any{
		"address": a.Address,
		"kind":    string(a.Kind),
	}
	if a.PkgPath != "" {
		m["pkgPath"] = a.PkgPath
	}
	return m
}

func UpsertBatch(ctx context.Context, dg *graph.Client, batch *Batch) error {
	accounts := make([]map[string]any, 0, len(batch.Accounts))
	for _, a := range batch.Accounts {
		accounts = append(accounts, accountInput(a))
	}
	transfers := make([]map[string]any, 0, len(batch.Transfers))
	for _, t := range batch.Transfers {
		denom := t.Denom
		if denom == "" {
			denom = "ugnot"
		}
		transfers = append(transfers, map[string]any{
			"uidKey": t.UIDKey,
			"hash":   t.Hash,
			"height": t.Height,
			"index":  t.Index,
			"amount": t.Amount,
			"denom":  denom,
			"from":   map[string]any{"address": t.From.Address},
			"to":     map[string]any{"address": t.To.Address},
		})
	}
	calls := make([]map[string]any, 0, len(batch.Calls))
	for _, cl := range batch.Calls {
		calls = append(calls, map[string]any{
			"uidKey":  cl.UIDKey,
			"hash":    cl.Hash,
			"height":  cl.Height,
			"index":   cl.Index,
			"func":    cl.Func,
			"pkgPath": cl.PkgPath,
			"send":    cl.Send,
			"caller":  map[string]any{"address": cl.Caller.Address},
			"realm":   map[string]any{"address": cl.Realm.Address},
		})
	}
	deploys := make([]map[string]any, 0, len(batch.Deploys))
	for _, d := range batch.Deploys {
		deploys = append(deploys, map[string]any{
			"uidKey":  d.UIDKey,
			"hash":    d.Hash,
			"height":  d.Height,
			"pkgPath": d.PkgPath,
			"creator": map[string]any{"address": d.Creator.Address},
			"package": map[string]any{"address": d.Package.Address},
		})
	}

	vars := map[string]any{
		"accounts":  accounts,
		"transfers": transfers,
		"calls":     calls,
		"deploys":   deploys,
	}
	if err := dg.GraphQL(ctx, mutUpsertBatch, vars, nil); err != nil {
		return fmt.Errorf("upsert batch (acc=%d xfer=%d call=%d deploy=%d): %w",
			len(accounts), len(transfers), len(calls), len(deploys), err)
	}
	return nil
}
