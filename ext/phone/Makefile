# kling phone — la extensión `kling-phone`: teléfonos Android en microVMs.
#
#   make build    kling-phone para esta máquina
#   make install  en el directorio de extensiones (~/.local/share/kling/plugins)
#   make test     gofmt, vet y tests con -race
#   make cross    compila para todas las plataformas

CROSS_PLATS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.Version=$(VERSION)
PLUGINS ?= $(or $(KLING_PLUGIN_PATH),$(or $(XDG_DATA_HOME),$(HOME)/.local/share)/kling/plugins)

.PHONY: build install test cross

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o kling-phone ./cmd/kling-phone

install: build
	@mkdir -p $(firstword $(subst :, ,$(PLUGINS)))
	install -m755 kling-phone $(firstword $(subst :, ,$(PLUGINS)))/kling-phone

test:
	gofmt -l . | tee /dev/stderr | (! read)
	go vet ./...
	go test -race ./...

cross:
	@set -e; for p in $(CROSS_PLATS); do \
	  os=$${p%/*}; arch=$${p#*/}; \
	  echo "  kling-phone $$os/$$arch"; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -o /dev/null ./cmd/kling-phone; \
	done
