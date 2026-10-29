# Releases

Cómo se distribuyen los binarios de kindling y cómo se crea una release.

Desde v0.13.0 hay **un repositorio, una etiqueta y una release**: el núcleo
(`kling`, el daemon, el agente de invitado, Chispa, `kling-vz` y `kling-hv`) y las
extensiones oficiales (`mcp`, `sandbox`, `db`, `phone`, el operador de Kubernetes)
salen juntos con la misma versión. Los ejemplos de `examples/` (como la demo de
domótica, `kindling-domotica`) no se publican: se compilan desde el repo. Todos los binarios de una release son
compatibles entre sí; no hay tabla de compatibilidades que consultar. Las
releases anteriores de kindling-mcp (hasta v0.4.0) y kindling-sandbox (hasta
v0.2.2) siguen en sus repos archivados.

## Canales

1. **Binarios pre-compilados** de
   [Releases](https://github.com/juan52878911/kindling/releases), que compila
   GitHub Actions al empujar una etiqueta `vX.Y.Z`. Es lo que usa la gente, vía
   `scripts/install.sh` y `kling plugin install`.
2. **Desde fuentes** (`make install`, `go build`), para quien quiere lo último
   entre releases o desarrolla el proyecto.

## Qué publica una etiqueta

Con `<os>` ∈ {`linux`, `darwin`} y `<arch>` ∈ {`amd64`, `arm64`}:

| Asset | Qué es | Plataformas |
|---|---|---|
| `kling-<os>-<arch>` | CLI y daemon (el daemon solo corre en Linux con KVM, o en macOS con `kling-vz` o `kling-hv`) | las cuatro |
| `kling-guest-linux-<arch>` | agente de invitado, estático, va dentro de las imágenes | linux |
| `kling-chispa-linux-<arch>` | invitado de Chispa serverless | linux |
| `kling-vz-darwin-arm64` | backend nativo de macOS (Virtualization.framework), firmado con su entitlement | darwin/arm64 |
| `kling-hv-darwin-arm64` | backend de macOS 15.2+ sobre Hypervisor.framework (`daemon.vmm: auto` lo elige), firmado con su entitlement | darwin/arm64 |
| `kling-mcp-<os>-<arch>` | extensión MCP: `mcp`, `add`, `connect`, `gateway`… | las cuatro |
| `kling-bridge-<os>-<arch>` | puente stdio↔HTTP; *companion* de `mcp` | las cuatro |
| `kling-sandbox-<os>-<arch>` | extensión de sandboxes multiinquilino (gateway, plantillas, fondo precalentado) | las cuatro |
| `kling-db-<os>-<arch>` | extensión de bases de datos desechables (`kling db`) | las cuatro |
| `kling-phone-<os>-<arch>` | extensión de teléfonos Android (`kling phone`) | las cuatro |
| `kindling-operator-linux-<arch>` | operador de Kubernetes | linux |
| `kindling-mcp-host.tar.gz` | lo que va en el host del daemon para MCP: unidades de systemd (`kling-gateway`, `kling-heal`), constructores y puente de invitado | — |
| `kindling-sandbox-host.tar.gz` | unidad `kling-sandbox.service` y lo que necesita en el host | — |
| `kindling-operator-deploy.tar.gz` | los manifiestos de `ext/sandbox/deploy/` (CRD, RBAC, namespace, deployment) | — |
| `vmlinux-<arch>.zst` | kernel de invitado (`scripts/builders/kernel`), comprimido; `kling up` lo baja en un Mac | linux |
| `min-<arch>.ext4.zst` | imagen base `min` (`scripts/70-build-minimal-image.sh`, con el `kling-bridge` de la release) | linux |
| `kling.rb` | fórmula de Homebrew con la versión y los sha256 de esta release (no se publica en ningún tap) | darwin/arm64, linux |
| `SHA256SUMS` | **un** fichero con el sha256 de todo lo anterior | — |

Además, la imagen del operador se publica en
`ghcr.io/juan52878911/kindling-operator:<tag>` y `:latest` (la ruta no cambia:
la imagen se llama por el paquete, no por el repo), y
`ext/sandbox/deploy/deployment.yaml` apunta a la etiqueta.

Windows no está soportado (el código usa `syscall.Kill`, `Setsid`, `Stat_t`).

## Instalación por parte del usuario

```sh
# el núcleo, última estable
curl -fsSL https://raw.githubusercontent.com/juan52878911/kindling/main/scripts/install.sh | sh

# versión concreta y extensiones de una vez
curl -fsSL .../install.sh | sh -s -- --tag v0.13.0 --with mcp,sandbox

# o las extensiones después, desde el propio kling
kling plugin install mcp
kling plugin install sandbox@v0.13.0
kling doctor
```

`install.sh` detecta la plataforma, baja el binario y `SHA256SUMS`, verifica el
hash **antes** de tocar el disco y deja los binarios en `--prefix` (por defecto
`~/.local/bin`).

Para actualizar una instalación que ya existe, `kling upgrade` (con `sudo` en
Linux, donde reinicia `kling.service`): baja la release de la plataforma, la
verifica contra el mismo `SHA256SUMS`, comprueba que el binario nuevo lee el
estado que hay, guarda los binarios de ahora, para el daemon, cambia, arranca,
comprueba `/info`, las máquinas y los dorados y, si algo falla, vuelve solo;
`kling upgrade -rollback` vuelve a mano. `install.sh` sobre un kling ya
instalado remite a él y solo reinstala con `--force`. Detalles en
[`actualizar.md`](actualizar.md) §3.4.

`kling plugin install <n>` baja
`https://github.com/juan52878911/kindling/releases/download/<tag>/kling-<n>-<os>-<arch>`
(con `<tag>` la versión del propio `kling`, o la de `@vX`), lo verifica contra el
`SHA256SUMS` de la misma release antes de escribirlo, comprueba su manifiesto,
baja sus *companions* (p. ej. `kling-bridge` para `mcp`) y lo deja en el
directorio de extensiones. Los detalles, en [`extensions.md`](extensions.md).

La parte de host (unidades de systemd de `mcp` y `sandbox`) viene en los
`kindling-*-host.tar.gz`, con `User=@KLING_USER@` y `Group=@KLING_GROUP@` por rellenar
con el usuario al que el daemon cede su socket (`KLING_SOCKET_USER`; `make deploy` lo
hace solo, y sin rellenar la unidad no arranca); el operador, en `kindling-operator-deploy.tar.gz` o
`kubectl apply -f ext/sandbox/deploy/` desde el repo
([`kubernetes.md`](kubernetes.md)).

## Compilar desde fuentes

El repo tiene seis módulos Go, unidos por un `go.work` comiteado, y un ayudante
en Rust:

| Directorio | Módulo | Dependencias |
|---|---|---|
| `.` | `github.com/juan52878911/kindling` | **ninguna** (cero `require`, sin cgo; lo guarda el CI) |
| `vz/` | `…/kindling/vz` | cgo, solo darwin/arm64 (`kling-vz` y `kling-hv`) |
| `ext/mcp/` | `…/kindling/ext/mcp` | el núcleo, por `replace => ../..` |
| `ext/sandbox/` | `…/kindling/ext/sandbox` | el núcleo, por `replace => ../..` |
| `ext/db/` | `…/kindling/ext/db` | el núcleo, por `replace => ../..` |
| `ext/phone/` | `…/kindling/ext/phone` | el núcleo, por `replace => ../..` |
| `rust/kling-unpack/` | crate de Rust con su `Cargo.lock` | descomprime capas OCI; opcional, el núcleo tiene el camino en Go |

El `replace` relativo está siempre, así que `GOWORK=off go build ./...` dentro de
cualquier `ext/*` también funciona; `go.work` es la comodidad de que un cambio en
`pkg/api` rompa la compilación de las extensiones en el momento.

Cada módulo tiene los mismos objetivos de `make`:

```sh
make test          # gofmt + go vet + go test -race
make cross         # sus compilaciones cruzadas
make -C ext/mcp test cross
make -C ext/sandbox test cross
```

## Crear una release

```sh
# 1. main al día y limpio
git checkout main && git pull --rebase && git status

# 2. CHANGELOG.md: "## [Unreleased]" pasa a "## [X.Y.Z] - AAAA-MM-DD"
#    (y su enlace al final del fichero)
git commit -am "changelog: vX.Y.Z"

# 3. actualizar desde la anterior, en el laboratorio (KVM y root; ver actualizar.md §3.5),
#    con los binarios sellados (-ldflags "-X main.Version=..."): si no, el script se para
sudo OLD_DIR=<kling de la anterior> NEW_DIR=<binarios de HEAD> scripts/94-e2e-upgrade.sh

# 3b. lo mismo en el Mac, con el agente de launchd (instala uno suyo y lo
#     quita; se niega si ya tienes dev.kindling.daemon): kling y kling-vz de
#     la anterior y de HEAD, sellados, y las imágenes de un daemon Linux arm64
OLD_DIR=<kling y kling-vz de la anterior> NEW_DIR=<los de HEAD> IMAGES_DIR=<vmlinux, min.ext4…> scripts/95-e2e-upgrade-mac.sh

# 4. la etiqueta dispara el workflow
./scripts/release.sh vX.Y.Z --wait
```

`scripts/release.sh` se niega a etiquetar sin ese bloque, `--dry-run` enseña
las notas que saldrán y `--wait` espera al workflow y abre la release. A mano:

```sh
scripts/release-notes.sh vX.Y.Z   # las notas, como las publicará el workflow
git tag -a vX.Y.Z -m "release vX.Y.Z — see CHANGELOG.md"
git push origin vX.Y.Z
```

`.github/workflows/release.yml`:

Corre con cada etiqueta `v*` (o a mano, `workflow_dispatch` con una etiqueta
que ya exista), con permiso de solo lectura salvo en los dos jobs que publican:

1. **test** (ubuntu, matriz `.`, `ext/db`, `ext/mcp`, `ext/phone`,
   `ext/sandbox`, `fail-fast`): `make -C <dir> test` (gofmt, vet y
   `go test -race -count=1`) sobre el commit de la etiqueta, con Go 1.27.3 y
   `GOWORK=off`. Nada se publica sin pasarlo.
2. **build** (tras test; matriz linux/darwin × amd64/arm64, `CGO_ENABLED=0`,
   `-trimpath -s -w -X main.Version=`): `kling`, `kling-mcp`, `kling-bridge`,
   `kling-sandbox`, `kling-db`, `kling-phone` y, en Linux, `kling-guest`,
   `kling-chispa` y `kindling-operator`.
3. **vz** (tras test, macos-15): compila y firma (ad hoc, con su entitlement)
   `kling-vz-darwin-arm64` para macOS 14.0 y `kling-hv-darwin-arm64` para 15.2,
   y comprueba el entitlement y el `minos` de cada uno.
4. **imagenes** (tras build, ubuntu x86_64): el kernel de invitado de las dos
   arquitecturas (`scripts/builders/kernel/build.sh`, cruzado para arm64) y
   `min` de las dos (`scripts/70-build-minimal-image.sh` con el `kling-bridge`
   de esta release; sin paquetes no hay chroot, así que también sale de
   x86_64), comprimidos con `zstd -19`.
5. **release** (tras build, vz e imagenes; `contents: write`): junta los
   artefactos, empaqueta los tres `.tar.gz`, **compara la lista de assets con
   el contrato escrito en el propio job** (si falta o sobra uno, no se
   publica), genera **un** `SHA256SUMS`, rellena la fórmula de Homebrew
   (`packaging/homebrew/formula.sh`) y añade su suma, y publica como notas el
   bloque de la versión en `CHANGELOG.md` más una línea de instalación
   (`scripts/release-notes.sh`; sin bloque no hay release).
6. **imagen** (tras release, `packages: write`): construye la imagen del
   operador para linux/amd64 y linux/arm64 con `context: .` y
   `file: ext/sandbox/Dockerfile.operator` (el `replace ../..` necesita el
   núcleo en el contexto) y la empuja a GHCR con la etiqueta y `:latest`.

Aparte, `android-kernels.yml` se dispara al publicarse la release y le sube los
kernels Android del prototipo (arm64 y amd64) con su sha256, su `.config` y una
atestación de procedencia de Sigstore.

El CI de cada push y PR (`.github/workflows/ci.yml`, solo lectura, actions
fijadas por SHA salvo en los `clon-*`):

| Job | Runner | Qué comprueba |
|---|---|---|
| **modulo** | ubuntu | `make -C <dir> test cross` para `.`, `ext/db`, `ext/mcp`, `ext/phone` y `ext/sandbox`, sin workspace y con la toolchain de la release (1.27.3) |
| **raiz-sin-deps** | ubuntu, Go 1.24 | el `go.mod` raíz sin `require` ni `replace`, `CGO_ENABLED=0 go build ./...` con la Go mínima que declaran los `go.mod`, que cada `ext/*` tenga su `replace … => ../..` y declare la misma `go` que el núcleo |
| **kling-unpack** | ubuntu | `cargo fmt`, `clippy -D warnings` y `build --locked` del ayudante de Rust, y los tests de `internal/oci` que lo comparan con el camino de Go |
| **workspace** | ubuntu | `go build` y `go vet` con el `go.work` (que `go.work.sum` no se quede atrás) |
| **clon-reflink** | ubuntu, Go 1.24 | los tests de clonado sobre un XFS y un btrfs en loop, exigiendo que el reflink haya ocurrido de verdad |
| **vz** | macos-15 | en `vz/`: gofmt, vet (también `GOOS=linux`), `go test -race`, y `build.sh`/`build-hv.sh` con su entitlement, su `minos` y `--version` |
| **clon-apfs** | macos-14, Go 1.24 | los mismos tests de clonado sobre APFS (`clonefile`) |

Ninguno arranca una microVM: los runners hospedados no dan KVM ni
virtualización anidada en macOS. Los e2e con VMs (`90-e2e.sh`,
`92-e2e-mac.sh`, `94`/`95-e2e-upgrade*.sh`) se corren a mano en el
laboratorio y en el Mac, como dice el paso 3 de arriba.

## Versionado

SemVer con prefijo `v`. La versión es una para todo el repo: una extensión
oficial lleva la del núcleo con el que salió. Qué cuenta como MAJOR, MINOR y
PATCH, qué superficies entran en la promesa de 1.x, la política de deprecación
y la ventana de versiones que se mezclan, en
[`compatibilidad.md`](compatibilidad.md). Seguimos en **0.x** hasta cumplir su
lista «Antes de 1.0».

Cómo funciona `kling upgrade` y qué hace con cada formato en disco, en
[`actualizar.md`](actualizar.md). Un cambio que
sube la versión de un formato persistido va en una MINOR, con su línea en
`Changed` del CHANGELOG; cómo volver atrás va en `actualizar.md`.

## Cómo escribir el CHANGELOG

[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), **en inglés y una
línea por cambio**:

- Cada versión, `## [X.Y.Z] - AAAA-MM-DD`; lo pendiente, en `## [Unreleased]`.
- Secciones `### Added`, `### Changed`, `### Deprecated`, `### Removed`,
  `### Fixed`, `### Security`, solo las que haya. Lo que se depreca va en
  `Deprecated` con la versión en la que se quitará
  ([`compatibilidad.md`](compatibilidad.md#deprecación)).
- Una línea de ≤ ~15 palabras, para quien usa kindling, con el PR al final:
  `` - `kling db ls` lists copies with engine, golden and state (#143) ``.
- Sin cifras de laboratorio salvo una si es lo que importa; el detalle va en
  el PR y en `docs/`.
- ≤ ~10 líneas por sección; lo interno se agrupa en una línea
  `Internal: …`.

Esas líneas son las notas de la release, tal cual.

## Qué no automatizamos (todavía)

- **Firma de los binarios y de `SHA256SUMS`**, y notarización en macOS:
  `SHA256SUMS` cubre la integridad, no la autoría (solo los kernels Android
  llevan atestación). Está en la lista de
  [`compatibilidad.md`](compatibilidad.md#antes-de-10).
- **Tap de Homebrew, scoop, aur**: la fórmula va como asset (`kling.rb`); nadie
  la empuja a un tap.
- **Binarios con símbolos**: se compila con `-trimpath -ldflags "-s -w"`; para
  depurar, `go build ./cmd/kling`.
