APP       := llm-gateway
PKG       := github.com/GenJi77JYXC/llm-gateway
BIN       := bin/$(APP)
CONFIG    ?= configs/config.yaml

.PHONY: all build run vet test tidy docker-up docker-down verify

all: vet build

build:
	go build -o $(BIN) ./cmd/gateway

run:
	go run ./cmd/gateway -config $(CONFIG)

vet:
	go vet ./...

test:
	go test -race ./...

tidy:
	go mod tidy

docker-up:
	docker compose up -d --build

docker-down:
	docker compose down

verify:
	bash scripts/verify.sh
