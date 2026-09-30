# kling db — la extensión `kling-db` y el banco `kling-dbbench`.
#
#   make test     gofmt, vet y tests con -race
#   make cross    compila los dos binarios para todas las plataformas
#   make build    kling-db para esta máquina

CROSS_PLATS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64
CMDS        := ./cmd/kling-db ./cmd/kling-dbbench

.PHONY: build test cross

build:
	go build -o kling-db ./cmd/kling-db

test:
	gofmt -l . | tee /dev/stderr | (! read -r _)
	go vet ./...
	go test -race ./...

cross:
	@set -e; for p in $(CROSS_PLATS); do \
	  os=$${p%/*}; arch=$${p#*/}; \
	  for c in $(CMDS); do \
	    echo "  $$(basename $$c) $$os/$$arch"; \
	    CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -o /dev/null $$c; \
	  done; \
	done
