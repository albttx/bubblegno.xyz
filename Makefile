.PHONY: up down schema run tidy

up:
	docker compose up -d

down:
	docker compose down

schema:
	curl -sS -X POST http://localhost:8080/admin/schema \
	  --data-binary @deploy/dgraph/schema.graphql

run:
	go run ./cmd/bubblegno

tidy:
	go mod tidy

build:
	go build -o bin/bubblegno ./cmd/bubblegno
