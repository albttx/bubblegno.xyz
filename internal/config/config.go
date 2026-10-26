package config

import (
	"os"
	"strconv"
)

type Config struct {
	HTTPAddr      string
	DgraphHTTP    string
	DgraphGRPC    string
	OnblocGraphQL string
	ChainID       string
	StartHeight   int64
}

func FromEnv() Config {
	return Config{
		HTTPAddr:      getenv("BUBBLEGNO_HTTP_ADDR", ":8090"),
		DgraphHTTP:    getenv("BUBBLEGNO_DGRAPH_HTTP", "http://localhost:8080"),
		DgraphGRPC:    getenv("BUBBLEGNO_DGRAPH_GRPC", "localhost:9080"),
		OnblocGraphQL: getenv("BUBBLEGNO_ONBLOC_GRAPHQL", "https://indexer.onbloc.xyz/graphql/query"),
		ChainID:       getenv("BUBBLEGNO_CHAIN_ID", "gnoland-1"),
		StartHeight:   getenvInt64("BUBBLEGNO_START_HEIGHT", 0),
	}
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func getenvInt64(k string, def int64) int64 {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return def
	}
	return n
}
