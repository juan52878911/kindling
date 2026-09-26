# Releases

Cómo se distribuyen los binarios de kindling y cómo se crea una release.

Desde v0.13.0 hay **un repositorio, una etiqueta y una release**: el núcleo
(`kling`, el daemon, el agente de invitado, Chispa y `kling-vz`) y las
extensiones oficiales (`mcp`, `sandbox`, `domotica`, el operador de Kubernetes)
salen juntos con la misma versión. Todos los binarios de una release son
compatibles entre sí; no hay tabla de compatibilidades que consultar. Las
releases anteriores de kindling-mcp (hasta v0.4.0) y kindling-sandbox (hasta
v0.2.2) siguen en sus repos archivados.

## Canales

1. **Binarios pre-compilados** de
   [Releases](https://github.com/juan52878911/kindling/releases), que compila
   GitHub Actions al empujar una etiqueta `vX.Y.Z`. Es lo que usa la gente, vía
   `scripts/install.sh` y `kling plugins install`.
2. **Desde fuentes** (`make install`, `go build`), para quien quiere lo último
   entre releases o desarrolla el proyecto.

## Qué publica una etiqueta

Con `<os>` ∈ {`linux`, `darwin`} y `<arch>` ∈ {`amd64`, `arm64`}:

| Asset | Qué es | Plataformas |
|---|---|---|
| `kling-<os>-<arch>` | CLI y daemon (el daemon solo corre en Linux con KVM, o en macOS con `kling-vz`) | las cuatro |
| `kling-guest-linux-<arch>` | agente de invitado, estático, va dentro de las imágenes | linux |
| `kling-chispa-linux-<arch>` | invitado de Chispa serverless | linux |
| `kling-vz-darwin-arm64` | backend nativo de macOS (Virtualization.framework), firmado con su entitlement | darwin/arm64 |
| `kling-mcp-<os>-<arch>` | extensión MCP: `mcp`, `add`, `connect`, `gateway`… | las cuatro |
| `kling-bridge-<os>-<arch>` | puente stdio↔HTTP; *companion* de `mcp` | las cuatro |
| `kling-sandbox-<os>-<arch>` | extensión de sandboxes multiinquilino (gateway, plantillas, fondo precalentado) | las cuatro |
| `kling-domotica-<os>-<arch>` | extensión de la demo de domótica: `kling domotica decide/eval/…` | las cuatro |
| `kindling-operator-linux-<arch>` | operador de Kubernetes | linux |
| `kindling-mcp-host.tar.gz` | lo que va en el host del daemon para MCP: unidades de systemd (`kling-gateway`, `kling-heal`), constructores y puente de invitado | — |
| `kindling-sandbox-host.tar.gz` | unidad `kling-sandbox.service` y lo que necesita en el host | — |
| `kindling-operator-deploy.tar.gz` | los manifiestos de `ext/sandbox/deploy/` (CRD, RBAC, namespace, deployment) | — |
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
curl -fsSL .../install.sh | sh -s -- --tag v0.13.0 --with mcp,sandbox,domotica

# o las extensiones después, desde el propio kling
kling plugins install mcp
kling plugins install sandbox@v0.13.0
kling doctor
```

`install.sh` detecta la plataforma, baja el binario y `SHA256SUMS`, verifica el
hash **antes** de tocar el disco y deja los binarios en `--prefix` (por defecto
`~/.local/bin`).

`kling plugins install <n>` baja
`https://github.com/juan52878911/kindling/releases/download/<tag>/kling-<n>-<os>-<arch>`
(con `<tag>` la versión del propio `kling`, o la de `@vX`), lo verifica contra el
`SHA256SUMS` de la misma release antes de escribirlo, comprueba su manifiesto,
baja sus *companions* (p. ej. `kling-bridge` para `mcp`) y lo deja en el
directorio de extensiones. Los detalles, en [`extensions.md`](extensions.md).

La parte de host (unidades de systemd de `mcp` y `sandbox`) viene en los
`kindling-*-host.tar.gz`; el operador, en `kindling-operator-deploy.tar.gz` o
`kubectl apply -f ext/sandbox/deploy/` desde el repo
([`kubernetes.md`](kubernetes.md)).

## Compilar desde fuentes

El repo tiene tres módulos Go, unidos por un `go.work` comiteado:

| Directorio | Módulo | Dependencias |
|---|---|---|
| `.` | `github.com/juan52878911/kindling` | **ninguna** (cero `require`, sin cgo; lo guarda el CI) |
| `vz/` | `…/kindling/vz` | cgo, solo darwin/arm64 |
| `ext/mcp/` | `…/kindling/ext/mcp` | el núcleo, por `replace => ../..` |
| `ext/sandbox/` | `…/kindling/ext/sandbox` | el núcleo, por `replace => ../..` |

El `replace` relativo está siempre, así que `GOWORK=off go build ./...` dentro de
`ext/mcp` también funciona; `go.work` es la comodidad de que un cambio en
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

# 2. CHANGELOG.md: la sección "## vX.Y.Z — AAAA-MM-DD" con sus subsecciones
#    ### Núcleo · ### kling-mcp · ### kling-sandbox y operador · ### Ejemplos
git commit -am "changelog: vX.Y.Z"

# 3. la etiqueta dispara el workflow
./scripts/release.sh vX.Y.Z --wait
```

`scripts/release.sh --wait` espera al workflow y abre la release. A mano:

```sh
git tag -a vX.Y.Z -m "release vX.Y.Z — ver CHANGELOG.md"
git push origin vX.Y.Z
```

`.github/workflows/release.yml`:

1. **build** (matriz os/arch): `kling`, `kling-guest`, `kling-chispa`,
   `kling-mcp`, `kling-bridge`, `kling-sandbox`, `kling-domotica` y, en Linux,
   `kindling-operator`; empaqueta los tres `.tar.gz`.
2. **vz** (macos-14): compila y firma `kling-vz-darwin-arm64`.
3. **release**: junta todo, genera **un** `SHA256SUMS`, extrae el bloque de
   `CHANGELOG.md` como notas y publica.
4. **imagen** (tras `release`, con `packages: write`): construye la imagen del
   operador con `context: .` y `file: ext/sandbox/Dockerfile.operator` (el
   `replace ../..` necesita el núcleo en el contexto) y la empuja a GHCR.

El CI de cada push (`ci.yml`) ejecuta `make -C <dir> test cross` para `.`,
`ext/mcp` y `ext/sandbox`, el job de `vz` en macOS, y el guard de que el
`go.mod` raíz sigue sin `require` ni `replace`.

## Versionado

SemVer con prefijo `v`: MAJOR para cambios incompatibles de CLI o API del
daemon, MINOR para funcionalidades compatibles, PATCH para arreglos. Seguimos en
**0.x** porque la API aún puede cambiar. La versión es una para todo el repo:
una extensión oficial lleva la del núcleo con el que salió.

## Qué no automatizamos (todavía)

- **Firma de los binarios** (cosign, sigstore): `SHA256SUMS` cubre la
  integridad; la firma llegará cuando alguien la necesite.
- **brew/scoop/aur**: `install.sh` cubre el caso general.
- **Binarios con símbolos**: se compila con `-trimpath -ldflags "-s -w"`; para
  depurar, `go build ./cmd/kling`.
