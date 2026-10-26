package onbloc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client talks to Onbloc / gnolang tx-indexer GraphQL.
type Client struct {
	URL        string
	HTTPClient *http.Client
}

func New(url string) *Client {
	return &Client{
		URL: url,
		HTTPClient: &http.Client{
			Timeout: 90 * time.Second,
		},
	}
}

type gqlRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables,omitempty"`
}

type gqlResponse struct {
	Data   json.RawMessage `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

func (c *Client) Query(ctx context.Context, query string, vars map[string]any, out any) error {
	body, err := json.Marshal(gqlRequest{Query: query, Variables: vars})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	res, err := c.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		return err
	}
	if res.StatusCode >= 300 {
		return fmt.Errorf("onbloc graphql: HTTP %d: %s", res.StatusCode, truncate(raw, 300))
	}
	var gr gqlResponse
	if err := json.Unmarshal(raw, &gr); err != nil {
		return err
	}
	if len(gr.Errors) > 0 {
		// indexer sometimes returns partial data + "max elements" — still usable if Data set
		if gr.Data == nil || string(gr.Data) == "null" {
			return fmt.Errorf("onbloc graphql: %s", gr.Errors[0].Message)
		}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(gr.Data, out)
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}

type MemPackage struct {
	Path string `json:"path"`
	Name string `json:"name"`
}

type MsgValue struct {
	Typename string `json:"__typename"`

	// MsgCall
	Caller  string `json:"caller"`
	Func    string `json:"func"`
	PkgPath string `json:"pkg_path"`
	Send    string `json:"send"`

	// MsgAddPackage
	Creator string      `json:"creator"`
	Package *MemPackage `json:"package"`

	// BankMsgSend
	FromAddress string `json:"from_address"`
	ToAddress   string `json:"to_address"`
	Amount      string `json:"amount"`
}

type Message struct {
	TypeURL string   `json:"typeUrl"`
	Value   MsgValue `json:"value"`
}

type Transaction struct {
	Hash        string    `json:"hash"`
	BlockHeight int64     `json:"block_height"`
	Index       int64     `json:"index"`
	Success     *bool     `json:"success"`
	Messages    []Message `json:"messages"`
}

const qLatest = `{ latestBlockHeight }`

type latestData struct {
	LatestBlockHeight int64 `json:"latestBlockHeight"`
}

func (c *Client) LatestHeight(ctx context.Context) (int64, error) {
	var data latestData
	if err := c.Query(ctx, qLatest, nil, &data); err != nil {
		return 0, err
	}
	return data.LatestBlockHeight, nil
}

// Inclusive height window via FilterInt gt/lt (no gte/lte on this indexer).
// For [fromH, toH]: block_height.gt = fromH-1 (if fromH>0), block_height.lt = toH+1.
const qTxWindow = `
query TxWindow($where: FilterTransaction!) {
  getTransactions(where: $where) {
    hash
    block_height
    index
    success
    messages {
      typeUrl
      value {
        __typename
        ... on MsgCall {
          caller
          func
          pkg_path
          send
        }
        ... on MsgAddPackage {
          creator
          send
          package { path name }
        }
        ... on BankMsgSend {
          from_address
          to_address
          amount
        }
      }
    }
  }
}
`

type txWindowData struct {
	GetTransactions []Transaction `json:"getTransactions"`
}

func (c *Client) TransactionsBetween(ctx context.Context, fromH, toH int64) ([]Transaction, error) {
	if toH < fromH {
		return nil, nil
	}
	where := map[string]any{}
	bh := map[string]any{"lt": toH + 1}
	if fromH > 0 {
		bh["gt"] = fromH - 1
	} else {
		// include height 0: only upper bound; may be large at genesis — callers should keep windows small
	}
	where["block_height"] = bh
	// prefer successful txs when field present
	where["success"] = map[string]any{"eq": true}

	var data txWindowData
	err := c.Query(ctx, qTxWindow, map[string]any{"where": where}, &data)
	return data.GetTransactions, err
}
