# kindling — un solo binario que hace de CLI y de daemon.
#
#   make install                       CLI en tu máquina
#   make deploy HOST=ssh://juan@lab    daemon en el host con KVM
#
# El CLI corre donde trabajas (macOS o Linux); el daemon solo donde hay KVM.

BIN     := kling
PKG     := ./cmd/kling

# Destino de instalación. Se elige el primer directorio del PATH que sea tuyo,
# para no pedir sudo: instalar una herramienta de usuario no debería requerirlo.
# Se puede forzar con PREFIX=/usr/local.
PREFIX  ?= $(shell for d in "$$HOME/.local" "$$HOME/go" /opt/homebrew /usr/local; do \
             [ -w "$$d/bin" ] && echo "$$d" && exit; done; echo "$$HOME/.local")
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.Version=$(VERSION)

# Host del daemon: ssh://usuario@maquina. Si no se pasa, se toma del contexto
# activo, para no repetirlo en cada despliegue.
HOST ?= $(shell $(BIN) config show 2>/dev/null | awk '/^contexto:/{print $$2}')

.PHONY: all build install uninstall daemon bridge deploy test clean fmt

all: build

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) $(PKG)

## install — pone el CLI en PREFIX/bin (usa sudo si hace falta escribir ahí)
install: build
	@mkdir -p $(PREFIX)/bin 2>/dev/null || sudo mkdir -p $(PREFIX)/bin
	@install -m755 $(BIN) $(PREFIX)/bin/$(BIN) 2>/dev/null \
		|| sudo install -m755 $(BIN) $(PREFIX)/bin/$(BIN)
	@echo "instalado: $(PREFIX)/bin/$(BIN)  ($(VERSION))"
	@case ":$$PATH:" in *":$(PREFIX)/bin:"*) ;; \
	  *) echo; echo "AVISO: $(PREFIX)/bin no está en tu PATH. Añádelo:"; \
	     echo "  echo 'export PATH=\"$(PREFIX)/bin:$$PATH\"' >> ~/.zshrc";; esac
	@echo
	@echo "Apúntalo a tu daemon:"
	@echo "  $(BIN) context add lab ssh://usuario@host"

uninstall:
	@rm -f $(PREFIX)/bin/$(BIN) 2>/dev/null || sudo rm -f $(PREFIX)/bin/$(BIN)
	@echo "desinstalado (la configuración en ~/.config/kling se conserva)"

## bridge — el puente stdio<->HTTP que corre DENTRO de las microVMs.
## Estático a propósito: el invitado es Alpine (musl) y no debe depender de libc.
bridge:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
		-ldflags "$(LDFLAGS)" -o kling-bridge ./cmd/kling-bridge
	@echo "kling-bridge  ($(VERSION))"

## daemon — compila el binario del host con KVM (siempre linux/amd64)
daemon:
	GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)-linux-amd64 $(PKG)
	@echo "$(BIN)-linux-amd64  ($(VERSION))"

## deploy — instala el daemon por SSH y lo reinicia
deploy: daemon
	@test -n "$(HOST)" || { echo "usa: make deploy HOST=ssh://usuario@maquina" >&2; exit 1; }
	$(eval TARGET := $(patsubst ssh://%,%,$(HOST)))
	scp -q $(BIN)-linux-amd64 $(TARGET):/tmp/$(BIN)
	scp -q packaging/$(BIN).service packaging/$(BIN)-gateway.service $(TARGET):/tmp/
	ssh $(TARGET) 'sudo install -m755 /tmp/$(BIN) /usr/local/bin/$(BIN) && \
		sudo install -m644 /tmp/$(BIN).service /etc/systemd/system/ && \
		sudo systemctl daemon-reload && sudo systemctl enable --now $(BIN) && \
		sleep 1 && systemctl is-active $(BIN)'
	@echo "daemon desplegado en $(TARGET)"

test:
	go vet ./...
	go build -race -o /dev/null $(PKG)

fmt:
	gofmt -l -w .

clean:
	rm -f $(BIN) $(BIN)-linux-amd64
