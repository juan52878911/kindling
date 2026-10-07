<p align="center">
  <img src="docs/kindling-banner.svg" alt="kindling — las ramitas secas que prenden primero" width="760">
</p>

<p align="center">
  <a href="https://github.com/juan52878911/kindling/releases"><img src="https://img.shields.io/github/v/release/juan52878911/kindling?label=release&color=e25822" alt="última release"></a>
  <a href="https://github.com/juan52878911/kindling/actions/workflows/ci.yml"><img src="https://img.shields.io/github/actions/workflow/status/juan52878911/kindling/ci.yml?label=ci" alt="CI"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/licencia-Apache%202.0-blue" alt="licencia"></a>
</p>

<p align="center"><a href="README.md">English</a> · <b>Español</b></p>

# kindling

kindling ejecuta microVMs —Firecracker en Linux, Virtualization.framework en macOS— que
despiertan en milisegundos desde plantillas congeladas (snapshots dorados) en vez de
arrancar. Una máquina congelada es un fichero en disco: no gasta CPU ni RAM hasta que se
descongela, y la aísla un hipervisor, no un kernel compartido. Es un daemon y un CLI,
`kling`, que crece con extensiones.

**Documentación completa: [kindling.asccilabs.com](https://kindling.asccilabs.com)** ·
en el repo: [`docs/`](docs/README.md), empezando por la [guía completa](docs/guia.md).

## Instalación

**Linux con KVM** (amd64 o arm64):

```sh
curl -fsSL https://raw.githubusercontent.com/juan52878911/kindling/main/scripts/install.sh | sh
kling up            # comprueba KVM, nftables, el usuario kindling y las imágenes; imprime lo que pide sudo
```

El daemon también puede correr como servicio de systemd en un host remoto, desplegado
desde una copia del repo: `make deploy HOST=ssh://usuario@host`. El CLI llega a él por
SSH (`kling context add lab ssh://usuario@host`): el daemon nunca escucha en un puerto de
red.

**Mac con Apple Silicon** (macOS 14+, backend nativo `vz`, sin root):

```sh
curl -fsSL https://raw.githubusercontent.com/juan52878911/kindling/main/scripts/install.sh | sh
brew install e2fsprogs
kling up -check
kling image copy min -from ssh://usuario@host-linux-arm64   # las imágenes se construyen en Linux
```

El script verifica cada binario contra el `SHA256SUMS` de la release, instala en
`~/.local/bin` sin sudo y acepta `--with mcp,sandbox`, `--tag vX.Y.Z` y `--prefix DIR`.
Para actualizar después: `kling upgrade` (`sudo` en Linux) cambia los binarios, reinicia
el daemon, lo comprueba y vuelve atrás solo si falla. Desde fuentes: `make install` (y
`make vz` en un Mac). Detalles:
[`docs/mac.md`](docs/mac.md), [`docs/releases.md`](docs/releases.md),
[`docs/actualizar.md`](docs/actualizar.md) (actualizaciones).

## Pruébalo

```sh
kling try -- uname -a        # microVM de usar y tirar: crear, ejecutar, imprimir, borrar
kling run -name demo         # arranca una máquina
kling freeze demo            # la congela en disco: 0 CPU, 0 RAM
kling thaw demo              # vuelve en decenas de milisegundos
```

`kling doctor` comprueba el daemon, las versiones y las extensiones y da el arreglo de
cada fallo; `kling help <orden>` muestra la ayuda de una orden.

## Extensiones

- **`kling mcp`** — cualquier servidor MCP (npm o PyPI) como servicio que despierta bajo
  demanda desde un snapshot dorado, detrás de un gateway para tu agente de IA.
  [`ext/mcp`](ext/mcp/README.es.md)
- **`kling sandbox`** — sandboxes multiinquilino para agentes de código: tokens, cuotas,
  plantillas y un fondo precalentado. [`ext/sandbox`](ext/sandbox/README.md)
- **`kling db`** — un Postgres (también MySQL, Redis y SQLite) desechable por test o por
  agente, copiado de una plantilla caliente en milisegundos. [`docs/db.md`](docs/db.md)
- **`kling phone`** — teléfonos Android (Redroid) en microVMs, cada uno con su identidad,
  un muro de pantallas y un servidor MCP. [`ext/phone`](ext/phone/README.md)

Se instalan con `kling plugin install <nombre>`; todos los binarios de una release son
compatibles entre sí. Para escribir la tuya: [`docs/extensions.md`](docs/extensions.md).

## Más

- [`SECURITY.md`](SECURITY.md) — el invitado se da por hostil: modelo de amenaza,
  barreras y lo que aún no está resuelto.
- [`CHANGELOG.md`](CHANGELOG.md) — qué trajo cada versión.
- [`docs/benchmarks.md`](docs/benchmarks.md) — cada cifra medida y su hardware.
- [`docs/api.md`](docs/api.md) — la API del daemon, para hablarle desde otro programa.

Apache-2.0 — ver [`LICENSE`](LICENSE) y [`NOTICE`](NOTICE).
