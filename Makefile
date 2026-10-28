export GOFLAGS ?= -mod=mod

.PHONY: build test run demo docker
build:
	CGO_ENABLED=1 go build -trimpath -tags sqlite_omit_load_extension -o bin/capscope ./cmd/capscope

test:
	go vet ./... && go test -race -count=1 ./...

# Run against the local Docker daemon with auth off (local testing only).
run:
	ALLOW_NO_AUTH=true DB_PATH=./data/capscope.db LISTEN=127.0.0.1:8080 go run ./cmd/capscope

# Browse the UI with fake data, no Docker needed for the data itself (a daemon is still pinged at startup).
demo:
	mkdir -p data && rm -f data/demo.db* && go run ./cmd/seed-demo data/demo.db
	ALLOW_NO_AUTH=true DB_PATH=./data/demo.db LISTEN=127.0.0.1:8080 go run ./cmd/capscope

docker:
	docker build -t capscope:dev .
