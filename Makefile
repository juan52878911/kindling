# kindling — un solo binario que hace de CLI y de daemon.
#
#   make install                       CLI en tu máquina
#   make deploy HOST=ssh://juan@lab    daemon en el host con KVM
#
# El CLI corre donde trabajas (macOS o Linux); el daemon solo donde hay KVM.

BIN     := kling
PKG     := ./cmd/kling

# Arquitectura de los binarios del daemon y del agente que se compilan para Linux.
# Por defecto amd64 (el laboratorio habitual). Para desplegar a una VM Linux
# arm64 (p.ej. Lima vz+nested en un Mac Apple Silicon):
#   make deploy GOARCH=arm64 HOST=ssh://usuario@vm-arm
# El CLI local (`make build`) no usa esta variable: se compila para tu máquina.
GOARCH  ?= amd64

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

.PHONY: all build install uninstall daemon daemon-full assets guest deploy deploy-mac test clean fmt

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
	@echo
	@echo "Para alojar servidores MCP, instala la extensión kindling-mcp:"
	@echo "  https://github.com/juan52878911/kindling-mcp"

uninstall:
	@rm -f $(PREFIX)/bin/$(BIN) 2>/dev/null || sudo rm -f $(PREFIX)/bin/$(BIN)
	@echo "desinstalado (la configuración en ~/.config/kling se conserva)"

## guest — el agente genérico de invitado (PID 1 de las microVMs sin servidor
## MCP: imágenes de herramientas, sandboxes). Estático por la misma razón que el
## puente.
guest:
	CGO_ENABLED=0 GOOS=linux GOARCH=$(GOARCH) go build -trimpath \
		-ldflags "$(LDFLAGS)" -o kling-guest ./cmd/kling-guest
	@echo "kling-guest  ($(VERSION), linux/$(GOARCH))"

## daemon — compila el binario del host con KVM (linux/$(GOARCH), amd64 por defecto)
daemon:
	GOOS=linux GOARCH=$(GOARCH) go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)-linux-$(GOARCH) $(PKG)
	@echo "$(BIN)-linux-$(GOARCH)  ($(VERSION))"

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
	GOOS=linux GOARCH=$(GOARCH) go build -trimpath -tags embed_assets \
		-ldflags "$(LDFLAGS)" -o $(BIN)-linux-$(GOARCH) $(PKG)
	@echo "$(BIN)-linux-$(GOARCH)  ($(VERSION))  con artefactos embebidos"

## deploy — instala el daemon por SSH y lo reinicia
## Van también el agente de invitado (kling-guest) y el constructor "base", que
## construye imágenes con él (la de herramientas de `volume populate`, por ejemplo):
## construir monta un loopback y hace chroot, y eso solo lo puede hacer root allí.
## Lo de MCP (puente, empaquetador, gateway) lo despliega kindling-mcp.
deploy: daemon guest
	@# --now no reinicia lo que ya corre: hace falta restart explícito.
	@test -n "$(HOST)" || { echo "usa: make deploy HOST=ssh://usuario@maquina" >&2; exit 1; }
	$(eval TARGET := $(patsubst ssh://%,%,$(HOST)))
	scp -q $(BIN)-linux-$(GOARCH) $(TARGET):/tmp/$(BIN)
	scp -q kling-guest scripts/81-base-image.sh $(TARGET):/tmp/
	scp -q scripts/builders/base $(TARGET):/tmp/builder-base
	scp -q packaging/$(BIN).service $(TARGET):/tmp/
	ssh $(TARGET) 'sudo install -m755 /tmp/$(BIN) /usr/local/bin/$(BIN) && \
		sudo install -d /usr/local/lib/kindling && \
		sudo install -m755 /tmp/kling-guest /usr/local/lib/kindling/kling-guest && \
		sudo install -d -m755 /usr/local/lib/kindling/builders && \
		sudo install -m755 /tmp/81-base-image.sh /usr/local/lib/kindling/81-base-image.sh && \
		sudo install -m755 /tmp/builder-base /usr/local/lib/kindling/builders/base && \
		sudo install -m644 /tmp/$(BIN).service /etc/systemd/system/ && \
		sudo systemctl daemon-reload && sudo systemctl enable $(BIN) && \
		sudo systemctl restart $(BIN) && \
		sleep 1 && systemctl is-active $(BIN)'
	@echo "daemon desplegado en $(TARGET)"
	@echo "  agente de invitado y constructor base en /usr/local/lib/kindling"
	@echo
	@echo "Imagen de herramientas para poblar volúmenes:  kling images toolchain"
	@echo "Servidores MCP:  despliega kindling-mcp (make deploy HOST=$(HOST) en su repositorio)"

## deploy-mac — atajo para desplegar a una VM Linux arm64 desde un Mac Apple Silicon.
##
## Firecracker es Linux/KVM: en un Mac el daemon no corre nativo, hace falta una
## VM Linux arm64 con virtualización anidada (Lima con vmType:vz + nestedVirtua-
## lization, solo M3+). Esto es un `make deploy` con GOARCH=arm64 ya fijado; el
## HOST es el ssh de esa VM (`limactl show-ssh <vm>` o ~/.lima/<vm>/ssh.config).
## Receta completa en docs/mac-arm64.md.
deploy-mac:
	@$(MAKE) deploy GOARCH=arm64 HOST="$(HOST)"

## test — lo mismo que corre el CI, para no descubrirlo después de empujar.
##
## `-race` no es opcional aquí: el daemon toca su estado desde varias goroutines
## y el planificador reparte instancias entre peticiones concurrentes. Sin él, un build
## limpio no dice nada sobre lo que de verdad rompe este proyecto.
test:
	gofmt -l . | tee /dev/stderr | (! read)
	go vet ./...
	go test -race ./...

fmt:
	gofmt -l -w .

clean:
	rm -f $(BIN) $(BIN)-linux-amd64 $(BIN)-linux-arm64 kling-guest
