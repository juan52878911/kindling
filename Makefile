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

# Artefactos que se pueden embeber en el binario del daemon (ver `make assets`).
# El kernel lo deja 30-fetch-artifacts.sh en /opt/fc; la imagen base la construye
# 70-build-minimal-image.sh en $KLING_ROOT/images.
FC_DIR     ?= /opt/fc
IMAGES_DIR ?= /var/lib/kindling/images
BLOBS      := internal/assets/blobs

.PHONY: all build install uninstall daemon daemon-full assets bridge bridge-local deploy test clean fmt

all: build

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) $(PKG)

## install — pone el CLI en PREFIX/bin (usa sudo si hace falta escribir ahí)
install: build bridge-local
	@mkdir -p $(PREFIX)/bin 2>/dev/null || sudo mkdir -p $(PREFIX)/bin
	@install -m755 $(BIN) $(PREFIX)/bin/$(BIN) 2>/dev/null \
		|| sudo install -m755 $(BIN) $(PREFIX)/bin/$(BIN)
	@# El puente se instala SIEMPRE aunque la memoria venga apagada: activarla
	@# debe ser un comando, no un proyecto.
	@install -m755 kling-bridge-local $(PREFIX)/bin/kling-bridge 2>/dev/null \
		|| sudo install -m755 kling-bridge-local $(PREFIX)/bin/kling-bridge
	@echo "instalado: $(PREFIX)/bin/$(BIN)  ($(VERSION))"
	@echo "           $(PREFIX)/bin/kling-bridge"
	@case ":$$PATH:" in *":$(PREFIX)/bin:"*) ;; \
	  *) echo; echo "AVISO: $(PREFIX)/bin no está en tu PATH. Añádelo:"; \
	     echo "  echo 'export PATH=\"$(PREFIX)/bin:$$PATH\"' >> ~/.zshrc";; esac
	@echo
	@echo "Apúntalo a tu daemon:"
	@echo "  $(BIN) context add lab ssh://usuario@host"
	@echo
	@echo "Memoria de uso (opcional, apagada):"
	@echo "  $(BIN) memory status"

uninstall:
	@rm -f $(PREFIX)/bin/$(BIN) 2>/dev/null || sudo rm -f $(PREFIX)/bin/$(BIN)
	@echo "desinstalado (la configuración en ~/.config/kling se conserva)"

## bridge — el puente stdio<->HTTP que corre DENTRO de las microVMs.
## Estático a propósito: el invitado es Alpine (musl) y no debe depender de libc.
bridge:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
		-ldflags "$(LDFLAGS)" -o kling-bridge ./cmd/kling-bridge
	@echo "kling-bridge  ($(VERSION))"

## bridge-local — el mismo puente, para TU máquina.
##
## Sirve para exponer por HTTP un servidor MCP de stdio que ya tengas instalado
## (engram, obsidian, lo que sea) y enlazarlo con `kling mcp link`, sin meterlo
## en una microVM.
bridge-local:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o kling-bridge-local ./cmd/kling-bridge
	@echo "kling-bridge-local  ($(VERSION))"
	@echo
	@echo "Expón un MCP de stdio que tengas en local:"
	@echo "  ./kling-bridge-local -listen 0.0.0.0:9100 -- engram mcp --tools=agent"
	@echo "  kling mcp link engram http://<tu-ip>:9100/mcp"

## daemon — compila el binario del host con KVM (siempre linux/amd64)
daemon:
	GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)-linux-amd64 $(PKG)
	@echo "$(BIN)-linux-amd64  ($(VERSION))"

## assets — reúne en internal/assets/blobs lo que va a ir DENTRO del binario.
##
## Los blobs no se versionan (están en .gitignore): pesan ~20 MB, se regeneran
## con los scripts y meterlos en git haría que cada clon los arrastrase para
## siempre. Este target solo los copia; conseguirlos sigue siendo trabajo de
## 30-fetch-artifacts.sh y 70-build-minimal-image.sh, que necesitan root.
assets:
	@test -f $(FC_DIR)/vmlinux || { \
	  echo "falta $(FC_DIR)/vmlinux — consíguelo con:  sudo ./scripts/30-fetch-artifacts.sh" >&2; exit 1; }
	@test -f $(IMAGES_DIR)/min.ext4 || { \
	  echo "falta $(IMAGES_DIR)/min.ext4 — constrúyela con:  sudo ./scripts/70-build-minimal-image.sh" >&2; exit 1; }
	@mkdir -p $(BLOBS)
	@# -L porque en /opt/fc `vmlinux` es un enlace a vmlinux-6.1.x, y go:embed
	@# no sigue enlaces: copiar el enlace embebería 20 bytes de ruta.
	@cp -L $(FC_DIR)/vmlinux $(BLOBS)/vmlinux
	@cp $(IMAGES_DIR)/min.ext4 $(BLOBS)/min.ext4
	@ls -lh $(BLOBS) | awk 'NR>1 {print "  " $$5, $$9}'

