# kindling-mcp — la extensión de kling que aloja servidores MCP en microVMs.
#
#   make install                       kling-mcp y el puente local en tu máquina
#   make deploy HOST=ssh://juan@lab    extensión, puente, empaquetador y gateway
#                                      en el host del daemon
#
# Necesita kindling ya instalado: kling-mcp no hace nada por su cuenta, añade
# comandos a `kling` (kling mcp, kling add, kling connect, kling gateway...) y
# habla con el daemon de kindling por su API.

BIN     := kling-mcp
PKG     := ./cmd/kling-mcp

# Arquitectura de lo que se compila para Linux (host del daemon y el puente que
# va dentro de las microVMs). Para una VM arm64 (Lima en un Mac Apple Silicon):
#   make deploy GOARCH=arm64 HOST=ssh://usuario@vm-arm
GOARCH  ?= amd64

# Destino de instalación: el primer directorio del PATH que sea tuyo, para no
# pedir sudo. Se puede forzar con PREFIX=/usr/local.
PREFIX  ?= $(shell for d in "$$HOME/.local" "$$HOME/go" /opt/homebrew /usr/local; do \
             [ -w "$$d/bin" ] && echo "$$d" && exit; done; echo "$$HOME/.local")
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.Version=$(VERSION)

# Host del daemon: ssh://usuario@maquina. Si no se pasa, se toma del contexto
# activo de kling.
HOST ?= $(shell kling config show 2>/dev/null | awk '/^contexto:|^context:/{print $$2}')

# Checkout de kindling para el test que ata las familias de runtime con
# 70-build-minimal-image.sh. Sin él ese test se salta.
KINDLING_DIR ?= $(wildcard ../kindling)

.PHONY: all build install uninstall bridge bridge-local linux deploy deploy-mac test clean fmt

all: build

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) $(PKG)

## install — kling-mcp y el puente local en PREFIX/bin
##
## `kling` descubre la extensión por estar en el PATH con el nombre kling-mcp.
## El puente se instala siempre aunque la memoria venga apagada: activarla debe
## ser un comando, no un proyecto.
install: build bridge-local
	@mkdir -p $(PREFIX)/bin 2>/dev/null || sudo mkdir -p $(PREFIX)/bin
	@install -m755 $(BIN) $(PREFIX)/bin/$(BIN) 2>/dev/null \
		|| sudo install -m755 $(BIN) $(PREFIX)/bin/$(BIN)
	@install -m755 kling-bridge-local $(PREFIX)/bin/kling-bridge 2>/dev/null \
		|| sudo install -m755 kling-bridge-local $(PREFIX)/bin/kling-bridge
	@echo "instalado: $(PREFIX)/bin/$(BIN)  ($(VERSION))"
	@echo "           $(PREFIX)/bin/kling-bridge"
	@command -v kling >/dev/null 2>&1 || { echo; \
	  echo "AVISO: no encuentro kling. kling-mcp es una extensión: instala kindling primero."; }
	@echo
	@echo "Comprueba que kling la ve:"
	@echo "  kling plugins"
	@echo "  kling mcp search github"

uninstall:
	@rm -f $(PREFIX)/bin/$(BIN) $(PREFIX)/bin/kling-bridge 2>/dev/null \
		|| sudo rm -f $(PREFIX)/bin/$(BIN) $(PREFIX)/bin/kling-bridge
	@echo "desinstalado"

## bridge — el puente stdio<->HTTP que corre DENTRO de las microVMs.
## Estático a propósito: el invitado es Alpine (musl) y no debe depender de libc.
bridge:
	CGO_ENABLED=0 GOOS=linux GOARCH=$(GOARCH) go build -trimpath \
		-ldflags "$(LDFLAGS)" -o kling-bridge ./cmd/kling-bridge
	@echo "kling-bridge  ($(VERSION), linux/$(GOARCH))"

## bridge-local — el mismo puente, para TU máquina: expone por HTTP un MCP de
## stdio que ya tengas instalado, para enlazarlo con `kling mcp link`.
bridge-local:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o kling-bridge-local ./cmd/kling-bridge
	@echo "kling-bridge-local  ($(VERSION))"

