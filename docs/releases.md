# Releases

Cómo se distribuyen los binarios de `kling` y cómo crear una nueva release.

## Visión general

Hay dos canales de distribución:

1. **Binarios pre-compilados** desde [Releases](https://github.com/juan52878911/kindling/releases).
   Es lo que usa la gente. Se compilan automáticamente con GitHub Actions al
   pushear un tag.
2. **`make install`** desde fuentes. Para quien quiere la última versión entre
   releases, o desarrolla el proyecto.

El binario CLI es el mismo para macOS y Linux. El **daemon** es solo Linux
(requiere KVM). El **bridge** también es solo Linux (corre dentro de las
microVMs Alpine, estático musl-safe).

## Plataformas soportadas

| OS      | Arch  | CLI | Daemon | Bridge | Notas |
|---------|-------|-----|--------|--------|-------|
| Linux   | amd64 | ✓   | ✓      | ✓      | El target "normal" — Proxmox, servidores, dev boxes |
| Linux   | arm64 | ✓   | ✓      | ✓      | AWS Graviton, Raspberry Pi 5 con KVM |
| macOS   | amd64 | ✓   | —      | —      | Intel Macs (raro hoy) |
| macOS   | arm64 | ✓   | —      | —      | Apple Silicon — el target de desarrollo |
| Windows | amd64 | —   | —      | —      | **No soportado** (usa `syscall.Kill`, `Setsid`, `Stat_t`) |

## Crear una nueva release

El flujo normal es:

```sh
# 1. Asegúrate de que main está al día y limpio
git checkout main
git pull --rebase
git status    # debe estar limpio

# 2. Edita CHANGELOG.md — añade un bloque para la nueva versión con
#    "Novedades", "Arreglos" y "Limitaciones conocidas".

# 3. Commit el changelog
git add CHANGELOG.md
git commit -m "changelog: preparar vX.Y.Z"

# 4. Crea y pushea el tag (esto activa el workflow)
./scripts/release.sh vX.Y.Z --wait
```

`scripts/release.sh --wait` espera a que termine el workflow y abre la
release en el browser. Si prefieres hacerlo en pasos:

```sh
git tag -a vX.Y.Z -m "release vX.Y.Z — ver CHANGELOG.md"
git push origin vX.Y.Z

# Mira el progreso en:
# https://github.com/juan52878911/kindling/actions

# Cuando termine, la release queda en:
# https://github.com/juan52878911/kindling/releases/tag/vX.Y.Z
```

El workflow (`/.github/workflows/release.yml`) detecta el tag, compila
para las cuatro plataformas, genera `SHA256SUMS`, extrae el bloque de
`CHANGELOG.md` para usarlo como notas, y publica el release con todos los
binarios como assets.

## Instalación por parte del usuario final

```sh
# macOS / Linux — última estable
curl -fsSL https://raw.githubusercontent.com/juan52878911/kindling/main/scripts/install.sh | sh

# Versión concreta
curl -fsSL .../install.sh | sh -s -- --tag v0.1.0

# Con bridge (necesario si vas a hacer `make deploy` desde esta máquina)
curl -fsSL .../install.sh | sh -s -- --bridge

# Prefijo personalizado (ej. instalación de sistema)
curl -fsSL .../install.sh | sh -s -- --prefix /usr/local --bridge
```

El instalador:
1. Detecta OS/arch (`linux/darwin` × `amd64/arm64`).
2. Descarga el binario y `SHA256SUMS`.
3. Verifica el checksum antes de tocar el disco.
4. Si se pasa `--bridge` y es Linux, descarga también `kling-bridge-<plat>`.
5. Mueve los binarios a `--prefix` (por defecto `~/.local/bin`).

## Versionado

Seguimos SemVer con prefijo `v`:

- **MAJOR** (1.0.0) — cambios incompatibles de CLI o API del daemon.
- **MINOR** (0.2.0) — funcionalidades nuevas compatibles.
- **PATCH** (0.1.1) — bugfixes.

El estado actual es **0.x** porque la API puede cambiar. Pasamos a 1.0.0
cuando la API del daemon y el formato de snapshots se estabilicen.

## Qué NO automatizamos (todavía)

- **Firma criptográfica de los binarios.** Hay herramientas (cosign, sigstore)
  pero añade complejidad que no compensa hasta que alguien pida verificar
  firmas. `SHA256SUMS` cubre la verificación de integridad.
- **Publicación en brew/scoop/aur.** Cada uno tiene su propio proceso de
  revisión. El script `install.sh` cubre el caso general; los packages de
  sistema son nice-to-have, no requisito.
- **Binarios de debug con símbolos.** `go build -trimpath -ldflags "-s -w"`
  quita la tabla de símbolos para hacer el binario pequeño. Si necesitas
  debug, compila desde fuentes: `go build ./cmd/kling`.