## daemon-full — el daemon CON el kernel y la imagen base dentro.
##
## Es lo que hace que instalar sea un binario: `kling up` los materializa en
## $KLING_ROOT/images y ya no hace falta correr ningún script a mano. El binario
## engorda ~20 MB, por eso `make daemon` a secas sigue existiendo y por eso el
## CLI (`make build`) no lleva nada: en un Mac ese kernel no se usa jamás.
daemon-full: assets
	GOOS=linux GOARCH=amd64 go build -trimpath -tags embed_assets \
		-ldflags "$(LDFLAGS)" -o $(BIN)-linux-amd64 $(PKG)
	@echo "$(BIN)-linux-amd64  ($(VERSION))  con artefactos embebidos"

## deploy — instala el daemon por SSH y lo reinicia
## Van también el puente y 80-mcp-image.sh: `kling add` los necesita EN el host,
## porque construir una imagen monta un loopback y hace chroot, y eso solo lo
## puede hacer quien ya es root allí.
deploy: daemon bridge
	@# --now no reinicia lo que ya corre: hace falta restart explícito.
	@test -n "$(HOST)" || { echo "usa: make deploy HOST=ssh://usuario@maquina" >&2; exit 1; }
	$(eval TARGET := $(patsubst ssh://%,%,$(HOST)))
	scp -q $(BIN)-linux-amd64 $(TARGET):/tmp/$(BIN)
	scp -q kling-bridge scripts/80-mcp-image.sh $(TARGET):/tmp/
	scp -q packaging/$(BIN).service packaging/$(BIN)-gateway.service $(TARGET):/tmp/
	ssh $(TARGET) 'sudo install -m755 /tmp/$(BIN) /usr/local/bin/$(BIN) && \
		sudo install -d /usr/local/lib/kindling && \
		sudo install -m755 /tmp/kling-bridge /usr/local/lib/kindling/kling-bridge && \
		sudo install -m755 /tmp/80-mcp-image.sh /usr/local/lib/kindling/80-mcp-image.sh && \
		sudo install -m644 /tmp/$(BIN).service /tmp/$(BIN)-gateway.service /etc/systemd/system/ && \
		sudo install -d -m755 /etc/kling && \
		( [ -s /etc/kling/gateway.env ] || \
		  printf "KLING_GATEWAY_TOKEN=%s\n" \
		    "$$(head -c32 /dev/urandom | base64 | tr "+/" "\-_" | tr -d "=")" \
		  | sudo tee /etc/kling/gateway.env >/dev/null ) && \
		sudo chmod 600 /etc/kling/gateway.env && \
		sudo systemctl daemon-reload && sudo systemctl enable $(BIN) && \
		sudo systemctl restart $(BIN) && sudo systemctl try-restart $(BIN)-gateway && \
		sleep 1 && systemctl is-active $(BIN)'
	@echo "daemon desplegado en $(TARGET)"
	@echo "  puente y empaquetador en /usr/local/lib/kindling (los usa 'kling add')"
	@echo
	@echo "Apunta tu CLI al token del gateway (se generó una vez, se conserva):"
	@echo "  kling config set gateway.token \\"
	@echo "    \$$(ssh $(TARGET) 'sudo cut -d= -f2 /etc/kling/gateway.env')"
	@echo "  kling connect -all -install all"

## test — lo mismo que corre el CI, para no descubrirlo después de empujar.
##
## `-race` no es opcional aquí: el daemon toca su estado desde varias goroutines
## y el puente reparte respuestas entre sesiones concurrentes. Sin él, un build
## limpio no dice nada sobre lo que de verdad rompe este proyecto.
test:
	gofmt -l . | tee /dev/stderr | (! read)
	go vet ./...
	go test -race ./...

fmt:
	gofmt -l -w .

clean:
	rm -f $(BIN) $(BIN)-linux-amd64
