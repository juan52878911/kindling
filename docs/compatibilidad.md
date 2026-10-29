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
  `credentials.enc` y los que se vayan versionando (ver
  [Antes de 1.0](#antes-de-10)).
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

**Al entrar en 1.0** se retiran los alias que hoy avisan "se retirarán":
`rmi`, `images`, `volumes`, `sandboxes`, `info`, `resize`, `squeeze`, `mmds`,
`models`, `chispa` y los verbos sueltos de MCP `add`, `search`, `gateway`,
`export`, `memory`, `migrate` (`cmd/kling/tree.go`). Se quedan para siempre, en
silencio, `commit`, `snapshots` y `plugins`.

---

## Ventana de versiones

| Mezcla | Promesa |
|---|---|
| CLI o extensión N con daemon N-1 o N+1 | funciona si tienen la misma `APIVersion`; lo que el daemon no anuncie se dice ("actualiza el daemon"), no se adivina |
| `kling upgrade` de N-1 a N | garantizado y probado en cada release; de más atrás, encadenando, sin prueba |
| `kling upgrade -rollback` de N a N-1 | garantizado: binarios, copias `.v<N>.bak` y recongelación |
| imagen con agente ≥ 1.0 en cualquier daemon 1.x | funciona; lo que el agente no sepa se degrada, nunca es un fallo duro ni un pánico |
| imagen con agente de v0.18 a 0.x | se intenta degradar, sin garantía; hoy hay tres fallos duros conocidos (abajo) |
| imagen con agente anterior a v0.18 | **fuera del contrato**: reimportarla |

---

## Matriz de compatibilidad

"Hoy" dice si v0.20 ya lo cumple; lo pendiente está en
[Antes de 1.0](#antes-de-10).

| Eje | Soportado en 1.x | Mecanismo | Hoy |
|---|---|---|---|
| CLI ↔ daemon | misma `APIVersion` (1). Aviso si el daemon es más nuevo; error si es más viejo que `MinDaemonAPI` | `X-Kling-API` en cada respuesta, `api` en `/info` | **sí** el mecanismo; **pendiente** que la regla de `APIVersion` cubra códigos de estado y semántica, y los goldens que la vigilen |
| Extensión ↔ daemon | `min_kling` ≤ versión del núcleo; `max_api` ≥ API del daemon | manifiesto v2 | **sí** `min_kling` (error); `max_api` solo avisa |
| Daemon ↔ agente | ver la ventana de arriba | `caps` de `/healthz`; `kling.*` desconocidos se ignoran | **pendiente**: `-e`, `cmd`/`entrypoint` y `volume_wait` con un agente que no los tiene dan la máquina por fallida ("reimport the image") |
| Daemon ↔ Firecracker | la MAJOR.MINOR fijada por cada release (hoy 1.17); otra deja los dorados `stale` | `vmm` en `meta.json` y en el sello | **sí** |
| Daemon ↔ kling-vz | la misma versión que el daemon: `kling upgrade` los cambia juntos | `kling_vz` del volcado (1-3) | **sí** el volcado; **pendiente** comparar la versión de `/kling/info` |
| Daemon ↔ kling-hv (experimental) | la misma versión que el daemon | `KLNGHV` (cabecera v2, lee v1) | **pendiente**: `upgrade` no mira `KLNGHV`, el daemon no traduce su error, la cabecera C no tiene test |
| macOS | kling-vz ≥ 14.0, kling-hv ≥ 15.2. El `mem.file` de vz está ligado a la build de macOS: si no restaura, se rehace | `minos` comprobado en CI; `macos` en el meta | **sí** |
| Kernel de invitado | cambiarlo avisa y el dorado sigue (el kernel vive en su memoria) | `kernel_sha256` en el meta | **sí**; **pendiente** que salga en `doctor` |
| Arquitectura | amd64 y arm64; los dorados no se mueven entre ellas | ninguno | **pendiente**: la arquitectura no cuenta para `stale` |
| Host Linux | KVM, ext4/XFS/btrfs (reflink opcional), systemd para `kling upgrade` | `kling doctor` | **sí** |
| Formatos en disco | los estables se leen de N-1 y se rechazan los del futuro sin pisarlos | `schema`, `.v<N>.bak` (`pkg/esquema`) | **sí** `state.json`, `meta.json`, `credentials.enc`; **pendiente** el resto y la disciplina de subir versión |
| Go API | `pkg/api`, `pkg/plugin`, `pkg/config` | SemVer de Go | **pendiente** `apidiff` en CI |

---

## Antes de 1.0

Lo que hay que hacer para que este documento sea verdad. Sale del inventario de
contratos del 2026-10-28; cuando se cumpla una línea, se quita de aquí y su fila
de la matriz pasa a **sí**.

**API y CLI**

- Golden de la tabla de rutas (método, patrón, acción de authz), de los códigos
  de estado por ruta y de los tags JSON de todo `pkg/api` (hoy solo
  `Machine`, `Snapshot` y `CredentialSpec`), y un test que exija subir
  `APIVersion` o la MAJOR si cambian.
- Arreglar las incoherencias de códigos de estado (los `400` para "no existe" o
  para errores del host, `DELETE /volumes/{name}` siempre `409`,
  `DELETE /sandboxes/{ref}` siempre `500`, `507` para disco lleno en
  volúmenes y en la caché OCI…) y fijar la tabla de
  [`api.md`](api.md#códigos-de-estado-por-ruta).
- Un `code` estable en `api.Error`; que `IsMachineLimit`/`IsTenantQuota` dejen
  de comparar el texto del mensaje.
- Capacidad y documentación para `GET/PUT /snapshots/{name}/pool` y
  `GET /machines/{ref}/egress`.
- Golden de la salida `-json` de cada comando; códigos de salida documentados
  (hoy 0 bien, 1 error, 2 uso, el del proceso remoto en `exec`, 130
  interrumpido) y específicos para daemon incompatible, dorado obsoleto y agente
  sin capacidad.
- `kling status -v` y `kling version` muestran API, VMM y agente; el árbol de
  ayuda de `compose` coincide con lo que acepta.
- Retirar los alias avisados (ver [Deprecación](#deprecación)) y decidir los
  flags deprecados (`run -cpu`, `chispa deploy -vcpus`).

**Formatos en disco**

- Versionar `publish.json`, `fondos.json`, el sello `volcado.ok`, los grafos
  (`format`), la configuración del CLI (`schema`, y conservar claves desconocidas
  al guardar) y MMDS.
- Copia `.v1.bak` al pasar `state.json` de 1 a 2; `credentials.enc` a `KLCS` 2 por
  los campos de SigV4 (o declararlos ignorables con test); decidir qué hacer
  con los campos de v0.19/v0.20 que se añadieron sin subir el `schema`.
- Fijaciones de v0.18, v0.19 y v0.20 en `internal/machine/testdata/esquema/`.
- Daemon estricto (o con eco de lo ignorado) en `POST /graphs`.

**VMM y agente**

- `stale` con arquitectura y con la versión de kling-vz/kling-hv; traducir el
  error de `KLNGHV`, test de su cabecera y que `kling upgrade` la compruebe.
- `kling doctor` e `image ls` señalan agentes viejos; la versión del agente va
  en `recipe.json`.
- Degradar en vez de fallar con `-e`, `cmd` y `volume_wait` en agentes
  anteriores, o dejarlo como requisito documentado de la ventana.

**Pruebas y release**

- e2e de `kling upgrade` desde la release N-1 real (hoy fijado a v0.17 y con
  `-from-dir -insecure`), en Linux y en el Mac, incluido kling-hv.
- e2e con microVMs en CI (Linux con KVM y un runner Apple Silicon).
- `apidiff` para `pkg/api|plugin|config`; mover el resto de `pkg/` a
  `internal/`.
- Firma de `SHA256SUMS` verificada por `kling upgrade`, `install.sh` y la
  fórmula, atestación de procedencia en `release.yml` y notarización de los
  binarios de macOS.
- `prerelease` para las `-rc` (que `kling upgrade` no las tome como la
  última).
