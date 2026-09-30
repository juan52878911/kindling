# kindling-sandbox — la extensión de kling que sirve sandboxes de agentes de
# código a través de un frontal.
#
#   make install                       kling-sandbox en tu máquina
#   make deploy HOST=ssh://usuario@lab    el frontal en el host del daemon
#
# Necesita kling instalado (de la misma release): esto no hace nada por su
# cuenta, añade `kling sbx` y habla con uno o varios daemons de kindling por su
# API. Es un módulo aparte dentro del repo de kindling (ext/sandbox): su go.mod
# apunta al núcleo del mismo commit con un replace a ../.. (sin workspace
# también vale).

BIN     := kling-sandbox
PKG     := ./cmd/kling-sandbox
GOARCH  ?= amd64
PREFIX  ?= $(shell for d in "$$HOME/.local" "$$HOME/go" /opt/homebrew /usr/local; do \
             [ -w "$$d/bin" ] && echo "$$d" && exit; done; echo "$$HOME/.local")
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.Version=$(VERSION)
# Plataformas que publica la release (ver .github/workflows/release.yml).
CROSS_PLATS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64
HOST ?= $(shell kling config show 2>/dev/null | awk '/^contexto:|^context:/{print $$2}')

.PHONY: all build install uninstall linux deploy test cross operator-image fmt clean

all: build

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) $(PKG)

## install — kling descubre la extensión por estar en el PATH con este nombre
install: build
	@mkdir -p $(PREFIX)/bin 2>/dev/null || sudo mkdir -p $(PREFIX)/bin
	@install -m755 $(BIN) $(PREFIX)/bin/$(BIN) 2>/dev/null \
		|| sudo install -m755 $(BIN) $(PREFIX)/bin/$(BIN)
	@echo "instalado: $(PREFIX)/bin/$(BIN)  ($(VERSION))"
	@command -v kling >/dev/null 2>&1 || echo "AVISO: no encuentro kling; instala kindling primero."
	@echo
	@echo "Comprueba que kling la ve:  kling plugins"

uninstall:
	@rm -f $(PREFIX)/bin/$(BIN) 2>/dev/null || sudo rm -f $(PREFIX)/bin/$(BIN)

linux:
	GOOS=linux GOARCH=$(GOARCH) go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)-linux-$(GOARCH) $(PKG)

## deploy — el frontal en el host del daemon, con su unidad y su token
deploy: linux
	@test -n "$(HOST)" || { echo "usa: make deploy HOST=ssh://usuario@maquina" >&2; exit 1; }
	$(eval TARGET := $(patsubst ssh://%,%,$(HOST)))
	@# A un directorio de mktemp -d (0700, del usuario de SSH), no a /tmp/<nombre>:
	@# otro usuario del host podía dejar antes un enlace con ese nombre y
	@# `sudo install` lo instalaba como root. Igual que el deploy de la raíz.
	@set -e; \
	D=$$(ssh $(TARGET) 'mktemp -d'); \
	case "$$D" in /*) ;; *) echo "mktemp -d on $(TARGET) failed: $$D" >&2; exit 1;; esac; \
	trap 'ssh $(TARGET) "rm -rf $$D"' EXIT; \
	scp -q $(BIN)-linux-$(GOARCH) $(TARGET):$$D/$(BIN); \
	scp -q packaging/kling-sandbox.service $(TARGET):$$D/; \
	ssh $(TARGET) "D=$$D; "'test -x /usr/local/bin/kling || { echo "falta kling en el host: despliega kindling primero" >&2; exit 1; } && \
		sudo install -m755 "$$D/$(BIN)" /usr/local/bin/$(BIN) && \
		KU=$$( . /etc/default/kling 2>/dev/null; echo "$${KLING_SOCKET_USER:-$$(id -un)}" ) && \
		KG=$$(id -gn "$$KU") && \
		for u in kling-sandbox.service; do \
		  sed -e "s/@KLING_USER@/$$KU/" -e "s/@KLING_GROUP@/$$KG/" "$$D/$$u" | sudo tee /etc/systemd/system/$$u >/dev/null && \
		  sudo chmod 644 /etc/systemd/system/$$u || exit 1; \
		done && \
		sudo install -d -m755 /etc/kling && \
		( [ -s /etc/kling/sandbox.env ] || \
		  ( sudo install -m600 /dev/null /etc/kling/sandbox.env && \
		  printf "KLING_SANDBOX_TOKEN=%s\n" \
		    "$$(head -c32 /dev/urandom | base64 | tr "+/" "\-_" | tr -d "=")" \
		  | sudo tee /etc/kling/sandbox.env >/dev/null ) ) && \
		sudo chmod 600 /etc/kling/sandbox.env && \
		sudo systemctl daemon-reload && sudo systemctl enable kling-sandbox && \
		sudo systemctl restart kling-sandbox && sleep 1 && systemctl is-active kling-sandbox'
	@echo "frontal desplegado en $(TARGET)"
	@echo
	@echo "Apunta tu CLI al token (por una tubería, no como argumento):"
	@echo "  ssh $(TARGET) 'sudo cut -d= -f2 /etc/kling/sandbox.env' | kling config set sandbox.token -"

## test — lo mismo que corre el CI
test:
	gofmt -l . | tee /dev/stderr | (! read -r _)
	go vet ./...
	go test -race ./...

## cross — compilaciones cruzadas de lo que publica la release desde este módulo,
## sin escribir nada: kling-sandbox en las cuatro plataformas (cliente en el
## portátil, frontal en el host del daemon) y el operador solo en linux, que es
## donde corre un clúster.
cross:
	@set -e; for p in $(CROSS_PLATS); do \
	  os=$${p%/*}; arch=$${p#*/}; \
	  echo "  kling-sandbox $$os/$$arch"; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -o /dev/null $(PKG); \
	  if [ $$os = linux ]; then \
	    echo "  kindling-operator $$os/$$arch"; \
	    CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -o /dev/null ./cmd/kindling-operator; \
	  fi; \
	done

## operator-image — la imagen del operador. El contexto es la raíz del repo: el
## replace ../.. necesita el núcleo del mismo commit.
operator-image:
	docker build -f Dockerfile.operator --build-arg VERSION=$(VERSION) \
		-t ghcr.io/juan52878911/kindling-operator:$(VERSION) ../..

fmt:
	gofmt -l -w .

clean:
	rm -f $(BIN) $(BIN)-linux-amd64 $(BIN)-linux-arm64
