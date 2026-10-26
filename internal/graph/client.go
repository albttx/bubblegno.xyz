package graph

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client talks to Dgraph HTTP (GraphQL + health + admin schema).
type Client struct {
	HTTPURL    string
	HTTPClient *http.Client
}

func New(httpURL string) *Client {
	return &Client{
		HTTPURL: httpURL,
		HTTPClient: &http.Client{
			Timeout: 60 * time.Second,
		},
	}
}

func (c *Client) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.HTTPURL+"/health", nil)
	if err != nil {
		return err
	}
	res, err := c.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		return fmt.Errorf("dgraph health: HTTP %d", res.StatusCode)
	}
	return nil
}

func (c *Client) ApplySchema(ctx context.Context, schema []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.HTTPURL+"/admin/schema", bytes.NewReader(schema))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	res, err := c.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode >= 300 {
		return fmt.Errorf("dgraph schema: HTTP %d: %s", res.StatusCode, string(body))
	}
	return nil
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

func (c *Client) GraphQL(ctx context.Context, query string, vars map[string]any, out any) error {
	body, err := json.Marshal(gqlRequest{Query: query, Variables: vars})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.HTTPURL+"/graphql", bytes.NewReader(body))
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
		return fmt.Errorf("dgraph graphql: HTTP %d: %s", res.StatusCode, truncate(raw, 400))
	}
	var gr gqlResponse
	if err := json.Unmarshal(raw, &gr); err != nil {
		return fmt.Errorf("dgraph graphql decode: %w body=%s", err, truncate(raw, 400))
	}
	if len(gr.Errors) > 0 {
		return fmt.Errorf("dgraph graphql: %s", gr.Errors[0].Message)
	}
	if out == nil || gr.Data == nil {
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
