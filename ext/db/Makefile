# kling db — la extensión `kling-db` y el banco `kling-dbbench`.
#
#   make test     gofmt, vet y tests con -race
#   make cross    compila los dos binarios para todas las plataformas
#   make build    kling-db para esta máquina

CROSS_PLATS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64
CMDS        := ./cmd/kling-db ./cmd/kling-dbbench
# Como ext/mcp y release.yml: sin -X la extensión dice "dev" y doctor no
# puede compararla.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.Version=$(VERSION)

.PHONY: build test cross

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o kling-db ./cmd/kling-db

test:
	gofmt -l . | tee /dev/stderr | (! read -r _)
	go vet ./...
	go test -race -count=1 ./...

cross:
	@set -e; for p in $(CROSS_PLATS); do \
	  os=$${p%/*}; arch=$${p#*/}; \
	  for c in $(CMDS); do \
	    echo "  $$(basename $$c) $$os/$$arch"; \
	    CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -ldflags "$(LDFLAGS)" -o /dev/null $$c; \
	  done; \
	done
