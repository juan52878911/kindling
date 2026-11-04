# Compatibilidad

Qué promete kindling 1.x. Este documento es el **qué**: es normativo, y si el
código no lo cumple el fallo es del código. El **cómo** (qué hace
`kling upgrade` con cada fichero, cómo volver atrás) está en
[`actualizar.md`](actualizar.md). Hasta 1.0 rige lo que dice ese documento para
0.x; lo que hoy no se cumple está en [Antes de 1.0](#antes-de-10).

---

## Alcance

Hay tres niveles. **Lo que no está en la lista de estable o experimental es
interno**, aunque se pueda usar.

| Nivel | Qué significa |
|---|---|
| **Estable** | cubierto por SemVer (abajo): solo cambia de forma incompatible en una MAJOR, y antes pasa por la deprecación |
| **Experimental** | puede cambiar o desaparecer en una MINOR, siempre con su línea en el CHANGELOG. Se usa bajo tu riesgo |
| **Interno** | sin ninguna garantía; puede cambiar en un PATCH |

**Estable:**

- El núcleo: `kling` (CLI y daemon), `kling-guest` y `kling-vz`.
- La API HTTP del daemon: rutas, campos JSON, capacidades de `GET /info`,
  códigos de estado documentados en [`api.md`](api.md) y las cabeceras
  `X-Kling-API`/`X-Kling-Version`.
- La CLI del núcleo: comandos y flags de `kling help all`, la salida `-json`
  (son los tipos de `pkg/api`), los códigos de salida y las variables de
  cliente `KLING_HOST`, `KLING_CONFIG` y `KLING_AUTHZ_TOKEN`.
- Go: **solo** `pkg/api`, `pkg/plugin` y `pkg/config`.
- Los formatos en disco versionados: `state.json`, `meta.json` de los dorados,
  `credentials.enc` (`KLCS`), `publish.json`, `fondos.json`, el sello
  `volcado.ok`, los grafos (`format`), la configuración del CLI (`schema`) y
  MMDS (`kling_mmds`).
- Los protocolos: daemon ↔ agente (`/healthz` con `caps`, parámetros `kling.*`),
  carpetas compartidas `kling-share/1`, extensiones (`KLING_PLUGIN_API=1`,
  manifiesto v2) y MMDS (`env`, `machine_env`, `machine_cmd`).
- Las imágenes publicadas en cada release: el kernel de invitado
  (`vmlinux-<arch>.zst`) y `min` (`min-<arch>.ext4.zst`).
- Las extensiones **mcp** (con `kling-bridge`) y **sandbox**.

**Experimental:** `db`, `phone`, `ai` y Chispa, Android, el operador de
Kubernetes, `compose` (el subconjunto de Compose que se acepta) y **kling-hv**
(como VMM, y su formato de volcado `KLNGHV`).

**Interno:** el resto de `pkg/` (`guest`, `credproxy`, `scheduler`,
`linkbroker`, `durable`, `esquema`, `lazyre`, `panico`, `units`…: pasará a
`internal/`), `internal/`, los comandos ocultos (`dial-stdio`, `builder`,
`upgrade -schemas`), las variables `KLING_VZ_*`, `KLING_HV_*` y `KLING_TEST_*`,
la salida para humanos de la CLI (columnas, textos, colores: para scripts,
`-json`), el texto de los mensajes de error (para decidir, el código), y los
ficheros de caché (`manifests.json`, la caché OCI).

---

## SemVer en 1.x

Una sola versión para todo el repo. En lo estable:

**MAJOR** (2.0) es necesaria para:

- quitar o cambiar de forma o de sentido una ruta, un campo JSON, un código
  de estado documentado, una capacidad, un comando, un flag, un campo de
  `-json`, un código de salida o una clave de configuración;
- quitar o cambiar un símbolo exportado de `pkg/api`, `pkg/plugin` o
  `pkg/config`;
- subir `APIVersion`, `KLING_PLUGIN_API`, `kling-share/N` o el mínimo de
  `manifest_version` que se acepta;
- dejar de leer un `schema` anterior de un formato estable;
- exigir al agente de invitado una capacidad sin la que antes se degradaba.

**MINOR** (1.x) puede:

- añadir rutas, campos (`omitempty`), capacidades, comandos, flags, códigos de
  salida nuevos para casos nuevos, `caps` del agente y opcodes de
  `kling-share`;
- subir el `schema` de un formato, **siempre** con migración desde el anterior
  y su copia `.v<N>.bak`;
- escribir una versión nueva de `kling_vz` o `KLNGHV`, siempre la más baja que
  describe la máquina;
- deprecar (abajo), y cambiar lo experimental.

**PATCH** (1.x.y): arreglos. Ningún formato cambia de versión, así que se puede
ir y volver entre parches sin copias.

Un campo nuevo persistido que un binario anterior **no debe perder** sube el
`schema` de su fichero (eso convierte la pérdida silenciosa en un rechazo). Uno
que sí puede perder se declara ignorable en
`internal/machine/testdata/esquema/campos-persistidos.txt` con su test.

### Dorados y congeladas son caché

Los dorados (`kling save`, plantillas) y las máquinas congeladas guardan memoria
de una VM, ligada al VMM, al kernel, a la arquitectura y, en el Mac, a la build
de macOS. **No se promete restaurarlos entre MINOR.** Lo que se promete:

1. **Detección**: uno que el host ya no puede restaurar sale `stale` con la causa
   y la orden para rehacerlo, antes de llegar al VMM; nunca un fallo crudo al
   despertar.
2. **Recongelación**: `kling upgrade` congela otra vez, con el VMM que lo vaya a
   despertar, lo que el cambio dejaría ilegible, o se niega antes de tocar nada.

Los datos (volúmenes, discos de máquinas paradas, imágenes) sí se conservan.

---

## Deprecación

Para quitar algo estable:

1. Se anuncia en el CHANGELOG, sección `### Deprecated`, con la versión a partir
   de la cual puede desaparecer.
2. Avisa al usarse: en stderr (nunca en la salida `-json` de stdout), en
   `kling doctor` si es configuración, y con `// Deprecated:` en Go.
3. Sigue funcionando **al menos 2 MINOR o 6 meses, lo que dure más**.
4. Se quita solo en una MAJOR.

Lo experimental no necesita deprecación: basta la línea en el CHANGELOG.

**En 1.0** se retiraron los alias que avisaban desde v0.18: `rmi`, `images`,
`volumes`, `sandboxes`, `info`, `resize`, `squeeze`, `mmds`, `models`, `chispa`
y los verbos sueltos de MCP `add`, `search`, `gateway`, `export`, `memory`,
`migrate`, y los flags `run -cpu` y `ai chispa deploy -vcpus`. Salen con el
código 2 y dicen el nombre nuevo. Se quedan para siempre, en silencio,
`commit`, `snapshots` y `plugins`.

---

## Ventana de versiones

| Mezcla | Promesa |
|---|---|
| CLI o extensión N con daemon N-1 o N+1 | funciona si tienen la misma `APIVersion`; lo que el daemon no anuncie se dice ("actualiza el daemon"), no se adivina |
| `kling upgrade` de N-1 a N | garantizado y probado en cada release; de más atrás, encadenando, sin prueba |
| `kling upgrade -rollback` de N a N-1 | garantizado: binarios, copias `.v<N>.bak` y recongelación |
| imagen con agente ≥ 1.0 en cualquier daemon 1.x | funciona; lo que el agente no sepa se degrada, nunca es un fallo duro ni un pánico |
| imagen con agente de v0.18 a 0.x | funciona con los requisitos de abajo; lo demás se degrada, sin garantía |
| imagen con agente anterior a v0.18 | **fuera del contrato**: reimportarla |

**Requisitos del agente.** Tres peticiones necesitan una capacidad del agente
(`caps` de `/healthz`) y **no se degradan**: un servicio que arranca sin su
entorno o con otro comando parecería sano y no lo es.

| Petición | Capacidad | Agente mínimo |
|---|---|---|
| entorno (`kling run -e`, `RunRequest.Env`) | `env` | v0.18 |
| comando (`RunRequest.Cmd`/`Entrypoint`: imágenes Docker, `kling compose`) | `cmd` | v0.19 |
| esperar a los volúmenes (`RunRequest.VolumeWait`) | `volume-wait` | v0.19 |

Con un agente que no la anuncia, el daemon da la máquina por fallida en cuanto
el agente contesta (`failed`, con un error que pide reimportar la imagen); si
la máquina sale de un dorado cuyo agente ya se conoce, `kling run -e` sale
además con el código 5 ([códigos de salida](guia.md#códigos-de-salida)).
`kling doctor` e `image ls` señalan antes las imágenes de agente viejo. Todo
agente ≥ 1.0 tiene las tres.

---

## Matriz de compatibilidad

"Hoy" dice si la rama de 1.0 ya lo cumple; lo pendiente está en
[Antes de 1.0](#antes-de-10).

| Eje | Soportado en 1.x | Mecanismo | Hoy |
|---|---|---|---|
| CLI ↔ daemon | misma `APIVersion` (1). Aviso si el daemon es más nuevo; error si es más viejo que `MinDaemonAPI` | `X-Kling-API` en cada respuesta, `api` en `/info`; goldens de rutas, códigos de estado, tags JSON y `-json` (`internal/contrato`) | **sí** |
| Extensión ↔ daemon | `min_kling` ≤ versión del núcleo; `max_api` ≥ API del daemon | manifiesto v2 | **sí** `min_kling` (error); `max_api` solo avisa |
| Daemon ↔ agente | ver la ventana de arriba | `caps` de `/healthz`; `kling.*` desconocidos se ignoran; `agent` en `recipe.json` | **sí**, con los [requisitos del agente](#ventana-de-versiones) |
| Daemon ↔ Firecracker | la MAJOR.MINOR fijada por cada release (hoy 1.17); otra deja los dorados `stale` | `vmm` en `meta.json` y en el sello | **sí** |
| Daemon ↔ kling-vz | la misma versión que el daemon: `kling upgrade` los cambia juntos | `kling_vz` del volcado (1-3): lo que este no lee sale `stale` | **sí** |
| Daemon ↔ kling-hv (experimental) | la misma versión que el daemon | `KLNGHV` (cabecera v2, lee v1, fijada por `TestCabeceraKLNGHV`); `stale` y `kling upgrade` la comprueban | **sí** |
| macOS | kling-vz ≥ 14.0, kling-hv ≥ 15.2. El `mem.file` de vz está ligado a la build de macOS: si no restaura, se rehace | `minos` comprobado en CI; `macos` en el meta | **sí** |
| Kernel de invitado | cambiarlo avisa y el dorado sigue (el kernel vive en su memoria) | `kernel_sha256` en el meta | **sí**; **pendiente** que salga en `doctor` |
| Arquitectura | amd64 y arm64; los dorados no se mueven entre ellas | `arch` en el meta y en el sello: otra sale `stale` | **sí** |
| Host Linux | KVM, ext4/XFS/btrfs (reflink opcional), systemd para `kling upgrade` | `kling doctor` | **sí** |
| Formatos en disco | los estables se leen de N-1 y se rechazan los del futuro sin pisarlos | `schema`, `.v<N>.bak` (`pkg/esquema`); fijaciones de v0.17 a v0.20 y `campos-persistidos.txt` en `internal/machine/testdata/esquema/` | **sí** |
| Go API | `pkg/api`, `pkg/plugin`, `pkg/config` | SemVer de Go; `scripts/apidiff.sh` en CI contra la etiqueta anterior | **sí** |

---

## Antes de 1.0

Lo que falta para que este documento sea verdad. Sale del inventario de
contratos del 2026-10-28; cuando se cumpla una línea, se quita de aquí y su fila
de la matriz pasa a **sí**.

**Go**

- Mover a `internal/` el `pkg/` que no es público (todo menos `pkg/api`,
  `pkg/plugin` y `pkg/config`).

**VMM y agente**

- `kling doctor` muestra el `kernel_sha256` del kernel instalado y avisa de los
  dorados congelados con otro.

**Pruebas y release**

- e2e de `kling upgrade` desde la release N-1 real (v0.20.1; hoy desde v0.17 y
  con `-from-dir -insecure`), en Linux y en el Mac, incluido kling-hv.
- e2e con microVMs en un runner Apple Silicon (el de Linux con KVM ya corre en
  `e2e-linux.yml`).
- Test cruzado de la firma con el `minisign` de verdad: lo que firma
  `internal/upgrade/firmar` lo verifica `minisign -V`, y al revés.
- El secreto `KLING_RELEASE_SIGNING_KEY` en el environment `release`
  ([`firmas.md`](firmas.md)): desde v1.0.0 `kling upgrade` rechaza una release
  sin firmar.
- Notarización de los binarios de macOS.
- El tap de Homebrew que publique la fórmula que ya genera `release.yml`.
