# Checklist: cortar v1.0.0

Auditoría del 2026-11-25 sobre `main` en `8b80131` (#343). Cómo se hace una release:
[`docs/releases.md`](../docs/releases.md). Aquí, solo lo que falta para la 1.0.0 y en qué orden.

## Lo primero: ¿existe `v1.0.0-rc.3`?

Casi seguro que **no**. Ni `CHANGELOG.md` tiene una sección `[1.0.0-rc.3]` (el enlace de
`[Unreleased]` comparaba desde `v1.0.0-rc.2`), ni los `go.mod` de `ext/*` y `vz/` la citaban
(seguían en `v1.0.0-rc.2`), y `release.sh` y `release.yml` se niegan a publicar sin las dos
cosas. El pin a rc.3 entró en el README con 848a7ca (23-nov) como preparación. No se pudo
confirmar contra GitHub: la API y `git ls-remote` daban 502/503 durante toda la auditoría.
Compruébalo con `gh api repos/juan52878911/kindling/releases --jq '.[].tag_name'`.

Si no existe, **el one-liner de instalación del README de `main` falla hoy** (404 en
`releases/download/v1.0.0-rc.3/…`), y sin pin también falla: la «latest» es la 0.20.1, sin
firma, e `install.sh` la rechaza. Hasta que la v1.0.0 esté publicada no hay forma de instalar
siguiendo el README. Anunciar el lanzamiento solo después del paso 7.

## Bloqueos, de más a menos grave

1. **Los README fijan una versión** (`README.md:29`, `README.md:50`, `README.es.md:29`,
   `README.es.md:51`: `--tag v1.0.0-rc.3` y `KLING_VERSION=v1.0.0-rc.3`). Para una estable,
   `release.sh` exige **quitar** el pin, no cambiarlo a v1.0.0 (se vuelve «latest»). Es de otro
   agente: tiene que ir en el mismo commit que el CHANGELOG o antes.
2. **Secretos que el workflow necesita** (no se pueden comprobar desde aquí):
   `KLING_RELEASE_SIGNING_KEY` en el environment `release` (sin él, una ≥ 1.0 falla al firmar;
   `KLING_RELEASE_SIGNING_KEY=… go run ./internal/upgrade/firmar check -tag v1.0.0` confirma que
   casa con la pública compilada) y `HOMEBREW_TAP_TOKEN` (sin él el tap no se actualiza y el
   `brew install juan52878911/kindling/kling` del README instala la versión vieja, o nada).
   Opcional: `KLING_RELEASE_SSH_KEY`.
3. **El e2e de kling-hv en un Apple Silicon** (`scripts/e2e-mac-release.sh`) sobre el commit
   exacto que se etiqueta: el status `e2e-mac/kling-hv` es por commit, así que hay que correrlo
   **después** de fusionar el commit de release, no antes.
4. **La imagen del operador que citan el repo y el chart no existe**: la rc.2 salió sin ella
   (`ci.yml:413`, CHANGELOG). `ext/sandbox/deploy/deployment.yaml:44` y
   `deploy/helm/kindling-sandbox/Chart.yaml:7` (`appVersion`) apuntan a `v1.0.0-rc.2`; quien
   haga `kubectl apply -f ext/sandbox/deploy/` o `helm install` sin `tag` se queda en
   `ImagePullBackOff`. Subir los dos a `v1.0.0` en el commit de release (el tar de la release ya
   reescribe el de `deploy/`; el chart no lo toca nadie).
5. **El quickstart de sandbox fija rc.3**: `scripts/quickstart-sandbox.sh:20` y
   `docs/quickstart-sandbox.md:25` (`--tag v1.0.0-rc.3`). Quitar el `--tag` en los dos a la vez
   (`scripts/quickstart_test.go` exige que coincidan).
6. **Docs de seguridad desfasadas**: `SECURITY.md:1768` y `docs/firmas.md:30` dicen que
   `install.sh` sin `minisign` falla desde 1.0; ahora cae a que el `kling` recién bajado se
   compruebe a sí mismo (`install.sh:509`), que **no** es una verificación independiente. Una 1.0
   debería decirlo así, y apuntarlo en «Lo que NO está resuelto»: mientras
   `KLING_RELEASE_SSH_SIGNERS` siga vacío (`install.sh:111`), un Mac sin `minisign` (el caso por
   defecto del README) no tiene verificación independiente.
7. **`install.sh:118` `PRIMER_USO_DESDE="v1.0.0-rc.3"`**: si rc.3 no existe, el mensaje de error
   de `install.sh:523` manda a `KLING_VERSION=v1.0.0-rc.3`. Pasarlo a `v1.0.0` cuando se quite el
   pin del README (`scripts/readme_test.go` exige pin ≥ `PRIMER_USO_DESDE`).

No hay TODO/FIXME marcados como bloqueo de release. Los enlaces relativos de `README.md`,
`README.es.md`, `docs/README.md`, `SECURITY.md` y las guías de inicio apuntan a ficheros que
existen; `kindling.asccilabs.com` no se pudo abrir desde aquí (proxy).

## Pasos para cortar v1.0.0

1. Fusionar esta rama: el CHANGELOG ya tiene `## [1.0.0] - YYYY-MM-DD` con `[Unreleased]` vacío,
   y `ext/*/go.mod` y `vz/go.mod` requieren `v1.0.0`. Si entró algo más en `[Unreleased]` de
   `main` entre tanto, resumirlo dentro de la sección 1.0.0.
2. En un commit `release: v1.0.0` (o el mismo): la fecha en `CHANGELOG.md:9`, el pin fuera de los
   dos README (bloqueo 1), el quickstart (5), `Chart.yaml` y `deployment.yaml` (4) y
   `PRIMER_USO_DESDE` (7).
3. `git checkout main && git pull --rebase`; esperar a `ci.yml` verde en ese commit (los 14 jobs
   de `CI_OBLIGATORIOS` de `release.sh`, ninguno saltado) y al nocturno `e2e-linux.yml`.
4. En un Mac Apple Silicon, sobre ese commit: `scripts/e2e-mac-release.sh` (92 y 95 con
   `VMM=hv`, estricto: hace falta `psql`, `git` y el dorado de Postgres). Publica
   `e2e-mac/kling-hv`. Recomendado, además, la base rc → final:
   `OLD_TAG=v1.0.0-rc.2` en 94/95.
5. `./scripts/release.sh v1.0.0 --dry-run`: tiene que salir con 0 y enseñar las notas.
6. `./scripts/release.sh v1.0.0 --wait`. El workflow: test → lint (`firmar check`, govulncheck,
   staticcheck, shellcheck, apidiff contra rc.2) → build/vz/unpack → imágenes (kernel y `min`) →
   e2e con KVM → android → release (status hv, contrato de assets, `SHA256SUMS`, `kling.rb`,
   firma minisign, atestación, borrador, comprobación, publicación como *latest*) → imagen GHCR
   (`:v1.0.0` y `:latest`) → tap de Homebrew.
7. Comprobar a mano: el one-liner del README en un Linux y en un Mac limpios,
   `brew install juan52878911/kindling/kling`, `kling upgrade` desde 0.20.1 y desde rc.2, y
   `docker pull ghcr.io/juan52878911/kindling-operator:v1.0.0`.
8. SDK (opcional, aparte): siguen en `0.1.0` y no van atados a la versión del repo. Para
   publicarlos, dar de alta `kindling-sandbox` en PyPI (trusted publisher) y `@kindling/sandbox`
   en npm (`NPM_TOKEN`), y lanzar `sdks-release.yml` a mano. Si se publican, las guías dejan de
   tener que decir «desde el checkout» (`docs/quickstart-sandbox.md:85`).

## Resultados de las pruebas (local, sin KVM, como root)

Con Go 1.27.4 (la de CI), `GOWORK=off`, por módulo como el job `modulo` de `ci.yml`:

| Qué | Resultado |
|---|---|
| gofmt, `go vet` (los 5 módulos; `vz/` con `GOOS=linux`) | limpio |
| `go build`/`go vet` con `go.work` | limpio |
| `go test -race -count=1` de `ext/db`, `ext/mcp`, `ext/phone`, `ext/sandbox` | pasa |
| `go test -race -count=1` del núcleo | 3 fallos, ninguno de producto (abajo); corregidos |
| staticcheck v0.8.2 `-matrix` (linux, darwin, linux/arm64), los 5 módulos | limpio |
| govulncheck v1.9.0 | no se pudo: `vuln.go.dev` bloqueado por el proxy (lo corre el job lint) |
| shellcheck `-x -S warning scripts/*.sh` (0.11.0) | limpio |
| `cargo fmt --check` y `clippy -D warnings` de `rust/kling-unpack` | limpio |
| `examples/sandbox/test-dry-run.sh`, bit de ejecución de los `.sh` | limpio |
| SDK Python (`unittest`, 3.13) | 63 pasan |
| SDK TypeScript (`npm test`, Node 22) | 34 pasan |
| `firmar check -tag v1.0.0` | 1 clave compilada, ok |
| `packaging/homebrew/formula.sh v1.0.0` con un `SHA256SUMS` de prueba | ok |
| apidiff, e2e con microVMs, `vz/` en macOS | no se pudo (sin etiquetas, sin KVM, sin Mac) |

Los tres fallos del núcleo, arreglados en los tests:

- `internal/daemon` `TestConstructorConNietoNoCuelga`: tomaba un zombi por vivo (`kill(pid, 0)`
  funciona con un zombi). Solo falla donde el PID 1 no recoge huérfanos, como este contenedor.
- `internal/transport` `TestSSHMultiplexArgsSinDirectorioCaeAlModoDeSiempre`: usaba
  `/nonexistent/ro`, que root sí puede crear. Ahora la ruta cuelga de un fichero normal.
- `internal/machine` `TestR6ReservaNoHaceUnStatPorMaquina`: comparaba tiempos de una sola ronda
  y, con `-race` y carga, se pasaba del margen por 1 µs. Ahora toma la mejor de tres en los dos
  lados; la comprobación de stats no cambia.

Sin diagnosticar: `internal/machine` `TestRotarConsolasSoloDeLasVivas` falló una vez en una
segunda pasada del paquete entero y pasó en la tercera y en 10 seguidas sola (el mensaje se
perdió). Vigilarlo en el CI del commit de release.

## Versiones que cambiar

Ya cambiadas en esta rama (`release.sh` lo exige a mano antes de etiquetar):

- `ext/db/go.mod:7`, `ext/mcp/go.mod:5`, `ext/phone/go.mod:5`, `ext/sandbox/go.mod:5`,
  `vz/go.mod:7`: `github.com/juan52878911/kindling v1.0.0-rc.2` → `v1.0.0`.

Por cambiar (no son de esta rama, o el proceso no las cita):

| Fichero:línea | Hoy | Para v1.0.0 |
|---|---|---|
| `README.md:29` | `… \| sudo sh -s -- --tag v1.0.0-rc.3` | `… \| sudo sh` |
| `README.md:50` | `… \| KLING_VERSION=v1.0.0-rc.3 sh` | `… \| sh` |
| `README.es.md:29` | `… \| sudo sh -s -- --tag v1.0.0-rc.3` | `… \| sudo sh` |
| `README.es.md:51` | `… \| KLING_VERSION=v1.0.0-rc.3 sh` | `… \| sh` |
| `CHANGELOG.md:9` | `## [1.0.0] - YYYY-MM-DD` | la fecha |
| `scripts/quickstart-sandbox.sh:20` | `--tag v1.0.0-rc.3 --with sandbox` | `--with sandbox` |
| `docs/quickstart-sandbox.md:25` | `--tag v1.0.0-rc.3 --with sandbox` | `--with sandbox` |
| `scripts/install.sh:118` | `PRIMER_USO_DESDE="v1.0.0-rc.3"` | `"v1.0.0"` (si rc.3 no existe) |
| `deploy/helm/kindling-sandbox/Chart.yaml:7` | `appVersion: "v1.0.0-rc.2"` | `"v1.0.0"` |
| `ext/sandbox/deploy/deployment.yaml:44` | `kindling-operator:v1.0.0-rc.2` | `:v1.0.0` |
| `deploy/helm/kindling-sandbox/ci/gateway-ingress.yaml:6` | `tag: v1.0.0-rc.2` | opcional (valores de CI) |
| `docs/mac.md:87` | `…/download/v1.0.0-rc.2/kling.rb` | opcional (ejemplo de -rc) |
| `scripts/bench/remoto/setup.sh:27` | `KB_RELEASE:-v1.0.0-rc.2` | opcional |
| `sdks/python/pyproject.toml:7`, `sdks/python/kindling_sandbox/__init__.py:13`, `sdks/typescript/package.json:3` | `0.1.0` | sin cambio salvo que se publiquen |

Sin cambio: las `Version` de Go salen de `-ldflags -X main.Version` y la fórmula de Homebrew de
la plantilla `packaging/homebrew/kling.rb`.