## linux — kling-mcp para el host del daemon (gateway, heal y el constructor mcp)
linux:
	GOOS=linux GOARCH=$(GOARCH) go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)-linux-$(GOARCH) $(PKG)
	@echo "$(BIN)-linux-$(GOARCH)  ($(VERSION))"

## deploy — instala la extensión en el host del daemon por SSH
##
## Va kling-mcp (lo usan el gateway, el heal y el constructor), el puente y
## 80-mcp-image.sh, que el constructor "mcp" ejecuta como root porque construir
## una imagen monta un loopback y hace chroot. El daemon de kindling tiene que
## estar ya desplegado (make deploy en kindling).
deploy: linux bridge
	@test -n "$(HOST)" || { echo "usa: make deploy HOST=ssh://usuario@maquina" >&2; exit 1; }
	$(eval TARGET := $(patsubst ssh://%,%,$(HOST)))
	scp -q $(BIN)-linux-$(GOARCH) $(TARGET):/tmp/$(BIN)
	scp -q kling-bridge scripts/80-mcp-image.sh $(TARGET):/tmp/
	scp -q scripts/builders/mcp $(TARGET):/tmp/builder-mcp
	scp -q packaging/kling-gateway.service packaging/kling-heal.service \
		packaging/kling-heal.timer $(TARGET):/tmp/
	ssh $(TARGET) 'test -x /usr/local/bin/kling || { echo "falta kling en el host: despliega kindling primero" >&2; exit 1; } && \
		sudo install -m755 /tmp/$(BIN) /usr/local/bin/$(BIN) && \
		sudo install -d /usr/local/lib/kindling /usr/local/lib/kindling/builders && \
		sudo install -m755 /tmp/kling-bridge /usr/local/lib/kindling/kling-bridge && \
		sudo install -m755 /tmp/80-mcp-image.sh /usr/local/lib/kindling/80-mcp-image.sh && \
		sudo install -m755 /tmp/builder-mcp /usr/local/lib/kindling/builders/mcp && \
		sudo install -m644 /tmp/kling-gateway.service /tmp/kling-heal.service \
			/tmp/kling-heal.timer /etc/systemd/system/ && \
		sudo install -d -m755 /etc/kling && \
		( [ -s /etc/kling/gateway.env ] || \
		  ( sudo install -m600 /dev/null /etc/kling/gateway.env && \
		  printf "KLING_GATEWAY_TOKEN=%s\n" \
		    "$$(head -c32 /dev/urandom | base64 | tr "+/" "\-_" | tr -d "=")" \
		  | sudo tee /etc/kling/gateway.env >/dev/null ) ) && \
		sudo chmod 600 /etc/kling/gateway.env && \
		sudo systemctl daemon-reload && \
		sudo systemctl enable --now kling-heal.timer && \
		sudo systemctl enable kling-gateway && sudo systemctl restart kling-gateway && \
		sleep 1 && systemctl is-active kling-gateway'
	@echo "kindling-mcp desplegado en $(TARGET)"
	@echo "  kling-mcp en /usr/local/bin; puente, empaquetador y constructor en /usr/local/lib/kindling"
	@echo
	@# El puente vive DENTRO de cada imagen: desplegarlo aquí no toca los
	@# servicios ya empaquetados, y uno antiguo puede no entender los parámetros
	@# nuevos del kernel.
	@echo "El puente vive DENTRO de cada imagen. Ponlo al día en las ya construidas:"
	@echo "  kling mcp refresh-bridge"
	@echo
	@echo "Apunta tu CLI al token del gateway (se generó una vez, se conserva):"
	@echo "  kling config set gateway.token \\"
	@echo "    \$$(ssh $(TARGET) 'sudo cut -d= -f2 /etc/kling/gateway.env')"
	@echo "  kling connect -all -install all"

## deploy-mac — deploy con GOARCH=arm64, para una VM Lima en un Mac Apple Silicon
deploy-mac:
	@$(MAKE) deploy GOARCH=arm64 HOST="$(HOST)"

## test — lo mismo que corre el CI
test:
	gofmt -l . | tee /dev/stderr | (! read)
	go vet ./...
	KINDLING_DIR="$(abspath $(KINDLING_DIR))" go test -race ./...

fmt:
	gofmt -l -w .

clean:
	rm -f $(BIN) $(BIN)-linux-amd64 $(BIN)-linux-arm64 kling-bridge kling-bridge-local
